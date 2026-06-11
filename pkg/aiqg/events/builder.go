package events

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"time"

	"github.com/tributary-ai/llm-router-waf/internal/instrumentation"
	"github.com/tributary-ai/llm-router-waf/pkg/clear"
)

// AIQGHeadersView is the subset of parsed TAS-* headers the event
// builder needs. Defined here as a tiny interface-shaped struct so the
// builder doesn't import internal/middleware (which would create a
// cycle: middleware → events → middleware). Callers in middleware
// construct one from their AIQGHeaders before invoking Build.
type AIQGHeadersView struct {
	SourceApp    string
	Workflow     string
	Policy       []string
	PolicyBundle string
	DryRun       bool
	Trace        bool

	// Identity (additive, self-asserted). The middleware projects these
	// from its parsed AIQGHeaders (TAS-Agent-* / traceparent) and the two
	// baggage keys it cares about. Build assembles them — with the token
	// principal — into the event AgentContext.
	AgentID          string
	AgentName        string
	AgentVersion     string
	FlowID           string
	ConversationID   string
	TraceID          string
	BaggageUserID    string
	BaggageSessionID string
}

// RoutingView is the subset of routing-layer state the event builder
// needs. Same anti-cycle motivation as AIQGHeadersView — the middleware
// projects its mutable middleware.Routing into this read-only struct
// before invoking Build.
//
// Fields default to zero-value when unset; the builder then falls back
// to its URL/method heuristics (e.g. Streaming reads the ?stream=true
// query param when StreamingSet is false).
type RoutingView struct {
	Vendor       string
	Model        string
	Streaming    bool
	StreamingSet bool

	// Token usage from the vendor response (stamped by handlers via
	// middleware.StampTokenUsage). UsageSet distinguishes "vendor
	// returned 0 tokens" (a legitimate value for content_filter
	// finish-reason) from "vendor never returned a usage block"
	// (events omit TokenAccounting entirely in the latter case).
	PromptTokens     int
	CompletionTokens int
	UsageSet         bool

	// Gatekeeper scan results. Inbound/Outbound are per-severity
	// counts ("low"/"medium"/"high"/"critical" → int). ScanRan
	// distinguishes "scan ran, no findings" from "scan never ran"
	// — events emit AssuranceSummary only when ScanRan is true.
	InboundFindings  map[string]int
	OutboundFindings map[string]int
	ScanRan          bool

	// Vendor finish_reason captured from the response. Used both
	// to populate ResponseEvent.FinishReason (taking precedence
	// over the BuildOptions value) and to drive clear.Efficacy.
	FinishReason string

	// Routing-layer retry signals from types.RouterMetadata. Drive
	// the MVP Reliability score (a gateway-fulfillment proxy for
	// the spec's pass@k). RetrySet=false means the routing layer
	// didn't surface metadata and Reliability stays nil.
	AttemptCount int
	FallbackUsed bool
	RetrySet     bool

	// Auto-classified workflow from internal/workflow.Classify.
	// Used as a fallback when the TAS-Workflow header isn't set —
	// the header always wins because it represents the customer's
	// explicit declaration of intent.
	Workflow string

	// NIST AI RMF characteristic → count for this request. Stamped
	// by the middleware after each scan call. Surfaces on the
	// AssuranceSummary so the Day-1 Report Trustworthiness section
	// can render per-characteristic findings.
	NISTFindings map[string]int

	// Gatekeeper pattern_id → count for this request. Drives the
	// /api/v1/metrics/tags endpoint. Nil when no findings.
	TagFindings map[string]int
}

// TokenView is the subset of resolved-token state the event builder
// needs. The Path A middleware constructs it from pkg/aiqg/tokens.Token
// after a successful Resolve. Pre-validation auth failures never
// reach Build (the middleware short-circuits with 401/403 before
// emitting), so TokenView is either fully populated or fully empty —
// never partial.
type TokenView struct {
	TenantID       string
	AIQGAccountID  string
	TASAuthTokenID string
	SourceApp      string // token-claim source_app; header overrides per spec §80
}

// GatewayVersion is the build version stamped on every event. Override
// at build time via -ldflags "-X .../events.GatewayVersion=v1.4.2".
var GatewayVersion = "dev"

// ScoringVersion is the pkg/clear version stamped on every event.
// Re-exported from pkg/clear so call sites don't need to import both
// packages just to read the version string. When pkg/clear bumps Version,
// every emitted event reflects the new value automatically.
var ScoringVersion = clear.Version

// BuildOptions captures inputs the middleware passes through that
// aren't on the request context (HTTP status, response status string,
// region). Fields default to sensible MVP values when zero.
type BuildOptions struct {
	HTTPStatus   int    // 0 if no response was written (gateway blocked)
	Status       string // explicit status; if empty, derived from HTTPStatus
	Region       string // deployment region; if empty, omitted from event
	FinishReason string

	// ResolvedPolicyBundle is the bundle aiqg-dashboard-be picked for
	// this request (Phase 4.0). Always set in production — the
	// middleware degrades to a Default() resolution when the resolver
	// is unavailable so "every event has a resolved_policy_bundle"
	// holds. Nil pointer → field omitted from the response event
	// (covers pre-Phase-4.0 callers + non-AIQG middleware).
	ResolvedPolicyBundle *ResolvedPolicyBundle
}

// Build constructs the paired (RequestEnvelope, ResponseEnvelope) from
// request, parsed headers, timing snapshot, and outcome. Returns the
// correlated IDs already cross-linked (request.CorrelatedResponseEventID,
// response.RequestEventID).
//
// Pure function: no context access, no I/O, no clock side effects beyond
// time.Now() as the fallback when the snapshot has no stamps. Callers
// (Path A middleware) extract the snapshot + headers from ctx and pass
// them in — this keeps the builder importable without cycling back into
// internal/middleware.
//
// Fields the gateway can't populate at MVP today (tenant_id, CLEAR
// scores beyond Latency) are left empty — downstream slices fill them
// either by enriching the returned envelopes before Emit, or by adding
// new args to Build.
//
// routing carries the vendor/model decision and the authoritative
// streaming flag. When routing.StreamingSet is false, the builder
// falls back to the URL-query heuristic — handy for tests but the
// completion handlers in production always stamp it explicitly.
//
// token carries the resolved tenant/account identifiers. Pre-validation
// auth failures don't reach Build (per spec §273), so token is either
// fully populated from a successful resolve or fully empty (resolver
// not configured / non-AIQG path).
// buildAgentContext resolves the self-asserted identity ladder for a
// request (docs/AIQG-AGENT-FLOW-ATTRIBUTION.md): baggage user.id is the
// cross-app user key; TAS-Agent-* / traceparent supply agent/flow; the
// AIQG token supplies the always-present principal tier. identity_source
// records the strongest tier that produced an identity. Returns nil only
// when nothing at all resolved (no token, no headers).
func buildAgentContext(h AIQGHeadersView, t TokenView) *AgentContext {
	ac := &AgentContext{
		AgentID:      h.AgentID,
		AgentName:    h.AgentName,
		AgentVersion: h.AgentVersion,
		UserID:       h.BaggageUserID,
	}
	// conversation: explicit header wins, else baggage session.id
	ac.ConversationID = h.ConversationID
	if ac.ConversationID == "" {
		ac.ConversationID = h.BaggageSessionID
	}
	// flow: explicit header wins, else traceparent trace-id
	ac.FlowID = h.FlowID
	if ac.FlowID == "" {
		ac.FlowID = h.TraceID
	}
	// principal: token source_app, else token id (always present in AIQG mode)
	ac.PrincipalID = t.SourceApp
	if ac.PrincipalID == "" {
		ac.PrincipalID = t.TASAuthTokenID
	}

	switch {
	case ac.UserID != "":
		ac.IdentitySource = "baggage"
	case ac.AgentID != "" || h.FlowID != "" || h.ConversationID != "":
		ac.IdentitySource = "asserted"
	case h.TraceID != "":
		ac.IdentitySource = "trace"
	case ac.PrincipalID != "":
		ac.IdentitySource = "principal"
	default:
		ac.IdentitySource = "unattributed"
	}

	if ac.AgentID == "" && ac.AgentName == "" && ac.UserID == "" &&
		ac.ConversationID == "" && ac.FlowID == "" && ac.PrincipalID == "" {
		return nil
	}
	return ac
}

func Build(r *http.Request, headers AIQGHeadersView, routing RoutingView, token TokenView, snap instrumentation.Snapshot, opts BuildOptions) (RequestEnvelope, ResponseEnvelope) {
	reqID := newUUID()
	respID := newUUID()
	now := time.Now().UTC()

	receivedAt := now
	completeAt := now
	if snap.RequestReceivedAt != nil {
		receivedAt = snap.RequestReceivedAt.UTC()
	}
	if snap.ResponseCompleteAt != nil {
		completeAt = snap.ResponseCompleteAt.UTC()
	}

	status := opts.Status
	if status == "" {
		status = StatusFromHTTP(opts.HTTPStatus)
	}

	streaming := isStreaming(r)
	if routing.StreamingSet {
		streaming = routing.Streaming
	}

	// SourceApp precedence per request-event.md §80: customer-supplied
	// TAS-Source-App header wins; falls back to the token's source_app
	// claim. Empty if neither side provides it.
	sourceApp := headers.SourceApp
	if sourceApp == "" {
		sourceApp = token.SourceApp
	}

	// Self-asserted identity attribution (shared by both events).
	agentCtx := buildAgentContext(headers, token)

	reqEvent := RequestEvent{
		RequestEventID:  reqID,
		TenantID:        token.TenantID,
		AIQGAccountID:   token.AIQGAccountID,
		TASAuthTokenID:  token.TASAuthTokenID,
		ReceivedAt:      receivedAt,
		Endpoint:        r.URL.Path,
		Method:          r.Method,
		SourceIP:        clientIP(r),
		SourceApp:       sourceApp,
		ClientRequestID: r.Header.Get("X-Request-ID"),
		Region:          opts.Region,
		Vendor:          routing.Vendor,
		Model:           routing.Model,
		Streaming:       streaming,
		IsAIQGMode:      true,
		DryRun:          headers.DryRun,
		TraceReturned:   headers.Trace,
		Workflow:        preferredWorkflow(headers.Workflow, routing.Workflow),
		PolicyNames:     headers.Policy,
		PolicyBundle:    headers.PolicyBundle,
		// SourceApp override from header was already captured above; the
		// AIQGHeadersView populates it for the field assignment below.
		CorrelatedResponseEventID: respID,
		ScoringVersion:            ScoringVersion,
		GatewayVersion:            GatewayVersion,
		LifecycleState:            LifecyclePairedWithResponse,
		AgentContext:              agentCtx,
	}

	// Routing's FinishReason wins over BuildOptions.FinishReason when
	// both are set — the routing sidecar is the authoritative path
	// (handler-stamped); BuildOptions.FinishReason exists for tests
	// and synthetic events.
	finishReason := opts.FinishReason
	if routing.FinishReason != "" {
		finishReason = routing.FinishReason
	}

	// Build the clear.Input once; reuse for the embedded TokenAccounting
	// dollar cost so Scores and the per-event accounting agree on prices.
	clearInput := clear.Input{
		EndToEndMs:                 snap.EndToEndMs,
		GatewayOverheadMs:          snap.GatewayOverheadMs,
		VendorTTFTMs:               snap.VendorTTFTMs,
		Workflow:                   preferredWorkflow(headers.Workflow, routing.Workflow),
		HTTPStatus:                 opts.HTTPStatus,
		Vendor:                     routing.Vendor,
		Model:                      routing.Model,
		AssuranceScanRan:           routing.ScanRan,
		InboundFindingsBySeverity:  routing.InboundFindings,
		OutboundFindingsBySeverity: routing.OutboundFindings,
		FinishReason:               finishReason,
		AttemptCount:               routing.AttemptCount,
		FallbackUsed:               routing.FallbackUsed,
	}
	var tokenAcct *TokenAccounting
	if routing.UsageSet {
		prompt := routing.PromptTokens
		completion := routing.CompletionTokens
		clearInput.PromptTokens = &prompt
		clearInput.CompletionTokens = &completion
		inputRate, outputRate, priced := clear.LookupPricing(routing.Vendor, routing.Model)
		ta := &TokenAccounting{
			PromptTokens:     prompt,
			CompletionTokens: completion,
			TotalTokens:      prompt + completion,
		}
		if priced {
			ta.InputCostUSD = (float64(prompt) / 1000.0) * inputRate
			ta.OutputCostUSD = (float64(completion) / 1000.0) * outputRate
			ta.TotalCostUSD = ta.InputCostUSD + ta.OutputCostUSD
			ta.ModelPricingVersion = clear.PricingVersion
		}
		tokenAcct = ta
	}

	var assuranceSummary *AssuranceSummary
	if routing.ScanRan {
		assuranceSummary = &AssuranceSummary{
			InboundCount:     sumCounts(routing.InboundFindings),
			OutboundCount:    sumCounts(routing.OutboundFindings),
			InboundFindings:  routing.InboundFindings,
			OutboundFindings: routing.OutboundFindings,
			WorstSeverity:    worstSeverityIn(routing.InboundFindings, routing.OutboundFindings),
			NISTFindings:     routing.NISTFindings,
			TagFindings:      routing.TagFindings,
		}
	}

	respEvent := ResponseEvent{
		ResponseEventID:      respID,
		RequestEventID:       reqID,
		TenantID:             token.TenantID,      // denormalized per response-event.md §"Denormalization rationale"
		AIQGAccountID:        token.AIQGAccountID, // ditto
		Vendor:               routing.Vendor,      // denormalized from routing decision (same as RequestEvent)
		Model:                routing.Model,       // ditto — powers per-vendor/model dashboards off the response stream
		CompleteAt:           completeAt,
		Status:               status,
		HTTPStatus:           opts.HTTPStatus,
		FinishReason:         finishReason,
		Streamed:             snap.ChunkCount > 0,
		ChunkCount:           snap.ChunkCount,
		ContentChunkCount:    snap.ContentChunkCount,
		EventTimestamps:      snap,
		TokenAccounting:      tokenAcct,
		Assurance:            assuranceSummary,
		CLEAR:                clear.Compute(clearInput),
		ScoringVersion:       ScoringVersion,
		GatewayVersion:       GatewayVersion,
		ResolvedPolicyBundle: opts.ResolvedPolicyBundle,
		AgentContext:         agentCtx,
	}

	reqEnv := RequestEnvelope{
		SpecVersion:     SpecVersion,
		Type:            TypeRequest,
		Source:          Source,
		ID:              reqID,
		Time:            receivedAt,
		DataContentType: DataContentType,
		Data:            reqEvent,
	}
	respEnv := ResponseEnvelope{
		SpecVersion:     SpecVersion,
		Type:            TypeResponse,
		Source:          Source,
		ID:              respID,
		Time:            completeAt,
		DataContentType: DataContentType,
		Data:            respEvent,
	}

	return reqEnv, respEnv
}

// newUUID returns a v4-shaped 128-bit random ID, formatted as the
// canonical 8-4-4-4-12 hex string. We don't pull in a UUID dep because
// the format is mechanical and we don't need RFC-strict version bits.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is catastrophic — every consumer of this
		// ID would silently get duplicates. Panic so the deployment
		// fails fast on a misconfigured entropy source.
		panic("aiqg/events: crypto/rand.Read failed: " + err.Error())
	}
	// Apply RFC 4122 version 4 + variant bits for compatibility.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexstr := hex.EncodeToString(b[:])
	return hexstr[0:8] + "-" + hexstr[8:12] + "-" + hexstr[12:16] + "-" +
		hexstr[16:20] + "-" + hexstr[20:32]
}

// clientIP applies trusted-proxy normalization to derive the original
// client address. Today this is a thin wrapper around X-Forwarded-For;
// when the gateway is fronted by a real LB, this will read a trusted-
// proxy chain from config. For MVP, take the first XFF entry, falling
// back to RemoteAddr's host (port-stripped).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// First entry is the original client.
		for i, c := range xff {
			if c == ',' {
				return string([]byte(xff)[:i])
			}
		}
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sumCounts totals the values in a per-severity count map. Used to
// populate AssuranceSummary.InboundCount / OutboundCount so dashboards
// have a concrete number to aggregate even on clean scans (where the
// per-severity maps would be stripped by omitempty).
func sumCounts(m map[string]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}

// worstSeverityIn returns the highest severity present across either
// the inbound or outbound finding-count maps. Empty string means no
// findings recorded. Mirrors the bucketing in pkg/clear's
// scoreAssurance so the AssuranceSummary surfaces the same severity
// the score was derived from.
func worstSeverityIn(in, out map[string]int) string {
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if in[sev] > 0 || out[sev] > 0 {
			return sev
		}
	}
	return ""
}

// isStreaming inspects the request to decide whether it asked for a
// streamed response. Today this is a content-type / accept / query
// heuristic; the routing slice will replace it with payload-aware
// detection (OpenAI body has `"stream": true`, Anthropic same).
func isStreaming(r *http.Request) bool {
	if r.URL.Query().Get("stream") == "true" {
		return true
	}
	if accept := r.Header.Get("Accept"); accept != "" {
		// SSE accept header indicates streaming
		for i := 0; i < len(accept); i++ {
			if accept[i] == 't' && i+9 <= len(accept) && accept[i:i+9] == "text/even" {
				return true
			}
		}
	}
	return false
}

// preferredWorkflow picks the customer-supplied workflow over the
// auto-classified value when both are present. Customer intent wins
// because the classifier is heuristic and may miscategorize legitimate
// edge cases (a single_turn_qa that happens to contain code blocks,
// for instance). An empty header falls through to the classifier.
func preferredWorkflow(headerVal, classified string) string {
	if headerVal != "" {
		return headerVal
	}
	return classified
}
