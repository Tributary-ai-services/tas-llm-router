package anthropic

import (
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
