package routing

import (
	"context"
	"strings"
	"testing"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func TestLimitsContextRoundTrip(t *testing.T) {
	l := resilience.Limits{MaxContextWindow: 50000}
	ctx := WithLimits(context.Background(), l)
	ctx = context.WithValue(ctx, struct{ k string }{"unrelated"}, 1)
	if got := LimitsFrom(ctx); got.MaxContextWindow != 50000 {
		t.Fatalf("limits did not survive context derivation: %+v", got)
	}
	// Empty limits must store nothing, so "no limits" and "limits with no
	// values" are the same code path.
	if got := LimitsFrom(WithLimits(context.Background(), resilience.Limits{})); !got.IsZero() {
		t.Fatal("an empty limits block was stored")
	}
}

// The breach message must name both numbers: "3,000 tokens over a 200,000
// window" tells an operator whether to trim a prompt or change models.
func TestBreachNamesWindowAndOverage(t *testing.T) {
	b := LimitBreach{Provider: "anthropic", Model: "claude-haiku-4-5", Window: 200000, Overage: 3000}
	msg := b.Error()
	for _, want := range []string{"3000", "200000", "anthropic/claude-haiku-4-5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q omits %q", msg, want)
		}
	}
}

// The two causes call for different fixes, and only one is the vendor's doing.
func TestBreachDistinguishesConfiguredLimitFromModelWindow(t *testing.T) {
	fromLimit := LimitBreach{Window: 50000, Overage: 10, FromLimit: true}.Error()
	fromModel := LimitBreach{Window: 200000, Overage: 10}.Error()
	if !strings.Contains(fromLimit, "configured context limit") {
		t.Fatalf("configured breach reads as a model limit: %q", fromLimit)
	}
	if !strings.Contains(fromModel, "model's context window") {
		t.Fatalf("model breach reads as a configured limit: %q", fromModel)
	}
}

func TestCheckLimitsIsInertWithoutCapability(t *testing.T) {
	r := chainRouter(t)
	// An unknown model must not become an implicit zero window, which would
	// reject everything.
	if b := r.CheckLimits(context.Background(), "openai", "unknown-model", &types.ChatRequest{}); b != nil {
		t.Fatalf("an unknown model produced a breach: %+v", b)
	}
}

func TestOutputCapLowersButNeverRaises(t *testing.T) {
	l := resilience.Limits{MaxOutputTokens: 100}
	// Smallest of advertised, configured and requested.
	if got := l.EffectiveOutputTokens(64000, 5000); got != 100 {
		t.Fatalf("cap did not bind: %d", got)
	}
	if got := l.EffectiveOutputTokens(64000, 50); got != 50 {
		t.Fatalf("a smaller request should win: %d", got)
	}
}
