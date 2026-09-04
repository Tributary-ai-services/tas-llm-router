package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
	"github.com/tributary-ai/llm-router-waf/pkg/clear"
)

func TestTierFor_Boundaries(t *testing.T) {
	cases := []struct {
		score int16
		want  string
	}{
		{100, TierHealthy},
		{75, TierHealthy},
		{74, TierMarginal},
		{50, TierMarginal},
		{49, TierFailing},
		{0, TierFailing},
	}
	for _, c := range cases {
		if got := TierFor(c.score); got != c.want {
			t.Errorf("TierFor(%d)=%s want=%s", c.score, got, c.want)
		}
	}
}

func TestTierForAssurance_Boundaries(t *testing.T) {
	cases := []struct {
		score int16
		want  string
	}{
		{100, TierHealthy},
		{90, TierHealthy},
		{89, TierMarginal},
		{75, TierMarginal},
		{74, TierFailing},
		{0, TierFailing},
	}
	for _, c := range cases {
		if got := TierForAssurance(c.score); got != c.want {
			t.Errorf("TierForAssurance(%d)=%s want=%s", c.score, got, c.want)
		}
	}
}

func TestObserveEmit_Success(t *testing.T) {
	before := testutil.ToFloat64(EventsEmittedTotal.WithLabelValues("test_emitter", OutcomeSuccess))
	var nilErr error
	ObserveEmit("test_emitter", time.Now().Add(-50*time.Millisecond), &nilErr)
	after := testutil.ToFloat64(EventsEmittedTotal.WithLabelValues("test_emitter", OutcomeSuccess))
	if after != before+1 {
		t.Errorf("success counter did not increment: before=%v after=%v", before, after)
	}
}

func TestObserveEmit_Failure(t *testing.T) {
	before := testutil.ToFloat64(EventsEmittedTotal.WithLabelValues("test_fail_emitter", OutcomeFailure))
	failErr := errors.New("boom")
	ObserveEmit("test_fail_emitter", time.Now(), &failErr)
	after := testutil.ToFloat64(EventsEmittedTotal.WithLabelValues("test_fail_emitter", OutcomeFailure))
	if after != before+1 {
		t.Errorf("failure counter did not increment: before=%v after=%v", before, after)
	}
}

func TestRecordEvent_FullCLEAR(t *testing.T) {
	cost, latency, efficacy := int16(80), int16(95), int16(60)
	assurance, reliability, composite := int16(50), int16(100), int16(78)

	beforeReq := testutil.ToFloat64(RequestsTotal)
	beforeCostHealthy := testutil.ToFloat64(RequestTierTotal.WithLabelValues("cost", TierHealthy))
	beforeEfficacyMarginal := testutil.ToFloat64(RequestTierTotal.WithLabelValues("efficacy", TierMarginal))
	beforeAssuranceFailing := testutil.ToFloat64(RequestTierTotal.WithLabelValues("assurance", TierFailing))

	RecordEvent(events.ResponseEnvelope{Data: events.ResponseEvent{
		CLEAR: &clear.Scores{
			Cost:        &cost,
			Latency:     &latency,
			Efficacy:    &efficacy,
			Assurance:   &assurance,
			Reliability: &reliability,
			Composite:   &composite,
		},
	}})

	if got := testutil.ToFloat64(RequestsTotal); got != beforeReq+1 {
		t.Errorf("RequestsTotal not incremented")
	}
	if got := testutil.ToFloat64(RequestTierTotal.WithLabelValues("cost", TierHealthy)); got != beforeCostHealthy+1 {
		t.Errorf("cost healthy bucket: before=%v after=%v", beforeCostHealthy, got)
	}
	// efficacy=60 → Marginal (≥50 <75)
	if got := testutil.ToFloat64(RequestTierTotal.WithLabelValues("efficacy", TierMarginal)); got != beforeEfficacyMarginal+1 {
		t.Errorf("efficacy marginal bucket: before=%v after=%v", beforeEfficacyMarginal, got)
	}
	// assurance=50 → Failing (stricter tier; needs ≥75 for Marginal)
	if got := testutil.ToFloat64(RequestTierTotal.WithLabelValues("assurance", TierFailing)); got != beforeAssuranceFailing+1 {
		t.Errorf("assurance failing bucket: before=%v after=%v", beforeAssuranceFailing, got)
	}
}

func TestRecordEvent_AssuranceFindings(t *testing.T) {
	beforeInLow := testutil.ToFloat64(ScanFindingsTotal.WithLabelValues("inbound", "low"))
	beforeOutCrit := testutil.ToFloat64(ScanFindingsTotal.WithLabelValues("outbound", "critical"))

	RecordEvent(events.ResponseEnvelope{Data: events.ResponseEvent{
		Assurance: &events.AssuranceSummary{
			InboundFindings:  map[string]int{"low": 3, "medium": 2},
			OutboundFindings: map[string]int{"critical": 1},
		},
	}})

	if got := testutil.ToFloat64(ScanFindingsTotal.WithLabelValues("inbound", "low")); got != beforeInLow+3 {
		t.Errorf("inbound low: before=%v after=%v want+3", beforeInLow, got)
	}
	if got := testutil.ToFloat64(ScanFindingsTotal.WithLabelValues("outbound", "critical")); got != beforeOutCrit+1 {
		t.Errorf("outbound critical: before=%v after=%v want+1", beforeOutCrit, got)
	}
}

// Nil CLEAR / Assurance must not panic.
func TestRecordEvent_NilSafety(t *testing.T) {
	RecordEvent(events.ResponseEnvelope{Data: events.ResponseEvent{}})
	// Should bump RequestsTotal but nothing else.
}

// #175: scan-finding series must exist (value 0) for every direction × severity
// from pod start, so a blank panel reads "zero findings", not "not recorded".
func TestScanFindingsSeededZeroSeries(t *testing.T) {
	if n := testutil.CollectAndCount(ScanFindingsTotal); n < 8 {
		t.Errorf("aiqg_scan_findings_total series=%d, want >=8 (2 directions x 4 severities seeded)", n)
	}
}
