package promptcache

import (
	"strings"
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// #100 §4 auto-placement. The engine's job is to put breakpoints only where the
// prefix is stable AND clears the model minimum, and never after the current
// question.

func bigText(tokens int) string {
	// ~4 bytes/token (clear.TokensFromBytes), so 4 chars/token clears the bar.
	return strings.Repeat("x", tokens*4)
}

func countBreakpoints(req *types.ChatRequest) int { return count(req) }

// §4.1: a large system prompt on a 4096-min model gets a system breakpoint.
func TestAuto_SystemBreakpointWhenPrefixClearsMinimum(t *testing.T) {
	req := &types.ChatRequest{
		Model: "claude-opus-4-8",
		Messages: []types.Message{
			{Role: "system", Content: bigText(5000)}, // > 4096
			{Role: "user", Content: "hi"},
		},
	}
	applied, n := Apply(req, ModeAuto)
	if applied != ModeAuto {
		t.Fatalf("applied = %v, want auto", applied)
	}
	if n != 1 {
		t.Fatalf("placed %d, want 1 (system breakpoint)", n)
	}
	if req.Messages[0].CacheControl == nil {
		t.Error("no breakpoint on the system block")
	}
}

// §4.5: below the model minimum, nothing is placed (a below-min breakpoint is a
// silent no-cache, so the slot would be wasted).
func TestAuto_NoSystemBreakpointBelowMinimum(t *testing.T) {
	req := &types.ChatRequest{
		Model: "claude-opus-4-8", // min 4096
		Messages: []types.Message{
			{Role: "system", Content: bigText(1000)}, // < 4096
			{Role: "user", Content: "hi"},
		},
	}
	_, n := Apply(req, ModeAuto)
	if n != 0 {
		t.Fatalf("placed %d, want 0 (prefix below the model minimum)", n)
	}
	if req.Messages[0].CacheControl != nil {
		t.Error("placed a breakpoint below the model minimum")
	}
}

// The same prefix that misses on Opus (4096) clears on Sonnet 4.5 (1024).
func TestAuto_MinimumIsModelDependent(t *testing.T) {
	mk := func(model string) *types.ChatRequest {
		return &types.ChatRequest{
			Model: model,
			Messages: []types.Message{
				{Role: "system", Content: bigText(2000)}, // between 1024 and 4096
				{Role: "user", Content: "hi"},
			},
		}
	}
	if _, n := Apply(mk("claude-opus-4-8"), ModeAuto); n != 0 {
		t.Errorf("opus: placed %d, want 0 (2000 < 4096)", n)
	}
	if _, n := Apply(mk("claude-sonnet-4-5"), ModeAuto); n != 1 {
		t.Errorf("sonnet-4-5: placed %d, want 1 (2000 > 1024)", n)
	}
}

// §4.2: a multi-turn conversation gets a breakpoint on the last complete turn
// (the last assistant message), never on the trailing user question.
func TestAuto_LastCompleteTurnBreakpoint(t *testing.T) {
	req := &types.ChatRequest{
		Model: "claude-sonnet-4-5", // min 1024
		Messages: []types.Message{
			{Role: "system", Content: bigText(1500)},
			{Role: "user", Content: "first question"},
			{Role: "assistant", Content: bigText(400)},
			{Role: "user", Content: "the current question"},
		},
	}
	_, n := Apply(req, ModeAuto)
	// system (idx0) clears 1024 → §4.1; assistant (idx2) prefix clears 1024 → §4.2.
	if n != 2 {
		t.Fatalf("placed %d, want 2 (system + last turn)", n)
	}
	if req.Messages[2].CacheControl == nil {
		t.Error("no breakpoint on the last complete (assistant) turn")
	}
	if req.Messages[3].CacheControl != nil {
		t.Error("breakpoint placed on the current user question — the shared-prefix mistake")
	}
}

// A first request (system + user only) has no completed turn to reuse: only the
// system breakpoint, never one on the pending question.
func TestAuto_NoTurnBreakpointOnFirstRequest(t *testing.T) {
	req := &types.ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []types.Message{
			{Role: "system", Content: bigText(1500)},
			{Role: "user", Content: "only question"},
		},
	}
	_, n := Apply(req, ModeAuto)
	if n != 1 {
		t.Fatalf("placed %d, want 1 (system only)", n)
	}
	if req.Messages[1].CacheControl != nil {
		t.Error("breakpoint on the sole user question")
	}
}

// OpenAI caches automatically — auto is a no-op, not an error, and must not
// attach Anthropic-shaped breakpoints.
func TestAuto_OpenAIIsNoOp(t *testing.T) {
	req := &types.ChatRequest{
		Model: "gpt-4o",
		Messages: []types.Message{
			{Role: "system", Content: bigText(5000)},
			{Role: "user", Content: "hi"},
		},
	}
	applied, n := Apply(req, ModeAuto)
	if applied != ModeAuto {
		t.Fatalf("applied = %v, want auto (no-op is still auto, not an error)", applied)
	}
	if n != 0 || countBreakpoints(req) != 0 {
		t.Fatalf("placed %d breakpoints on OpenAI; want 0", n)
	}
}

// Auto replaces client breakpoints (§3): a caller's stray breakpoint on the
// question is gone, and only the gateway's placement remains.
func TestAuto_ReplacesClientBreakpoints(t *testing.T) {
	req := &types.ChatRequest{
		Model: "claude-opus-4-8",
		Messages: []types.Message{
			{Role: "system", Content: bigText(5000)},
			{Role: "user", Content: []types.ContentPart{
				{Type: "text", Text: "hi", CacheControl: &types.CacheControl{Type: "ephemeral"}},
			}},
		},
	}
	_, n := Apply(req, ModeAuto)
	if n != 1 {
		t.Fatalf("placed %d, want 1 (system only)", n)
	}
	// The client's breakpoint on the user question must be stripped.
	parts := req.Messages[1].Content.([]types.ContentPart)
	if parts[0].CacheControl != nil {
		t.Error("client breakpoint survived; auto must replace it")
	}
}

func TestMinCacheTokens_Table(t *testing.T) {
	cases := []struct {
		model    string
		wantMin  int
		wantAppl bool
	}{
		{"claude-opus-4-8", 4096, true},
		{"claude-haiku-4-5-20251001", 4096, true},
		{"claude-fable-5", 2048, true},
		{"claude-sonnet-4-6", 2048, true},
		{"claude-sonnet-4-5", 1024, true},
		{"claude-3-7-sonnet", 1024, true},
		{"gpt-4o", 0, false},
		{"text-embedding-3-small", 0, false},
		{"claude-something-new", 4096, true}, // unknown Claude → conservative
	}
	for _, c := range cases {
		min, appl := minCacheTokens(c.model)
		if min != c.wantMin || appl != c.wantAppl {
			t.Errorf("minCacheTokens(%q) = (%d,%v), want (%d,%v)", c.model, min, appl, c.wantMin, c.wantAppl)
		}
	}
}
