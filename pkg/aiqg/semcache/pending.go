package semcache

import "context"

// Pending carries the semantic-cache store intent from the lookup site (which
// has the request → prompt + scope) to the store site (which has the response),
// since those run in different functions sharing one request context. Set only
// on a cacheable miss.
type Pending struct {
	Scope  Scope
	Key    string
	Prompt string
}

type ctxKey int

const pendingCtxKey ctxKey = 0

// WithPending returns a context carrying the store intent.
func WithPending(ctx context.Context, p *Pending) context.Context {
	return context.WithValue(ctx, pendingCtxKey, p)
}

// PendingFromContext returns the store intent stamped by WithPending, if any.
func PendingFromContext(ctx context.Context) (*Pending, bool) {
	p, ok := ctx.Value(pendingCtxKey).(*Pending)
	return p, ok && p != nil
}
