// Package upstreamkey carries an optional per-request override for the vendor
// API key through context (Plan #14 BYOK). When set, providers use it for the
// outbound vendor call instead of their statically-configured key. Neutral +
// dependency-free so both the server (which sets it) and the providers (which
// read it) can import it without a cycle. The value is a plaintext secret held
// only for the request's lifetime — never logged or persisted.
package upstreamkey

import "context"

type ctxKey struct{}

// With returns ctx carrying key as the upstream override. Empty key = no-op
// (providers fall back to their configured key).
func With(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, key)
}

// From returns the upstream override key on ctx, or "" when none is set.
func From(ctx context.Context) string {
	s, _ := ctx.Value(ctxKey{}).(string)
	return s
}
