package events

import (
	"crypto/rand"
	"crypto/sha256"
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

	// OTel GenAI declared signals (gen_ai.*), projected + mapped by the
	// middleware. OTelWorkflow is the op-name mapped to a workflow_type
	// ("" when the op-name falls through/excluded — see
	// otel-genai-ingestion.md §3); OTelOperation is the raw op-name kept
	// for classification drift. The agent/conversation values feed the
	// `otel` rung of the identity ladder.
	OTelWorkflow       string
	OTelOperation      string
	OTelAgentID        string
	OTelAgentName      string
	OTelConversationID string
	OTelSystem         string
	OTelMapVersion     string // op-name→workflow map version (set when an op-name was present)
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

	// Prompt-cache outcome: the mode actually applied and the number of
	// breakpoints that actually reached the vendor.
	PromptCacheMode        string
	PromptCacheBreakpoints int

	// Token usage from the vendor response (stamped by handlers via
	// middleware.StampTokenUsage). UsageSet distinguishes "vendor
	// returned 0 tokens" (a legitimate value for content_filter
	// finish-reason) from "vendor never returned a usage block"
	// (events omit TokenAccounting entirely in the latter case).
	PromptTokens     int
	CompletionTokens int
	UsageSet         bool

	// Cache-token breakdown from the vendor response, for cache-aware cost
	// accounting. Anthropic reports these separately from PromptTokens
	// (which is uncached input only). Zero when the response carried no
	// cache tokens (no caching, or a non-Anthropic provider).
	CacheCreationTokens int
	CacheReadTokens     int

	// Gatekeeper scan results. Inbound/Outbound are per-severity
	// counts ("low"/"medium"/"high"/"critical" → int). ScanRan
	// distinguishes "scan ran, no findings" from "scan never ran"
	// — events emit AssuranceSummary only when ScanRan is true.
	InboundFindings  map[string]int
	OutboundFindings map[string]int
	ScanRan          bool

	// RedactionCount is the number of PII findings redacted from the
	// inbound messages before the vendor call (G1). 0 = none/off.
	RedactionCount int

	// CacheState / CacheKeyHash carry the C1 response-cache interaction
	// (docs/AIQG-CACHING.md §6): "hit" / "miss" / "bypass" / "" and the
	// exact-match key. A hit means the vendor was not called.
	CacheState   string
	CacheKeyHash string

	// Cache savings (C2, §6): tokens + dollars a hit avoided. Zero on miss/bypass.
	CacheSavedPromptTokens     int
	CacheSavedCompletionTokens int
	CacheSavedCostUSD          float64

	// CacheSimilarity / CacheThreshold — the L1 similarity + admitting threshold
	// of a served C4 semantic hit (zero unless cache_state=semantic_hit).
	CacheSimilarity float64
	CacheThreshold  float64

	// Vendor finish_reason captured from the response. Used both
	// to populate ResponseEvent.FinishReason (taking precedence
	// over the BuildOptions value) and to drive clear.Efficacy.
	FinishReason string

	// BYOK credential attribution (Plan #14): which key served the vendor
	// call and the stored credential id. Never the key itself.
	CredentialSource string
	CredentialID     string

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
	HTTPStatus    int    // 0 if no response was written (gateway blocked)
	Status        string // explicit status; if empty, derived from HTTPStatus
	Region        string // deployment region; if empty, omitted from event
	IPCaptureMode string // off | minimized (default) | full — gates client IP
	FinishReason  string

	// ResponseEventID, when set, is used as the response event's id
	// instead of minting a fresh one. The Path A middleware pre-generates
	// it so it can set the TAS-Response-Event-Id response header before the
	// body flushes (clients/feedback correlate against it). Empty = mint.
	ResponseEventID string

	// ResolvedPolicyBundle is the bundle aiqg-dashboard-be picked for
	// this request (Phase 4.0). Always set in production — the
	// middleware degrades to a Default() resolution when the resolver
	// is unavailable so "every event has a resolved_policy_bundle"
	// holds. Nil pointer → field omitted from the response event
	// (covers pre-Phase-4.0 callers + non-AIQG middleware).
	ResolvedPolicyBundle *ResolvedPolicyBundle

	// Linkage is the resolved `linked`-tier flow/step topology for this
	// request (the middleware resolves it via pkg/aiqg/linkage before Build).
	Linkage Linkage

	// ExperimentID + ExperimentVariant stamp the experiment that claimed this
	// request (Phase D). Empty for untouched traffic.
	ExperimentID      string
	ExperimentVariant string

	// ReductionMeasurement is the result of a shadow payload-reduction
	// run (Plan #7 Phase 2). Nil when no extraction ran (the common
	// case — projected-only). When set on priced traffic, Build fills the
	// Contract v2 measured fields on TokenAccounting.
	ReductionMeasurement *ReductionMeasurement
}

// ReductionMeasurement carries the byte sizes from a real (shadow)
// extractor run so the builder can convert them to measured token/USD
// reduction. Bytes (not tokens) because the extractor works on bytes;
// conversion happens once in pkg/clear.
type ReductionMeasurement struct {
	Mode                    string // "shadow" | "active"
	Sampled                 bool
	OriginalBytes           int
	ExtractedBytes          int // after all enabled steps (= after relevance when SLM off)
	SizeAfterRelevanceBytes int
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
// Linkage is the deterministic `linked`-tier result the middleware resolves
// (via pkg/aiqg/linkage) before Build: this request's step id and, when it
// echoed a tool_call_id we served, the parent step + flow it belongs to.
// "asserted wins for WHO, linked wins for SHAPE" — StepID/ParentStepID are
// always applied; LinkedFlowID only fills FlowID when no header/trace flow
// was asserted.
type Linkage struct {
	StepID       string
	ParentStepID string
	// FlowID is the effective flow anchor to stamp when no TAS-Flow-Id /
	// traceparent flow was asserted: the resolved link's flow for an echoing
	// child, or a minted anchor for a tool-serving root. Filling FlowID does
	// NOT by itself mean "linked".
	FlowID string
	// Linked is true only when evidence matched — an echoed tool_call_id we
	// served (§A.1) OR a message-prefix that matched a state we served
	// (§A.2 prefix chaining). That's what promotes identity_source to `linked`.
	Linked      bool
	FlowStepSeq int
	// ConversationID is the thread this request continues, proven by prefix
	// chaining (the request's message prefix == a conversation state we
	// previously served). Fills conversation_id only when no TAS-Conversation-Id
	// header / baggage session.id was asserted. Empty for non-chained traffic.
	ConversationID string
	// Fingerprint is the request's structural signature — a hash of its
	// sorted tool names + param schemas + config tuple (model, sampling, stop,
	// response_format). Empty unless the request carries a fingerprintable
	// structure. Combined with (tenant, principal) in buildAgentContext to mint
	// the per-customer AgentSurrogateID for the `fingerprinted` tier (§B).
	Fingerprint string
}

func buildAgentContext(h AIQGHeadersView, t TokenView, clientIP string, lk Linkage) *AgentContext {
	ac := &AgentContext{
		AgentID:      h.AgentID,
		AgentName:    h.AgentName,
		AgentVersion: h.AgentVersion,
		UserID:       h.BaggageUserID,
		ClientIP:     clientIP,
		StepID:       lk.StepID,
		ParentStepID: lk.ParentStepID,
		FlowStepSeq:  lk.FlowStepSeq,
	}
	// conversation: explicit header wins, else baggage session.id, else the
	// thread proven by prefix chaining (a served-state echo). Naming, like
	// FlowID — header always wins to preserve customer intent.
	ac.ConversationID = h.ConversationID
	if ac.ConversationID == "" {
		ac.ConversationID = h.BaggageSessionID
	}
	if ac.ConversationID == "" {
		ac.ConversationID = h.OTelConversationID
	}
	if ac.ConversationID == "" {
		ac.ConversationID = lk.ConversationID
	}
	// flow: explicit header wins, else traceparent trace-id, else the
	// linked flow proven by a tool_call_id echo (shape, not naming).
	ac.FlowID = h.FlowID
	if ac.FlowID == "" {
		ac.FlowID = h.TraceID
	}
	if ac.FlowID == "" {
		ac.FlowID = lk.FlowID
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
		// asserted = TAS-* headers only. ac.AgentID is still TAS-only here
		// (the OTel fold below runs after this switch), so an OTel-supplied
		// agent id never mislabels as asserted.
		ac.IdentitySource = "asserted"
	case h.OTelAgentID != "" || h.OTelConversationID != "":
		// otel = declared via gen_ai.* — stronger than the gateway's own
		// inference (trace/linked/fingerprint), weaker than a TAS assertion.
		ac.IdentitySource = "otel"
	case h.TraceID != "":
		ac.IdentitySource = "trace"
	case lk.Linked:
		ac.IdentitySource = "linked"
	case ac.PrincipalID != "":
		ac.IdentitySource = "principal"
	case ac.ClientIP != "":
		ac.IdentitySource = "transport"
	default:
		ac.IdentitySource = "unattributed"
	}

	// OTel fold: an OTel-declared agent fills agent_id/agent_name when no
	// TAS-Agent-* header supplied them. Runs AFTER the identity_source switch
	// (so it can't mislabel as asserted) and BEFORE the fingerprint block
	// (so the surrogate never overwrites a declared OTel agent).
	if ac.AgentID == "" {
		ac.AgentID = h.OTelAgentID
	}
	if ac.AgentName == "" {
		ac.AgentName = h.OTelAgentName
	}

	// Fingerprinted tier (§B): when the request leaks a structural signature
	// (tools / response_format / stop), mint a per-(tenant, principal)
	// surrogate id. Always recorded for the asserted-vs-inferred cross-check
	// (§E). It only PROMOTES identity when nothing stronger than the principal
	// tier resolved — a fingerprint is weaker evidence than baggage / asserted
	// / trace / linked, but stronger than "just a token + IP". The (tenant,
	// principal) scoping means two customers running the same OSS agent never
	// collide into one surrogate.
	if lk.Fingerprint != "" {
		ac.AgentSurrogateID = agentSurrogateID(t.TenantID, ac.PrincipalID, lk.Fingerprint)
		switch ac.IdentitySource {
		case "principal", "transport", "unattributed":
			if ac.AgentID == "" {
				ac.AgentID = ac.AgentSurrogateID
			}
			ac.IdentitySource = "fingerprinted"
		}
	}

	// Attribution drift (Axis-1, classification-drift.md §3). Record the
	// declared agent + the gateway's independent inference (the surrogate) so
	// disagreement is queryable. v0.1 flags the clean cross-source case: both
	// declared channels present (TAS-Agent-Id AND OTel gen_ai.agent.id) and
	// differing. Surrogate-lineage matching is deferred to Axis-2.
	switch {
	case h.AgentID != "":
		ac.AgentDeclared = h.AgentID
		ac.DriftSource = "tas_asserted"
	case h.OTelAgentID != "":
		ac.AgentDeclared = h.OTelAgentID
		ac.DriftSource = "otel"
	}
	// Record the inferred surrogate for the cross-check only when a declared
	// agent exists to compare against (keeps drift fields off the vast
	// majority of events that carry no declared signal).
	if ac.AgentDeclared != "" {
		ac.AgentInferred = ac.AgentSurrogateID
	}
	if h.AgentID != "" && h.OTelAgentID != "" {
		d := h.AgentID != h.OTelAgentID
		ac.AgentDrift = &d
	}

	if ac.AgentID == "" && ac.AgentName == "" && ac.UserID == "" &&
		ac.ConversationID == "" && ac.FlowID == "" && ac.PrincipalID == "" && ac.ClientIP == "" &&
		ac.StepID == "" && ac.ParentStepID == "" {
		return nil
	}
	return ac
}

// agentSurrogateID mints the fingerprinted-tier identity: a short, stable id
// scoped to (tenant, principal) so the same structural signature recurs as one
// inferred agent for a customer, and never collides across customers. The
// "fp_" prefix marks it as inferred (vs an asserted TAS-Agent-Id) wherever it
// surfaces in agent_id.
func agentSurrogateID(tenantID, principalID, fingerprint string) string {
	h := sha256.New()
	h.Write([]byte(tenantID))
	h.Write([]byte{0})
	h.Write([]byte(principalID))
	h.Write([]byte{0})
	h.Write([]byte(fingerprint))
	return "fp_" + hex.EncodeToString(h.Sum(nil))[:16]
}

// applyIPMode gates a raw client IP per the deployment's capture mode:
// "off" drops it, "full" keeps it raw, anything else (incl. empty)
// minimizes to a /24 (IPv4) or /48 (IPv6) prefix.
func applyIPMode(ip, mode string) string {
	switch mode {
	case "off":
		return ""
	case "full":
		return ip
	default:
		return truncateIP(ip)
	}
}

// truncateIP reduces an IP to its /24 (IPv4) or /48 (IPv6) network
// prefix. Returns "" for an empty or unparseable address.
func truncateIP(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String() + "/24"
	}
	return parsed.Mask(net.CIDRMask(48, 128)).String() + "/48"
}

func Build(r *http.Request, headers AIQGHeadersView, routing RoutingView, token TokenView, snap instrumentation.Snapshot, opts BuildOptions) (RequestEnvelope, ResponseEnvelope) {
	reqID := newUUID()
	respID := opts.ResponseEventID
	if respID == "" {
		respID = newUUID()
	}
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

	// Client IP, gated by the deployment's IP-capture mode.
	clientIPVal := applyIPMode(clientIP(r), opts.IPCaptureMode)

	// Self-asserted identity attribution (shared by both events).
	agentCtx := buildAgentContext(headers, token, clientIPVal, opts.Linkage)

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
		Workflow:        preferredWorkflow(headers.Workflow, headers.OTelWorkflow, routing.Workflow),
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
		Workflow:                   preferredWorkflow(headers.Workflow, headers.OTelWorkflow, routing.Workflow),
		HTTPStatus:                 opts.HTTPStatus,
		Vendor:                     routing.Vendor,
		Model:                      routing.Model,
		AssuranceScanRan:           routing.ScanRan,
		InboundFindingsBySeverity:  routing.InboundFindings,
		OutboundFindingsBySeverity: routing.OutboundFindings,
		FinishReason:               finishReason,
		AttemptCount:               routing.AttemptCount,
		FallbackUsed:               routing.FallbackUsed,
		InboundBloatFindings:       bloatCount(routing.TagFindings),
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

			// Cache-aware accounting: when the vendor reported cache tokens,
			// price them at their real multiples of the input rate (creation
			// 1.25×, read 0.10×) and record the true billed total. `prompt` is
			// the uncached input (vendor input_tokens); cache tokens are
			// separate. TotalCostUSD stays the legacy uncached+output figure.
			if routing.CacheCreationTokens > 0 || routing.CacheReadTokens > 0 {
				ca := clear.CacheAwareCost(routing.Vendor, routing.Model, prompt,
					routing.CacheCreationTokens, routing.CacheReadTokens, completion, routing.UsageSet)
				ta.CacheCreationTokens = routing.CacheCreationTokens
				ta.CacheReadTokens = routing.CacheReadTokens
				ta.CacheCreationCostUSD = ca.CacheCreationUSD
				ta.CacheReadCostUSD = ca.CacheReadUSD
				ta.CacheAwareTotalCostUSD = ca.TotalUSD
			}

			// Cost decomposition (CLEAR v0.2, Contract v1 — projected).
			// Only on priced traffic; never fabricate waste otherwise.
			actual := clear.ActualCost(routing.Vendor, routing.Model, prompt, completion, routing.UsageSet)
			d := clear.DecomposeCost(clearInput, actual)
			ta.ActualCostUSD = actual.TotalUSD
			ta.ActualCostSource = actual.Source
			ta.ReductionMode = d.ReductionMode
			ta.ContextEfficiencyRatio = d.ContextEfficiencyRatio
			ta.ProjectedDirectPayloadWasteTokens = d.DirectPayloadWasteTokens
			ta.ProjectedDirectPayloadWasteUSD = d.DirectPayloadWasteUSD
			ta.DirectPayloadWasteTokens = d.DirectPayloadWasteTokens // alias = projected
			ta.DirectPayloadWasteUSD = d.DirectPayloadWasteUSD       // alias = projected
			ta.ProjectedReductionRelevanceUSD = d.ProjectedReductionRelevanceUSD
			ta.ProjectedReductionRelevanceConfidence = d.ProjectedReductionRelevanceConfidence
			ta.ProjectedReductionSLMUSD = d.ProjectedReductionSLMUSD
			ta.ProjectedReductionSLMConfidence = d.ProjectedReductionSLMConfidence
			ta.ProjectedReductionCombinedUSD = d.ProjectedReductionCombinedUSD
			ta.InducedOutputWasteEstimatedUSD = d.InducedOutputWasteEstimatedUSD
			ta.GenuinePostModelWasteUSD = d.GenuinePostModelWasteUSD
			ta.GatewayAddressablePct = d.GatewayAddressablePct

			// Measured reduction (Contract v2) — a real shadow/active
			// extractor run. Cost only; quality deltas need a baseline
			// re-run (left unmeasured). Bytes→tokens once via pkg/clear.
			if rm := opts.ReductionMeasurement; rm != nil && rm.OriginalBytes > 0 {
				reducedBytes := rm.OriginalBytes - rm.ExtractedBytes
				if reducedBytes < 0 {
					reducedBytes = 0
				}
				reducedTokens := clear.TokensFromBytes(reducedBytes)
				reducedUSD := (float64(reducedTokens) / 1000.0) * inputRate
				ta.ReductionMode = rm.Mode // "shadow" / "active" (overrides "projected")
				ta.ReductionSampled = rm.Sampled
				ta.ActualDirectPayloadReductionTokens = reducedTokens
				ta.ActualDirectPayloadReductionUSD = reducedUSD
				// Split the measured saving across the two steps via the
				// post-relevance size: relevance = original−afterRelevance,
				// SLM = afterRelevance−extracted. When afterRelevance is unknown
				// (0) attribute it all to relevance (relevance-only run).
				usdFor := func(b int) float64 {
					if b < 0 {
						b = 0
					}
					return (float64(clear.TokensFromBytes(b)) / 1000.0) * inputRate
				}
				if rm.SizeAfterRelevanceBytes > 0 {
					ta.ActualReductionRelevanceUSD = usdFor(rm.OriginalBytes - rm.SizeAfterRelevanceBytes)
					ta.ActualReductionSLMUSD = usdFor(rm.SizeAfterRelevanceBytes - rm.ExtractedBytes)
				} else {
					ta.ActualReductionRelevanceUSD = reducedUSD
				}
			}
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

	// Classification drift (Axis-1, classification-drift.md §3). The declared
	// type is a TAS-Workflow assertion or the mapped OTel op-name; the inferred
	// type is the heuristic classifier. Both recorded so declared-vs-inferred
	// disagreement is queryable even though `Workflow` above is the winner.
	wfDeclared := headers.Workflow
	if wfDeclared == "" {
		wfDeclared = headers.OTelWorkflow
	}
	wfInferred := routing.Workflow
	var wfDrift *bool
	if wfDeclared != "" && wfInferred != "" {
		d := wfDeclared != wfInferred
		wfDrift = &d
	}

	respEvent := ResponseEvent{
		ResponseEventID:            respID,
		RequestEventID:             reqID,
		TenantID:                   token.TenantID,                                                              // denormalized per response-event.md §"Denormalization rationale"
		AIQGAccountID:              token.AIQGAccountID,                                                         // ditto
		Vendor:                     routing.Vendor,                                                              // denormalized from routing decision (same as RequestEvent)
		Model:                      routing.Model,                                                               // ditto — powers per-vendor/model dashboards off the response stream
		Workflow:                   preferredWorkflow(headers.Workflow, headers.OTelWorkflow, routing.Workflow), // ditto — carries the workflow_type dimension on the response stream
		SourceApp:                  sourceApp,                                                                   // ditto — carries the source_app dimension on the response stream
		WorkflowDeclared:           wfDeclared,
		WorkflowDeclaredOp:         headers.OTelOperation,
		WorkflowInferred:           wfInferred,
		WorkflowDrift:              wfDrift,
		OTelMapVersion:             headers.OTelMapVersion,
		ExperimentID:               opts.ExperimentID, // Phase D — experiment that claimed this request (per-variant rollup key)
		ExperimentVariant:          opts.ExperimentVariant,
		CompleteAt:                 completeAt,
		Status:                     status,
		HTTPStatus:                 opts.HTTPStatus,
		FinishReason:               finishReason,
		PromptCacheMode:            routing.PromptCacheMode,
		PromptCacheBreakpoints:     routing.PromptCacheBreakpoints,
		CredentialSource:           routing.CredentialSource,
		CredentialID:               routing.CredentialID,
		Streamed:                   snap.ChunkCount > 0,
		ChunkCount:                 snap.ChunkCount,
		ContentChunkCount:          snap.ContentChunkCount,
		EventTimestamps:            snap,
		TokenAccounting:            tokenAcct,
		Assurance:                  assuranceSummary,
		RedactionApplied:           routing.RedactionCount > 0,
		RedactedCount:              routing.RedactionCount,
		CacheState:                 routing.CacheState,
		CacheKeyHash:               routing.CacheKeyHash,
		CacheSavedPromptTokens:     routing.CacheSavedPromptTokens,
		CacheSavedCompletionTokens: routing.CacheSavedCompletionTokens,
		CacheSavedCostUSD:          routing.CacheSavedCostUSD,
		CacheSimilarity:            routing.CacheSimilarity,
		CacheThreshold:             routing.CacheThreshold,
		CLEAR:                      clear.Compute(clearInput),
		ScoringVersion:             ScoringVersion,
		GatewayVersion:             GatewayVersion,
		ResolvedPolicyBundle:       opts.ResolvedPolicyBundle,
		AgentContext:               agentCtx,
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
// NewEventID mints a fresh event id. Exported so the Path A middleware can
// pre-generate a response_event_id (to set the TAS-Response-Event-Id header
// before the body flushes) and pass it back via BuildOptions.ResponseEventID.
func NewEventID() string { return newUUID() }

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

// bloatCount sums the inbound bloat / instruction-stuffing matches from
// the per-pattern TagFindings map — the signal the cost decomposer uses
// to discount context efficiency. nil-safe.
func bloatCount(tagFindings map[string]int) int {
	if tagFindings == nil {
		return 0
	}
	return tagFindings["aiqg-bloated-context"] + tagFindings["aiqg-instruction-stuffing"]
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

// preferredWorkflow resolves the effective workflow_type across the three
// sources in precedence order: explicit TAS-Workflow header > OTel-declared
// (gen_ai.operation.name mapped to a workflow_type) > heuristic classifier.
// Customer intent wins outright because the header is authoritative; an
// OTel-declared type beats the heuristic because it's a high-confidence
// client assertion; both empty falls through to the classifier. See
// aether-shared/data-models/aiqg/otel-genai-ingestion.md §4.1.
func preferredWorkflow(headerVal, otelVal, classified string) string {
	if headerVal != "" {
		return headerVal
	}
	if otelVal != "" {
		return otelVal
	}
	return classified
}
