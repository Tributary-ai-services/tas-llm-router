package routing

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func chainRouter(t *testing.T) *Router {
	t.Helper()
	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)
	r := NewRouter(logger)
	r.RegisterProvider("openai", &stubProvider{name: "openai"})
	r.RegisterProvider("anthropic", &stubProvider{name: "anthropic"})
	r.updateHealthStatus(context.Background())
	return r
}

func fb(entries ...resilience.ChainEntry) *resilience.Fallback {
	return &resilience.Fallback{Chain: entries}
}

// ---------------------------------------------------------------------------
// Walking the chain.
// ---------------------------------------------------------------------------

func TestTierReturnsEntriesInOrder(t *testing.T) {
	r := chainRouter(t)
	c := NewChain(fb(
		resilience.ChainEntry{Provider: "anthropic", Model: "claude-haiku-4-5"},
		resilience.ChainEntry{Provider: "openai", Model: "gpt-4o-mini"},
	), nil)

	_, t1, ok := r.Tier(c, 1)
	if !ok || t1.Provider != "anthropic" || t1.Position != 1 {
		t.Fatalf("tier 1 = %+v ok=%v, want anthropic at position 1", t1, ok)
	}
	_, t2, ok := r.Tier(c, 2)
	if !ok || t2.Provider != "openai" || t2.Position != 2 {
		t.Fatalf("tier 2 = %+v ok=%v, want openai at position 2", t2, ok)
	}
	if _, _, ok := r.Tier(c, 3); ok {
		t.Fatal("a tier past the end of the chain was returned")
	}
}

// Constraints bind at RUN time, not only at write time: a chain written before
// a vendor was denied must stop reaching it, or every pre-existing rule becomes
// a standing exception.
func TestDeniedTierIsSkippedAtRuntime(t *testing.T) {
	r := chainRouter(t)
	c := NewChain(
		fb(
			resilience.ChainEntry{Provider: "anthropic", Model: "claude-haiku-4-5"},
			resilience.ChainEntry{Provider: "openai", Model: "gpt-4o-mini"},
		),
		&resilience.Constraints{DenyVendors: []string{"anthropic"}},
	)
	_, tier, ok := r.Tier(c, 1)
	if !ok {
		t.Fatal("the whole chain was abandoned because tier 1 is denied")
	}
	if tier.Provider != "openai" {
		t.Fatalf("tier = %s, want the denied tier skipped in favour of openai", tier.Provider)
	}
}

// A tier this gateway does not run cannot be attempted; failing the request
// because a later tier is unreachable would waste the tiers that are.
func TestUnconfiguredTierIsSkipped(t *testing.T) {
	r := chainRouter(t)
	c := NewChain(fb(
		resilience.ChainEntry{Provider: "cohere", Model: "command"},
		resilience.ChainEntry{Provider: "openai", Model: "gpt-4o-mini"},
	), nil)
	_, tier, ok := r.Tier(c, 1)
	if !ok || tier.Provider != "openai" {
		t.Fatalf("tier = %+v ok=%v, want the unconfigured provider skipped", tier, ok)
	}
}

func TestNilChainNeverAdvances(t *testing.T) {
	var c *Chain
	if c.Configured() {
		t.Fatal("nil chain reported Configured")
	}
	if c.Advances(resilience.FailureVendorError) {
		t.Fatal("nil chain advanced")
	}
	if !c.AllowedTarget("openai") {
		t.Fatal("nil chain denied a target; with no constraints everything is allowed")
	}
}

func TestChainRespectsConfiguredFailureClasses(t *testing.T) {
	c := NewChain(&resilience.Fallback{
		Chain: []resilience.ChainEntry{{Provider: "openai", Model: "gpt-4o-mini"}},
		On:    []resilience.FailureClass{resilience.FailureTimeout},
	}, nil)
	if !c.Advances(resilience.FailureTimeout) {
		t.Fatal("configured class did not advance")
	}
	if c.Advances(resilience.FailureVendorError) {
		t.Fatal("explicit `on` must replace the default set, not extend it")
	}
}

// ---------------------------------------------------------------------------
// Context carrier.
// ---------------------------------------------------------------------------

func TestChainSurvivesContextDerivation(t *testing.T) {
	c := NewChain(fb(resilience.ChainEntry{Provider: "openai", Model: "gpt-4o-mini"}), nil)
	ctx := WithChain(context.Background(), c)
	ctx = context.WithValue(ctx, struct{ k string }{"unrelated"}, 1)
	if got := ChainFrom(ctx); got == nil || !got.Configured() {
		t.Fatal("chain did not survive context derivation")
	}
	// An empty plan must store nothing, so "no chain" and "empty chain" are
	// the same code path.
	if ChainFrom(WithChain(context.Background(), NewChain(nil, nil))) != nil {
		t.Fatal("an empty plan should store nothing on the context")
	}
}

// Constraints alone are worth carrying: they bound routing whether or not a
// rule configured failover.
func TestConstraintsAloneAreCarried(t *testing.T) {
	c := NewChain(nil, &resilience.Constraints{DenyVendors: []string{"openai"}})
	got := ChainFrom(WithChain(context.Background(), c))
	if got == nil {
		t.Fatal("constraints without a chain were dropped")
	}
	if got.AllowedTarget("openai") {
		t.Fatal("carried constraints are not being applied")
	}
}

// ---------------------------------------------------------------------------
// Open question 2: the pin is now a pin, with the chain as its only escape.
// ---------------------------------------------------------------------------

func TestUnusablePinEntersTheChain(t *testing.T) {
	r := chainRouter(t)
	c := NewChain(fb(resilience.ChainEntry{Provider: "anthropic", Model: "claude-haiku-4-5"}), nil)
	ctx := WithChain(WithPinnedProvider(context.Background(), "cohere"), c)

	dec, prov, err := r.routeWithPin(ctx, &types.ChatRequest{Model: "x"}, RoutingStrategySpecific)
	if err != nil {
		t.Fatal(err)
	}
	if dec.SelectedProvider != "anthropic" {
		t.Fatalf("selected %q; an unusable pin must enter the chain, not the strategy", dec.SelectedProvider)
	}
	if prov == nil {
		t.Fatal("nil provider returned")
	}
	if !reasoningMentions(dec.Reasoning, "entered fallback chain") {
		t.Fatalf("reasoning %v does not record that the pin was escaped via the chain", dec.Reasoning)
	}
}

// A pin to a denied vendor must never be honoured, even though it is
// configured and healthy.
func TestDeniedPinIsNotHonoured(t *testing.T) {
	r := chainRouter(t)
	c := NewChain(
		fb(resilience.ChainEntry{Provider: "anthropic", Model: "claude-haiku-4-5"}),
		&resilience.Constraints{DenyVendors: []string{"openai"}},
	)
	ctx := WithChain(WithPinnedProvider(context.Background(), "openai"), c)

	dec, _, err := r.routeWithPin(ctx, &types.ChatRequest{Model: "x"}, RoutingStrategySpecific)
	if err != nil {
		t.Fatal(err)
	}
	if dec.SelectedProvider == "openai" {
		t.Fatal("a pin to a denied vendor was honoured")
	}
	if !reasoningMentions(dec.Reasoning, "denied by tenant constraints") {
		t.Fatalf("reasoning %v does not name the constraint", dec.Reasoning)
	}
}

// With no chain the request still falls through — deliberately, so a provider
// blip does not become a tenant outage for rules that have not adopted chains.
func TestUnusablePinWithoutAChainStillFallsThrough(t *testing.T) {
	r := chainRouter(t)
	ctx := WithPinnedProvider(context.Background(), "cohere")
	dec, _, err := r.routeWithPin(ctx, &types.ChatRequest{Model: "gpt-4o-mini"}, RoutingStrategyCostOptimized)
	if err != nil {
		t.Fatal(err)
	}
	if dec == nil {
		t.Fatal("no decision returned")
	}
	if !reasoningMentions(dec.Reasoning, "no usable fallback chain") {
		t.Fatalf("reasoning %v should record the fall-through explicitly", dec.Reasoning)
	}
}

// Unlike an ejection — where proceeding beats refusing, since the target may
// have recovered — a constraint says this vendor must NEVER be used. With
// nothing permitted, the request fails rather than committing the breach.
func TestNoPermittedProviderIsAnError(t *testing.T) {
	r := chainRouter(t)
	c := NewChain(nil, &resilience.Constraints{DenyVendors: []string{"openai", "anthropic"}})
	ctx := WithChain(context.Background(), c)

	_, _, err := r.routeWithPin(ctx, &types.ChatRequest{Model: "gpt-4o-mini"}, RoutingStrategyCostOptimized)
	if err == nil {
		t.Fatal("routing succeeded with every provider denied; a constraint breach must fail instead")
	}
}
