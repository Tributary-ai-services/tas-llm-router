package routing

import (
	"context"

	"github.com/tributary-ai/llm-router-waf/internal/providers"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/affinity"
)

// Provider affinity in the routing decision — routing-decision.md §5.5,
// cache-keys-and-sessions.md §4, step 4.
//
// Affinity is an ECONOMIC optimisation: it keeps a conversation on the provider
// whose vendor prompt cache is currently warm, because switching providers
// discards that cache and pays a full cold rebuild. That framing determines its
// precedence — it is the weakest input to a routing decision, and yields to
// every correctness concern above it.

// affinityKey carries the resolved affinity decision on the request context,
// alongside the pin, the resilience overrides and the chain. Same reason as
// those: routing metadata must never reach a vendor payload.
type affinityKey struct{}

// WithAffinity returns a context carrying an affinity decision.
func WithAffinity(ctx context.Context, d affinity.Decision) context.Context {
	return context.WithValue(ctx, affinityKey{}, d)
}

// AffinityFrom returns the request's affinity decision.
func AffinityFrom(ctx context.Context) affinity.Decision {
	d, _ := ctx.Value(affinityKey{}).(affinity.Decision)
	return d
}

// applyAffinity prefers the affine provider when one is held and usable.
//
// Runs AFTER the pin and the breaker, and never overrides either. The ordering
// is the whole design:
//
//	constraints  a warm cache on a denied vendor is a compliance breach
//	health       a warm cache on an ejected provider is worthless
//	pin          an operator naming a provider outranks an inferred preference
//	affinity     only then, and only as a preference
//
// Getting this backwards would let an economic optimisation quietly defeat a
// compliance control, which is why affinity is consulted last rather than
// folded into selection.
func (r *Router) applyAffinity(ctx context.Context, decision *RoutingDecision, provider providers.LLMProvider) (*RoutingDecision, providers.LLMProvider) {
	d := AffinityFrom(ctx)
	if !d.Held || decision == nil {
		return decision, provider
	}
	if decision.SelectedProvider == d.Target.Provider {
		// Already going there. Recorded anyway, so a reader can tell "affinity
		// held" from "affinity happened to agree with the strategy" — they
		// look identical on the event otherwise, and they mean different
		// things when hit rates fall.
		decision.Reasoning = append(decision.Reasoning,
			"affinity held on "+d.Target.Provider+" (strategy agreed)")
		return decision, provider
	}
	alt, ok := r.providers[d.Target.Provider]
	if !ok || !r.isProviderHealthy(ctx, d.Target.Provider) {
		decision.Reasoning = append(decision.Reasoning,
			"affinity to "+d.Target.Provider+" not honoured: unconfigured or unhealthy")
		return decision, provider
	}
	// A pin is an explicit operator instruction; affinity is inferred. The
	// explicit one wins.
	if PinnedProviderFrom(ctx) != "" {
		decision.Reasoning = append(decision.Reasoning,
			"affinity to "+d.Target.Provider+" not honoured: a route rule pins a provider")
		return decision, provider
	}
	return &RoutingDecision{
		SelectedProvider: d.Target.Provider,
		EstimatedCost:    decision.EstimatedCost,
		Reasoning: append(decision.Reasoning,
			"affinity: kept on "+d.Target.Provider+" to preserve a warm vendor prompt cache"),
	}, alt
}
