package routing

import (
	"context"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"

	"github.com/tributary-ai/llm-router-waf/internal/providers"
)

// Ordered failover — routing-decision.md §5.4, step 3.
//
// The chain is walked by the CALLER, not by Route(), because only the caller
// knows whether an attempt actually succeeded: Route hands back a provider and
// never sees the result. This file gives the caller the two things it needs —
// the next tier, and whether a given failure justifies advancing at all.
//
// # Why the chain is not just "retry somewhere else"
//
// A retry re-attempts the same target and is governed by budgets. A fallback
// moves to a DIFFERENT, explicitly named target, in an order the operator
// chose. Conflating them produces the failure mode this design exists to
// remove: a chain whose first tier is the target spends a whole request
// timeout re-attempting what just failed, before trying anything new.

// Tier is one step of a failover plan.
type Tier struct {
	Provider string
	Model    string
	// Position is 0 for the primary target and 1..n for chain entries, so an
	// event can say HOW FAR the chain walked rather than only that it did.
	Position int
	// Reason records why this tier was reached. Empty for the primary target.
	Reason string
}

// Chain carries a resolved failover plan for one request, together with the
// constraints it must respect.
type Chain struct {
	fallback    *resilience.Fallback
	constraints *resilience.Constraints
}

// NewChain builds a walker. Both arguments may be nil, yielding a chain that
// never advances — so callers need no nil checks.
func NewChain(f *resilience.Fallback, c *resilience.Constraints) *Chain {
	return &Chain{fallback: f, constraints: c}
}

// Configured reports whether any failover tier exists.
func (c *Chain) Configured() bool {
	return c != nil && c.fallback != nil && len(c.fallback.Chain) > 0
}

// Advances reports whether this failure justifies moving to the next tier.
// A nil or empty chain never advances.
func (c *Chain) Advances(class resilience.FailureClass) bool {
	if !c.Configured() {
		return false
	}
	return c.fallback.Advances(class)
}

// Tier returns the nth chain entry (1-based; position 0 is the primary
// target), resolved against the router's configured providers.
//
// A tier naming a provider the tenant is denied is SKIPPED, not returned.
// Constraints are validated at write time, but a constraint can be tightened
// after a rule was stored — and the moment a tenant declares a vendor
// forbidden, traffic must stop reaching it, including through a chain written
// before the declaration. Enforcing only at write time would leave every
// pre-existing rule as a standing exception.
//
// A tier naming a provider this gateway does not run is also skipped: it
// cannot be attempted, and failing the request because a later tier is
// unreachable would waste the tiers that are.
func (r *Router) Tier(c *Chain, n int) (providers.LLMProvider, Tier, bool) {
	if !c.Configured() || n < 1 || n > len(c.fallback.Chain) {
		return nil, Tier{}, false
	}
	for i := n; i <= len(c.fallback.Chain); i++ {
		e := c.fallback.Chain[i-1]
		if c.constraints != nil && c.constraints.Denies(e.Provider) {
			continue
		}
		prov, ok := r.providers[e.Provider]
		if !ok {
			continue
		}
		return prov, Tier{Provider: e.Provider, Model: e.Model, Position: i}, true
	}
	return nil, Tier{}, false
}

// AllowedTarget reports whether the tenant's constraints permit a provider at
// all. Applied to the PRIMARY target as well as chain tiers — policing the
// fallback path more strictly than the path taken by default would be absurd,
// and a constraint tightened after a rule was written must bind the rule.
func (c *Chain) AllowedTarget(provider string) bool {
	if c == nil || c.constraints == nil {
		return true
	}
	return !c.constraints.Denies(provider)
}

// chainKey carries the resolved chain on the request context, alongside the
// pin and the resilience overrides, for the same reason: routing metadata must
// never reach a vendor payload.
type chainKey struct{}

// WithChain returns a context carrying a resolved failover plan.
func WithChain(ctx context.Context, c *Chain) context.Context {
	if c == nil || (!c.Configured() && (c.constraints == nil || c.constraints.IsZero())) {
		return ctx
	}
	return context.WithValue(ctx, chainKey{}, c)
}

// ChainFrom returns the request's failover plan, or nil.
func ChainFrom(ctx context.Context) *Chain {
	v, _ := ctx.Value(chainKey{}).(*Chain)
	return v
}
