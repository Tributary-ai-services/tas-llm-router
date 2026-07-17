package responsecache

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func f32(v float32) *float32 { return &v }
func iptr(v int) *int        { return &v }

func baseReq() *types.ChatRequest {
	return &types.ChatRequest{
		Model:       "claude-opus-4-8",
		Messages:    []types.Message{{Role: "user", Content: "hello"}},
		Temperature: f32(0),
	}
}

func TestKeyHash_DeterministicAndSensitive(t *testing.T) {
	r1 := baseReq()
	r2 := baseReq()
	h1 := KeyHash("tenant-a", "anthropic", r1, "clear-v1")
	h2 := KeyHash("tenant-a", "anthropic", r2, "clear-v1")
	if h1 != h2 {
		t.Fatalf("identical requests must hash equal: %s != %s", h1, h2)
	}

	// Each output-affecting axis must move the hash.
	cases := map[string]func(*types.ChatRequest){
		"tenant":  nil, // handled separately below
		"model":   func(r *types.ChatRequest) { r.Model = "claude-sonnet-5" },
		"message": func(r *types.ChatRequest) { r.Messages[0].Content = "hello!" },
		"temp":    func(r *types.ChatRequest) { r.Temperature = f32(0.7) },
		"maxtok":  func(r *types.ChatRequest) { r.MaxTokens = iptr(64) },
		"seed":    func(r *types.ChatRequest) { r.Seed = iptr(7) },
		"stop":    func(r *types.ChatRequest) { r.Stop = []string{"\n"} },
	}
	for name, mut := range cases {
		if mut == nil {
			continue
		}
		r := baseReq()
		mut(r)
		if got := KeyHash("tenant-a", "anthropic", r, "clear-v1"); got == h1 {
			t.Errorf("%s change did not alter the hash", name)
		}
	}

	// Tenant, vendor, and scoring_version are all in the key.
	if KeyHash("tenant-b", "anthropic", baseReq(), "clear-v1") == h1 {
		t.Error("tenant must alter the hash (isolation)")
	}
	if KeyHash("tenant-a", "openai", baseReq(), "clear-v1") == h1 {
		t.Error("vendor must alter the hash")
	}
	if KeyHash("tenant-a", "anthropic", baseReq(), "clear-v2") == h1 {
		t.Error("scoring_version must alter the hash")
	}
}

func TestKeyHash_IgnoresNonOutputFields(t *testing.T) {
	r := baseReq()
	base := KeyHash("t", "anthropic", r, "v1")
	r.ID = "req-123"
	r.UserID = "user-9"
	r.ApplicationID = "app-x"
	r.Timestamp = time.Unix(999, 0)
	r.Stream = false
	if KeyHash("t", "anthropic", r, "v1") != base {
		t.Error("non-output-affecting fields must not change the hash")
	}
}

func TestDecide(t *testing.T) {
	cfg := Config{Enabled: true, RequireDeterministic: true}

	if d := Decide(baseReq(), cfg, ""); !d.Cacheable || d.Bypass {
		t.Fatalf("deterministic req should be cacheable: %+v", d)
	}
	for _, h := range []string{"off", "bypass", "No-Cache", " no-store "} {
		if d := Decide(baseReq(), cfg, h); d.Cacheable || !d.Bypass {
			t.Errorf("header %q should bypass: %+v", h, d)
		}
	}
	// Streaming, tools, functions never cache.
	rs := baseReq()
	rs.Stream = true
	if Decide(rs, cfg, "").Cacheable {
		t.Error("streaming must not be cacheable")
	}
	rt := baseReq()
	rt.Tools = []types.Tool{{Type: "function"}}
	if Decide(rt, cfg, "").Cacheable {
		t.Error("tool request must not be cacheable")
	}
	// Non-deterministic gated by RequireDeterministic.
	rn := baseReq()
	rn.Temperature = f32(0.9)
	if Decide(rn, cfg, "").Cacheable {
		t.Error("temperature>0 must not cache when RequireDeterministic")
	}
	rn.Seed = iptr(3) // a seed pins sampling → deterministic again
	if !Decide(rn, cfg, "").Cacheable {
		t.Error("seed should make it cacheable")
	}
	// Disabled config caches nothing (but a bypass header still reads as bypass).
	if Decide(baseReq(), Config{Enabled: false}, "").Cacheable {
		t.Error("disabled config must not cache")
	}
	if !Decide(baseReq(), Config{Enabled: false}, "off").Bypass {
		t.Error("bypass header should register even when disabled")
	}
	// nil-temperature default is treated as non-deterministic.
	rd := baseReq()
	rd.Temperature = nil
	if Decide(rd, cfg, "").Cacheable {
		t.Error("nil temperature (vendor default sampling) must not cache under RequireDeterministic")
	}
	// RequireDeterministic=false caches regardless of sampling.
	if !Decide(rd, Config{Enabled: true, RequireDeterministic: false}, "").Cacheable {
		t.Error("RequireDeterministic=false should cache nondeterministic req")
	}
}

func entryFor(text string) *Entry {
	resp := types.ChatResponse{
		ID:      "resp-1",
		Choices: []types.Choice{{Message: types.Message{Role: "assistant", Content: text}}},
	}
	b, _ := json.Marshal(resp)
	return &Entry{Response: b, Vendor: "anthropic", Model: "claude-opus-4-8", StoredAtUnix: 1}
}

// cacheContract exercises any Cache implementation.
func cacheContract(t *testing.T, c Cache) {
	t.Helper()
	ctx := context.Background()

	if _, ok, _ := c.Get(ctx, "t1", "h1"); ok {
		t.Fatal("empty cache should miss")
	}
	if err := c.Set(ctx, "t1", "h1", entryFor("answer"), time.Minute); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, ok, err := c.Get(ctx, "t1", "h1")
	if err != nil || !ok {
		t.Fatalf("expected hit: ok=%v err=%v", ok, err)
	}
	if string(got.Response) != string(entryFor("answer").Response) {
		t.Errorf("round-trip mismatch: %s", got.Response)
	}

	// Tenant isolation: same hash, different tenant, must miss.
	if _, ok, _ := c.Get(ctx, "t2", "h1"); ok {
		t.Error("tenant isolation violated — t2 read t1's entry")
	}

	// PurgeTenant scopes to the tenant.
	if err := c.Set(ctx, "t1", "h2", entryFor("b"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(ctx, "t2", "h9", entryFor("c"), time.Minute); err != nil {
		t.Fatal(err)
	}
	n, err := c.PurgeTenant(ctx, "t1")
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 purged, got %d", n)
	}
	if _, ok, _ := c.Get(ctx, "t1", "h1"); ok {
		t.Error("t1 entry survived purge")
	}
	if _, ok, _ := c.Get(ctx, "t2", "h9"); !ok {
		t.Error("purge of t1 removed t2's entry")
	}
}

func TestMemoryCache_Contract(t *testing.T) {
	cacheContract(t, NewMemoryCache(0))
}

func TestMemoryCache_TTLExpiry(t *testing.T) {
	c := NewMemoryCache(0)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	timeNow = func() time.Time { return now }
	defer func() { timeNow = time.Now }()

	if err := c.Set(ctx, "t", "h", entryFor("x"), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Get(ctx, "t", "h"); !ok {
		t.Fatal("should hit before TTL")
	}
	now = now.Add(31 * time.Second)
	if _, ok, _ := c.Get(ctx, "t", "h"); ok {
		t.Error("should miss after TTL")
	}
}

func TestMemoryCache_Eviction(t *testing.T) {
	c := NewMemoryCache(2)
	ctx := context.Background()
	for _, h := range []string{"a", "b", "c"} {
		if err := c.Set(ctx, "t", h, entryFor(h), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	c.mu.Lock()
	n := len(c.m)
	c.mu.Unlock()
	if n > 2 {
		t.Errorf("eviction failed: %d entries, cap 2", n)
	}
}

func TestResponseCacheable(t *testing.T) {
	ok := &types.ChatResponse{Choices: []types.Choice{{Message: types.Message{Content: "hi"}}}}
	if !ResponseCacheable(ok) {
		t.Error("plain response should be cacheable")
	}
	if ResponseCacheable(nil) || ResponseCacheable(&types.ChatResponse{}) {
		t.Error("nil / no-choice response must not be cacheable")
	}
	tool := &types.ChatResponse{Choices: []types.Choice{{Message: types.Message{ToolCalls: []types.ToolCall{{ID: "x"}}}}}}
	if ResponseCacheable(tool) {
		t.Error("response with tool calls must not be cacheable")
	}
}

func TestPendingContext(t *testing.T) {
	ctx := context.Background()
	if _, ok := PendingFromContext(ctx); ok {
		t.Fatal("empty ctx should have no pending")
	}
	ctx = WithPending(ctx, "tenant-a", "hash-1")
	p, ok := PendingFromContext(ctx)
	if !ok || p.TenantID != "tenant-a" || p.Hash != "hash-1" {
		t.Fatalf("pending round-trip failed: %+v ok=%v", p, ok)
	}
}
