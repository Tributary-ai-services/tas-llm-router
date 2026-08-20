package routing

import "context"

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
