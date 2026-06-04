// Package metrics defines the Prometheus metrics surface for the AIQG
// pipeline. Lives in pkg/aiqg/* so it sits alongside events, clear,
// tokens — the rest of the AIQG-specific Go code.
//
// Why a dedicated registry instead of prometheus.DefaultRegisterer:
// the existing internal/server.handleMetrics is hand-rolled and writes
// plain-text metrics directly; mixing client_golang's automatic
// gathering with that hand-rolled output is awkward. Instead we expose
// these on a separate /aiqg/metrics endpoint via promhttp, scraped
// independently in the Prometheus config. Migration of the legacy
// handler to client_golang is deferred to its own slice.
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

func init() {
	Registry.MustRegister(
		EventsEmittedTotal,
		EmitDurationSeconds,
		ScanFindingsTotal,
		RequestTierTotal,
		RequestsTotal,
	)
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
