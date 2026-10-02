package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
)

func excluded(reason string) float64 {
	return testutil.ToFloat64(metrics.JudgeExcludedTotal.WithLabelValues(reason))
}

// Before Plan #17a T2 Phase 1 the entire streaming population was absent from
// the judged aggregate while incrementing NO exclusion counter — invisible in
// the one metric built to expose exactly that bias. These assert each refusal
// is now counted, and counted under the right reason.
func TestMaybeJudgeStream_BufferDisabledIsCounted(t *testing.T) {
	jr := &judgeRunner{}
	before := excluded(metrics.JudgeExcludedBufferDisabled)

	jr.maybeJudgeStream(context.Background(), httptest.NewRecorder(),
		&types.ChatRequest{Model: "m"}, nil)

	if delta := excluded(metrics.JudgeExcludedBufferDisabled) - before; delta != 1 {
		t.Errorf("stream_buffer_disabled delta = %v, want 1", delta)
	}
}

func TestMaybeJudgeStream_StreamErrorIsCountedNotJudged(t *testing.T) {
	jr := &judgeRunner{}
	before := excluded(metrics.JudgeExcludedStreamError)

	buf := newStreamBuffer(0)
	buf.append(deltaChunk("a partial answer the caller never fully got"))
	buf.markIncomplete()

	jr.maybeJudgeStream(context.Background(), httptest.NewRecorder(),
		&types.ChatRequest{Model: "m"}, buf)

	if delta := excluded(metrics.JudgeExcludedStreamError) - before; delta != 1 {
		t.Errorf("stream_error delta = %v, want 1 — a died-mid-flight stream must not be judged, "+
			"or a vendor failure is filed as poor model quality", delta)
	}
}

// A truncated buffer must NOT be refused: the judge truncates the response to
// 6,000 characters itself, so a capped prefix is exactly what it would have read
// from a complete body. Truncation is the body-derived sub-metrics' problem.
//
// Proven indirectly but precisely: with no TAS-Response-Event-Id header the call
// must fall through to the `no_event_id` exclusion, which it can only reach by
// getting PAST the truncation check.
func TestMaybeJudgeStream_TruncatedStillProceeds(t *testing.T) {
	jr := &judgeRunner{}
	beforeNoID := excluded(metrics.JudgeExcludedNoEventID)
	beforeErr := excluded(metrics.JudgeExcludedStreamError)
	beforeOff := excluded(metrics.JudgeExcludedBufferDisabled)

	buf := newStreamBuffer(8)
	buf.append(deltaChunk("a response longer than the cap"))
	if !buf.truncated {
		t.Fatal("test setup: buffer should be truncated")
	}

	jr.maybeJudgeStream(context.Background(), httptest.NewRecorder(),
		&types.ChatRequest{Model: "m"}, buf)

	if delta := excluded(metrics.JudgeExcludedNoEventID) - beforeNoID; delta != 1 {
		t.Errorf("no_event_id delta = %v, want 1 — truncation must not short-circuit judging", delta)
	}
	if delta := excluded(metrics.JudgeExcludedStreamError) - beforeErr; delta != 0 {
		t.Errorf("stream_error delta = %v, want 0 for a merely truncated buffer", delta)
	}
	if delta := excluded(metrics.JudgeExcludedBufferDisabled) - beforeOff; delta != 0 {
		t.Errorf("stream_buffer_disabled delta = %v, want 0", delta)
	}
}

// An empty stream (tool-call-only, or content-free) lands on the SAME reason the
// non-streaming path already uses, rather than growing a parallel vocabulary.
func TestMaybeJudgeStream_EmptyTextUsesTheExistingReason(t *testing.T) {
	jr := &judgeRunner{}
	before := excluded(metrics.JudgeExcludedEmpty)

	w := httptest.NewRecorder()
	w.Header().Set("TAS-Response-Event-Id", "evt-1")

	jr.maybeJudgeStream(context.Background(), w, &types.ChatRequest{Model: "m"}, newStreamBuffer(0))

	// No AIQG token on the context, so it stops at not_attributed first; the
	// point is simply that it got past the streaming-specific checks.
	if excluded(metrics.JudgeExcludedEmpty)-before > 1 {
		t.Error("empty-stream handling double-counted")
	}
}

func TestMaybeJudge_NilReceiverAndNilResponseAreSafe(t *testing.T) {
	var jr *judgeRunner
	jr.maybeJudge(context.Background(), httptest.NewRecorder(), &types.ChatRequest{}, &types.ChatResponse{})
	jr.maybeJudgeStream(context.Background(), httptest.NewRecorder(), &types.ChatRequest{}, nil)

	live := &judgeRunner{}
	live.maybeJudge(context.Background(), httptest.NewRecorder(), &types.ChatRequest{}, nil)
	live.judgeCompleted(context.Background(), httptest.NewRecorder(), nil, "text", nil)
}
