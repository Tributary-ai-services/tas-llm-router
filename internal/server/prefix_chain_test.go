package server

import (
	"encoding/json"
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func userMsg(s string) types.Message      { return types.Message{Role: "user", Content: s} }
func assistantMsg(s string) types.Message { return types.Message{Role: "assistant", Content: s} }

func respWith(content string) *types.ChatResponse {
	return &types.ChatResponse{Choices: []types.Choice{{Message: assistantMsg(content)}}}
}

// The load-bearing property of prefix chaining: the state hash we index after
// serving turn N must equal the prefix hash the client produces on turn N+1
// (which re-sends turn N's messages + our assistant reply + a new user turn).
// If these don't match, conversations never thread.
func TestPrefixChain_StateHashMatchesNextPrefix(t *testing.T) {
	turn1Req := &types.ChatRequest{Messages: []types.Message{
		{Role: "system", Content: "You are helpful."},
		userMsg("What is the capital of France?"),
	}}
	answer := "The capital of France is Paris."
	stateAfterTurn1 := conversationStateHash(turn1Req, respWith(answer))
	if stateAfterTurn1 == "" {
		t.Fatal("state hash should be non-empty for a text response")
	}

	// Turn 2: client re-sends everything verbatim + our reply + a follow-up.
	turn2Req := &types.ChatRequest{Messages: []types.Message{
		{Role: "system", Content: "You are helpful."},
		userMsg("What is the capital of France?"),
		assistantMsg(answer),
		userMsg("And its population?"),
	}}
	prefixOfTurn2 := conversationPrefixHash(turn2Req)
	if prefixOfTurn2 != stateAfterTurn1 {
		t.Errorf("turn-2 prefix hash %q != turn-1 state hash %q — conversation would not thread",
			prefixOfTurn2, stateAfterTurn1)
	}
}

func TestPrefixChain_FirstTurnHasNoPrefix(t *testing.T) {
	// A fresh thread (no assistant message) has nothing to chain back to.
	req := &types.ChatRequest{Messages: []types.Message{userMsg("Hello")}}
	if h := conversationPrefixHash(req); h != "" {
		t.Errorf("first turn should have empty prefix hash, got %q", h)
	}
}

func TestPrefixChain_ToolOnlyResponseHasNoStateHash(t *testing.T) {
	// A response with no assistant text (tool-call-only) is handled by the
	// tool_call_id echo tier, not prefix chaining — no state hash.
	req := &types.ChatRequest{Messages: []types.Message{userMsg("Weather?")}}
	toolResp := &types.ChatResponse{Choices: []types.Choice{{
		Message: types.Message{Role: "assistant", ToolCalls: []types.ToolCall{{ID: "call_1"}}},
	}}}
	if h := conversationStateHash(req, toolResp); h != "" {
		t.Errorf("tool-only response should have empty state hash, got %q", h)
	}
}

func tool(name string) types.Tool {
	return types.Tool{Type: "function", Function: types.Function{Name: name}}
}

func TestAgentFingerprint_GateAndStability(t *testing.T) {
	// A bare chat (no tools / response_format / stop) is not fingerprintable.
	bare := &types.ChatRequest{Model: "gpt-4o-mini", Messages: []types.Message{userMsg("hi")}}
	if h := agentFingerprint(bare); h != "" {
		t.Errorf("bare chat should not be fingerprinted, got %q", h)
	}

	// A request declaring tools fingerprints, and tool-array order doesn't matter.
	a := &types.ChatRequest{Model: "gpt-4o-mini", Tools: []types.Tool{tool("get_weather"), tool("search")}}
	b := &types.ChatRequest{Model: "gpt-4o-mini", Tools: []types.Tool{tool("search"), tool("get_weather")}}
	fa, fb := agentFingerprint(a), agentFingerprint(b)
	if fa == "" {
		t.Fatal("tool-bearing request should fingerprint")
	}
	if fa != fb {
		t.Error("tool ordering must not change the fingerprint")
	}

	// A different toolset → different fingerprint.
	c := &types.ChatRequest{Model: "gpt-4o-mini", Tools: []types.Tool{tool("get_weather")}}
	if agentFingerprint(c) == fa {
		t.Error("a different toolset must change the fingerprint")
	}

	// Same toolset, different model → different fingerprint (config tie-breaker).
	d := &types.ChatRequest{Model: "gpt-4o", Tools: []types.Tool{tool("get_weather"), tool("search")}}
	if agentFingerprint(d) == fa {
		t.Error("model is part of the config signature")
	}
}

func TestAgentFingerprint_MaxTokensIgnored(t *testing.T) {
	mt1, mt2 := 8, 4096
	base := func(mt int) *types.ChatRequest {
		return &types.ChatRequest{Model: "gpt-4o-mini", MaxTokens: &mt, Tools: []types.Tool{tool("search")}}
	}
	if agentFingerprint(base(mt1)) != agentFingerprint(base(mt2)) {
		t.Error("max_tokens must NOT split one agent's fingerprint")
	}
}

func TestApplyExperimentOverride(t *testing.T) {
	// Model-swap override reroutes by changing req.Model.
	req := &types.ChatRequest{Model: "gpt-4o"}
	applyExperimentOverride(req, json.RawMessage(`{"model":"gpt-4o-mini"}`))
	if req.Model != "gpt-4o-mini" {
		t.Errorf("model override not applied: %q", req.Model)
	}

	// Param-sweep override.
	req2 := &types.ChatRequest{Model: "gpt-4o"}
	applyExperimentOverride(req2, json.RawMessage(`{"params":{"temperature":0.2,"max_tokens":256}}`))
	if req2.Temperature == nil || *req2.Temperature != 0.2 {
		t.Errorf("temperature override not applied: %v", req2.Temperature)
	}
	if req2.MaxTokens == nil || *req2.MaxTokens != 256 {
		t.Errorf("max_tokens override not applied: %v", req2.MaxTokens)
	}
	if req2.Model != "gpt-4o" {
		t.Errorf("model should be untouched when override has no model: %q", req2.Model)
	}

	// Empty / control override is a no-op.
	req3 := &types.ChatRequest{Model: "gpt-4o"}
	applyExperimentOverride(req3, nil)
	applyExperimentOverride(req3, json.RawMessage(`{}`))
	if req3.Model != "gpt-4o" {
		t.Errorf("empty override must not change the request: %q", req3.Model)
	}
}

func TestPrefixChain_DistinctConversationsDiffer(t *testing.T) {
	a := conversationStateHash(
		&types.ChatRequest{Messages: []types.Message{userMsg("A")}}, respWith("reply-A"))
	b := conversationStateHash(
		&types.ChatRequest{Messages: []types.Message{userMsg("B")}}, respWith("reply-B"))
	if a == b || a == "" || b == "" {
		t.Errorf("distinct conversations must hash differently: a=%q b=%q", a, b)
	}
}
