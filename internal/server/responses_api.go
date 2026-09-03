// OpenAI Responses API wire support for POST /v1/responses.
//
// The Responses API is OpenAI's newer surface (the default in the current
// OpenAI SDK). Its request/response shapes differ from Chat Completions: input
// is a string or a list of typed items, output is a list of items (message /
// function_call), and streaming is a taxonomy of named events
// (response.created, response.output_text.delta, response.completed, …).
//
// Like /v1/messages, /v1/responses translates at the boundary and reuses the
// shared chat pipeline: handleResponses parses the Responses body into the
// internal ChatRequest, marks the context with responseFormatResponses, and
// hands off to handleChatCompletion. The shared response write-sites render the
// Responses object (non-streaming) or the Responses event stream (streaming).
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// ---- inbound wire types ------------------------------------------------------

type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"` // string | []responsesInputItem
	Instructions    string          `json:"instructions,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Temperature     *float32        `json:"temperature,omitempty"`
	TopP            *float32        `json:"top_p,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Tools           []responsesTool `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`

	// TAS-native routing extensions, settable via the OpenAI SDK's extra_body.
	tasExtensions
}

// responsesTool is a Responses-API tool: unlike Chat Completions it is FLAT —
// {type:"function", name, description, parameters} — not nested under "function".
type responsesTool struct {
	Type        string                 `json:"type"`
	Name        string                 `json:"name,omitempty"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

type responsesInputItem struct {
	Type    string          `json:"type,omitempty"` // message (default) | function_call | function_call_output
	Role    string          `json:"role,omitempty"` // user | assistant | system | developer
	Content json.RawMessage `json:"content,omitempty"`

	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// function_call_output
	Output string `json:"output,omitempty"`
}

type responsesInputContentPart struct {
	Type string `json:"type"` // input_text | output_text
	Text string `json:"text,omitempty"`
}

// ---- outbound wire types -----------------------------------------------------

type responsesOutputContent struct {
	Type        string        `json:"type"` // output_text
	Text        string        `json:"text"`
	Annotations []interface{} `json:"annotations"`
}

type responsesOutputItem struct {
	Type    string                   `json:"type"` // message | function_call
	ID      string                   `json:"id,omitempty"`
	Status  string                   `json:"status,omitempty"`
	Role    string                   `json:"role,omitempty"`    // message
	Content []responsesOutputContent `json:"content,omitempty"` // message

	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type responsesObject struct {
	ID        string                `json:"id"`
	Object    string                `json:"object"` // "response"
	CreatedAt int64                 `json:"created_at"`
	Status    string                `json:"status"` // in_progress | completed
	Model     string                `json:"model"`
	Output    []responsesOutputItem `json:"output"`
	Usage     *responsesUsage       `json:"usage,omitempty"`
}

// ---- inbound translation -----------------------------------------------------

// parseResponsesToChatRequest decodes a Responses-API request and translates it
// into the internal ChatRequest.
func parseResponsesToChatRequest(body []byte) (*types.ChatRequest, error) {
	var rr responsesRequest
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&rr); err != nil {
		return nil, fmt.Errorf("invalid Responses request: %v", err)
	}
	if strings.TrimSpace(rr.Model) == "" {
		return nil, fmt.Errorf("field 'model' is required")
	}

	req := &types.ChatRequest{
		Model:       rr.Model,
		Stream:      rr.Stream,
		Temperature: rr.Temperature,
		TopP:        rr.TopP,
	}
	if rr.MaxOutputTokens != nil {
		req.MaxTokens = rr.MaxOutputTokens
	}

	// instructions → leading system message.
	if strings.TrimSpace(rr.Instructions) != "" {
		req.Messages = append(req.Messages, types.Message{Role: "system", Content: rr.Instructions})
	}

	// input: a bare string, or an array of typed items.
	var asString string
	if err := json.Unmarshal(rr.Input, &asString); err == nil {
		req.Messages = append(req.Messages, types.Message{Role: "user", Content: asString})
	} else {
		var items []responsesInputItem
		if err := json.Unmarshal(rr.Input, &items); err != nil {
			return nil, fmt.Errorf("field 'input' must be a string or an array of items")
		}
		for _, it := range items {
			switch it.Type {
			case "function_call":
				req.Messages = append(req.Messages, types.Message{
					Role: "assistant",
					ToolCalls: []types.ToolCall{{
						ID:       it.CallID,
						Type:     "function",
						Function: types.Function{Name: it.Name, Arguments: it.Arguments},
					}},
				})
			case "function_call_output":
				req.Messages = append(req.Messages, types.Message{
					Role:       "tool",
					ToolCallID: it.CallID,
					Content:    it.Output,
				})
			default: // "message" or unset
				role := it.Role
				if role == "" {
					role = "user"
				}
				if role == "developer" {
					role = "system" // Responses' developer role ≈ system
				}
				req.Messages = append(req.Messages, types.Message{Role: role, Content: responsesContentText(it.Content)})
			}
		}
	}

	for _, t := range rr.Tools {
		if t.Type != "function" && t.Type != "" {
			continue // only function tools are translatable to the chat pipeline
		}
		req.Tools = append(req.Tools, types.Tool{
			Type: "function",
			Function: types.Function{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}

	if len(rr.ToolChoice) > 0 {
		if tc := responsesToolChoiceToInternal(rr.ToolChoice); tc != nil {
			req.ToolChoice = tc
		}
	}

	applyTASExtensions(req, rr.tasExtensions)

	return req, nil
}

// responsesContentText coerces a Responses input item's content (string or an
// array of content parts) into plain text.
func responsesContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []responsesInputContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "input_text" || p.Type == "output_text" || p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// responsesToolChoiceToInternal maps the Responses tool_choice to the internal
// (OpenAI chat) tool_choice: "auto"/"none"/"required" pass through; a
// {type:"function",name:X} object becomes {type:function,function:{name:X}}.
func responsesToolChoiceToInternal(raw json.RawMessage) interface{} {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Type == "function" && obj.Name != "" {
		return map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": obj.Name}}
	}
	return nil
}

// ---- outbound translation (non-streaming) ------------------------------------

// chatResponseToResponses renders an internal ChatResponse as a Responses
// object. Provider-agnostic.
func chatResponseToResponses(resp *types.ChatResponse) responsesObject {
	out := responsesObject{
		ID:        responsesObjectID(resp.ID),
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    "completed",
		Model:     resp.Model,
		Output:    []responsesOutputItem{},
	}

	if len(resp.Choices) > 0 {
		c := resp.Choices[0]
		if text := messageContentToString(c.Message.Content); text != "" {
			out.Output = append(out.Output, responsesOutputItem{
				Type:   "message",
				ID:     "msg_" + strings.TrimPrefix(out.ID, "resp_"),
				Status: "completed",
				Role:   "assistant",
				Content: []responsesOutputContent{{
					Type:        "output_text",
					Text:        text,
					Annotations: []interface{}{},
				}},
			})
		}
		for i, tc := range c.Message.ToolCalls {
			out.Output = append(out.Output, responsesOutputItem{
				Type:      "function_call",
				ID:        fmt.Sprintf("fc_%s_%d", strings.TrimPrefix(out.ID, "resp_"), i),
				Status:    "completed",
				CallID:    tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}

	if resp.Usage != nil {
		out.Usage = &responsesUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		}
	}
	return out
}

// responsesObjectID ensures the response id carries the Responses `resp_` prefix.
func responsesObjectID(id string) string {
	if id == "" {
		return "resp_gateway"
	}
	if strings.HasPrefix(id, "resp_") {
		return id
	}
	return "resp_" + strings.TrimPrefix(id, "chatcmpl-")
}

// ---- HTTP handler ------------------------------------------------------------

func (s *Server) writeResponsesObject(w http.ResponseWriter, resp *types.ChatResponse) {
	obj := chatResponseToResponses(resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(obj)
}

// handleResponses implements POST /v1/responses: parse the Responses body,
// translate to the internal ChatRequest, flag the context for Responses output,
// and hand off to the shared completion pipeline.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeErrorResponse(w, http.StatusBadRequest, "could not read request body")
		return
	}
	chatReq, err := parseResponsesToChatRequest(body)
	if err != nil {
		s.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	buf, err := json.Marshal(chatReq)
	if err != nil {
		s.writeErrorResponse(w, http.StatusInternalServerError, "internal translation error")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(buf))
	r.ContentLength = int64(len(buf))
	ctx := context.WithValue(r.Context(), responseFormatCtxKey{}, responseFormatResponses)
	s.handleChatCompletion(w, r.WithContext(ctx))
}

// ---- streaming encoder -------------------------------------------------------

// responsesStreamEncoder translates the internal chunk stream into the Responses
// event taxonomy. Text streams live via response.output_text.delta; tool calls
// are buffered and emitted as function_call output items at done(), analogous to
// the Anthropic encoder.
type responsesStreamEncoder struct {
	w       io.Writer
	flusher http.Flusher
	logger  *logrus.Logger

	respID string
	model  string
	seq    int

	started      bool // response.created emitted
	msgOpen      bool // message item + text content part opened
	msgID        string
	text         strings.Builder
	outputIndex  int
	inputTokens  int
	outputTokens int

	toolOrder []string
	tools     map[string]*responsesStreamTool

	// completed output items, accumulated as each item finishes, so the final
	// response.completed event carries the full output (the SDK reconstructs
	// get_final_response()/output_text from it).
	finalOutput []map[string]interface{}
}

type responsesStreamTool struct {
	callID string
	name   string
	args   strings.Builder
}

func newResponsesStreamEncoder(w io.Writer, flusher http.Flusher, req *types.ChatRequest, logger *logrus.Logger) *responsesStreamEncoder {
	model := ""
	if req != nil {
		model = req.Model
	}
	return &responsesStreamEncoder{
		w:       w,
		flusher: flusher,
		logger:  logger,
		model:   model,
		tools:   map[string]*responsesStreamTool{},
	}
}

func (e *responsesStreamEncoder) emit(event string, payload map[string]interface{}) {
	payload["type"] = event
	payload["sequence_number"] = e.seq
	e.seq++
	data, err := json.Marshal(payload)
	if err != nil {
		if e.logger != nil {
			e.logger.WithError(err).Error("failed to marshal responses stream event")
		}
		return
	}
	fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", event, data)
	if e.flusher != nil {
		e.flusher.Flush()
	}
}

func (e *responsesStreamEncoder) start(c *types.ChatChunk) {
	e.started = true
	if c != nil {
		if c.Model != "" {
			e.model = c.Model
		}
		if c.ID != "" {
			e.respID = responsesObjectID(c.ID)
		}
	}
	if e.respID == "" {
		e.respID = "resp_gateway"
	}
	e.msgID = "msg_" + strings.TrimPrefix(e.respID, "resp_")
	e.emit("response.created", map[string]interface{}{
		"response": e.responseEnvelope("in_progress", nil),
	})
}

func (e *responsesStreamEncoder) ensureMessage() {
	if e.msgOpen {
		return
	}
	e.msgOpen = true
	e.emit("response.output_item.added", map[string]interface{}{
		"output_index": e.outputIndex,
		"item": map[string]interface{}{
			"type": "message", "id": e.msgID, "status": "in_progress",
			"role": "assistant", "content": []interface{}{},
		},
	})
	e.emit("response.content_part.added", map[string]interface{}{
		"item_id": e.msgID, "output_index": e.outputIndex, "content_index": 0,
		"part": map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
	})
}

func (e *responsesStreamEncoder) writeChunk(c *types.ChatChunk) {
	if !e.started {
		e.start(c)
	}
	if c.Usage != nil {
		if c.Usage.PromptTokens > 0 {
			e.inputTokens = c.Usage.PromptTokens
		}
		if c.Usage.CompletionTokens > 0 {
			e.outputTokens = c.Usage.CompletionTokens
		}
	}
	for _, ch := range c.Choices {
		if ch.Delta == nil {
			continue
		}
		if txt := messageContentToString(ch.Delta.Content); txt != "" {
			e.ensureMessage()
			e.text.WriteString(txt)
			e.emit("response.output_text.delta", map[string]interface{}{
				"item_id": e.msgID, "output_index": e.outputIndex, "content_index": 0,
				"delta": txt,
			})
		}
		for _, tc := range ch.Delta.ToolCalls {
			e.bufferTool(tc)
		}
	}
}

func (e *responsesStreamEncoder) bufferTool(tc types.ToolCall) {
	key := tc.ID
	if key == "" && len(e.toolOrder) > 0 {
		key = e.toolOrder[len(e.toolOrder)-1]
	}
	if key == "" {
		key = fmt.Sprintf("call_%d", len(e.toolOrder))
	}
	t, ok := e.tools[key]
	if !ok {
		t = &responsesStreamTool{callID: tc.ID, name: tc.Function.Name}
		if t.callID == "" {
			t.callID = key
		}
		e.tools[key] = t
		e.toolOrder = append(e.toolOrder, key)
	}
	if t.name == "" && tc.Function.Name != "" {
		t.name = tc.Function.Name
	}
	t.args.WriteString(tc.Function.Arguments)
}

func (e *responsesStreamEncoder) done() {
	if !e.started {
		e.start(nil)
	}
	// Close the text message item, if any.
	if e.msgOpen {
		full := e.text.String()
		e.emit("response.output_text.done", map[string]interface{}{
			"item_id": e.msgID, "output_index": e.outputIndex, "content_index": 0, "text": full,
		})
		e.emit("response.content_part.done", map[string]interface{}{
			"item_id": e.msgID, "output_index": e.outputIndex, "content_index": 0,
			"part": map[string]interface{}{"type": "output_text", "text": full, "annotations": []interface{}{}},
		})
		msgItem := map[string]interface{}{
			"type": "message", "id": e.msgID, "status": "completed", "role": "assistant",
			"content": []map[string]interface{}{{"type": "output_text", "text": full, "annotations": []interface{}{}}},
		}
		e.emit("response.output_item.done", map[string]interface{}{"output_index": e.outputIndex, "item": msgItem})
		e.finalOutput = append(e.finalOutput, msgItem)
		e.outputIndex++
	}
	// Emit buffered tool calls as function_call output items.
	for _, key := range e.toolOrder {
		t := e.tools[key]
		fcID := fmt.Sprintf("fc_%s_%d", strings.TrimPrefix(e.respID, "resp_"), e.outputIndex)
		item := map[string]interface{}{
			"type": "function_call", "id": fcID, "status": "completed",
			"call_id": t.callID, "name": t.name, "arguments": t.args.String(),
		}
		e.emit("response.output_item.added", map[string]interface{}{"output_index": e.outputIndex, "item": item})
		e.emit("response.output_item.done", map[string]interface{}{"output_index": e.outputIndex, "item": item})
		e.finalOutput = append(e.finalOutput, item)
		e.outputIndex++
	}
	// The completed event carries the FULL output so the SDK can reconstruct
	// the final response object (output_text, tool calls) from it.
	env := e.responseEnvelope("completed", &responsesUsage{
		InputTokens: e.inputTokens, OutputTokens: e.outputTokens, TotalTokens: e.inputTokens + e.outputTokens,
	})
	env["output"] = e.finalOutput
	e.emit("response.completed", map[string]interface{}{"response": env})
}

// writeError emits a Responses-API terminal error event so an SDK consuming the
// typed event stream sees a failure rather than a stream that simply stops
// before `response.completed`.
func (e *responsesStreamEncoder) writeError(se *types.StreamError) {
	e.emit("error", map[string]interface{}{
		"type": "error", "code": se.Code, "message": se.Message, "param": nil,
	})
}

// responseEnvelope builds the response object embedded in response.created /
// response.completed events. The full output list is only materialized on
// completion; the created event carries an empty output list.
func (e *responsesStreamEncoder) responseEnvelope(status string, usage *responsesUsage) map[string]interface{} {
	env := map[string]interface{}{
		"id":         e.respID,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      e.model,
		"output":     []interface{}{},
	}
	if usage != nil {
		env["usage"] = usage
	}
	return env
}
