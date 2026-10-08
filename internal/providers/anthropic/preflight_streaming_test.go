package anthropic

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/shared/constant"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// The SDK refuses a large non-streaming request CLIENT-SIDE, before any HTTP
// request, with a plain fmt.Errorf. That reached the caller as an opaque 500
// labelled vendor_error -- when the vendor had never been contacted. These
// tests pin the mirrored threshold and the 400.

func TestRequiresStreaming_MirrorsTheSDKFormula(t *testing.T) {
	// expected = 1h * maxTokens/128000 > 10m  =>  maxTokens > 128000/6 ~= 21333
	cases := map[int64]bool{
		0:      false, // unset: not our business
		1024:   false,
		21000:  false,
		21334:  true,
		64000:  true, // the measured AIQG-50 case
		128000: true,
	}
	for mt, want := range cases {
		if got := requiresStreaming("claude-sonnet-5-5", mt); got != want {
			t.Errorf("requiresStreaming(max_tokens=%d) = %v, want %v", mt, got, want)
		}
	}
}

// The per-model cap is the SDK's own table, read rather than copied, so the two
// cannot drift. If the SDK drops the table this test says so instead of
// silently passing.
func TestRequiresStreaming_UsesTheSDKsOwnModelTable(t *testing.T) {
	if len(constant.ModelNonStreamingTokens) == 0 {
		t.Skip("the SDK no longer publishes ModelNonStreamingTokens; the mirror needs revisiting")
	}
	for model, limit := range constant.ModelNonStreamingTokens {
		// One token over that model's cap must require streaming even when the
		// duration heuristic alone would not trip.
		if over := int64(limit) + 1; !requiresStreaming(model, over) {
			t.Errorf("model %q: max_tokens %d exceeds its cap %d but streaming was not required",
				model, over, limit)
		}
		break // one entry is enough to prove the table is consulted
	}
}

// The classifier upstream of this relays a 4xx verbatim, so the status and the
// message are the contract: a 400 (not 500) carrying the fix.
func TestChatCompletion_LargeNonStreamingIsA400NamingTheFix(t *testing.T) {
	provider := createTestProvider(t)
	maxTokens := 64000
	_, err := provider.ChatCompletion(context.Background(), &types.ChatRequest{
		Model:     "claude-sonnet-5-5",
		MaxTokens: &maxTokens,
		Stream:    false,
		Messages:  []types.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("expected a pre-flight rejection")
	}
	var ue *types.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("not an UpstreamError, so the server cannot classify it: %T %v", err, err)
	}
	if ue.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want 400 -- a 5xx would be reported as vendor_error for a request the vendor never saw", ue.Status)
	}
	if ue.Retryable() {
		t.Errorf("a deterministic invalid request must not be reported retryable")
	}
	if !strings.Contains(ue.Message, "stream") {
		t.Errorf("the message does not name the fix: %q", ue.Message)
	}
}
