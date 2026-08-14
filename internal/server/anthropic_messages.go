// Anthropic Messages API wire support for POST /v1/messages.
//
// The gateway's internal representation (types.ChatRequest / ChatResponse) is
// OpenAI-shaped, and the whole processing pipeline (classification, AIQG
// stamping, Gatekeeper scanning, caching, routing, retry/fallback) is written
// against it. Rather than duplicate that pipeline, /v1/messages translates at
// the two boundaries:
//
//	inbound:  Anthropic Messages request  → types.ChatRequest   (this file)
//	outbound: types.ChatResponse / stream → Anthropic wire shape (this file)
//
// handleMessages parses the native Anthropic body, re-encodes it as the
// internal ChatRequest, marks the request context with responseFormatAnthropic,
// and hands off to the shared handleChatCompletion. The shared response
// write-sites consult the context flag and emit the native Anthropic envelope
// (non-streaming) or the native named-event SSE stream (streaming) instead of
// the OpenAI shape. This makes
//
//	Anthropic(api_key="tas_qg_live_…", base_url="<gw>").messages.create(...)
//
// work against the gateway, and — because the translation is at the boundary,
// not the provider — a /v1/messages request can still be cost-routed to ANY
// provider; whatever ChatResponse comes back is rendered in Anthropic shape.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// responseFormat selects the wire shape the shared completion handlers render.
type responseFormat int

const (
	responseFormatOpenAI    responseFormat = iota // default — OpenAI ChatCompletion
	responseFormatAnthropic                       // Anthropic Messages
)

type responseFormatCtxKey struct{}

// ---- inbound wire types (what a stock Anthropic SDK sends) --------------------

type anthropicMessagesRequest struct {
	Model         string                 `json:"model"`
	Messages      []anthropicWireMessage `json:"messages"`
	System        json.RawMessage        `json:"system,omitempty"` // string | []block
	MaxTokens     int                    `json:"max_tokens"`
	Temperature   *float32               `json:"temperature,omitempty"`
	TopP          *float32               `json:"top_p,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Stream        bool                   `json:"stream,omitempty"`
	Tools         []anthropicWireTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage        `json:"tool_choice,omitempty"`
}

type anthropicWireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string | []block
}

type anthropicWireTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema,omitempty"`
}

// anthropicInputBlock is one element of a content-block array on an inbound
// message or system field. Only the fields relevant to the block's `type` are
// populated.
type anthropicInputBlock struct {
	Type string `json:"type"` // text | image | tool_use | tool_result

	// text
	Text string `json:"text,omitempty"`

	// image
	Source *anthropicImageSource `json:"source,omitempty"`

	// tool_use (assistant asking to call a tool)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result (user returning a tool's output)
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string | []block
}

type anthropicImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicToolChoice struct {
	Type string `json:"type"` // auto | any | tool | none
	Name string `json:"name,omitempty"`
}

// ---- outbound wire types (what we render back to the SDK) --------------------

type anthropicContentBlock struct {
	Type  string          `json:"type"`            // text | tool_use
	Text  string          `json:"text,omitempty"`  // type=text
	ID    string          `json:"id,omitempty"`    // type=tool_use
	Name  string          `json:"name,omitempty"`  // type=tool_use
	Input json.RawMessage `json:"input,omitempty"` // type=tool_use (object)
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

type anthropicMessage struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"` // "message"
	Role         string                  `json:"role"` // "assistant"
	Model        string                  `json:"model"`
	Content      []anthropicContentBlock `json:"content"`
	StopReason   string                  `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        anthropicUsage          `json:"usage"`
}

// ---- inbound translation -----------------------------------------------------

// parseAnthropicToChatRequest decodes a native Anthropic Messages request body
// and translates it into the gateway's internal ChatRequest. Returns a
// client-facing error string (safe to echo) for malformed / unsupported input.
func parseAnthropicToChatRequest(body []byte) (*types.ChatRequest, error) {
	var ar anthropicMessagesRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&ar); err != nil {
		return nil, fmt.Errorf("invalid Anthropic messages request: %v", err)
	}
	if strings.TrimSpace(ar.Model) == "" {
		return nil, fmt.Errorf("field 'model' is required")
	}
	// Anthropic requires max_tokens; mirror that so callers get a clear 400
	// here rather than a downstream provider error.
	if ar.MaxTokens <= 0 {
		return nil, fmt.Errorf("field 'max_tokens' is required and must be greater than 0")
	}

	req := &types.ChatRequest{
		Model:       ar.Model,
		Stream:      ar.Stream,
		Temperature: ar.Temperature,
		TopP:        ar.TopP,
		Stop:        ar.StopSequences,
	}
	mt := ar.MaxTokens
	req.MaxTokens = &mt

	// System prompt → a leading system message (the internal + OpenAI
	// convention; the Anthropic provider lifts it back to top-level `system`).
	if len(ar.System) > 0 {
		if sys := anthropicTextFromField(ar.System); sys != "" {
			req.Messages = append(req.Messages, types.Message{Role: "system", Content: sys})
		}
	}

	for _, m := range ar.Messages {
		msgs, err := anthropicMessageToInternal(m)
		if err != nil {
			return nil, err
		}
		req.Messages = append(req.Messages, msgs...)
	}

	for _, t := range ar.Tools {
		req.Tools = append(req.Tools, types.Tool{
			Type: "function",
			Function: types.Function{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	if len(ar.ToolChoice) > 0 {
		if tc := anthropicToolChoiceToInternal(ar.ToolChoice); tc != nil {
			req.ToolChoice = tc
		}
	}

	return req, nil
}

// anthropicMessageToInternal translates one Anthropic message (which may carry
// a string or a content-block array) into one or more internal messages.
// A user message bearing tool_result blocks fans out into role=tool messages
// (emitted first, matching OpenAI ordering) plus an optional user message for
// any accompanying text/image.
func anthropicMessageToInternal(m anthropicWireMessage) ([]types.Message, error) {
	// Simple string content.
	var asString string
	if err := json.Unmarshal(m.Content, &asString); err == nil {
		return []types.Message{{Role: m.Role, Content: asString}}, nil
	}

	var blocks []anthropicInputBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("message content must be a string or an array of blocks")
	}

	var toolMsgs []types.Message // tool_result → role=tool (must precede the user text)
	var textParts []string
	var parts []types.ContentPart // multimodal accumulation (text + image)
	var toolCalls []types.ToolCall
	hasImage := false

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
			parts = append(parts, types.ContentPart{Type: "text", Text: b.Text})
		case "image":
			if b.Source != nil {
				url := b.Source.URL
				if b.Source.Type == "base64" && b.Source.Data != "" {
					url = fmt.Sprintf("data:%s;base64,%s", b.Source.MediaType, b.Source.Data)
				}
				if url != "" {
					hasImage = true
					parts = append(parts, types.ContentPart{Type: "image_url", ImageURL: &types.ImageURL{URL: url}})
				}
			}
		case "tool_use":
			toolCalls = append(toolCalls, types.ToolCall{
				ID:   b.ID,
				Type: "function",
				Function: types.Function{
					Name:      b.Name,
					Arguments: string(b.Input),
				},
			})
		case "tool_result":
			toolMsgs = append(toolMsgs, types.Message{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    anthropicTextFromField(b.Content),
			})
		}
	}

	out := make([]types.Message, 0, len(toolMsgs)+1)
	out = append(out, toolMsgs...)

	// Assemble the primary message for this turn (if any non-tool_result
	// content was present).
	if len(toolCalls) > 0 || len(textParts) > 0 || hasImage {
		msg := types.Message{Role: m.Role}
		switch {
		case hasImage:
			msg.Content = parts // multimodal array
		default:
			msg.Content = strings.Join(textParts, "")
		}
		if len(toolCalls) > 0 {
			msg.ToolCalls = toolCalls
		}
		out = append(out, msg)
	}

	if len(out) == 0 {
		// Content was e.g. an empty array — preserve an empty turn.
		out = append(out, types.Message{Role: m.Role, Content: ""})
	}
	return out, nil
}

// anthropicTextFromField coerces an Anthropic field that may be a bare string
// or a content-block array (system, tool_result content) into plain text.
func anthropicTextFromField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicInputBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// anthropicToolChoiceToInternal maps Anthropic's tool_choice to the OpenAI
// tool_choice the internal pipeline + providers understand:
//
//	{"type":"auto"}          → "auto"
//	{"type":"any"}           → "required"
//	{"type":"tool","name":X} → {"type":"function","function":{"name":X}}
//	{"type":"none"}          → "none"
func anthropicToolChoiceToInternal(raw json.RawMessage) interface{} {
	var tc anthropicToolChoice
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if tc.Name != "" {
			return map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": tc.Name},
			}
		}
	}
	return nil
}

// ---- outbound translation (non-streaming) ------------------------------------

// chatResponseToAnthropic renders an internal ChatResponse in the native
// Anthropic Messages shape. Provider-agnostic: works whether the response came
// from Anthropic, OpenAI, or any other routed provider.
func chatResponseToAnthropic(resp *types.ChatResponse) anthropicMessage {
	msg := anthropicMessage{
		ID:      anthropicMessageID(resp.ID),
		Type:    "message",
		Role:    "assistant",
		Model:   resp.Model,
		Content: []anthropicContentBlock{},
	}

	var finish string
	if len(resp.Choices) > 0 {
		c := resp.Choices[0]
		finish = c.FinishReason
		if text := messageContentToString(c.Message.Content); text != "" {
			msg.Content = append(msg.Content, anthropicContentBlock{Type: "text", Text: text})
		}
		for _, tcall := range c.Message.ToolCalls {
			input := json.RawMessage(tcall.Function.Arguments)
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			msg.Content = append(msg.Content, anthropicContentBlock{
				Type:  "tool_use",
				ID:    tcall.ID,
				Name:  tcall.Function.Name,
				Input: input,
			})
		}
	}
	msg.StopReason = finishReasonToAnthropic(finish, len(msg.Content) > 0 && msg.Content[len(msg.Content)-1].Type == "tool_use")

	if resp.Usage != nil {
		msg.Usage = anthropicUsage{
			InputTokens:              resp.Usage.PromptTokens,
			OutputTokens:             resp.Usage.CompletionTokens,
			CacheCreationInputTokens: resp.Usage.CacheCreationTokens,
			CacheReadInputTokens:     resp.Usage.CacheReadTokens,
		}
	}
	return msg
}

// finishReasonToAnthropic maps the OpenAI finish_reason vocabulary (what the
// internal ChatResponse always carries) back to Anthropic's stop_reason.
func finishReasonToAnthropic(finish string, endedOnToolUse bool) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	case "stop", "":
		if endedOnToolUse {
			return "tool_use"
		}
		return "end_turn"
	default:
		if endedOnToolUse {
			return "tool_use"
		}
		return "end_turn"
	}
}

// anthropicMessageID ensures the response id carries Anthropic's `msg_` prefix.
func anthropicMessageID(id string) string {
	if id == "" {
		return "msg_gateway"
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	// Normalize an OpenAI-style "chatcmpl-…" id into a stable msg_ id.
	trimmed := strings.TrimPrefix(id, "chatcmpl-")
	return "msg_" + trimmed
}

// messageContentToString coerces a Message.Content (string or []ContentPart)
// into plain text — concatenating text parts, ignoring images.
func messageContentToString(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []types.ContentPart:
		var sb strings.Builder
		for _, p := range v {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	case []interface{}:
		var sb strings.Builder
		for _, part := range v {
			if pm, ok := part.(map[string]interface{}); ok {
				if t, _ := pm["type"].(string); t == "text" {
					if txt, _ := pm["text"].(string); txt != "" {
						sb.WriteString(txt)
					}
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// ---- HTTP helpers ------------------------------------------------------------

// writeAnthropicMessage writes a ChatResponse as a native Anthropic message.
func (s *Server) writeAnthropicMessage(w http.ResponseWriter, resp *types.ChatResponse) {
	msg := chatResponseToAnthropic(resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(msg)
}

// writeAnthropicError renders an error in Anthropic's {type:"error",error:{…}}
// envelope so the SDK surfaces a proper APIError.
func (s *Server) writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": message},
	})
}

// handleMessages implements POST /v1/messages: parse the native Anthropic body,
// translate to the internal ChatRequest, mark the context so the shared
// completion handlers render Anthropic-shaped output, and hand off to the full
// OpenAI-path pipeline (classification, scanning, caching, routing, retry).
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
		return
	}
	chatReq, err := parseAnthropicToChatRequest(body)
	if err != nil {
		s.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	buf, err := json.Marshal(chatReq)
	if err != nil {
		s.writeAnthropicError(w, http.StatusInternalServerError, "api_error", "internal translation error")
		return
	}

	// Replace the body with the internal ChatRequest JSON so the shared
	// handler decodes it unchanged, and flag the context for Anthropic output.
	r.Body = io.NopCloser(bytes.NewReader(buf))
	r.ContentLength = int64(len(buf))
	ctx := context.WithValue(r.Context(), responseFormatCtxKey{}, responseFormatAnthropic)
	s.handleChatCompletion(w, r.WithContext(ctx))
}

// responseFormatFromContext reports which wire shape the completion handlers
// should render. Defaults to OpenAI when unset (every non-/v1/messages path).
func responseFormatFromContext(ctx context.Context) responseFormat {
	if ctx == nil {
		return responseFormatOpenAI
	}
	if f, ok := ctx.Value(responseFormatCtxKey{}).(responseFormat); ok {
		return f
	}
	return responseFormatOpenAI
}

// writeChatResponse writes a completed ChatResponse in the wire shape selected
// by the request context — Anthropic message for /v1/messages, OpenAI
// ChatCompletion otherwise. Replaces the raw json-encode at the non-streaming
// write-sites.
func (s *Server) writeChatResponse(w http.ResponseWriter, r *http.Request, resp *types.ChatResponse) {
	if responseFormatFromContext(r.Context()) == responseFormatAnthropic {
		s.writeAnthropicMessage(w, resp)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// writeErrorCtx writes an error in the wire shape selected by the request
// context. For /v1/messages it renders Anthropic's {type:"error",error:{…}}
// envelope; otherwise the gateway's standard OpenAI-ish error. Used at the
// shared completion-pipeline error sites so both SDKs get a parseable error.
func (s *Server) writeErrorCtx(w http.ResponseWriter, r *http.Request, status int, message string) {
	if responseFormatFromContext(r.Context()) == responseFormatAnthropic {
		s.writeAnthropicError(w, status, anthropicErrorType(status), message)
		return
	}
	s.writeErrorResponse(w, status, message)
}

// anthropicErrorType maps an HTTP status to Anthropic's error `type` string.
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusPaymentRequired, http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		if status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}

// ---- streaming encoders ------------------------------------------------------

// streamEncoder renders the internal chunk stream in a specific wire format.
// The shared streaming loop drives it (one writeChunk per chunk, then done),
// keeping AIQG stamping in one place regardless of output format.
type streamEncoder interface {
	writeChunk(*types.ChatChunk)
	done()
}

// newStreamEncoder picks the encoder for the request's wire format: native
// Anthropic named-event SSE for /v1/messages, OpenAI `data:`/[DONE] SSE
// otherwise.
func (s *Server) newStreamEncoder(w http.ResponseWriter, r *http.Request, req *types.ChatRequest) streamEncoder {
	flusher, _ := w.(http.Flusher)
	if responseFormatFromContext(r.Context()) == responseFormatAnthropic {
		return newAnthropicStreamEncoder(w, flusher, req, s.logger)
	}
	return &openAIStreamEncoder{w: w, flusher: flusher, logger: s.logger}
}

// openAIStreamEncoder emits the historical OpenAI SSE stream: one
// `data: <chunk-json>` frame per chunk, terminated by `data: [DONE]`.
type openAIStreamEncoder struct {
	w       io.Writer
	flusher http.Flusher
	logger  *logrus.Logger
}

func (e *openAIStreamEncoder) writeChunk(c *types.ChatChunk) {
	// The OpenAI SDK's high-level .stream() helper calls delta.to_dict() on
	// every choice, so a choice with no `delta` (e.g. the terminal
	// finish_reason chunk, which providers emit without one) crashes it with
	// "'NoneType' object has no attribute 'to_dict'". Guarantee a (possibly
	// empty) delta object on every choice so the helper — not just the
	// low-level stream=True iterator — works against the gateway.
	for i := range c.Choices {
		if c.Choices[i].Delta == nil {
			c.Choices[i].Delta = &types.Message{}
		}
	}
	data, err := json.Marshal(c)
	if err != nil {
		if e.logger != nil {
			e.logger.WithError(err).Error("Failed to marshal chunk")
		}
		return
	}
	fmt.Fprintf(e.w, "data: %s\n\n", data)
	if e.flusher != nil {
		e.flusher.Flush()
	}
}

func (e *openAIStreamEncoder) done() {
	fmt.Fprint(e.w, "data: [DONE]\n\n")
	if e.flusher != nil {
		e.flusher.Flush()
	}
}

// anthropicStreamEncoder translates the internal (OpenAI-shaped) chunk stream
// into Anthropic's native named-event SSE sequence:
//
//	message_start → content_block_start(text) → content_block_delta(text_delta)…
//	→ content_block_stop → [tool_use blocks]… → message_delta(stop_reason,usage)
//	→ message_stop
//
// Text deltas stream live. Tool calls (which OpenAI streams as fragmented
// function-argument deltas) are buffered per call and emitted as complete
// tool_use content blocks at done() — a valid Anthropic event sequence a
// client reconstructs into the same final tool_use, without the fragility of
// mapping partial OpenAI tool-call indices onto Anthropic block indices.
type anthropicStreamEncoder struct {
	w       io.Writer
	flusher http.Flusher
	logger  *logrus.Logger

	model string
	id    string

	started      bool
	textOpen     bool
	textIndex    int
	nextIndex    int
	stopReason   string
	inputTokens  int
	outputTokens int

	toolOrder []string
	tools     map[string]*anthropicStreamTool
}

type anthropicStreamTool struct {
	id   string
	name string
	args strings.Builder
}

func newAnthropicStreamEncoder(w io.Writer, flusher http.Flusher, req *types.ChatRequest, logger *logrus.Logger) *anthropicStreamEncoder {
	model := ""
	if req != nil {
		model = req.Model
	}
	return &anthropicStreamEncoder{
		w:       w,
		flusher: flusher,
		logger:  logger,
		model:   model,
		tools:   map[string]*anthropicStreamTool{},
	}
}

func (e *anthropicStreamEncoder) emit(event string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		if e.logger != nil {
			e.logger.WithError(err).Error("failed to marshal anthropic stream event")
		}
		return
	}
	fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", event, data)
	if e.flusher != nil {
		e.flusher.Flush()
	}
}

func (e *anthropicStreamEncoder) start(c *types.ChatChunk) {
	e.started = true
	if c != nil {
		if c.Model != "" {
			e.model = c.Model
		}
		if c.ID != "" {
			e.id = anthropicMessageID(c.ID)
		}
	}
	if e.id == "" {
		e.id = "msg_gateway"
	}
	e.emit("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            e.id,
			"type":          "message",
			"role":          "assistant",
			"model":         e.model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]interface{}{"input_tokens": e.inputTokens, "output_tokens": 0},
		},
	})
}

func (e *anthropicStreamEncoder) ensureTextBlock() {
	if e.textOpen {
		return
	}
	e.textIndex = e.nextIndex
	e.nextIndex++
	e.textOpen = true
	e.emit("content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         e.textIndex,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})
}

func (e *anthropicStreamEncoder) bufferTool(tc types.ToolCall) {
	key := tc.ID
	if key == "" && len(e.toolOrder) > 0 {
		key = e.toolOrder[len(e.toolOrder)-1] // fragment continues the latest tool
	}
	if key == "" {
		key = fmt.Sprintf("tool_%d", len(e.toolOrder))
	}
	t, ok := e.tools[key]
	if !ok {
		t = &anthropicStreamTool{id: tc.ID, name: tc.Function.Name}
		if t.id == "" {
			t.id = key
		}
		e.tools[key] = t
		e.toolOrder = append(e.toolOrder, key)
	}
	if t.name == "" && tc.Function.Name != "" {
		t.name = tc.Function.Name
	}
	t.args.WriteString(tc.Function.Arguments)
}

func (e *anthropicStreamEncoder) writeChunk(c *types.ChatChunk) {
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
		if ch.Delta != nil {
			if txt := messageContentToString(ch.Delta.Content); txt != "" {
				e.ensureTextBlock()
				e.emit("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": e.textIndex,
					"delta": map[string]interface{}{"type": "text_delta", "text": txt},
				})
			}
			for _, tc := range ch.Delta.ToolCalls {
				e.bufferTool(tc)
			}
		}
		if ch.FinishReason != "" {
			e.stopReason = finishReasonToAnthropic(ch.FinishReason, len(e.toolOrder) > 0)
		}
	}
}

func (e *anthropicStreamEncoder) done() {
	if !e.started {
		e.start(nil)
	}
	if e.textOpen {
		e.emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": e.textIndex})
		e.textOpen = false
	}
	for _, key := range e.toolOrder {
		t := e.tools[key]
		idx := e.nextIndex
		e.nextIndex++
		input := t.args.String()
		if strings.TrimSpace(input) == "" {
			input = "{}"
		}
		e.emit("content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         idx,
			"content_block": map[string]interface{}{"type": "tool_use", "id": t.id, "name": t.name, "input": map[string]interface{}{}},
		})
		e.emit("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": input},
		})
		e.emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": idx})
	}
	if e.stopReason == "" {
		if len(e.toolOrder) > 0 {
			e.stopReason = "tool_use"
		} else {
			e.stopReason = "end_turn"
		}
	}
	e.emit("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": e.stopReason, "stop_sequence": nil},
		"usage": map[string]interface{}{"output_tokens": e.outputTokens},
	})
	e.emit("message_stop", map[string]interface{}{"type": "message_stop"})
}
