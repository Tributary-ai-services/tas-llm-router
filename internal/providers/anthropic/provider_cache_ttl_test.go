package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// AIQG-38 left one thing undone: the gateway honoured a caller's breakpoint but
// not its TTL, because the pinned SDK v1.7.0 had no field to carry one. Claude
// Code asks for `ttl: "1h"` on every request, so every pause longer than five
// minutes re-paid cache creation. These tests assert the TTL now reaches the
// wire on each surface that can carry a breakpoint.

func ttlControl(ttl string) *types.CacheControl {
	return &types.CacheControl{Type: "ephemeral", TTL: ttl}
}

// marshalled returns the converted request as the SDK would send it, so the
// assertions are about the wire rather than about our structs.
func marshalled(t *testing.T, req *types.ChatRequest) string {
	t.Helper()
	provider := createTestProvider(t)
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestCacheTTL_SystemBlockCarries1h(t *testing.T) {
	wire := marshalled(t, &types.ChatRequest{
		Model: "claude-sonnet-5-5",
		Messages: []types.Message{
			{Role: "system", Content: "a long reusable preamble", CacheControl: ttlControl("1h")},
			{Role: "user", Content: "hi"},
		},
	})
	if !strings.Contains(wire, `"ttl":"1h"`) {
		t.Errorf(`system breakpoint lost its 1h TTL; wire = %s`, wire)
	}
}

func TestCacheTTL_ToolAndContentPartCarry1h(t *testing.T) {
	wire := marshalled(t, &types.ChatRequest{
		Model: "claude-sonnet-5-5",
		Messages: []types.Message{{Role: "user", Content: []types.ContentPart{
			{Type: "text", Text: "cached context", CacheControl: ttlControl("1h")},
		}}},
		Tools: []types.Tool{{
			Type: "function",
			Function: types.Function{
				Name:       "get_weather",
				Parameters: map[string]interface{}{"type": "object"},
			},
			CacheControl: ttlControl("1h"),
		}},
	})
	if n := strings.Count(wire, `"ttl":"1h"`); n < 2 {
		t.Errorf("expected a 1h TTL on both the tool and the content part, found %d; wire = %s", n, wire)
	}
}

// An explicit 5m must still be expressible -- it is a legitimate choice, not
// merely the default, and treating it as "no preference" would make the two
// indistinguishable on the wire.
func TestCacheTTL_ExplicitFiveMinutesIsSent(t *testing.T) {
	wire := marshalled(t, &types.ChatRequest{
		Model: "claude-sonnet-5-5",
		Messages: []types.Message{
			{Role: "system", Content: "preamble", CacheControl: ttlControl("5m")},
			{Role: "user", Content: "hi"},
		},
	})
	if !strings.Contains(wire, `"ttl":"5m"`) {
		t.Errorf("explicit 5m TTL not sent; wire = %s", wire)
	}
}

// A TTL the vendor does not define must be DROPPED, not forwarded: an invalid
// ttl 400s the whole request, so losing the preference is strictly better than
// losing the work. The breakpoint itself must survive.
func TestCacheTTL_UnknownValueIsDroppedButBreakpointSurvives(t *testing.T) {
	wire := marshalled(t, &types.ChatRequest{
		Model: "claude-sonnet-5-5",
		Messages: []types.Message{
			{Role: "system", Content: "preamble", CacheControl: ttlControl("7d")},
			{Role: "user", Content: "hi"},
		},
	})
	if strings.Contains(wire, `"ttl"`) {
		t.Errorf("an unrecognised TTL reached the wire; wire = %s", wire)
	}
	if !strings.Contains(wire, `"cache_control"`) {
		t.Errorf("the breakpoint was lost along with its bad TTL; wire = %s", wire)
	}
}

// MaxCacheTTL is what the cost model reads, so "strongest wins" is a pricing
// statement: a mixed request priced at the longer TTL can over-report but never
// under-report.
func TestMaxCacheTTL_StrongestWins(t *testing.T) {
	cases := []struct {
		name string
		req  *types.ChatRequest
		want string
	}{
		{"none", &types.ChatRequest{Messages: []types.Message{{Role: "user", Content: "hi"}}}, ""},
		{"only 5m", &types.ChatRequest{Messages: []types.Message{
			{Role: "user", Content: "hi", CacheControl: ttlControl("5m")}}}, "5m"},
		{"1h on a tool beats 5m on a message", &types.ChatRequest{
			Messages: []types.Message{{Role: "user", Content: "hi", CacheControl: ttlControl("5m")}},
			Tools: []types.Tool{{Type: "function",
				Function:     types.Function{Name: "f"},
				CacheControl: ttlControl("1h")}},
		}, "1h"},
		{"1h on a content part", &types.ChatRequest{Messages: []types.Message{
			{Role: "user", Content: []types.ContentPart{
				{Type: "text", Text: "x", CacheControl: ttlControl("1h")}}}}}, "1h"},
		{"unknown ttl is not a preference", &types.ChatRequest{Messages: []types.Message{
			{Role: "user", Content: "hi", CacheControl: ttlControl("7d")}}}, ""},
	}
	for _, c := range cases {
		if got := c.req.MaxCacheTTL(); got != c.want {
			t.Errorf("%s: MaxCacheTTL() = %q, want %q", c.name, got, c.want)
		}
	}
}
