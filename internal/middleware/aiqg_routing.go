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
	mu                  sync.Mutex
	vendor              string
	model               string
	streaming           bool
	streamSet           bool
	promptTokens        int
	completionTokens    int
	cacheCreationTokens int
	cacheReadTokens     int
	usageSet            bool

	// Gatekeeper findings — counts per severity per direction. The
	// scanRan flag distinguishes "scan ran, no findings" (Healthy
	// Assurance) from "scan didn't run" (no Assurance score at all).
	inboundFindings  map[string]int
	outboundFindings map[string]int
	scanRan          bool

	// Vendor finish_reason — "stop" / "length" / "content_filter" /
	// "tool_calls" / "function_call". Empty string means unset (no
	// vendor response). First-write-wins like the other stampers.
	finishReason string

	// Routing-layer retry metadata for the MVP Reliability proxy.
	// attemptCount=0 means unset; 1 = clean first try.
	attemptCount int
	fallbackUsed bool
	retrySet     bool

	// Auto-classified workflow string (rag / code_generation / etc.)
	// from internal/workflow.Classify. Empty when classification was
	// skipped or returned no match. The events package uses this as a
	// fallback when the TAS-Workflow header was NOT supplied —
	// header value always wins to preserve customer intent.
	workflow string

	// NIST AI RMF characteristic → finding count for this request.
	// Driven by MapPatternToNIST in nist_classifier.go. Keyed by the
	// NIST<Characteristic> constants. Direction-agnostic — Section 5
	// of the Day-1 Report shows the aggregate, not in/out split.
	nistFindings map[string]int

	// Gatekeeper pattern_id → count for this request. Powers the
	// /api/v1/metrics/tags endpoint (which shows "what matchers are
	// firing most often for this tenant"). Direction-agnostic.
	tagFindings map[string]int

	// linked tier (docs/AIQG-AGENT-FLOW-ATTRIBUTION.md §A): tool_call_ids
	// this request echoed back in role=tool messages (the resolve key) and
	// the ids our served response minted (the index key). The middleware
	// resolves echoed→(flow,step) and indexes served→(flow,step) via
	// pkg/aiqg/linkage on response completion.
	echoedToolCallIDs []string
	servedToolCallIDs []string

	// prefix-chaining tier (§A.2): canonical hash of the conversation state
	// this request CONTINUES (its message prefix up to the last assistant
	// turn = the resolve key) and the state serving this response LEAVES
	// (request messages + our reply = the index key). Empty when not
	// applicable (fresh thread / tool-call-only response).
	prefixHash string
	stateHash  string

	// fingerprinted tier (§B): the request's structural signature (sorted
	// tool names + param schemas + config tuple). Empty when the request
	// carries nothing fingerprintable. Combined with (tenant, principal) by
	// the events builder to mint the AgentSurrogateID.
	fingerprint string

	// redaction (G1): number of PII findings redacted from the inbound messages
	// before the vendor call. 0 when redaction is off or found nothing. Surfaced
	// on the event as redaction_applied/redacted_count so a quality regression on
	// a redacting route is attributable.
	redactionCount int

	// prompt-cache probe (docs/AIQG-PROMPT-CACHE-CONTROL.md §9, P0): canonical
	// hash of the tools+system span a vendor cache breakpoint would cover.
	// Measurement only — the middleware probes it against a TTL'd index to
	// report whether this prefix recurred inside one cache lifetime. Empty when
	// the request has nothing cacheable (no system text, no tools).
	cachePrefixHash string

	// experiments (Phase D): the experiment + variant that claimed this
	// request. Stamped at request time (so the routing override + the event
	// stamp use one decision). Empty when no experiment claimed it.
	experimentID      string
	experimentVariant string
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
	PromptTokens        int
	CompletionTokens    int
	CacheCreationTokens int
	CacheReadTokens     int
	UsageSet            bool

	// Gatekeeper scan results, severity-count maps per direction.
	// ScanRan distinguishes "scan ran, no findings" from "scan never
	// ran" — the latter means Assurance scoring is skipped entirely.
	InboundFindings  map[string]int
	OutboundFindings map[string]int
	ScanRan          bool

	// Vendor finish_reason captured from the response. Empty when
	// the vendor never returned one (gateway-blocked, streaming
	// truncation).
	FinishReason string

	// Routing-layer retry signals (from types.RouterMetadata).
	// RetrySet distinguishes "router didn't surface metadata" (no
	// score) from "1 attempt, no fallback" (Healthy score).
	AttemptCount int
	FallbackUsed bool
	RetrySet     bool

	// Auto-classified workflow from internal/workflow.Classify.
	// Empty when not classified; events.Build uses this as a
	// fallback when the TAS-Workflow header wasn't sent.
	Workflow string

	// NIST AI RMF characteristic → finding count for this request.
	// Direction-agnostic aggregate; Day-1 Report Section 5 renders
	// these per-characteristic. Nil when no findings.
	NISTFindings map[string]int

	// Gatekeeper pattern_id → count for this request. Nil when no
	// findings. Drives /api/v1/metrics/tags.
	TagFindings map[string]int

	// linked tier: tool_call_ids this request echoed (role=tool) and the
	// ids our served response minted. Nil when none.
	EchoedToolCallIDs []string
	ServedToolCallIDs []string

	// prefix-chaining tier (§A.2): resolve key (prefix of the incoming
	// request) and index key (state left by serving this response). Empty
	// when not applicable.
	PrefixHash string
	StateHash  string

	// fingerprinted tier (§B): the request's structural signature. Empty
	// when nothing fingerprintable.
	Fingerprint string

	// redaction (G1): count of PII findings redacted inbound. 0 = none.
	RedactionCount int

	// prompt-cache probe (P0): hash of the tools+system span a vendor cache
	// breakpoint would cover. Empty when nothing is cacheable.
	CachePrefixHash string

	// experiments (Phase D): claiming experiment + variant. Empty when none.
	ExperimentID      string
	ExperimentVariant string
}

// Snapshot returns a read-only view of the current routing state.
// Finding maps are copied so the caller can mutate without racing
// future stamps.
func (r *Routing) Snapshot() RoutingSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RoutingSnapshot{
		Vendor:              r.vendor,
		Model:               r.model,
		Streaming:           r.streaming,
		StreamingSet:        r.streamSet,
		PromptTokens:        r.promptTokens,
		CompletionTokens:    r.completionTokens,
		CacheCreationTokens: r.cacheCreationTokens,
		CacheReadTokens:     r.cacheReadTokens,
		UsageSet:            r.usageSet,
		InboundFindings:     copyCounts(r.inboundFindings),
		OutboundFindings:    copyCounts(r.outboundFindings),
		ScanRan:             r.scanRan,
		FinishReason:        r.finishReason,
		AttemptCount:        r.attemptCount,
		FallbackUsed:        r.fallbackUsed,
		RetrySet:            r.retrySet,
		Workflow:            r.workflow,
		NISTFindings:        copyCounts(r.nistFindings),
		TagFindings:         copyCounts(r.tagFindings),
		EchoedToolCallIDs:   append([]string(nil), r.echoedToolCallIDs...),
		ServedToolCallIDs:   append([]string(nil), r.servedToolCallIDs...),
		PrefixHash:          r.prefixHash,
		StateHash:           r.stateHash,
		Fingerprint:         r.fingerprint,
		RedactionCount:      r.redactionCount,
		CachePrefixHash:     r.cachePrefixHash,
		ExperimentID:        r.experimentID,
		ExperimentVariant:   r.experimentVariant,
	}
}

// StampEchoedToolCalls records the tool_call_ids this request echoed back in
// role=tool messages — the linked-tier resolve key. Append semantics (a
// request can carry several tool results).
func StampEchoedToolCalls(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.echoedToolCallIDs = append(r.echoedToolCallIDs, ids...)
}

// StampServedToolCalls records the tool_call_ids our response minted — the
// linked-tier index key (a future request echoing them links to this step).
func StampServedToolCalls(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.servedToolCallIDs = append(r.servedToolCallIDs, ids...)
}

// StampPrefixHash records the canonical hash of the conversation state this
// request continues (its message prefix up to the last assistant turn) — the
// prefix-chaining resolve key. First-write-wins; empty is a no-op.
func StampPrefixHash(ctx context.Context, hash string) {
	if hash == "" {
		return
	}
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prefixHash == "" {
		r.prefixHash = hash
	}
}

// StampCachePrefixHash records the canonical hash of the tools+system span a
// vendor prompt-cache breakpoint would cover (docs/AIQG-PROMPT-CACHE-CONTROL.md
// §9, P0). Measurement only — nothing derived from it reaches the vendor.
// First-write-wins; empty is a no-op (nothing cacheable in this request).
func StampCachePrefixHash(ctx context.Context, hash string) {
	if hash == "" {
		return
	}
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cachePrefixHash == "" {
		r.cachePrefixHash = hash
	}
}

// StampStateHash records the canonical hash of the conversation state serving
// this response leaves behind (request messages + our reply) — the
// prefix-chaining index key. First-write-wins; empty is a no-op.
func StampStateHash(ctx context.Context, hash string) {
	if hash == "" {
		return
	}
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stateHash == "" {
		r.stateHash = hash
	}
}

// StampExperiment records the experiment + variant that claimed this request
// (Phase D). First-write-wins; empty experiment id is a no-op.
func StampExperiment(ctx context.Context, experimentID, variant string) {
	if experimentID == "" {
		return
	}
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.experimentID == "" {
		r.experimentID = experimentID
		r.experimentVariant = variant
	}
}

// StampFingerprint records the request's structural signature for the
// fingerprinted tier (§B). First-write-wins; empty is a no-op.
func StampFingerprint(ctx context.Context, fp string) {
	if fp == "" {
		return
	}
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fingerprint == "" {
		r.fingerprint = fp
	}
}

// StampRedaction records the number of PII findings redacted from the inbound
// messages (G1). First-write-wins; count<=0 is a no-op.
func StampRedaction(ctx context.Context, count int) {
	if count <= 0 {
		return
	}
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.redactionCount == 0 {
		r.redactionCount = count
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

// StampWorkflow records the auto-classified workflow string (rag,
// code_generation, etc.) for the request. Always loses to the
// TAS-Workflow header — the events.Build path prefers the header
// value when set. First write wins. Empty string is a no-op so the
// classifier can call this unconditionally without overwriting a
// later, better-informed classification.
func StampWorkflow(ctx context.Context, workflow string) {
	if workflow == "" {
		return
	}
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.workflow == "" {
		r.workflow = workflow
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
func StampTokenUsage(ctx context.Context, promptTokens, completionTokens, cacheCreationTokens, cacheReadTokens int) {
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.usageSet {
		r.promptTokens = promptTokens
		r.completionTokens = completionTokens
		r.cacheCreationTokens = cacheCreationTokens
		r.cacheReadTokens = cacheReadTokens
		r.usageSet = true
	}
}

// StampFinishReason records the vendor-reported finish_reason on the
// routing sidecar. First-write-wins: the streaming path may stamp
// from each chunk's choice.FinishReason (only the last chunk
// typically populates it), but if a later fallback path tries to
// stamp again the first authoritative value wins.
func StampFinishReason(ctx context.Context, reason string) {
	if reason == "" {
		return
	}
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finishReason == "" {
		r.finishReason = reason
	}
}

// StampRetryMetadata records the routing layer's per-request retry
// outcome — attempt count and whether a cross-provider fallback fired.
// Drives the MVP Reliability score. First-write-wins; subsequent calls
// are ignored so the authoritative end-of-request value sticks.
//
// Calling with attemptCount=0 is treated as a no-op (the routing layer
// didn't surface metadata for this request) so RetrySet stays false
// and Reliability stays nil. Pass attemptCount>=1 once the router
// returns metadata, even if no retries fired (1 = clean first try,
// which scores 100).
func StampRetryMetadata(ctx context.Context, attemptCount int, fallbackUsed bool) {
	if attemptCount <= 0 {
		return
	}
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.retrySet {
		r.attemptCount = attemptCount
		r.fallbackUsed = fallbackUsed
		r.retrySet = true
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

// StampNISTFindings accumulates per-NIST-characteristic finding
// counts onto the routing sidecar. Direction-agnostic — Section 5
// of the Day-1 Report shows the request-wide aggregate. Counts are
// SUMMED across calls (vs StampGatekeeperFindings which takes the
// max) because inbound+outbound findings of the same characteristic
// are distinct events, not the same event reported twice.
func StampNISTFindings(ctx context.Context, nistCounts map[string]int) {
	if len(nistCounts) == 0 {
		return
	}
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nistFindings == nil {
		r.nistFindings = make(map[string]int, len(nistCounts))
	}
	for k, v := range nistCounts {
		r.nistFindings[k] += v
	}
}

// StampTagFindings accumulates per-pattern_id finding counts.
// Same SUM semantic as NIST — inbound+outbound counts of the same
// matcher are distinct findings, not the same one reported twice.
func StampTagFindings(ctx context.Context, tagCounts map[string]int) {
	if len(tagCounts) == 0 {
		return
	}
	r := RoutingFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tagFindings == nil {
		r.tagFindings = make(map[string]int, len(tagCounts))
	}
	for k, v := range tagCounts {
		r.tagFindings[k] += v
	}
}
