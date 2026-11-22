package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tributary-ai/llm-router-waf/internal/gatekeeper"
	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/toolcalls"
)

func toolServer(t *testing.T) *Server {
	t.Helper()
	tr := toolcalls.New(toolcalls.NewMemoryStore(), &events.MemoryEmitter{}, logrus.New(),
		toolcalls.Config{SweepInterval: time.Hour}, toolcalls.Hooks{})
	t.Cleanup(func() { tr.Close(context.Background()) })
	lg := logrus.New()
	lg.SetLevel(logrus.PanicLevel)
	return &Server{logger: lg, toolCalls: tr}
}

func aiqgCtx() (context.Context, *middleware.Routing) {
	r := middleware.NewRouting()
	return middleware.WithRouting(context.Background(), r), r
}

// OpenAI shape, as it reaches handleChatCompletion: a re-sent history with
// two answered calls, then a new assistant turn and its two results.
const openAIBody = `{
  "model": "gpt-4o",
  "tools": [
    {"type":"function","function":{"name":"get_weather","description":"Weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}},
    {"type":"function","function":{"name":"crm_lookup","parameters":{"type":"object"}}}
  ],
  "messages": [
    {"role":"user","content":"hi"},
    {"role":"assistant","content":null,"tool_calls":[{"id":"call_old","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"call_old","content":"sunny"},
    {"role":"assistant","content":null,"tool_calls":[
      {"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}},
      {"id":"call_2","type":"function","function":{"name":"crm_lookup","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"call_1","content":"rain, 12C"},
    {"role":"tool","tool_call_id":"call_2","content":[{"type":"text","text":"jane@example.com"}]}
  ]
}`

func TestToolCallRequest_OpenAIShape(t *testing.T) {
	var req types.ChatRequest
	require.NoError(t, json.Unmarshal([]byte(openAIBody), &req))
	s := toolServer(t)
	ctx, r := aiqgCtx()
	s.stampToolCallRequest(ctx, &req)
	snap := r.Snapshot()

	assert.Equal(t, []toolcalls.Result{{ID: "call_1", Bytes: len("rain, 12C")}, {ID: "call_2", Bytes: len("jane@example.com")}},
		snap.ToolResults, "only results after the last assistant turn; history is not re-counted")
	require.Len(t, snap.ToolsOffered, 2)
	assert.Equal(t, "get_weather", snap.ToolsOffered[0].Name)
	b, _ := json.Marshal(req.Tools[0].Function)
	assert.Equal(t, len(b), snap.ToolsOffered[0].SchemaBytes)
	assert.Equal(t, snap.ToolsOffered[0].SchemaBytes+snap.ToolsOffered[1].SchemaBytes, snap.ToolsSchemaBytes)
}

// Anthropic shape through the real /v1/messages translator: tool_result
// blocks (inside a user message) become role=tool messages keyed by
// tool_use_id, and the declared tools carry name + input_schema.
const anthropicBody = `{
  "model": "claude-sonnet-4-5",
  "max_tokens": 512,
  "tools": [{"name":"draw","description":"Draw a diagram","input_schema":{"type":"object","properties":{"spec":{"type":"string"}}}}],
  "messages": [
    {"role":"user","content":"make a diagram"},
    {"role":"assistant","content":[
      {"type":"text","text":"Drawing."},
      {"type":"tool_use","id":"toolu_01A","name":"draw","input":{"spec":"a->b"}},
      {"type":"tool_use","id":"toolu_01B","name":"draw","input":{"spec":"b->c"}}]},
    {"role":"user","content":[
      {"type":"tool_result","tool_use_id":"toolu_01A","content":"ok: 2 nodes"},
      {"type":"tool_result","tool_use_id":"toolu_01B","content":[{"type":"text","text":"ok: 3 nodes"}]},
      {"type":"text","text":"thanks"}]}
  ]
}`

func TestToolCallRequest_AnthropicShape(t *testing.T) {
	req, err := parseAnthropicToChatRequest([]byte(anthropicBody), true)
	require.NoError(t, err)
	// handleMessages re-marshals the internal request and handleChatCompletion
	// decodes it again; do the same so content arrives in its served form.
	buf, _ := json.Marshal(req)
	var served types.ChatRequest
	require.NoError(t, json.Unmarshal(buf, &served))

	s := toolServer(t)
	ctx, r := aiqgCtx()
	s.stampToolCallRequest(ctx, &served)
	snap := r.Snapshot()
	require.Len(t, snap.ToolResults, 2)
	assert.Equal(t, "toolu_01A", snap.ToolResults[0].ID)
	assert.Equal(t, len("ok: 2 nodes"), snap.ToolResults[0].Bytes)
	assert.Equal(t, "toolu_01B", snap.ToolResults[1].ID)
	assert.Equal(t, len("ok: 3 nodes"), snap.ToolResults[1].Bytes)
	require.Len(t, snap.ToolsOffered, 1)
	assert.Equal(t, "draw", snap.ToolsOffered[0].Name)
	assert.Greater(t, snap.ToolsOffered[0].SchemaBytes, len(`"draw"`))
}

func TestToolCallsMade_NonStreaming(t *testing.T) {
	s := toolServer(t)
	ctx, r := aiqgCtx()
	s.stampToolCallsMade(ctx, &types.ChatResponse{Choices: []types.Choice{{Message: types.Message{
		Role: "assistant",
		ToolCalls: []types.ToolCall{
			{ID: "toolu_01A", Type: "function", Function: types.Function{Name: "draw", Arguments: `{"spec":"a"}`}},
			{ID: "toolu_01B", Type: "function", Function: types.Function{Name: "erase"}},
		},
	}}}})
	assert.Equal(t, []toolcalls.Call{{ID: "toolu_01A", Name: "draw"}, {ID: "toolu_01B", Name: "erase"}}, r.Snapshot().ServedToolCalls)
}

type nopEncoder struct{ chunks int }

func (e *nopEncoder) writeChunk(*types.ChatChunk)   { e.chunks++ }
func (e *nopEncoder) writeError(*types.StreamError) {}
func (e *nopEncoder) done()                         {}

func toolDelta(tcs ...types.ToolCall) *types.ChatChunk {
	return &types.ChatChunk{Choices: []types.ChoiceChunk{{Delta: &types.Message{Role: "assistant", ToolCalls: tcs}}}}
}

func runToolStream(t *testing.T, s *Server, chunks []*types.ChatChunk) []toolcalls.Call {
	t.Helper()
	ctx, r := aiqgCtx()
	ch := make(chan *types.ChatChunk, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	s.streamChunks(ctx, &nopEncoder{}, ch, "p", "m")
	return r.Snapshot().ServedToolCalls
}

// OpenAI streams a call as a first fragment with id + name, then argument
// fragments without an id. Two parallel calls interleave the same way.
func TestToolCallsMade_StreamingOpenAI(t *testing.T) {
	s := toolServer(t)
	got := runToolStream(t, s, []*types.ChatChunk{
		toolDelta(types.ToolCall{ID: "call_1", Type: "function", Function: types.Function{Name: "get_weather"}}),
		toolDelta(types.ToolCall{Function: types.Function{Arguments: `{"ci`}}),
		toolDelta(types.ToolCall{Function: types.Function{Arguments: `ty":"Oslo"}`}}),
		toolDelta(types.ToolCall{ID: "call_2", Type: "function", Function: types.Function{Name: "crm_lookup"}}),
		toolDelta(types.ToolCall{Function: types.Function{Arguments: `{}`}}),
		{Choices: []types.ChoiceChunk{{FinishReason: "tool_calls", Delta: &types.Message{}}}},
	})
	assert.Equal(t, []toolcalls.Call{{ID: "call_1", Name: "get_weather"}, {ID: "call_2", Name: "crm_lookup"}}, got)
}

// Anthropic, as the provider translates it: content_block_start carries id
// + name, input_json_delta fragments carry no id (convertStreamEvent).
func TestToolCallsMade_StreamingAnthropic(t *testing.T) {
	s := toolServer(t)
	text := &types.ChatChunk{Choices: []types.ChoiceChunk{{Delta: &types.Message{Role: "assistant", Content: "Drawing."}}}}
	got := runToolStream(t, s, []*types.ChatChunk{
		text,
		toolDelta(types.ToolCall{ID: "toolu_01A", Type: "function", Function: types.Function{Name: "draw"}}),
		toolDelta(types.ToolCall{Type: "function", Function: types.Function{Arguments: `{"spec":`}}),
		toolDelta(types.ToolCall{Type: "function", Function: types.Function{Arguments: `"a->b"}`}}),
		{Choices: []types.ChoiceChunk{{FinishReason: "tool_use", Delta: &types.Message{}}}},
	})
	assert.Equal(t, []toolcalls.Call{{ID: "toolu_01A", Name: "draw"}}, got)
}

func TestToolCallsMade_FailedStreamMadeNone(t *testing.T) {
	s := toolServer(t)
	got := runToolStream(t, s, []*types.ChatChunk{
		toolDelta(types.ToolCall{ID: "call_1", Function: types.Function{Name: "x"}}),
		{Error: &types.StreamError{Message: "upstream reset"}},
	})
	assert.Empty(t, got)
}

func TestToolCallEvents_OffDoesNothing(t *testing.T) {
	s := &Server{logger: logrus.New()} // no tracker
	var req types.ChatRequest
	require.NoError(t, json.Unmarshal([]byte(openAIBody), &req))
	ctx, r := aiqgCtx()
	s.stampToolCallRequest(ctx, &req)
	s.stampToolCallsMade(ctx, &types.ChatResponse{Choices: []types.Choice{{Message: types.Message{ToolCalls: []types.ToolCall{{ID: "c"}}}}}})
	got := runToolStream(t, s, []*types.ChatChunk{toolDelta(types.ToolCall{ID: "c", Function: types.Function{Name: "x"}})})
	snap := r.Snapshot()
	assert.Empty(t, snap.ToolResults)
	assert.Empty(t, snap.ToolsOffered)
	assert.Empty(t, snap.ServedToolCalls)
	assert.Empty(t, got)
}

func TestToolCallScan_Stamped(t *testing.T) {
	s := toolServer(t)
	ctx, r := aiqgCtx()
	s.stampToolCallScan(ctx, &gatekeeper.InboundScan{ToolResults: map[string]gatekeeper.ToolResultScan{
		"call_2": {Findings: map[string]int{"pii-email": 1}},
	}})
	assert.Equal(t, map[string]middleware.ToolResultScan{"call_2": {Findings: map[string]int{"pii-email": 1}}}, r.Snapshot().ToolResultScans)
}
