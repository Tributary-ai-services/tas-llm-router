package server

import (
	"context"
	"encoding/json"

	"github.com/tributary-ai/llm-router-waf/internal/gatekeeper"
	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/toolcalls"
)

// Plan #22 T.5: what the gateway reads for tool-call events. Every
// /v1/messages and /v1/responses request is translated into a ChatRequest at
// the boundary, so one extraction covers both vendor shapes:
//   - Anthropic tool_use blocks become ToolCalls on the assistant message
//     (and on stream deltas); tool_result blocks become role=tool messages
//     whose ToolCallID is the tool_use_id (anthropic_messages.go).
//   - OpenAI tool_calls / role=tool messages are the internal shape already.
//
// All of it runs only when the tracker is on (s.toolCalls != nil).

// stampToolCallRequest stamps the request half: the offered toolset and the
// tool results this request carries for the first time.
func (s *Server) stampToolCallRequest(ctx context.Context, req *types.ChatRequest) {
	if s.toolCalls == nil || req == nil || middleware.RoutingFromContext(ctx) == nil {
		return
	}
	tools, total, over := offeredTools(req.Tools)
	if len(tools) > 0 {
		middleware.StampToolsOffered(ctx, tools, total)
	}
	s.toolCalls.DropOverflow(toolcalls.DropTooManyTools, over)
	results, over := newToolResults(req.Messages)
	middleware.StampToolResults(ctx, results, over)
}

// maxOfferedTools caps the catalogue rows one request contributes. Large
// MCP-heavy clients offer a few hundred tools; past this the rest are
// counted (reason "too_many_tools"), not listed. tools_schema_bytes still
// covers the whole toolset.
const maxOfferedTools = 512

// offeredTools returns each tool's name and definition size (the JSON of its
// function: name, description, parameters — what the vendor is sent and
// bills as input), the total over the whole toolset, and how many tools were
// left out of the list by maxOfferedTools.
func offeredTools(tools []types.Tool) ([]events.OfferedTool, int, int) {
	if len(tools) == 0 {
		return nil, 0, 0
	}
	n := len(tools)
	if n > maxOfferedTools {
		n = maxOfferedTools
	}
	out := make([]events.OfferedTool, 0, n)
	total, over := 0, 0
	for _, t := range tools {
		if t.Function.Name == "" {
			continue
		}
		b, err := json.Marshal(t.Function)
		size := 0
		if err == nil {
			size = len(b)
		}
		total += size
		if len(out) >= maxOfferedTools {
			over++
			continue
		}
		out = append(out, events.OfferedTool{Name: t.Function.Name, SchemaBytes: size})
	}
	return out, total, over
}

// newToolResults returns the tool results after the last assistant message.
// Those are the results this request carries for the first time; everything
// earlier is history the client re-sends on every turn, and its calls were
// closed on the turn that first carried them. Considering only the tail also
// keeps the store work per request proportional to one turn's calls, not to
// the length of the conversation. At most toolcalls.MaxIDsPerObservation are
// collected; the rest are only counted (returned as overflow).
func newToolResults(msgs []types.Message) ([]toolcalls.Result, int) {
	last := -1
	for i, m := range msgs {
		if m.Role == "assistant" {
			last = i
		}
	}
	var out []toolcalls.Result
	over := 0
	for _, m := range msgs[last+1:] {
		if m.Role != "tool" || m.ToolCallID == "" {
			continue
		}
		if len(out) >= toolcalls.MaxIDsPerObservation {
			over++
			continue
		}
		out = append(out, toolcalls.Result{ID: m.ToolCallID, Bytes: contentBytes(m.Content)})
	}
	return out, over
}

// contentBytes is the text size of a message's content: a string, or the
// text of its parts.
func contentBytes(c interface{}) int {
	switch v := c.(type) {
	case string:
		return len(v)
	case []types.ContentPart:
		n := 0
		for _, p := range v {
			n += len(p.Text)
		}
		return n
	case []interface{}:
		n := 0
		for _, p := range v {
			switch pp := p.(type) {
			case map[string]interface{}:
				if t, ok := pp["text"].(string); ok {
					n += len(t)
				}
			case string:
				n += len(pp)
			}
		}
		return n
	}
	return 0
}

// stampToolCallScan credits the inbound scan's per-tool-result findings.
func (s *Server) stampToolCallScan(ctx context.Context, in *gatekeeper.InboundScan) {
	if s.toolCalls == nil || in == nil || len(in.ToolResults) == 0 {
		return
	}
	m := make(map[string]middleware.ToolResultScan, len(in.ToolResults))
	for id, tr := range in.ToolResults {
		m[id] = middleware.ToolResultScan{Findings: tr.Findings, Skipped: tr.Skipped}
	}
	middleware.StampToolResultScans(ctx, m)
}

// stampToolCallsMade stamps the calls a non-streamed response made.
func (s *Server) stampToolCallsMade(ctx context.Context, resp *types.ChatResponse) {
	if s.toolCalls == nil || resp == nil {
		return
	}
	var calls []toolcalls.Call
	for _, ch := range resp.Choices {
		for _, tc := range ch.Message.ToolCalls {
			if tc.ID != "" {
				calls = append(calls, toolcalls.Call{ID: tc.ID, Name: tc.Function.Name})
			}
		}
	}
	middleware.StampToolCallsMade(ctx, calls)
}

// streamToolCalls assembles the calls a streamed response made from its
// deltas. A call's first fragment carries its id (OpenAI) or arrives at
// content_block_start (Anthropic, translated by the provider); later
// fragments carry argument text and no id, so they continue the latest call
// (the same rule as anthropicStreamEncoder.bufferTool).
type streamToolCalls struct {
	calls []toolcalls.Call
	index map[string]int
}

func (st *streamToolCalls) add(tc types.ToolCall) {
	if tc.ID == "" {
		if n := len(st.calls); n > 0 && st.calls[n-1].Name == "" && tc.Function.Name != "" {
			st.calls[n-1].Name = tc.Function.Name
		}
		return
	}
	if st.index == nil {
		st.index = map[string]int{}
	}
	if i, ok := st.index[tc.ID]; ok {
		if st.calls[i].Name == "" {
			st.calls[i].Name = tc.Function.Name
		}
		return
	}
	st.index[tc.ID] = len(st.calls)
	st.calls = append(st.calls, toolcalls.Call{ID: tc.ID, Name: tc.Function.Name})
}
