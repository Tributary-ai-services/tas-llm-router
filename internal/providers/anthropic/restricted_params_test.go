package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

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
