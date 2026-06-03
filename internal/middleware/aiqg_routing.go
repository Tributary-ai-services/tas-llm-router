package middleware

import (
	"context"
	"sync"
)

// Routing carries the routing-layer decision (vendor, model, streaming
// flag) for an in-flight AIQG request. It's a context-scoped mutable
// sidecar — same pattern as instrumentation.TimingCollector — so the
// chat-completion handlers can stamp the decision as it becomes known
// (model from the inbound body, vendor from router.Route, streaming
// from the request body) without having to thread a new ctx through
// every call site.
//
// Concurrent-safe: in practice the stampers all run in the request
// goroutine, but the mutex is cheap and futureproofs against stream
// consumers that might stamp from a different goroutine.
//
// All stamps are first-write-wins (idempotent) so a later code path
// can't accidentally overwrite an earlier decision with a fallback
// guess.
type Routing struct {
	mu        sync.Mutex
	vendor    string
	model     string
	streaming bool
	streamSet bool
}

// NewRouting returns an empty Routing. Attach to ctx via WithRouting.
func NewRouting() *Routing {
	return &Routing{}
}

// RoutingSnapshot is the marshalable read-only view returned by
// Routing.Snapshot. Construction is the only thread-safe way to read
// the underlying fields.
type RoutingSnapshot struct {
	Vendor    string
	Model     string
	Streaming bool
	// StreamingSet distinguishes "explicitly stamped streaming=false"
	// from "never stamped" — the events package uses this to decide
	// whether to fall back to its URL-query heuristic.
	StreamingSet bool
}

// Snapshot returns a read-only view of the current routing state.
func (r *Routing) Snapshot() RoutingSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RoutingSnapshot{
		Vendor:       r.vendor,
		Model:        r.model,
		Streaming:    r.streaming,
		StreamingSet: r.streamSet,
	}
}

type routingCtxKey struct{}

var routingKey = routingCtxKey{}

// WithRouting attaches a Routing to ctx. The Path A middleware does
// this on AIQG-mode requests; non-AIQG requests carry no Routing and
// every Stamp* helper below no-ops.
func WithRouting(ctx context.Context, r *Routing) context.Context {
	return context.WithValue(ctx, routingKey, r)
}

// RoutingFromContext returns the Routing attached to ctx, or nil.
// Callers stamping fields should prefer the package-level Stamp*
// helpers below — they handle the nil case.
func RoutingFromContext(ctx context.Context) *Routing {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(routingKey).(*Routing)
	return r
}

// StampVendor records the chosen provider's name (e.g. "openai",
// "anthropic"). Called by the completion handlers after router.Route
// returns the selected provider. First write wins.
func StampVendor(ctx context.Context, vendor string) {
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.vendor == "" {
		r.vendor = vendor
	}
}

// StampModel records the vendor-side model identifier requested
// (e.g. "gpt-4o-mini", "claude-3-7-sonnet-20250219"). Called by the
// completion handlers after they decode the inbound JSON body. First
// write wins.
func StampModel(ctx context.Context, model string) {
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.model == "" {
		r.model = model
	}
}

// StampStreaming records whether the inbound request asked for a
// streamed response. Unlike Vendor/Model where "" means "not set",
// streaming has a meaningful false value, so we track set-ness
// separately via StreamingSet. First call wins.
func StampStreaming(ctx context.Context, streaming bool) {
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.streamSet {
		r.streaming = streaming
		r.streamSet = true
	}
}
