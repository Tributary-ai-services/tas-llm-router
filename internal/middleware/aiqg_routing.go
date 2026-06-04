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
	mu               sync.Mutex
	vendor           string
	model            string
	streaming        bool
	streamSet        bool
	promptTokens     int
	completionTokens int
	usageSet         bool

	// Gatekeeper findings — counts per severity per direction. The
	// scanRan flag distinguishes "scan ran, no findings" (Healthy
	// Assurance) from "scan didn't run" (no Assurance score at all).
	inboundFindings  map[string]int
	outboundFindings map[string]int
	scanRan          bool
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

	// Token usage from the vendor response. UsageSet distinguishes
	// "vendor returned 0 tokens" (which is legitimate for some
	// finish_reason=content_filter responses) from "vendor never
	// returned a usage block" (the events package emits the former
	// as `prompt_tokens: 0`, the latter omits the field).
	PromptTokens     int
	CompletionTokens int
	UsageSet         bool

	// Gatekeeper scan results, severity-count maps per direction.
	// ScanRan distinguishes "scan ran, no findings" from "scan never
	// ran" — the latter means Assurance scoring is skipped entirely.
	InboundFindings  map[string]int
	OutboundFindings map[string]int
	ScanRan          bool
}

// Snapshot returns a read-only view of the current routing state.
// Finding maps are copied so the caller can mutate without racing
// future stamps.
func (r *Routing) Snapshot() RoutingSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RoutingSnapshot{
		Vendor:           r.vendor,
		Model:            r.model,
		Streaming:        r.streaming,
		StreamingSet:     r.streamSet,
		PromptTokens:     r.promptTokens,
		CompletionTokens: r.completionTokens,
		UsageSet:         r.usageSet,
		InboundFindings:  copyCounts(r.inboundFindings),
		OutboundFindings: copyCounts(r.outboundFindings),
		ScanRan:          r.scanRan,
	}
}

func copyCounts(in map[string]int) map[string]int {
	if in == nil {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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

// StampTokenUsage records the vendor-reported token counts. Same
// first-write-wins semantic as the other stampers — for streaming
// responses, the final chunk's usage is the authoritative value,
// and subsequent fallback paths must not overwrite it. Calling with
// both counts zero is still treated as a valid stamp (UsageSet flips
// true) so the event distinguishes "vendor returned 0 tokens" from
// "vendor never returned a usage block".
func StampTokenUsage(ctx context.Context, promptTokens, completionTokens int) {
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.usageSet {
		r.promptTokens = promptTokens
		r.completionTokens = completionTokens
		r.usageSet = true
	}
}

// Direction selects which side of the Gatekeeper scan a finding came
// from. Both directions land on the same Routing sidecar and feed the
// per-request Assurance score; dashboards can separate them via the
// embedded AssuranceSummary.
const (
	GatekeeperDirectionInbound  = "inbound"
	GatekeeperDirectionOutbound = "outbound"
)

// StampGatekeeperFindings records the per-severity count of findings
// surfaced by a Gatekeeper scan. Direction selects inbound or outbound;
// passing an empty severityCounts map is treated as "scan ran, no
// findings" — ScanRan flips true so Assurance scores as Healthy=100
// rather than nil-not-scored. Subsequent calls for the same direction
// accumulate (max-per-severity) so multiple scan stages in the same
// direction don't accidentally clobber each other.
func StampGatekeeperFindings(ctx context.Context, direction string, severityCounts map[string]int) {
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scanRan = true
	target := &r.inboundFindings
	if direction == GatekeeperDirectionOutbound {
		target = &r.outboundFindings
	}
	if *target == nil {
		*target = make(map[string]int, len(severityCounts))
	}
	for sev, count := range severityCounts {
		if count > (*target)[sev] {
			(*target)[sev] = count
		}
	}
}
