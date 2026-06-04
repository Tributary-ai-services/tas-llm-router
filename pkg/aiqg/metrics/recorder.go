package metrics

// Recording helpers used by emitters + the path-A middleware to
// avoid importing prometheus types into every call site. Keeps the
// instrumentation surface to a few well-typed function calls.

import (
	"time"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
)

// ObserveEmit records a single Emitter.Emit call's duration and
// outcome. Call with `defer metrics.ObserveEmit("log", time.Now(), &err)`
// from within Emit so the deferred read of `err` captures the eventual
// return value.
func ObserveEmit(emitter string, start time.Time, errPtr *error) {
	EmitDurationSeconds.WithLabelValues(emitter).Observe(time.Since(start).Seconds())
	outcome := OutcomeSuccess
	if errPtr != nil && *errPtr != nil {
		outcome = OutcomeFailure
	}
	EventsEmittedTotal.WithLabelValues(emitter, outcome).Inc()
}

// RecordEvent extracts the per-request metrics from a response
// envelope and pushes them onto the registry. Called from the emitter
// wrappers so every emitted event contributes regardless of which
// Emitter implementation handled it.
//
// Counts that bump:
//   - aiqg_requests_total (always)
//   - aiqg_request_tier_total{dimension, tier} for each non-nil
//     CLEAR dimension
//   - aiqg_scan_findings_total{direction, severity} for each non-zero
//     severity-count in the AssuranceSummary
func RecordEvent(resp events.ResponseEnvelope) {
	RequestsTotal.Inc()

	if c := resp.Data.CLEAR; c != nil {
		if c.Cost != nil {
			RequestTierTotal.WithLabelValues("cost", TierFor(*c.Cost)).Inc()
		}
		if c.Latency != nil {
			RequestTierTotal.WithLabelValues("latency", TierFor(*c.Latency)).Inc()
		}
		if c.Efficacy != nil {
			RequestTierTotal.WithLabelValues("efficacy", TierFor(*c.Efficacy)).Inc()
		}
		if c.Reliability != nil {
			RequestTierTotal.WithLabelValues("reliability", TierFor(*c.Reliability)).Inc()
		}
		if c.Assurance != nil {
			// Assurance uses stricter tier boundaries per §2.2.3.
			RequestTierTotal.WithLabelValues("assurance", TierForAssurance(*c.Assurance)).Inc()
		}
		if c.Composite != nil {
			RequestTierTotal.WithLabelValues("composite", TierFor(*c.Composite)).Inc()
		}
	}

	if a := resp.Data.Assurance; a != nil {
		for sev, count := range a.InboundFindings {
			if count > 0 {
				ScanFindingsTotal.WithLabelValues("inbound", sev).Add(float64(count))
			}
		}
		for sev, count := range a.OutboundFindings {
			if count > 0 {
				ScanFindingsTotal.WithLabelValues("outbound", sev).Add(float64(count))
			}
		}
	}
}
