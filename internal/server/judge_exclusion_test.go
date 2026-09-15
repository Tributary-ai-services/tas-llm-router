package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
)

// The judged population is not a random sample of served traffic (#184). These
// tests pin the two exclusion paths that are reachable without minting an AIQG
// token, so a future refactor cannot quietly stop counting them.
//
// The third and most consequential reason, blocked_outbound, is incremented in
// the HTTP handler at the outbound ShouldBlock site rather than here — it needs
// a configured Gatekeeper and a full request round-trip, so it is deliberately
// NOT asserted in this file rather than given the appearance of coverage.

func TestMaybeJudge_CountsMissingEventID(t *testing.T) {
	before := testutil.ToFloat64(metrics.JudgeExcludedTotal.WithLabelValues(metrics.JudgeExcludedNoEventID))

	jr := &judgeRunner{log: logrus.New()}
	w := httptest.NewRecorder() // no TAS-Response-Event-Id header
	jr.maybeJudge(context.Background(), w, &types.ChatRequest{}, &types.ChatResponse{})

	if got := testutil.ToFloat64(metrics.JudgeExcludedTotal.WithLabelValues(metrics.JudgeExcludedNoEventID)); got != before+1 {
		t.Errorf("no_event_id: got delta %v, want 1", got-before)
	}
}

func TestMaybeJudge_CountsUnattributedTraffic(t *testing.T) {
	before := testutil.ToFloat64(metrics.JudgeExcludedTotal.WithLabelValues(metrics.JudgeExcludedNotAttributed))

	jr := &judgeRunner{log: logrus.New()}
	w := httptest.NewRecorder()
	w.Header().Set("TAS-Response-Event-Id", "evt-1") // emitted, but no AIQG token on the context
	jr.maybeJudge(context.Background(), w, &types.ChatRequest{}, &types.ChatResponse{})

	if got := testutil.ToFloat64(metrics.JudgeExcludedTotal.WithLabelValues(metrics.JudgeExcludedNotAttributed)); got != before+1 {
		t.Errorf("not_attributed: got delta %v, want 1", got-before)
	}
}

// A nil runner means judging is switched off entirely — that is a configuration
// state, not an exclusion from a population that is being measured, so it must
// not inflate the exclusion counters.
func TestMaybeJudge_NilRunnerCountsNothing(t *testing.T) {
	var jr *judgeRunner
	reasons := []string{
		metrics.JudgeExcludedNoEventID,
		metrics.JudgeExcludedNotAttributed,
		metrics.JudgeExcludedEmpty,
		metrics.JudgeExcludedBlocked,
	}
	before := make([]float64, len(reasons))
	for i, r := range reasons {
		before[i] = testutil.ToFloat64(metrics.JudgeExcludedTotal.WithLabelValues(r))
	}

	jr.maybeJudge(context.Background(), httptest.NewRecorder(), &types.ChatRequest{}, &types.ChatResponse{})

	for i, r := range reasons {
		if got := testutil.ToFloat64(metrics.JudgeExcludedTotal.WithLabelValues(r)); got != before[i] {
			t.Errorf("%s moved on a disabled judge: delta %v", r, got-before[i])
		}
	}
}
