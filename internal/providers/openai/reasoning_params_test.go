package openai

import (
	"testing"

	"github.com/sashabaranov/go-openai"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// Measured against the vendor 2026-10-06, one parameter at a time. These cases
// encode what OpenAI actually rejects, not what the docs imply:
//
//	max_tokens         -> 400, use max_completion_tokens
//	temperature != 1   -> 400, only the default is accepted
//	top_p              -> 400
//	frequency_penalty  -> 400
//	presence_penalty   -> 400
//	stop               -> 400
//	seed               -> accepted
//
// Each assertion below fails against the pre-RT-5 translation, which passed all
// of them straight through.
func testProvider() *OpenAIProvider {
	return &OpenAIProvider{
		config: &OpenAIConfig{
			Models: []types.ModelInfo{
				{Name: "gpt-5-mini", ProviderModelID: "gpt-5-mini", RestrictedParams: true},
				{Name: "gpt-4o-mini", ProviderModelID: "gpt-4o-mini"},
			},
		},
	}
}

func mustConvert(t *testing.T, p *OpenAIProvider, req *types.ChatRequest) *openai.ChatCompletionRequest {
	t.Helper()
	got, err := p.convertToOpenAIRequest(req)
	if err != nil {
		t.Fatalf("convertToOpenAIRequest: %v", err)
	}
	return got
}

func f32(v float32) *float32 { return &v }
func ip(v int) *int          { return &v }

func TestReasoningModel_UsesMaxCompletionTokens(t *testing.T) {
	p := testProvider()
	got := mustConvert(t, p, &types.ChatRequest{
		Model: "gpt-5-mini", MaxTokens: ip(64),
		Messages: []types.Message{{Role: "user", Content: "hi"}},
	})
	if got.MaxCompletionTokens != 64 {
		t.Errorf("MaxCompletionTokens = %d, want 64", got.MaxCompletionTokens)
	}
	if got.MaxTokens != 0 {
		t.Errorf("MaxTokens = %d, want 0 — sending it is a hard 400", got.MaxTokens)
	}
}

func TestClassicModel_StillUsesMaxTokens(t *testing.T) {
	p := testProvider()
	got := mustConvert(t, p, &types.ChatRequest{
		Model: "gpt-4o-mini", MaxTokens: ip(64),
		Messages: []types.Message{{Role: "user", Content: "hi"}},
	})
	if got.MaxTokens != 64 {
		t.Errorf("MaxTokens = %d, want 64 (no regression for gpt-4-era)", got.MaxTokens)
	}
	if got.MaxCompletionTokens != 0 {
		t.Errorf("MaxCompletionTokens = %d, want 0", got.MaxCompletionTokens)
	}
}

func TestReasoningModel_DropsUnsupportedSamplingParams(t *testing.T) {
	p := testProvider()
	got := mustConvert(t, p, &types.ChatRequest{
		Model:       "gpt-5-mini",
		Temperature: f32(0.2), TopP: f32(0.9),
		FrequencyPenalty: f32(0.5), PresencePenalty: f32(0.5),
		Stop:     []string{"xyz"},
		Messages: []types.Message{{Role: "user", Content: "hi"}},
	})
	if got.Temperature != 0 {
		t.Errorf("Temperature = %v, want 0 (omitted); only the default 1 is accepted", got.Temperature)
	}
	if got.TopP != 0 {
		t.Errorf("TopP = %v, want 0 (omitted)", got.TopP)
	}
	if got.FrequencyPenalty != 0 {
		t.Errorf("FrequencyPenalty = %v, want 0 (omitted)", got.FrequencyPenalty)
	}
	if got.PresencePenalty != 0 {
		t.Errorf("PresencePenalty = %v, want 0 (omitted)", got.PresencePenalty)
	}
	if len(got.Stop) != 0 {
		t.Errorf("Stop = %v, want empty", got.Stop)
	}
}

// temperature 1 is the one value the vendor accepts, so it must survive —
// dropping it unconditionally would be over-correction.
func TestReasoningModel_KeepsDefaultTemperature(t *testing.T) {
	p := testProvider()
	got := mustConvert(t, p, &types.ChatRequest{
		Model: "gpt-5-mini", Temperature: f32(1),
		Messages: []types.Message{{Role: "user", Content: "hi"}},
	})
	if got.Temperature != 1 {
		t.Errorf("Temperature = %v, want 1 (the accepted default)", got.Temperature)
	}
}

func TestClassicModel_KeepsAllSamplingParams(t *testing.T) {
	p := testProvider()
	got := mustConvert(t, p, &types.ChatRequest{
		Model:       "gpt-4o-mini",
		Temperature: f32(0.2), TopP: f32(0.9),
		FrequencyPenalty: f32(0.5), PresencePenalty: f32(0.5),
		Stop:     []string{"xyz"},
		Messages: []types.Message{{Role: "user", Content: "hi"}},
	})
	if got.Temperature != 0.2 || got.TopP != 0.9 || got.FrequencyPenalty != 0.5 ||
		got.PresencePenalty != 0.5 || len(got.Stop) != 1 {
		t.Errorf("classic model lost a parameter: %+v", got)
	}
}

// An unknown model must fall back to the classic set, so a model missing from
// the catalog behaves exactly as it did before RT-5 rather than losing params.
func TestUnknownModel_FallsBackToClassic(t *testing.T) {
	p := testProvider()
	got := mustConvert(t, p, &types.ChatRequest{
		Model: "some-model-we-never-catalogued", MaxTokens: ip(32), Temperature: f32(0.3),
		Messages: []types.Message{{Role: "user", Content: "hi"}},
	})
	if got.MaxTokens != 32 || got.Temperature != 0.3 {
		t.Errorf("unknown model should keep classic params, got %+v", got)
	}
}

// OpenAI rejects multipart content on the tool role, so a tool result carrying
// an image — which only the Anthropic surface can produce — must be flattened
// to its text rather than sent as blocks. Sending blocks would 400 the whole
// request, which is worse than losing an image this vendor cannot accept in
// that position at all.
func TestToolResultWithImage_FlattensForOpenAI(t *testing.T) {
	p := &OpenAIProvider{config: &OpenAIConfig{Models: []types.ModelInfo{{Name: "gpt-4o-mini", ProviderModelID: "gpt-4o-mini"}}}}
	got, err := p.convertToOpenAIRequest(&types.ChatRequest{
		Model: "gpt-4o-mini",
		Messages: []types.Message{
			{Role: "user", Content: "screenshot it"},
			{Role: "tool", ToolCallID: "call_1", Content: []types.ContentPart{
				{Type: "text", Text: "captured"},
				{Type: "image_url", ImageURL: &types.ImageURL{URL: "data:image/png;base64,iVBORw0KGgo="}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var toolMsg *openai.ChatCompletionMessage
	for i := range got.Messages {
		if got.Messages[i].Role == "tool" {
			toolMsg = &got.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("tool message missing")
	}
	if len(toolMsg.MultiContent) != 0 {
		t.Errorf("tool role must carry a string, not %d multipart items — OpenAI 400s on this", len(toolMsg.MultiContent))
	}
	if toolMsg.Content != "captured" {
		t.Errorf("tool content = %q, want the flattened text", toolMsg.Content)
	}
}
