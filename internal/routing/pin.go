package routing

import (
	"context"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"
)

// Provider pinning — the mechanism by which a resolved routing decision steers
// execution, rather than merely describing policy.
//
// Carried on the context rather than on types.ChatRequest deliberately:
// ChatRequest is serialised toward vendors, and a routing-only field there
// would need a json:"-" tag to avoid leaking into a provider payload. Context
// cannot leak.

type pinnedProviderKey struct{}

// WithPinnedProvider returns a context carrying a provider the router should
// prefer over its configured strategy. Empty name is a no-op.
func WithPinnedProvider(ctx context.Context, provider string) context.Context {
	if provider == "" {
		return ctx
	}
	return context.WithValue(ctx, pinnedProviderKey{}, provider)
}

// PinnedProviderFrom returns the pinned provider, or "" when none is set.
func PinnedProviderFrom(ctx context.Context) string {
	v, _ := ctx.Value(pinnedProviderKey{}).(string)
	return v
}

// Resilience overrides ride the context alongside the provider pin, for the
// same reason: they are routing metadata that must never reach a vendor
// payload. They are resolved once per request by the policy middleware and read
// by the router and the completion handler, which are several context
// derivations apart.

type resilienceKey struct{}

type resilienceOverride struct {
	Health  *resilience.Health
	Budgets *resilience.Budgets
}

// WithResilience returns a context carrying per-request breaker overrides.
// Both nil is a no-op, so "no rule matched" costs nothing.
func WithResilience(ctx context.Context, h *resilience.Health, b *resilience.Budgets) context.Context {
	if h == nil && b == nil {
		return ctx
	}
	return context.WithValue(ctx, resilienceKey{}, resilienceOverride{Health: h, Budgets: b})
}

// ResilienceFrom returns the per-request overrides, or nils when none are set.
func ResilienceFrom(ctx context.Context) (*resilience.Health, *resilience.Budgets) {
	v, _ := ctx.Value(resilienceKey{}).(resilienceOverride)
	return v.Health, v.Budgets
}
