package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/responsecache"
)

func cacheTestServer() *Server {
	return &Server{
		logger:    logrus.New(),
		respCache: responsecache.NewMemoryCache(0),
		respCacheCfg: responsecache.Config{
			Enabled:              true,
			TTL:                  time.Minute,
			RequireDeterministic: true,
		},
	}
}

func newCacheReq(t *testing.T) (*httptest.ResponseRecorder, *http.Request, *types.ChatRequest) {
	t.Helper()
	req := &types.ChatRequest{
		Model:       "claude-opus-4-8",
		Messages:    []types.Message{{Role: "user", Content: "what is 2+2?"}},
		Temperature: f32p(0),
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Tenant-ID", "tenant-a")
	// Attach a Routing sidecar so cache_state / cache_key_hash stamps land.
	r = r.WithContext(middleware.WithRouting(r.Context(), middleware.NewRouting()))
	return httptest.NewRecorder(), r, req
}

func f32p(v float32) *float32 { return &v }

// Full lifecycle: first request misses and marks a pending store; storing the
// produced response makes an identical second request a hit that skips routing.
func TestResponseCache_MissStoreHit(t *testing.T) {
	s := cacheTestServer()
	vendor := "anthropic"

	// --- request 1: miss ---
	w1, r1, req := newCacheReq(t)
	got := s.maybeServeFromCache(w1, r1, req, vendor)
	if got == nil {
		t.Fatal("first request should miss, not serve a hit")
	}
	if snap := middleware.RoutingFromContext(got.Context()).Snapshot(); snap.CacheState != "miss" {
		t.Fatalf("expected cache_state=miss, got %q", snap.CacheState)
	}
	if _, ok := responsecache.PendingFromContext(got.Context()); !ok {
		t.Fatal("cacheable miss must stash a pending store intent")
	}

	// --- store the produced response (as the completion handler would) ---
	resp := &types.ChatResponse{
		ID:      "resp-1",
		Model:   "claude-opus-4-8",
		Choices: []types.Choice{{Message: types.Message{Role: "assistant", Content: "4"}, FinishReason: "stop"}},
		Usage:   &types.Usage{PromptTokens: 10, CompletionTokens: 1},
	}
	s.maybeStoreInCache(got, resp)

	// --- request 2: identical → hit, served without routing ---
	w2, r2, req2 := newCacheReq(t)
	got2 := s.maybeServeFromCache(w2, r2, req2, vendor)
	if got2 != nil {
		t.Fatal("second identical request should be a hit (nil return)")
	}
	snap := middleware.RoutingFromContext(r2.Context()).Snapshot()
	if snap.CacheState != "hit" {
		t.Fatalf("expected cache_state=hit, got %q", snap.CacheState)
	}
	// C2: the hit records the avoided tokens (from the stored entry). Cost may be
	// 0 if this vendor:model isn't priced, but the token counts must carry.
	if snap.CacheSavedPromptTokens != 10 || snap.CacheSavedCompletionTokens != 1 {
		t.Errorf("expected saved tokens 10/1 from the cached entry, got %d/%d",
			snap.CacheSavedPromptTokens, snap.CacheSavedCompletionTokens)
	}
	if snap.CacheSavedCostUSD < 0 {
		t.Errorf("saved cost must be non-negative, got %v", snap.CacheSavedCostUSD)
	}
	if w2.Header().Get("X-TAS-Cache") != "hit" {
		t.Errorf("hit should set X-TAS-Cache: hit, got %q", w2.Header().Get("X-TAS-Cache"))
	}
	var served types.ChatResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &served); err != nil {
		t.Fatalf("hit body not JSON: %v", err)
	}
	if len(served.Choices) != 1 || served.Choices[0].Message.Content != "4" {
		t.Fatalf("hit served wrong body: %+v", served)
	}
}

// A different tenant with the same prompt must not read another tenant's entry.
func TestResponseCache_TenantIsolation(t *testing.T) {
	s := cacheTestServer()
	_, r1, req := newCacheReq(t)
	s.maybeStoreInCache(r1.WithContext(responsecache.WithPending(r1.Context(), "tenant-a",
		responsecache.KeyHash("tenant-a", "anthropic", req, "", ""))),
		&types.ChatResponse{ID: "x", Model: "m", Choices: []types.Choice{{Message: types.Message{Content: "secret"}}}})

	// tenant-b, same prompt → miss.
	w, r2, req2 := newCacheReq(t)
	r2.Header.Set("X-Tenant-ID", "tenant-b")
	if got := s.maybeServeFromCache(w, r2, req2, "anthropic"); got == nil {
		t.Fatal("tenant-b must not hit tenant-a's entry")
	}
}

// TAS-Cache: bypass forces a fresh call and stamps bypass, even with a warm entry.
func TestResponseCache_HeaderBypass(t *testing.T) {
	s := cacheTestServer()
	// warm the cache
	_, r0, req := newCacheReq(t)
	got := s.maybeServeFromCache(nil, r0, req, "anthropic") // nil w is fine on a miss
	s.maybeStoreInCache(got, &types.ChatResponse{ID: "x", Model: "claude-opus-4-8",
		Choices: []types.Choice{{Message: types.Message{Content: "4"}}}})

	w, r, req2 := newCacheReq(t)
	r.Header.Set("TAS-Cache", "bypass")
	if s.maybeServeFromCache(w, r, req2, "anthropic") == nil {
		t.Fatal("bypass must not serve a hit even with a warm entry")
	}
	if snap := middleware.RoutingFromContext(r.Context()).Snapshot(); snap.CacheState != "bypass" {
		t.Fatalf("expected cache_state=bypass, got %q", snap.CacheState)
	}
}

// Default (InExperiments=false): an experiment-claimed request bypasses the
// cache so each variant's measurement reflects real variant calls (§7).
func TestResponseCache_ExperimentBypassByDefault(t *testing.T) {
	s := cacheTestServer()
	w, r, req := newCacheReq(t)
	middleware.StampExperiment(r.Context(), "exp-1", "A")
	got := s.maybeServeFromCache(w, r, req, "anthropic")
	if got == nil {
		t.Fatal("experiment request should not be served a hit")
	}
	if snap := middleware.RoutingFromContext(r.Context()).Snapshot(); snap.CacheState != "bypass" {
		t.Fatalf("expected cache_state=bypass, got %q", snap.CacheState)
	}
	if _, ok := responsecache.PendingFromContext(got.Context()); ok {
		t.Error("bypassed experiment request must not stash a store intent")
	}
}

// InExperiments=true: cache within the experiment, but variant A and B never
// share an entry (the key is variant-scoped).
func TestResponseCache_CacheWithinExperiment(t *testing.T) {
	s := cacheTestServer()
	s.respCacheCfg.InExperiments = true

	// Variant A miss → store.
	_, rA, reqA := newCacheReq(t)
	middleware.StampExperiment(rA.Context(), "exp-1", "A")
	gotA := s.maybeServeFromCache(nil, rA, reqA, "anthropic")
	if gotA == nil {
		t.Fatal("variant A should be a cacheable miss, not a bypass")
	}
	if snap := middleware.RoutingFromContext(rA.Context()).Snapshot(); snap.CacheState != "miss" {
		t.Fatalf("expected miss for variant A, got %q", snap.CacheState)
	}
	s.maybeStoreInCache(gotA, &types.ChatResponse{ID: "a", Model: "claude-opus-4-8",
		Choices: []types.Choice{{Message: types.Message{Content: "answer-A"}}}})

	// Variant B, same prompt → MISS (separate entry), not A's hit.
	wB, rB, reqB := newCacheReq(t)
	middleware.StampExperiment(rB.Context(), "exp-1", "B")
	gotB := s.maybeServeFromCache(wB, rB, reqB, "anthropic")
	if gotB == nil {
		t.Fatal("variant B must not read variant A's entry")
	}
	if snap := middleware.RoutingFromContext(rB.Context()).Snapshot(); snap.CacheState != "miss" {
		t.Fatalf("expected miss for variant B, got %q", snap.CacheState)
	}

	// Variant A again, same prompt → HIT (its own entry).
	wA2, rA2, reqA2 := newCacheReq(t)
	middleware.StampExperiment(rA2.Context(), "exp-1", "A")
	if s.maybeServeFromCache(wA2, rA2, reqA2, "anthropic") != nil {
		t.Fatal("variant A repeat should hit its own entry")
	}
	if snap := middleware.RoutingFromContext(rA2.Context()).Snapshot(); snap.CacheState != "hit" {
		t.Fatalf("expected hit for variant A repeat, got %q", snap.CacheState)
	}
}

// A tool-calling request is never cached (side-effecting).
func TestResponseCache_ToolRequestNotCached(t *testing.T) {
	s := cacheTestServer()
	w, r, req := newCacheReq(t)
	req.Tools = []types.Tool{{Type: "function", Function: types.Function{Name: "lookup"}}}
	got := s.maybeServeFromCache(w, r, req, "anthropic")
	if got == nil {
		t.Fatal("tool request should not be served from cache")
	}
	if _, ok := responsecache.PendingFromContext(got.Context()); ok {
		t.Error("tool request must not stash a store intent")
	}
}

// A response that comes back with tool calls is not stored even on a cacheable
// miss (defense in depth).
func TestResponseCache_ToolResponseNotStored(t *testing.T) {
	s := cacheTestServer()
	_, r, req := newCacheReq(t)
	got := s.maybeServeFromCache(nil, r, req, "anthropic")
	if got == nil {
		t.Fatal("expected miss")
	}
	toolResp := &types.ChatResponse{ID: "x", Model: "claude-opus-4-8",
		Choices: []types.Choice{{Message: types.Message{ToolCalls: []types.ToolCall{{ID: "c1"}}}}}}
	s.maybeStoreInCache(got, toolResp)

	// nothing should have been stored → next identical request misses.
	w2, r2, req2 := newCacheReq(t)
	if s.maybeServeFromCache(w2, r2, req2, "anthropic") == nil {
		t.Fatal("tool-call response must not have been cached")
	}
}
