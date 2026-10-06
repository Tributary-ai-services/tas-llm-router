package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// Measured against the vendor 2026-10-06, one parameter at a time. The 4.7+
// generation answers 400 "`temperature` is deprecated for this model" (and the
// same for top_p / top_k), while stop_sequences and max_tokens stay fine --
// which is where Anthropic's restricted set differs from OpenAI's.
//
// These fail against the pre-RT-5 translation, which sent everything through.
func testProvider() *AnthropicProvider {
	return &AnthropicProvider{
		config: &AnthropicConfig{
			Models: []types.ModelInfo{
				{Name: "claude-sonnet-5-5", ProviderModelID: "claude-sonnet-5-5", RestrictedParams: true},
				{Name: "claude-haiku-4-5-20251001", ProviderModelID: "claude-haiku-4-5-20251001"},
			},
		},
	}
}

func f32(v float32) *float32 { return &v }
func ip(v int) *int          { return &v }

func TestRestricted_DropsTemperatureAndTopP(t *testing.T) {
	p := testProvider()
	got, err := p.convertToAnthropicRequest(&types.ChatRequest{
		Model: "claude-sonnet-5-5", MaxTokens: ip(16),
		Temperature: f32(0.3), TopP: f32(0.9),
		Stop:     []string{"xyz"},
		Messages: []types.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got.Temperature.Valid() {
		t.Errorf("Temperature was sent; the vendor 400s on anything but 1")
	}
	if got.TopP.Valid() {
		t.Errorf("TopP was sent; the vendor 400s on it")
	}
	// stop_sequences and max_tokens are still accepted here, unlike OpenAI.
	if len(got.StopSequences) != 1 {
		t.Errorf("StopSequences dropped, but Anthropic accepts them: %v", got.StopSequences)
	}
	if got.MaxTokens != 16 {
		t.Errorf("MaxTokens = %d, want 16 — Anthropic has no max_completion_tokens", got.MaxTokens)
	}
}

// Temperature 1 is accepted, so dropping it would be over-correction.
func TestRestricted_KeepsDefaultTemperature(t *testing.T) {
	p := testProvider()
	got, err := p.convertToAnthropicRequest(&types.ChatRequest{
		Model: "claude-sonnet-5-5", MaxTokens: ip(16), Temperature: f32(1),
		Messages: []types.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !got.Temperature.Valid() {
		t.Errorf("Temperature 1 was dropped, but the vendor accepts it")
	}
}

func TestUnrestricted_KeepsTemperature(t *testing.T) {
	p := testProvider()
	got, err := p.convertToAnthropicRequest(&types.ChatRequest{
		Model: "claude-haiku-4-5-20251001", MaxTokens: ip(16),
		Temperature: f32(0.3),
		Messages:    []types.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !got.Temperature.Valid() {
		t.Errorf("<=4.6 model lost temperature, which it accepts")
	}
}

func TestUnknownModel_KeepsEverything(t *testing.T) {
	p := testProvider()
	got, err := p.convertToAnthropicRequest(&types.ChatRequest{
		Model: "claude-not-in-our-catalog", MaxTokens: ip(16), Temperature: f32(0.3),
		Messages: []types.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !got.Temperature.Valid() {
		t.Errorf("unknown model should keep classic params (pre-RT-5 behaviour)")
	}
}

// Anthropic 400s when temperature and top_p are both present, on EVERY model
// (measured on claude-haiku-4-5, which has no other restriction). OpenAI
// accepts both, so an OpenAI-shaped client setting both is an ordinary request
// that must not fail. temperature wins; top_p is dropped and counted.
func TestUnrestricted_TemperatureAndTopPAreMutuallyExclusive(t *testing.T) {
	p := testProvider()
	got, err := p.convertToAnthropicRequest(&types.ChatRequest{
		Model: "claude-haiku-4-5-20251001", MaxTokens: ip(16),
		Temperature: f32(0.3), TopP: f32(0.9),
		Messages: []types.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !got.Temperature.Valid() {
		t.Errorf("temperature should win; it is the knob callers actually set")
	}
	if got.TopP.Valid() {
		t.Errorf("top_p was sent alongside temperature; the vendor 400s on the pair")
	}
}

// top_p alone must still reach the vendor — it is accepted on its own.
func TestUnrestricted_TopPAloneSurvives(t *testing.T) {
	p := testProvider()
	got, err := p.convertToAnthropicRequest(&types.ChatRequest{
		Model: "claude-haiku-4-5-20251001", MaxTokens: ip(16), TopP: f32(0.9),
		Messages: []types.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !got.TopP.Valid() {
		t.Errorf("top_p alone was dropped, but the vendor accepts it")
	}
}

// RT-6: extended thinking, decided per model on RT-5's declared flag.
//
// The pinned SDK can express only {enabled, budget_tokens} and {disabled}, and
// `adaptive` — the one form the 4.7+ generation accepts — has no representation
// in it. That is survivable because omitting the parameter on those models runs
// thinking adaptively, which is what the caller wanted. These tests pin both
// halves of that decision, since getting it backwards means either a 400 or a
// silently non-thinking model.
func TestThinking_RestrictedModelOmitsTheParameter(t *testing.T) {
	provider := testProvider()
	req := &types.ChatRequest{
		Model:     "claude-sonnet-5-5", // declared RestrictedParams
		MaxTokens: ip(64000),
		Thinking:  &types.ThinkingConfig{Type: "enabled", BudgetTokens: 31999},
		Messages:  []types.Message{{Role: "user", Content: "hi"}},
	}
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, _ := json.Marshal(got)
	// budget_tokens on this generation is a 400. Omitting the whole block is the
	// only legal way to ask for thinking here, and it still gets it.
	if strings.Contains(string(b), "budget_tokens") {
		t.Errorf("sent budget_tokens to a restricted model — the vendor rejects it:\n%s", b)
	}
	if strings.Contains(string(b), `"thinking"`) {
		t.Errorf("sent a thinking block to a restricted model; omission is what runs adaptive:\n%s", b)
	}
}

func TestThinking_UnrestrictedModelKeepsTheBudget(t *testing.T) {
	provider := testProvider()
	req := &types.ChatRequest{
		Model:     "claude-haiku-4-5-20251001", // not restricted
		MaxTokens: ip(8000),
		Thinking:  &types.ThinkingConfig{Type: "enabled", BudgetTokens: 4096},
		Messages:  []types.Message{{Role: "user", Content: "hi"}},
	}
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), "budget_tokens") || !strings.Contains(string(b), "4096") {
		t.Errorf("a model that accepts a thinking budget did not get one:\n%s", b)
	}
}

// The budget must stay below max_tokens or the vendor 400s.
func TestThinking_BudgetClampedBelowMaxTokens(t *testing.T) {
	provider := testProvider()
	req := &types.ChatRequest{
		Model:     "claude-haiku-4-5-20251001",
		MaxTokens: ip(2048),
		Thinking:  &types.ThinkingConfig{Type: "enabled", BudgetTokens: 31999},
		Messages:  []types.Message{{Role: "user", Content: "hi"}},
	}
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got.Thinking.OfEnabled == nil {
		t.Fatal("thinking dropped entirely where it could have been clamped")
	}
	if got.Thinking.OfEnabled.BudgetTokens >= 2048 {
		t.Errorf("budget %d must be below max_tokens 2048", got.Thinking.OfEnabled.BudgetTokens)
	}
}

// A thinking block replayed without its signature cannot be sent: the vendor
// rejects it. Dropping it is the only safe handling, and it must not take the
// rest of the turn with it.
func TestThinking_UnsignedBlockIsDroppedNotSent(t *testing.T) {
	provider := testProvider()
	req := &types.ChatRequest{
		Model: "claude-haiku-4-5-20251001",
		Messages: []types.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: []types.ContentPart{
				{Type: "thinking", Thinking: "unsigned reasoning"},
				{Type: "text", Text: "answer"},
			}},
			{Role: "user", Content: "more"},
		},
	}
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, _ := json.Marshal(got.Messages)
	if strings.Contains(string(b), "unsigned reasoning") {
		t.Errorf("an unsigned thinking block was sent; the vendor rejects it:\n%s", b)
	}
	if !strings.Contains(string(b), "answer") {
		t.Errorf("dropping the unsigned block took the rest of the turn with it:\n%s", b)
	}
}

// A signed block must survive, signature intact.
func TestThinking_SignedBlockIsReplayed(t *testing.T) {
	provider := testProvider()
	req := &types.ChatRequest{
		Model: "claude-haiku-4-5-20251001",
		Messages: []types.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: []types.ContentPart{
				{Type: "thinking", Thinking: "step one", Signature: "sig-abc"},
				{Type: "text", Text: "answer"},
			}},
			{Role: "user", Content: "more"},
		},
	}
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, _ := json.Marshal(got.Messages)
	for _, want := range []string{"step one", "sig-abc"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("replayed thinking lost %q:\n%s", want, b)
		}
	}
}

// Beta forwarding (AIQG-43 item 1). The gateway re-serialises every request
// rather than proxying it, so a header the caller set reaches the vendor only
// if something puts it back — and nothing did, so `context-1m` never arrived
// and a request over 200k tokens was rejected outright.
func TestBetaHeader_ForwardsOnlyTheAllowlist(t *testing.T) {
	// Exactly what a captured Claude Code request carries, all nine.
	got := betaHeaderFor([]string{
		"claude-code-20250219",
		"oauth-2025-04-20",
		"interleaved-thinking-2025-05-14",
		"context-management-2025-06-27",
		"context-1m-2025-08-07",
		"extended-cache-ttl-2025-04-11",
		"prompt-caching-scope-2026-01-05",
		"thinking-token-count-2026-05-13",
		"advisor-tool-2026-03-01",
	})
	for _, want := range []string{"context-1m-2025-08-07", "interleaved-thinking-2025-05-14", "extended-cache-ttl-2025-04-11"} {
		if !strings.Contains(got, want) {
			t.Errorf("allowlisted beta %q was not forwarded: %q", want, got)
		}
	}
	// An unrecognised beta can change the RESPONSE shape, and this gateway
	// parses every response — an unparseable one is a 500 for the caller.
	for _, never := range []string{"oauth-2025-04-20", "claude-code-20250219", "context-management-2025-06-27", "advisor-tool-2026-03-01"} {
		if strings.Contains(got, never) {
			t.Errorf("forwarded %q, which is not on the allowlist: %q", never, got)
		}
	}
}

func TestBetaHeader_NoneSurvivingMeansNoHeader(t *testing.T) {
	if h := betaHeaderFor([]string{"oauth-2025-04-20"}); h != "" {
		t.Errorf("want no header when nothing survives, got %q", h)
	}
	if opts := betaOptions(&types.ChatRequest{Betas: []string{"oauth-2025-04-20"}}); opts != nil {
		t.Error("an unforwardable beta must add no request option at all")
	}
	// The ordinary case: a caller who sent no betas must produce a request
	// byte-identical to what it was before this feature existed.
	if opts := betaOptions(&types.ChatRequest{}); opts != nil {
		t.Error("no betas must mean no options")
	}
}

// The header has to survive the SDK, not just our own function, so this asserts
// what actually lands on the wire.
func TestBetaHeader_ReachesTheWire(t *testing.T) {
	var seen string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		seen = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001",
			"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":5,"output_tokens":2}}`))
	}))
	defer srv.Close()

	p := NewAnthropicProvider(&AnthropicConfig{
		APIKey:  "test-key",
		BaseURL: srv.URL,
		Models:  []types.ModelInfo{{Name: "claude-haiku-4-5-20251001", ProviderModelID: "claude-haiku-4-5-20251001"}},
	}, logrus.New())

	_, err := p.ChatCompletion(context.Background(), &types.ChatRequest{
		Model:     "claude-haiku-4-5-20251001",
		MaxTokens: ip(16),
		Messages:  []types.Message{{Role: "user", Content: "hi"}},
		Betas:     []string{"context-1m-2025-08-07", "oauth-2025-04-20"},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one upstream call, got %d", calls)
	}
	if seen != "context-1m-2025-08-07" {
		t.Errorf("wire header = %q, want only the allowlisted beta", seen)
	}
}

// Images (AIQG-43 item 3). `convertMessage` carried a literal
// "// Skip image parts for now", so a screenshot in a user turn never reached
// the model at all — the model answered about an image it had not seen, which
// reads as the model being wrong rather than as a gateway dropping content.
func TestImages_UserTurnCarriesBase64AndURL(t *testing.T) {
	p := testProvider()
	got, err := p.convertToAnthropicRequest(&types.ChatRequest{
		Model: "claude-haiku-4-5-20251001", MaxTokens: ip(64),
		Messages: []types.Message{{Role: "user", Content: []types.ContentPart{
			{Type: "text", Text: "What is in these?"},
			{Type: "image_url", ImageURL: &types.ImageURL{URL: "data:image/png;base64,iVBORw0KGgo="}},
			{Type: "image_url", ImageURL: &types.ImageURL{URL: "https://example.com/diagram.png"}},
		}}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, _ := json.Marshal(got.Messages)
	for _, want := range []string{"iVBORw0KGgo=", "image/png", "https://example.com/diagram.png", "What is in these?"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("image content lost (%q):\n%s", want, b)
		}
	}
}

// A URL shape we cannot classify is reported, never sent as nothing: a model
// answering about an image it never received looks like a wrong model.
func TestImages_UnusableSourceIsDroppedAndCounted(t *testing.T) {
	p := testProvider()
	got, err := p.convertToAnthropicRequest(&types.ChatRequest{
		Model: "claude-haiku-4-5-20251001", MaxTokens: ip(64),
		Messages: []types.Message{{Role: "user", Content: []types.ContentPart{
			{Type: "text", Text: "keep me"},
			{Type: "image_url", ImageURL: &types.ImageURL{URL: "blob:https://app.local/9f2c"}},
			{Type: "image_url", ImageURL: &types.ImageURL{URL: "data:text/plain,hello"}},
		}}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, _ := json.Marshal(got.Messages)
	for _, never := range []string{"blob:", "text/plain"} {
		if strings.Contains(string(b), never) {
			t.Errorf("unusable source %q was sent upstream:\n%s", never, b)
		}
	}
	if !strings.Contains(string(b), "keep me") {
		t.Errorf("dropping an unusable image took the rest of the turn with it:\n%s", b)
	}
}

// The second half of the finding: a tool that returns a screenshot. Anthropic
// accepts text AND image inside a tool_result, so it must survive.
func TestImages_ToolResultCarriesAnImage(t *testing.T) {
	p := testProvider()
	got, err := p.convertToAnthropicRequest(&types.ChatRequest{
		Model: "claude-haiku-4-5-20251001", MaxTokens: ip(64),
		Messages: []types.Message{
			{Role: "user", Content: "screenshot the page"},
			{Role: "assistant", ToolCalls: []types.ToolCall{{ID: "toolu_1", Type: "function",
				Function: types.Function{Name: "screenshot", Arguments: "{}"}}}},
			{Role: "tool", ToolCallID: "toolu_1", Content: []types.ContentPart{
				{Type: "text", Text: "captured"},
				{Type: "image_url", ImageURL: &types.ImageURL{URL: "data:image/jpeg;base64,/9j/4AAQ"}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, _ := json.Marshal(got.Messages)
	for _, want := range []string{"tool_result", "toolu_1", "captured", "/9j/4AAQ", "image/jpeg"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("tool_result lost %q:\n%s", want, b)
		}
	}
}

// A plain text tool result must still travel as a plain string, so nothing
// downstream meets a shape it has not seen before.
func TestImages_PlainToolResultShapeUnchanged(t *testing.T) {
	p := testProvider()
	got, err := p.convertToAnthropicRequest(&types.ChatRequest{
		Model: "claude-haiku-4-5-20251001", MaxTokens: ip(64),
		Messages: []types.Message{
			{Role: "user", Content: "read it"},
			{Role: "tool", ToolCallID: "toolu_2", Content: "file contents"},
		},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, _ := json.Marshal(got.Messages)
	if !strings.Contains(string(b), "file contents") || !strings.Contains(string(b), "toolu_2") {
		t.Errorf("ordinary tool result changed shape:\n%s", b)
	}
}

// The /v1/messages handler marshals the internal request between the boundary
// and the provider, so every block-form message makes a JSON round trip. Before
// Message.UnmarshalJSON existed, Content came back as []interface{}, every
// `case []types.ContentPart:` missed, and the block array was stringified into
// the vendor request by a `fmt.Sprintf("%v", …)` default.
//
// Found live: a 512x512 image cost 2,822 input tokens (text tokenisation of its
// base64) and the model replied "the image data came through as a raw encoded
// string". The same loss hit per-block cache_control and replayed thinking
// blocks, so this test covers all three.
func TestRoundTrip_BlockContentSurvivesTheHandlerMarshal(t *testing.T) {
	orig := &types.ChatRequest{
		Model: "claude-haiku-4-5-20251001", MaxTokens: ip(64),
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentPart{
				{Type: "text", Text: "what is this", CacheControl: &types.CacheControl{Type: "ephemeral"}},
				{Type: "image_url", ImageURL: &types.ImageURL{URL: "data:image/png;base64,iVBORw0KGgo="}},
			}},
			{Role: "assistant", Content: []types.ContentPart{
				{Type: "thinking", Thinking: "step one", Signature: "sig-abc"},
				{Type: "text", Text: "answer"},
			}},
			{Role: "user", Content: "and then?"},
		},
	}
	buf, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round types.ChatRequest
	if err := json.Unmarshal(buf, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := round.Messages[0].Content.([]types.ContentPart); !ok {
		t.Fatalf("content lost its type across the round trip: %T", round.Messages[0].Content)
	}
	if s, ok := round.Messages[2].Content.(string); !ok || s != "and then?" {
		t.Errorf("a plain string message must stay a string, got %T", round.Messages[2].Content)
	}

	p := testProvider()
	got, err := p.convertToAnthropicRequest(&round)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, _ := json.Marshal(got.Messages)
	s := string(b)
	for _, want := range []string{`"source"`, "iVBORw0KGgo=", "sig-abc", "step one", "cache_control"} {
		if !strings.Contains(s, want) {
			t.Errorf("round trip lost %q — it would reach the vendor as text:\n%.400s", want, s)
		}
	}
	// The giveaway when this regresses: Go's rendering of a map slice.
	if strings.Contains(s, "map[") {
		t.Errorf("block array was stringified into the request:\n%.400s", s)
	}
}
