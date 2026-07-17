package responsecache

import (
	"context"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// Pending carries the store decision from the lookup site (before routing) to
// the store site (after the outbound scan), which run in different functions
// sharing one request context. Set only on a cacheable miss; its presence is
// the signal to store the produced response.
type Pending struct {
	TenantID string
	Hash     string
}

type ctxKey int

const pendingCtxKey ctxKey = 0

// WithPending returns a context carrying the store intent. The caller reassigns
// its *http.Request to the returned context so the downstream handler sees it.
func WithPending(ctx context.Context, tenantID, hash string) context.Context {
	return context.WithValue(ctx, pendingCtxKey, &Pending{TenantID: tenantID, Hash: hash})
}

// PendingFromContext returns the store intent stamped by WithPending, if any.
func PendingFromContext(ctx context.Context) (*Pending, bool) {
	p, ok := ctx.Value(pendingCtxKey).(*Pending)
	return p, ok && p != nil
}

// ResponseCacheable reports whether a produced response may be stored. A
// response that carries tool calls is side-effecting downstream and is never
// cached, even if the request itself passed Decide (defense in depth — a vendor
// can emit tool calls the request didn't ask for).
func ResponseCacheable(resp *types.ChatResponse) bool {
	if resp == nil || len(resp.Choices) == 0 {
		return false
	}
	for _, ch := range resp.Choices {
		if len(ch.Message.ToolCalls) > 0 {
			return false
		}
	}
	return true
}
