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
