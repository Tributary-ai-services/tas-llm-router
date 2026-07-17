package server

import (
	"context"
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/promptcache"
)

// userMsg/assistantMsg/tool live in prefix_chain_test.go; these add the shapes
// this file needs (a system turn, and a tool with description + schema).
func sysMsg(s string) types.Message { return types.Message{Role: "system", Content: s} }

func toolFull(name, desc string, params interface{}) types.Tool {
	return types.Tool{Type: "function", Function: types.Function{Name: name, Description: desc, Parameters: params}}
}

// Nothing cacheable → no hash. A bare chat has no stable prefix; hashing one
// anyway would alias every ad-hoc call onto a single key and report a reuse
// rate that is pure artifact.
func TestCachePrefixHash_EmptyWhenNothingCacheable(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *types.ChatRequest
	}{
		{"nil request", nil},
		{"no messages", &types.ChatRequest{Model: "claude-haiku-4-5"}},
		{"user-only, no tools", &types.ChatRequest{Model: "claude-haiku-4-5", Messages: []types.Message{userMsg("hi")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cachePrefixHash(tc.req); got != "" {
				t.Fatalf("cachePrefixHash = %q, want \"\" (nothing cacheable)", got)
			}
		})
	}
}

func TestCachePrefixHash_SystemAloneIsCacheable(t *testing.T) {
	req := &types.ChatRequest{Model: "m", Messages: []types.Message{sysMsg("you are a bot"), userMsg("hi")}}
	if cachePrefixHash(req) == "" {
		t.Fatal("system-only request produced no hash; a stable system prompt is the primary cacheable span (§4.1)")
	}
}

func TestCachePrefixHash_ToolsAloneIsCacheable(t *testing.T) {
	req := &types.ChatRequest{Model: "m", Messages: []types.Message{userMsg("hi")}, Tools: []types.Tool{toolFull("get_weather", "", nil)}}
	if cachePrefixHash(req) == "" {
		t.Fatal("tools-only request produced no hash; tools render first and are cacheable without a system prompt")
	}
}

// The volatile tail must not participate. This is the whole point of hashing
// the tools+system span rather than the request: if the user's question moved
// the hash, every request would look cold and the P0 number would read ~0%
// regardless of how cacheable the traffic actually is.
func TestCachePrefixHash_IgnoresNonSystemMessages(t *testing.T) {
	a := &types.ChatRequest{Model: "m", Messages: []types.Message{sysMsg("stable"), userMsg("question one")}}
	b := &types.ChatRequest{Model: "m", Messages: []types.Message{sysMsg("stable"), userMsg("a totally different question")}}
	if cachePrefixHash(a) != cachePrefixHash(b) {
		t.Fatal("a differing user turn changed the hash; the cacheable span ends at system, so the varying tail must not participate")
	}
}

func TestCachePrefixHash_GrowingConversationKeepsSamePrefix(t *testing.T) {
	turn1 := &types.ChatRequest{Model: "m", Messages: []types.Message{sysMsg("stable"), userMsg("q1")}}
	turn3 := &types.ChatRequest{Model: "m", Messages: []types.Message{
		sysMsg("stable"), userMsg("q1"),
		{Role: "assistant", Content: "a1"}, userMsg("q2"),
		{Role: "assistant", Content: "a2"}, userMsg("q3"),
	}}
	if cachePrefixHash(turn1) != cachePrefixHash(turn3) {
		t.Fatal("hash changed as the conversation grew; the tools+system span is invariant across turns, which is exactly why it caches well")
	}
}

// Model is in the hash because vendor caches are model-scoped: a switch is a
// full cold rebuild, not a hit. Our own cost-optimized routing can switch models
// mid-conversation (§5.1) — the measurement must expose that, not hide it.
func TestCachePrefixHash_ModelIsPartOfIdentity(t *testing.T) {
	a := &types.ChatRequest{Model: "claude-haiku-4-5", Messages: []types.Message{sysMsg("stable")}}
	b := &types.ChatRequest{Model: "claude-opus-4-6", Messages: []types.Message{sysMsg("stable")}}
	if cachePrefixHash(a) == cachePrefixHash(b) {
		t.Fatal("same hash across models; caches are model-scoped, so this would report a hit where the vendor would rebuild")
	}
}

// Tool array order is not semantic to the caller but IS to a byte-level prefix
// match. Sorting means reordering alone doesn't read as a miss.
func TestCachePrefixHash_ToolOrderDoesNotMatter(t *testing.T) {
	a := &types.ChatRequest{Model: "m", Tools: []types.Tool{toolFull("alpha", "", nil), toolFull("zeta", "", nil)}}
	b := &types.ChatRequest{Model: "m", Tools: []types.Tool{toolFull("zeta", "", nil), toolFull("alpha", "", nil)}}
	if cachePrefixHash(a) != cachePrefixHash(b) {
		t.Fatal("tool reordering changed the hash; array order is not semantic to the caller and would read as a spurious miss")
	}
}

// ...but tool *content* is. A changed schema or description changes the
// rendered prefix, so the vendor would rebuild and we must not call it a hit.
func TestCachePrefixHash_ToolContentMatters(t *testing.T) {
	base := &types.ChatRequest{Model: "m", Tools: []types.Tool{toolFull("t", "does a thing", map[string]any{"type": "object"})}}
	for _, tc := range []struct {
		name string
		req  *types.ChatRequest
	}{
		{"different name", &types.ChatRequest{Model: "m", Tools: []types.Tool{toolFull("other", "does a thing", map[string]any{"type": "object"})}}},
		{"different description", &types.ChatRequest{Model: "m", Tools: []types.Tool{toolFull("t", "does another thing", map[string]any{"type": "object"})}}},
		{"different schema", &types.ChatRequest{Model: "m", Tools: []types.Tool{toolFull("t", "does a thing", map[string]any{"type": "string"})}}},
		{"extra tool", &types.ChatRequest{Model: "m", Tools: []types.Tool{toolFull("t", "does a thing", map[string]any{"type": "object"}), toolFull("t2", "", nil)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if cachePrefixHash(base) == cachePrefixHash(tc.req) {
				t.Fatalf("%s produced the same hash; tools render into the cached prefix, so any change rebuilds it", tc.name)
			}
		})
	}
}

func TestCachePrefixHash_SystemContentMatters(t *testing.T) {
	a := &types.ChatRequest{Model: "m", Messages: []types.Message{sysMsg("prompt A")}}
	b := &types.ChatRequest{Model: "m", Messages: []types.Message{sysMsg("prompt B")}}
	if cachePrefixHash(a) == cachePrefixHash(b) {
		t.Fatal("differing system prompts produced the same hash")
	}
}

// Multiple system blocks concatenate in order — order is semantic here, since
// this is the rendered prefix, not a set.
func TestCachePrefixHash_SystemOrderMatters(t *testing.T) {
	a := &types.ChatRequest{Model: "m", Messages: []types.Message{sysMsg("first"), sysMsg("second")}}
	b := &types.ChatRequest{Model: "m", Messages: []types.Message{sysMsg("second"), sysMsg("first")}}
	if cachePrefixHash(a) == cachePrefixHash(b) {
		t.Fatal("reordered system blocks produced the same hash; system renders in order, so the byte prefix differs")
	}
}

// The separator exists so concatenation can't alias: ["ab","c"] must not equal
// ["a","bc"]. Without it the two render identically and we'd report a hit
// across genuinely different prefixes.
func TestCachePrefixHash_SystemConcatenationDoesNotAlias(t *testing.T) {
	a := &types.ChatRequest{Model: "m", Messages: []types.Message{sysMsg("ab"), sysMsg("c")}}
	b := &types.ChatRequest{Model: "m", Messages: []types.Message{sysMsg("a"), sysMsg("bc")}}
	if cachePrefixHash(a) == cachePrefixHash(b) {
		t.Fatal(`["ab","c"] and ["a","bc"] hashed the same; the separator must prevent concatenation aliasing`)
	}
}

// Params that don't render into the tools+system span must not split the
// prefix. Per the vendor's invalidation hierarchy these preserve the tools and
// system tiers; including them would under-report reuse — the exact opposite of
// agentFingerprint's purpose, which is why this is a separate hash.
func TestCachePrefixHash_IgnoresNonRenderingParams(t *testing.T) {
	f32 := func(v float32) *float32 { return &v }
	i := func(v int) *int { return &v }
	base := &types.ChatRequest{Model: "m", Messages: []types.Message{sysMsg("stable")}}
	varied := &types.ChatRequest{
		Model:       "m",
		Messages:    []types.Message{sysMsg("stable")},
		Temperature: f32(0.9),
		TopP:        f32(0.5),
		MaxTokens:   i(4096),
		Seed:        i(42),
		Stop:        []string{"END"},
	}
	if cachePrefixHash(base) != cachePrefixHash(varied) {
		t.Fatal("sampling/stop/max_tokens changed the hash; they don't render into the tools+system span, so splitting on them under-reports reuse")
	}
}

func TestCachePrefixHash_IsDeterministic(t *testing.T) {
	mk := func() *types.ChatRequest {
		return &types.ChatRequest{
			Model:    "m",
			Messages: []types.Message{sysMsg("stable"), userMsg("q")},
			Tools: []types.Tool{
				toolFull("b", "second", map[string]any{"type": "object", "zeta": 1, "alpha": 2}),
				toolFull("a", "first", map[string]any{"type": "object", "middle": 3}),
			},
		}
	}
	want := cachePrefixHash(mk())
	if want == "" {
		t.Fatal("empty hash")
	}
	// Re-hashing an equivalent request must be stable across runs — map-valued
	// schemas included (encoding/json sorts map keys, so this holds).
	for range 200 {
		if got := cachePrefixHash(mk()); got != want {
			t.Fatalf("non-deterministic hash: got %q want %q", got, want)
		}
	}
}

// End-to-end: the claim P0 rests on. Two DIFFERENT user questions sent against
// the same agent (same system prompt + tools) must register the second as a
// forgone cache read — that is precisely the traffic vendor prompt caching is
// for, and today we pay full price for it (§9.1: zero cached tokens in 30d).
//
// Exercises the real chain hash → probe rather than trusting the unit tests to
// compose: a bug in either half (a hash that moves with the question, a probe
// that doesn't recognize a recurrence) makes this fail.
func TestCachePrefixHash_ProbeSeesRepeatedAgentPrefix(t *testing.T) {
	ctx := context.Background()
	probe := promptcache.NewMemoryProbe(0)

	agent := func(question string) *types.ChatRequest {
		return &types.ChatRequest{
			Model: "claude-haiku-4-5",
			Messages: []types.Message{
				sysMsg("You are a support agent. Be concise."),
				userMsg(question),
			},
			Tools: []types.Tool{toolFull("lookup_order", "Look up an order", map[string]any{"type": "object"})},
		}
	}

	h1 := cachePrefixHash(agent("where is my order?"))
	seen, err := probe.Observe(ctx, "tenant-1", h1)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if seen {
		t.Fatal("first request reported a hit; nothing has been observed yet")
	}

	// A different question, same agent. The vendor would serve tools+system
	// from cache; we currently pay full price for it.
	h2 := cachePrefixHash(agent("cancel my order please"))
	seen, err = probe.Observe(ctx, "tenant-1", h2)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !seen {
		t.Fatal("second request against the same agent reported cold; this is the forgone cache read P0 exists to count")
	}

	// A different tenant with a byte-identical prefix is NOT a hit — vendor
	// caches are per-account. Counting it would inflate the rollout number.
	if seen, _ := probe.Observe(ctx, "tenant-2", h2); seen {
		t.Fatal("same prefix under another tenant reported a hit; vendor caches are per-account")
	}

	// Routing the same agent to another model is a cold rebuild, not a hit
	// (§5.1) — the measurement must expose that tension, not mask it.
	other := agent("where is my order?")
	other.Model = "claude-opus-4-6"
	if seen, _ := probe.Observe(ctx, "tenant-1", cachePrefixHash(other)); seen {
		t.Fatal("a model switch reported a hit; caches are model-scoped, so routing away is a full rebuild")
	}
}

// Non-string system content can't be a system prompt Anthropic accepts (the
// provider rejects it), so it must not contribute — and must not, by itself,
// manufacture a cacheable span.
func TestCachePrefixHash_NonStringSystemContentIgnored(t *testing.T) {
	req := &types.ChatRequest{Model: "m", Messages: []types.Message{
		{Role: "system", Content: []any{map[string]any{"type": "text", "text": "x"}}},
		userMsg("hi"),
	}}
	if got := cachePrefixHash(req); got != "" {
		t.Fatalf("cachePrefixHash = %q, want \"\"; non-text system content isn't a cacheable system prompt", got)
	}
}
