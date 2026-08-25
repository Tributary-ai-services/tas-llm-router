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

type controlsKey struct{}

// WithControls returns a context carrying the tenant's per-tenant feature
// switches. A nil block is a no-op — "the tenant expressed no preference"
// costs nothing and leaves ControlsFrom yielding the zero value.
func WithControls(ctx context.Context, c *resilience.Controls) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, controlsKey{}, *c)
}

// ControlsFrom returns the tenant's switches as a value, or the zero Controls
// (both prefs nil → inherit the gateway default) when none are set. Returning a
// value keeps the caller nil-safe: Controls{}.BreakerEnabledOr(def) yields def.
func ControlsFrom(ctx context.Context) resilience.Controls {
	v, _ := ctx.Value(controlsKey{}).(resilience.Controls)
	return v
}

type breakerTenantKey struct{}

// WithBreakerTenant carries the resolved tenant so the breaker can key its
// state per tenant when isolation is on. Empty is a no-op — a request with no
// tenant simply cannot isolate. The routing package cannot import middleware
// (import cycle), so the server threads the tenant in explicitly, like the
// controls above.
func WithBreakerTenant(ctx context.Context, tenant string) context.Context {
	if tenant == "" {
		return ctx
	}
	return context.WithValue(ctx, breakerTenantKey{}, tenant)
}

// BreakerTenantFrom returns the resolved tenant, or "" when none was set.
func BreakerTenantFrom(ctx context.Context) string {
	t, _ := ctx.Value(breakerTenantKey{}).(string)
	return t
}

type isolatedEjectedKey struct{}

// WithIsolatedEjected stashes the per-request ejection verdict for an
// isolation-enabled tenant, computed once at the top of Route so the
// per-candidate isProviderHealthy checks stay a map read rather than a store
// round-trip each. Keyed by provider name. A no-op for the shared path, which
// keeps using the fleet-wide background cache.
func WithIsolatedEjected(ctx context.Context, ejected map[string]bool) context.Context {
	return context.WithValue(ctx, isolatedEjectedKey{}, ejected)
}

// IsolatedEjectedFrom returns the per-request isolated ejection map, or nil.
func IsolatedEjectedFrom(ctx context.Context) map[string]bool {
	m, _ := ctx.Value(isolatedEjectedKey{}).(map[string]bool)
	return m
}
