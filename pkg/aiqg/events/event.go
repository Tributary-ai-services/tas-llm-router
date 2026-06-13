// Package events defines the AIQG request/response event schema and its
// CloudEvents 1.0 envelope. Per build-vs-reuse §1 placement decision,
// AIQG-specific code lives in pkg/aiqg/* inside tas-llm-router (not in
// a separate repo).
//
// Two event types are emitted per AIQG request:
//   - RequestEnvelope (type=com.tas.aiqg.request.v1) — captured at
//     request open / first inspection.
//   - ResponseEnvelope (type=com.tas.aiqg.response.v1) — captured at
//     response close. Carries the timing snapshot, outcome, CLEAR scores
//     (when scored), and the back-reference to its paired request_event_id.
//
// MVP scope: the gateway emits both events at response-close in a single
// defer (atomic from the request-handler's perspective). Spark/consumers
// pair them by request_event_id. Future slice may split emission across
// the request lifecycle if the consumer side justifies it.
//
// Fields the gateway can populate at MVP today are concrete types. Fields
// that depend on still-unmerged slices (token resolution → tenant_id,
// routing decisions → vendor/model, scorer → clear_*) are pointer types so
// downstream consumers can distinguish "not yet computed" from "computed
// as zero." The schema mirrors aether-shared/data-models/aiqg/
// {request,response}-event.md.
package events

import (
	"time"

	"github.com/tributary-ai/llm-router-waf/internal/instrumentation"
	"github.com/tributary-ai/llm-router-waf/pkg/clear"
)

// CloudEvents 1.0 type identifiers. Bump the suffix when the data shape
// changes in a breaking way; non-breaking additions don't require a bump
// (consumers ignore unknown fields).
const (
	TypeRequest  = "com.tas.aiqg.request.v1"
	TypeResponse = "com.tas.aiqg.response.v1"

	SpecVersion     = "1.0"
	DataContentType = "application/json"

	// Source URI matches Gatekeeper's urn:tas:service:<name> scheme so
	// every TAS service that emits CloudEvents shares one URI namespace.
	Source = "urn:tas:service:tas-llm-router"
)

// RequestEnvelope wraps a RequestEvent in a CloudEvents 1.0 envelope.
// Marshalled JSON matches the structured-mode binding (§3.5 of the
// CloudEvents 1.0 spec).
type RequestEnvelope struct {
	SpecVersion     string       `json:"specversion"`
	Type            string       `json:"type"`
	Source          string       `json:"source"`
	ID              string       `json:"id"`
	Time            time.Time    `json:"time"`
	Subject         string       `json:"subject,omitempty"`
	DataContentType string       `json:"datacontenttype"`
	Data            RequestEvent `json:"data"`
}

// ResponseEnvelope wraps a ResponseEvent in a CloudEvents 1.0 envelope.
type ResponseEnvelope struct {
	SpecVersion     string        `json:"specversion"`
	Type            string        `json:"type"`
	Source          string        `json:"source"`
	ID              string        `json:"id"`
	Time            time.Time     `json:"time"`
	Subject         string        `json:"subject,omitempty"`
	DataContentType string        `json:"datacontenttype"`
	Data            ResponseEvent `json:"data"`
}

// RequestEvent mirrors aether-shared/data-models/aiqg/request-event.md §2.1.
// Only fields populatable from the gateway today are concrete; the rest
// are optional and omitted from JSON when unset.
type RequestEvent struct {
	// Identity & linkage
	RequestEventID string `json:"request_event_id"`
	TenantID       string `json:"tenant_id,omitempty"`         // filled by token-resolver slice
	AIQGAccountID  string `json:"aiqg_account_id,omitempty"`   // filled by token-resolver slice
	TASAuthTokenID string `json:"tas_auth_token_id,omitempty"` // filled by token-resolver slice

	// Request basics (populatable today)
	ReceivedAt      time.Time `json:"received_at"`
	Endpoint        string    `json:"endpoint"`
	Method          string    `json:"method"`
	SourceIP        string    `json:"source_ip,omitempty"`
	SourceApp       string    `json:"source_app,omitempty"`
	ClientRequestID string    `json:"client_request_id,omitempty"`
	Region          string    `json:"region,omitempty"`

	// Routing (filled by router slice)
	Vendor    string `json:"vendor,omitempty"`
	Model     string `json:"model,omitempty"`
	Streaming bool   `json:"streaming"`

	// Mode
	IsAIQGMode bool `json:"is_aiqg_mode"`

	// Headers reflected
	DryRun        bool     `json:"dry_run"`
	TraceReturned bool     `json:"trace_returned"`
	Workflow      string   `json:"workflow,omitempty"`
	PolicyNames   []string `json:"policy_names,omitempty"`
	PolicyBundle  string   `json:"policy_bundle,omitempty"`

	// Correlation to response (always populated when emitted as a pair)
	CorrelatedResponseEventID string `json:"correlated_response_event_id,omitempty"`

	// Versioning
	ScoringVersion string `json:"scoring_version"`
	GatewayVersion string `json:"gateway_version"`
	LifecycleState string `json:"lifecycle_state"`

	// Identity attribution (additive) — see AgentContext.
	AgentContext *AgentContext `json:"agent_context,omitempty"`
}

// AgentContext is the self-asserted identity attribution for a request:
// the cross-app user/agent/flow keys lifted from the W3C `baggage` and
// TAS-Agent-* / `traceparent` headers, plus the AIQG token for the
// principal tier. Additive and optional — nil when nothing resolved.
// See docs/AIQG-AGENT-FLOW-ATTRIBUTION.md. The emitter promotes the
// non-empty fields to top-level Loki labels so dashboards can group by
// user_id / agent_id / flow_id without parsing the payload.
type AgentContext struct {
	AgentID        string `json:"agent_id,omitempty"`        // TAS-Agent-Id
	AgentName      string `json:"agent_name,omitempty"`      // TAS-Agent-Name
	AgentVersion   string `json:"agent_version,omitempty"`   // TAS-Agent-Version
	UserID         string `json:"user_id,omitempty"`         // baggage user.id (pseudonymous)
	ConversationID string `json:"conversation_id,omitempty"` // TAS-Conversation-Id or baggage session.id
	FlowID         string `json:"flow_id,omitempty"`         // TAS-Flow-Id or traceparent trace-id
	PrincipalID    string `json:"principal_id,omitempty"`    // AIQG token source_app / token id
	ClientIP       string `json:"client_ip,omitempty"`       // gated client IP (raw/truncated/off per ip_capture_mode)
	IdentitySource string `json:"identity_source,omitempty"` // baggage|asserted|trace|principal|transport|unattributed
}

// ResponseEvent mirrors aether-shared/data-models/aiqg/response-event.md §2.2.
type ResponseEvent struct {
	// Identity & linkage
	ResponseEventID string `json:"response_event_id"`
	RequestEventID  string `json:"request_event_id"`
	TenantID        string `json:"tenant_id,omitempty"`
	AIQGAccountID   string `json:"aiqg_account_id,omitempty"`

	// Routing attribution — denormalized from the request's routing
	// decision so the response event is self-describing: per-vendor and
	// per-model cost/token dashboards read these directly off the
	// response stream, and they survive a lost request event (see
	// response-event.md KI-2). Same values as the paired RequestEvent.
	Vendor    string `json:"vendor,omitempty"`
	Model     string `json:"model,omitempty"`
	Workflow  string `json:"workflow,omitempty"`   // CLEAR workflow_type — denormalized so the response stream carries the type dimension for per-type rollups (TimescaleDB)
	SourceApp string `json:"source_app,omitempty"` // calling app — denormalized like vendor/model/workflow so the response stream carries the source dimension

	// Outcome
	CompleteAt      time.Time `json:"complete_at"`
	Status          string    `json:"status"` // success | vendor_error | gateway_error | policy_blocked | client_disconnect | timeout
	HTTPStatus      int       `json:"http_status"`
	FinishReason    string    `json:"finish_reason,omitempty"`
	VendorRequestID string    `json:"vendor_request_id,omitempty"`

	// Streaming telemetry (from timing snapshot)
	Streamed          bool `json:"streamed"`
	ChunkCount        int  `json:"chunk_count"`
	ContentChunkCount int  `json:"content_chunk_count"`

	// Embedded timing block — see event-timestamps.md
	EventTimestamps instrumentation.Snapshot `json:"event_timestamps"`

	// Embedded token-accounting block — see token-accounting.md.
	// Omitted when the vendor returned no usage data (UsageSet=false
	// on the routing snapshot) — distinct from "0 tokens used".
	TokenAccounting *TokenAccounting `json:"token_accounting,omitempty"`

	// Compact Gatekeeper scan summary. Full tag_set integration is a
	// later slice; for MVP this carries the per-severity counts +
	// derived worst severity so dashboards can slice Assurance scoring
	// by direction. Omitted when no scan ran (e.g. internal pass-
	// through traffic or Gatekeeper-disabled deployments).
	Assurance *AssuranceSummary `json:"assurance,omitempty"`

	// CLEAR scores — populated by pkg/clear.Compute(). A nil *Scores
	// (rather than a *Scores with all-nil dimension fields) means the
	// scorer wasn't run at all, which today only happens on the noop
	// path; once events.Build always invokes the scorer this should be
	// non-nil on every emitted response event.
	CLEAR *clear.Scores `json:"clear_scores,omitempty"`

	// Versioning
	ScoringVersion string `json:"scoring_version"`
	GatewayVersion string `json:"gateway_version"`

	// ResolvedPolicyBundle is the bundle aiqg-dashboard-be's
	// /internal/policy/resolve picked for this request (Phase 4.0).
	// Always populated — nil/empty would defeat the "every request
	// resolves to exactly one bundle" guarantee. When source="default"
	// the bundle ID is empty and bundle name is "(default)", and the
	// (Stage 4.1+) enforcement engine treats this as observe-only.
	ResolvedPolicyBundle *ResolvedPolicyBundle `json:"resolved_policy_bundle,omitempty"`

	// Identity attribution (additive) — see AgentContext.
	AgentContext *AgentContext `json:"agent_context,omitempty"`
}

// ResolvedPolicyBundle mirrors the dashboard-be ResolveResponse shape.
// The middleware fills this in after calling the resolver; the emitter
// promotes the fields to top-level logrus labels (resolved_policy_bundle_id
// + resolved_policy_bundle_name + resolved_policy_bundle_source) so LogQL
// can aggregate without parsing the embedded payload.
type ResolvedPolicyBundle struct {
	BundleID   string `json:"bundle_id"`
	BundleName string `json:"bundle_name"`
	Source     string `json:"source"` // "explicit" | "tenant_active" | "default"
}

// TokenAccounting carries the vendor-reported token counts and the
// gateway-computed dollar costs. Mirrors aether-shared/data-models/aiqg/
// token-accounting.md.
//
// Dollar costs are zero when the vendor:model pair isn't in the
// pkg/clear pricing table — distinct from "request used zero tokens"
// (in which case the *Tokens fields are zero too). The
// ModelPricingVersion stamps the pricing-table revision used so a
// Spark re-score job can identify rows scored under stale rates.
type TokenAccounting struct {
	PromptTokens        int     `json:"prompt_tokens"`
	CompletionTokens    int     `json:"completion_tokens"`
	TotalTokens         int     `json:"total_tokens"`
	InputCostUSD        float64 `json:"input_cost_usd"`
	OutputCostUSD       float64 `json:"output_cost_usd"`
	TotalCostUSD        float64 `json:"total_cost_usd"`
	ModelPricingVersion string  `json:"model_pricing_version,omitempty"`
}

// AssuranceSummary is the lightweight per-event view of Gatekeeper
// scan output. The full tag_set + per-NIST-characteristic breakdown
// belongs on its own embedded sub-document (deferred slice — see
// aether-shared/data-models/aiqg/tag-set.md). This summary keeps the
// event payload small while still letting Spark/dashboards segment
// by severity and direction.
//
// InboundCount / OutboundCount are total finding counts per direction.
// They always emit (no `omitempty`) — zero is a meaningful value
// ("scan ran, found nothing") and dashboards need a concrete number
// to aggregate. The per-severity maps stay omitempty so a clean scan
// doesn't bloat the event payload with empty objects.
type AssuranceSummary struct {
	InboundCount     int            `json:"inbound_count"`
	OutboundCount    int            `json:"outbound_count"`
	InboundFindings  map[string]int `json:"inbound_findings,omitempty"`
	OutboundFindings map[string]int `json:"outbound_findings,omitempty"`
	WorstSeverity    string         `json:"worst_severity,omitempty"`
	// NISTFindings buckets findings by NIST AI RMF characteristic
	// (secure_resilient / privacy_enhanced / valid_reliable / safe).
	// Computed from per-finding pattern IDs via
	// middleware.MapPatternToNIST. Drives the Day-1 Report
	// Trustworthiness section. omitempty when no findings.
	NISTFindings map[string]int `json:"nist_findings,omitempty"`

	// TagFindings buckets findings by Gatekeeper pattern_id
	// (aiqg-role-claim, pii-email, …). Drives /api/v1/metrics/tags
	// — "what matchers are firing most often for this tenant".
	// omitempty when no findings.
	TagFindings map[string]int `json:"tag_findings,omitempty"`
}

// Status enum values for ResponseEvent.Status.
const (
	StatusSuccess          = "success"
	StatusVendorError      = "vendor_error"
	StatusGatewayError     = "gateway_error"
	StatusPolicyBlocked    = "policy_blocked"
	StatusClientDisconnect = "client_disconnect"
	StatusTimeout          = "timeout"
)

// Lifecycle state enum values for RequestEvent.LifecycleState.
const (
	LifecycleReceived           = "received"
	LifecycleValidated          = "validated"
	LifecyclePolicyResolved     = "policy_resolved"
	LifecycleForwarded          = "forwarded"
	LifecyclePairedWithResponse = "paired_with_response"
	LifecycleArchived           = "archived"
)

// StatusFromHTTP maps an HTTP status code to the spec's status enum.
// Heuristic, used when the gateway doesn't have a more specific signal
// (e.g. policy_blocked is set by the policy layer; client_disconnect by
// the response writer).
func StatusFromHTTP(code int) string {
	switch {
	case code == 0:
		// 0 means we never wrote a response — gateway blocked before forwarding.
		return StatusGatewayError
	case code >= 200 && code < 400:
		return StatusSuccess
	case code == 504, code == 408:
		return StatusTimeout
	case code >= 400 && code < 500:
		return StatusGatewayError
	default:
		return StatusVendorError
	}
}
