// Package metrics defines the Prometheus metrics surface for the AIQG
// pipeline. Lives in pkg/aiqg/* so it sits alongside events, clear,
// tokens — the rest of the AIQG-specific Go code.
//
// Why a dedicated registry instead of prometheus.DefaultRegisterer:
// an explicit registry keeps the surface enumerable and lets tests call
// Registry.Gather() to assert on samples. Exposed on /aiqg/metrics via
// promhttp, scraped independently in the Prometheus config.
//
// The legacy hand-rolled internal/server.handleMetrics this once had to
// coexist with is gone — see internal/metrics, which now serves /metrics
// from a real client_golang registry.
//
// Naming follows the Prometheus best-practice guide:
//   - aiqg_<subsystem>_<noun>_<unit_or_total>
//   - counters always end in _total
//   - histograms use seconds for duration
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Registry is the dedicated registerer for AIQG metrics. Wired into
// /aiqg/metrics via promhttp.HandlerFor(Registry, ...). Tests can use
// Registry.Gather() to inspect collected samples.
var Registry = prometheus.NewRegistry()

// EventsEmittedTotal counts paired-emit attempts by Emitter type
// (log, kafka, memory, noop) and outcome (success, failure). Bumps
// once per (req, resp) pair regardless of how many envelopes a single
// emit produces — semantics match Emitter.Emit's API.
var EventsEmittedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_events_emitted_total",
		Help: "AIQG event emit attempts by emitter type and outcome.",
	},
	[]string{"emitter", "outcome"},
)

// EmitDurationSeconds is the latency of Emitter.Emit per emitter type.
// LogEmitter is fast (in-process logrus); KafkaEmitter can spike on
// broker backpressure — alerts watch the p99 here.
var EmitDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "aiqg_emit_duration_seconds",
		Help:    "Latency of AIQG Emitter.Emit calls, by emitter type.",
		Buckets: []float64{0.0001, 0.001, 0.01, 0.05, 0.1, 0.5, 1, 5},
	},
	[]string{"emitter"},
)

// ScanFindingsTotal counts Gatekeeper findings surfaced into AIQG
// events, broken out by direction and severity. Driven by the
// finding counts on the routing snapshot at emit time — each request
// contributes once per (direction, severity) bucket.
var ScanFindingsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_scan_findings_total",
		Help: "AIQG Gatekeeper findings count by direction (inbound/outbound) and severity.",
	},
	[]string{"direction", "severity"},
)

// RequestTierTotal counts AIQG requests by CLEAR dimension and tier
// (healthy / marginal / failing). One bump per (dimension, tier) for
// each emitted response event; dimensions with nil scores skip.
// Spec tier boundaries differ per dimension — see pkg/clear scorers
// for the buckets. The tierFor helper in this package picks the
// label using the default thresholds (Healthy ≥75 / Marginal 50-74 /
// Failing <50) which apply to Latency / Cost / Efficacy / Reliability;
// Assurance uses stricter ≥90 / 75-89 / <75, exposed via tierForAssurance.
var RequestTierTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_request_tier_total",
		Help: "AIQG requests bucketed into Healthy/Marginal/Failing tiers per CLEAR dimension.",
	},
	[]string{"dimension", "tier"},
)

// RequestsTotal is a simple counter of every response event emitted.
// Useful as the denominator for tier and finding rates without having
// to sum across all the labeled variants.
var RequestsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "aiqg_requests_total",
		Help: "Total AIQG response events emitted.",
	},
)

// ShadowReplaysTotal counts pairwise shadow-eval variant replays by outcome
// (recorded / judge_abstained / replay_failed / judge_failed).
//
// A shadow replay is a live, billed call whose response is never served, and
// until this counter existed that spend appeared in no metric, report or cost
// total (tas-llm-router#184). It bumps on EVERY replay attempt — including the
// ones whose result is discarded — because an abstaining judge costs exactly
// as much as an agreeing one, and a spend figure that only counts successes
// understates the bill in the one direction nobody notices.
//
// Deliberately unlabelled by tenant: per-tenant attribution belongs on the
// event path, not in a metric whose cardinality would then track customer
// count. That remains the open half of #184.
var ShadowReplaysTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_shadow_replays_total",
		Help: "Pairwise shadow-eval variant replays by outcome.",
	},
	[]string{"outcome"},
)

// ShadowTokensTotal sums tokens billed by shadow replays, by direction.
// Counted from the provider's own usage report, so it reflects what was
// actually billed rather than what was requested.
var ShadowTokensTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_shadow_tokens_total",
		Help: "Tokens billed by pairwise shadow-eval replays, by direction (input/output).",
	},
	[]string{"direction"},
)

// ShadowTruncatedTotal counts replays the provider stopped at the token cap.
//
// Exists because a truncated variant judged against a complete control
// response measures our cap rather than the model (tas-llm-router#182). Once
// the replay mirrors the caller's own limit this should track the control
// arm's truncation rate; a persistent gap means the mirroring regressed.
var ShadowTruncatedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "aiqg_shadow_truncated_total",
		Help: "Shadow-eval replays that hit the token cap (finish_reason=length).",
	},
)

// Outcome label values for ShadowReplaysTotal.
const (
	ShadowRecorded     = "recorded"
	ShadowJudgeAbstain = "judge_abstained"
	ShadowReplayFailed = "replay_failed"
	ShadowJudgeFailed  = "judge_failed"
	ShadowRecordFailed = "record_failed"
)

// JudgeCallsTotal counts LLM-as-judge calls by outcome.
//
// The judge is the other half of the spend that AIQG bills but never accounts
// for (tas-llm-router#184). Shadow replays got counters first, but shadow-eval
// is OFF by default while pointwise judging runs at a live sample rate — so
// this path, not the replay path, is the unaccounted spend actually being
// incurred today.
//
// Counted at the router adapter rather than at the scoring call, because that
// is the only point every judge call passes through *before* its result can be
// thrown away. A judge that abstains, returns unparseable JSON, or fails to
// record costs exactly what a successful one does; counting at the call site
// would drop all three and understate the bill in the one direction nobody
// checks. `completed_no_usage` is kept distinct from `completed` so a provider
// that stops reporting usage shows up as a gap rather than as a drop in spend.
var JudgeCallsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_judge_calls_total",
		Help: "LLM-as-judge calls by outcome (completed/completed_no_usage/route_failed/provider_failed).",
	},
	[]string{"outcome"},
)

// JudgeTokensTotal sums tokens billed by judge calls, by direction. Taken from
// the provider's own usage report, so it reflects what was billed rather than
// what was asked for. Mirrors ShadowTokensTotal deliberately: the two paths are
// summed together when answering "what did evaluation cost?".
var JudgeTokensTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_judge_tokens_total",
		Help: "Tokens billed by LLM-as-judge calls, by direction (input/output).",
	},
	[]string{"direction"},
)

// UnbilledSpendUSDTotal is the dollar cost of calls the gateway makes on its
// own behalf — judge scoring and shadow replays — which never appear on a
// customer's invoice and never reach the event path.
//
// Tokens alone cannot be summed into money: the two paths run different models
// at rates that differ by ~19× between haiku and opus, so a token total is not
// a spend total. Priced here from the same clear.DollarCost table the billing
// figures use, so this metric and the invoice cannot disagree about rates.
//
// Labelled by path, NOT by tenant: per-tenant attribution belongs on the event
// path, not in a metric whose cardinality would track customer count. That
// remains the open half of #184.
var UnbilledSpendUSDTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_unbilled_spend_usd_total",
		Help: "USD spent on gateway-initiated evaluation calls, by path (judge/shadow_replay).",
	},
	[]string{"path"},
)

// UnpricedCallsTotal counts evaluation calls whose vendor:model is absent from
// the pricing table, so their cost could not be added to UnbilledSpendUSDTotal.
//
// Without this, an unpriced judge model makes evaluation look FREE rather than
// unmeasured — the spend total stays flat and nothing indicates it is missing
// rows. Any sustained non-zero value here means UnbilledSpendUSDTotal is an
// undercount, and names which path to go look at.
var UnpricedCallsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_unpriced_eval_calls_total",
		Help: "Evaluation calls skipped by cost accounting for want of a pricing-table entry, by path.",
	},
	[]string{"path"},
)

// Outcome label values for JudgeCallsTotal.
const (
	JudgeCompleted        = "completed"
	JudgeCompletedNoUsage = "completed_no_usage"
	JudgeRouteFailed      = "route_failed"
	JudgeProviderFailed   = "provider_failed"
)

// Path label values for UnbilledSpendUSDTotal / UnpricedCallsTotal.
const (
	SpendPathJudge        = "judge"
	SpendPathShadowReplay = "shadow_replay"
)

// JudgeExcludedTotal counts responses that reached the judge decision point and
// were never judged, by reason.
//
// The judged population is not a random sample of served traffic, and nothing
// else in the system makes that visible. Random sampling IS unbiased by
// construction, so unsampled responses are deliberately not counted here —
// folding them in would bury the exclusions that actually skew the result
// under a number driven by the sample rate. Each reason below removes a
// *particular kind* of response:
//
//   - blocked_outbound: the content scanner refused the response, so the
//     handler returned 403 well before maybeJudge. These are precisely the
//     responses most likely to score badly, so their absence flatters every
//     judge aggregate — the measurement bias this counter exists to expose.
//   - empty_response: tool-call-only turns, which skew toward agentic traffic.
//   - not_attributed: no AIQG token, so there is no tenant to scope a score to.
//   - no_event_id: no response event was emitted for a score to attach to.
//
// Without this, a judge score reads as "the quality of our traffic" when it is
// really "the quality of the traffic that survived to be judged".
var JudgeExcludedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_judge_excluded_total",
		Help: "Responses never judged, by reason (blocked_outbound/empty_response/not_attributed/no_event_id).",
	},
	[]string{"reason"},
)

// Reason label values for JudgeExcludedTotal.
const (
	JudgeExcludedBlocked       = "blocked_outbound"
	JudgeExcludedEmpty         = "empty_response"
	JudgeExcludedNotAttributed = "not_attributed"
	JudgeExcludedNoEventID     = "no_event_id"
	// JudgeExcludedBYOKOnly: the tenant is BYOK-only and has no stored key for
	// the vendor the evaluation would have used, so the call was not made. The
	// alternative — spending the gateway's shared key on an evaluation the
	// tenant never asked for, against a policy that explicitly forbids the
	// shared key — would be a consent violation dressed up as a fallback.
	JudgeExcludedBYOKOnly = "byok_only_no_key"
)

// EvalCredentialSourceTotal records which key an evaluation call billed, by
// path and source (tenant_stored / tas_shared / resolver_error).
//
// The request path stamps this on its routing sidecar, but an evaluation call
// has no sidecar — StampCredentialSource is a silent no-op there — so without
// this counter there is no way to answer "whose key paid for that judge call?".
// That question is the whole point of the BYOK half of #184: a tenant who
// brought their own key should not discover the gateway quietly used its own.
var EvalCredentialSourceTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_eval_credential_source_total",
		Help: "Key used by gateway evaluation calls, by path and source (tenant_stored/tas_shared/resolver_error).",
	},
	[]string{"path", "source"},
)

// Source label values for EvalCredentialSourceTotal.
const (
	EvalCredTenantStored = "tenant_stored"
	EvalCredTASShared    = "tas_shared"
	EvalCredResolverErr  = "resolver_error"
)

// EvalEventsTotal counts AIQG events emitted for gateway-initiated evaluation
// calls, by path (judge / shadow_replay).
//
// These events are what give evaluation spend PER-TENANT attribution, which
// the spend counters deliberately cannot carry: a tenant label there would
// make the series count track the customer count. The event path already
// carries tenant, experiment and variant for real traffic, so evaluation
// spend joins the same pipeline rather than needing a parallel one.
var EvalEventsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_eval_events_total",
		Help: "AIQG events emitted for gateway evaluation calls, by path (judge/shadow_replay).",
	},
	[]string{"path"},
)

// EvalEventsFailedTotal counts evaluation events that could not be emitted.
//
// A failure here is a silent attribution loss, not a lost request: the call
// still happened and still billed, the spend counters still moved, but no
// per-tenant row exists for it. Without this counter the two views would drift
// apart with nothing indicating why — the global total would stay right while
// the per-tenant sum quietly ran short.
var EvalEventsFailedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_eval_events_failed_total",
		Help: "Evaluation events that failed to emit, by path — each one is unattributed spend.",
	},
	[]string{"path"},
)

// EmitterDegraded is 1 when the configured Kafka event emitter could not be
// built at startup and the gateway degraded to the log emitter, 0 otherwise.
// A Gauge exports 0 from process start (no seeding needed), so a healthy gateway
// reads 0 and the degraded state is alertable. Serving continues either way —
// a Kafka outage costs telemetry, not availability (#176).
var EmitterDegraded = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "aiqg_emitter_degraded",
		Help: "1 when the Kafka event emitter fell back to the log emitter at startup, else 0.",
	},
)

// PromptCacheRequestsTotal counts requests by the prompt-cache mode that was
// APPLIED (passthrough / off / auto / none) and vendor. It is the denominator
// the zero-hit alert needs: "auto-mode requests are happening but read tokens
// stay flat at zero" is the silent failure prompt caching exists to end, and it
// is only expressible with a per-mode request count alongside the read-token
// counter. Synthetic traffic is excluded at the call site.
var PromptCacheRequestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_prompt_cache_requests_total",
		Help: "Requests by applied prompt-cache mode and vendor (synthetic excluded).",
	},
	[]string{"vendor", "mode"},
)

// PromptCacheReadTokensTotal sums vendor-reported cache-READ (hit) tokens.
// Non-zero means a breakpoint the gateway forwarded actually paid off.
var PromptCacheReadTokensTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_prompt_cache_read_tokens_total",
		Help: "Vendor-reported prompt-cache read (hit) tokens, by vendor.",
	},
	[]string{"vendor"},
)

// PromptCacheCreationTokensTotal sums cache-WRITE (creation) tokens — the 1.25×
// cost of warming a cache. Watched next to reads: creations with no subsequent
// reads is money spent warming a cache nothing hit (e.g. model switching mid
// conversation cold-rebuilds every turn).
var PromptCacheCreationTokensTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_prompt_cache_creation_tokens_total",
		Help: "Vendor-reported prompt-cache creation (write) tokens, by vendor.",
	},
	[]string{"vendor"},
)

// PromptCacheSavingsUSDTotal is the dollar spend AVOIDED by cache reads: the
// same read tokens at full input price would have cost read_cost/mult, of which
// read_cost was actually paid, so the saving is the remainder. Computed from the
// event's own cache-read cost and clear.CacheReadMultiplier so the metric and
// the billing figure cannot disagree.
var PromptCacheSavingsUSDTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aiqg_prompt_cache_savings_usd_total",
		Help: "Estimated USD avoided by prompt-cache reads (vs full input price), by vendor.",
	},
	[]string{"vendor"},
)

func init() {
	Registry.MustRegister(
		EventsEmittedTotal,
		EmitDurationSeconds,
		ScanFindingsTotal,
		RequestTierTotal,
		RequestsTotal,
		ShadowReplaysTotal,
		ShadowTokensTotal,
		ShadowTruncatedTotal,
		JudgeCallsTotal,
		JudgeTokensTotal,
		UnbilledSpendUSDTotal,
		UnpricedCallsTotal,
		JudgeExcludedTotal,
		EvalEventsTotal,
		EvalEventsFailedTotal,
		EvalCredentialSourceTotal,
		EmitterDegraded,
		PromptCacheRequestsTotal,
		PromptCacheReadTokensTotal,
		PromptCacheCreationTokensTotal,
		PromptCacheSavingsUSDTotal,
	)
	seed()
}

// seed pre-initializes the scan-findings series to zero for every
// direction × severity combination. A CounterVec child is exported only once
// incremented, so a freshly restarted pod would otherwise serve a scrape with
// aiqg_scan_findings_total ABSENT — and a blank panel reads as "scanning found
// nothing" when it means "scanning has recorded nothing since pod start" (#175).
// Seeding makes the honest zero visible from the first scrape.
func seed() {
	for _, direction := range []string{"inbound", "outbound"} {
		for _, severity := range []string{"low", "medium", "high", "critical"} {
			ScanFindingsTotal.WithLabelValues(direction, severity).Add(0)
		}
	}
}

// Tier label values — kept as constants so call sites can't typo them.
const (
	TierHealthy  = "healthy"
	TierMarginal = "marginal"
	TierFailing  = "failing"
)

// Outcome label values for EventsEmittedTotal.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
)

// TierFor returns the standard-dimension tier for a 0-100 score
// (Cost / Latency / Efficacy / Reliability). Healthy ≥75 / Marginal
// 50-74 / Failing <50.
func TierFor(score int16) string {
	switch {
	case score >= 75:
		return TierHealthy
	case score >= 50:
		return TierMarginal
	default:
		return TierFailing
	}
}

// TierForAssurance returns the stricter Assurance tier per source-spec
// §2.2.3 (Healthy ≥90 / Marginal 75-89 / Failing <75).
func TierForAssurance(score int16) string {
	switch {
	case score >= 90:
		return TierHealthy
	case score >= 75:
		return TierMarginal
	default:
		return TierFailing
	}
}
