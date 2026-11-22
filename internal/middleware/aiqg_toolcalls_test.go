package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/linkage"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/tokens"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/toolcalls"
)

// End to end through the middleware: turn 1's response makes two calls,
// turn 2 echoes one of them, and the events carry turn 1's identity.
func TestAIQG_ToolCallEvents_EndToEnd(t *testing.T) {
	em := &events.MemoryEmitter{}
	tracker := toolcalls.New(toolcalls.NewMemoryStore(), em, silentLogger(),
		toolcalls.Config{SweepInterval: time.Hour}, toolcalls.Hooks{})
	resolver := tokens.NewMapResolver([]tokens.ConfigToken{
		{Token: "tas_qg_live_abc", TenantID: "t1", AIQGAccountID: "a1"},
	})
	mw := NewAIQG(AIQGConfig{Logger: silentLogger(), Resolver: resolver, Emitter: em,
		Linkage: linkage.NewMemoryStore(), ToolCalls: tracker})

	serve := func(h http.HandlerFunc) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("TAS-Auth", "tas_qg_live_abc")
		req.Header.Set("TAS-Agent-Id", "release-bot")
		req.Header.Set("baggage", "user.id=u-42")
		mw(h).ServeHTTP(httptest.NewRecorder(), req)
	}

	serve(func(w http.ResponseWriter, r *http.Request) {
		StampToolsOffered(r.Context(), []events.OfferedTool{{Name: "draw", SchemaBytes: 120}, {Name: "erase", SchemaBytes: 80}}, 200)
		StampServedToolCalls(r.Context(), []string{"toolu_A", "toolu_B"})
		StampToolCallsMade(r.Context(), []toolcalls.Call{{ID: "toolu_A", Name: "draw"}, {ID: "toolu_B", Name: "erase"}})
		w.WriteHeader(http.StatusOK)
	})
	time.Sleep(50 * time.Millisecond) // let the async open land before turn 2
	serve(func(w http.ResponseWriter, r *http.Request) {
		StampToolsOffered(r.Context(), []events.OfferedTool{{Name: "draw", SchemaBytes: 120}, {Name: "erase", SchemaBytes: 80}}, 200)
		StampEchoedToolCalls(r.Context(), []string{"toolu_A"})
		StampToolResults(r.Context(), []toolcalls.Result{{ID: "toolu_A", Bytes: 400}}, 0)
		StampToolResultScans(r.Context(), map[string]ToolResultScan{"toolu_A": {Findings: map[string]int{"pii-email": 1}}})
		w.WriteHeader(http.StatusOK)
	})
	tracker.Close(context.Background())

	resps := em.Responses()
	require.Len(t, resps, 2)
	first := resps[0].Data
	require.NotNil(t, first.ToolsSchemaBytes)
	assert.Equal(t, 200, *first.ToolsSchemaBytes)
	assert.Len(t, first.ToolsOffered, 2, "catalogue rows on the first sighting")
	require.NotNil(t, resps[1].Data.ToolsSchemaBytes)
	assert.Empty(t, resps[1].Data.ToolsOffered, "same toolset, same hour: not repeated")

	calls := em.ToolCalls()
	require.Len(t, calls, 2)
	by := map[string]events.ToolCallEvent{}
	for _, c := range calls {
		by[c.Data.ToolCallID] = c.Data
	}
	a, b := by["toolu_A"], by["toolu_B"]
	assert.Equal(t, "ok", a.Outcome)
	assert.Equal(t, "no_result", b.Outcome, "turn 2 came back without B's result")
	for _, d := range []events.ToolCallEvent{a, b} {
		assert.Equal(t, "gateway", d.ObservedBy)
		assert.Equal(t, "t1", d.TenantID)
		assert.Equal(t, first.AgentContext.StepID, d.StepID, "identity of the response that made the call")
		assert.Equal(t, first.AgentContext.FlowID, d.FlowID)
		assert.Equal(t, "release-bot", d.AgentID)
		assert.Equal(t, "u-42", d.UserID)
	}
	require.NotNil(t, a.RoundTripMs)
	assert.GreaterOrEqual(t, *a.RoundTripMs, int64(0))
	assert.Equal(t, map[string]int{"pii-email": 1}, a.Findings)
	assert.EqualValues(t, 400, *a.ResultBytes)
}

// Off by default: no tracker, no tool fields on the response event.
func TestAIQG_ToolCallEvents_OffByDefault(t *testing.T) {
	em := &events.MemoryEmitter{}
	mw := NewAIQG(AIQGConfig{Logger: silentLogger(), Emitter: em})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StampServedToolCalls(r.Context(), []string{"toolu_A"})
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), req)
	require.Len(t, em.Responses(), 1)
	assert.Nil(t, em.Responses()[0].Data.ToolsSchemaBytes)
	assert.Empty(t, em.ToolCalls())
}
