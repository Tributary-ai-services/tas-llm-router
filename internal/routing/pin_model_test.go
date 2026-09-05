package routing

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// #151: a pin whose provider is configured and healthy but does not serve the
// requested model is the third way a pin can be unusable. Before the fix it
// routed anyway and surfaced as an opaque upstream 404 -> 500; it must now
// degrade like the other two cases, with the reason on the decision.

// modelRouter builds a router where anthropic advertises only Claude models and
// openai advertises gpt-4o-mini — so a pin to anthropic for gpt-4o-mini is
// configured and healthy yet cannot serve the model.
func modelRouter(t *testing.T) *Router {
	t.Helper()
	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)
	r := NewRouter(logger)
	r.RegisterProvider("anthropic", &stubProvider{
		name:   "anthropic",
		models: []types.ModelInfo{{Name: "claude-haiku-4-5", ProviderModelID: "claude-haiku-4-5"}},
	})
	r.RegisterProvider("openai", &stubProvider{
		name:   "openai",
		models: []types.ModelInfo{{Name: "gpt-4o-mini", ProviderModelID: "gpt-4o-mini"}},
	})
	r.updateHealthStatus(context.Background())
	return r
}

func TestPinnedProviderNotServingModelEntersChain(t *testing.T) {
	r := modelRouter(t)
	c := NewChain(fb(resilience.ChainEntry{Provider: "openai", Model: "gpt-4o-mini"}), nil)
	ctx := WithChain(WithPinnedProvider(context.Background(), "anthropic"), c)

	dec, prov, err := r.routeWithPin(ctx, &types.ChatRequest{Model: "gpt-4o-mini"}, RoutingStrategySpecific)
	if err != nil {
		t.Fatal(err)
	}
	if dec.SelectedProvider != "openai" {
		t.Fatalf("selected %q; a pin that cannot serve the model must enter the chain, not 404 upstream", dec.SelectedProvider)
	}
	if prov == nil {
		t.Fatal("nil provider returned")
	}
	if !reasoningMentions(dec.Reasoning, "does not serve model gpt-4o-mini") {
		t.Fatalf("reasoning %v does not name the unservable-model reason", dec.Reasoning)
	}
}

// Without a chain the request still falls through (the interim posture shared
// by every unusable-pin case), but the reason — not an opaque 500 — is on the
// decision.
func TestPinnedProviderNotServingModelFallsThroughWithoutChain(t *testing.T) {
	r := modelRouter(t)
	ctx := WithPinnedProvider(context.Background(), "anthropic")

	dec, _, err := r.routeWithPin(ctx, &types.ChatRequest{Model: "gpt-4o-mini"}, RoutingStrategySpecific)
	if err != nil {
		t.Fatal(err)
	}
	if dec == nil {
		t.Fatal("no decision returned")
	}
	if !reasoningMentions(dec.Reasoning, "does not serve model gpt-4o-mini") {
		t.Fatalf("reasoning %v does not name the unservable-model reason", dec.Reasoning)
	}
}

// Regression guard: a pin whose provider DOES serve the model is still honoured.
func TestPinnedProviderServingModelIsHonoured(t *testing.T) {
	r := modelRouter(t)
	ctx := WithPinnedProvider(context.Background(), "anthropic")

	dec, prov, err := r.routeWithPin(ctx, &types.ChatRequest{Model: "claude-haiku-4-5"}, RoutingStrategySpecific)
	if err != nil {
		t.Fatal(err)
	}
	if dec.SelectedProvider != "anthropic" || prov == nil {
		t.Fatalf("a pin to a provider that serves the model must be honoured; got %q", dec.SelectedProvider)
	}
}

// Safety guard: a provider that advertises NO models is treated as "cannot
// tell" and the pin is honoured. An unpopulated model list must not silently
// drop working pins — the fix targets a NON-empty list that excludes the model.
func TestPinnedProviderEmptyModelListIsHonoured(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)
	r := NewRouter(logger)
	r.RegisterProvider("anthropic", &stubProvider{name: "anthropic"}) // empty models
	r.updateHealthStatus(context.Background())
	ctx := WithPinnedProvider(context.Background(), "anthropic")

	dec, prov, err := r.routeWithPin(ctx, &types.ChatRequest{Model: "anything"}, RoutingStrategySpecific)
	if err != nil {
		t.Fatal(err)
	}
	if dec.SelectedProvider != "anthropic" || prov == nil {
		t.Fatalf("an empty model list must not drop the pin; got %q", dec.SelectedProvider)
	}
}
