package middleware

import (
	"context"
	"testing"
	"time"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
)

func TestReductionHolderGetWaitImmediate(t *testing.T) {
	h := &reductionMeasurementHolder{}
	// Not pending → returns immediately (nil here).
	start := time.Now()
	if m := h.getWait(time.Second); m != nil {
		t.Errorf("expected nil, got %+v", m)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Error("getWait blocked when no measurement was expected")
	}
}

func TestReductionHolderExpectThenSet(t *testing.T) {
	h := &reductionMeasurementHolder{}
	h.expect()
	want := &events.ReductionMeasurement{Mode: "shadow", Sampled: true, OriginalBytes: 100, ExtractedBytes: 40}
	go func() {
		time.Sleep(20 * time.Millisecond)
		h.set(want)
	}()
	got := h.getWait(time.Second)
	if got != want {
		t.Fatalf("getWait did not return the stamped measurement: %+v", got)
	}
}

func TestReductionHolderWaitTimesOut(t *testing.T) {
	h := &reductionMeasurementHolder{}
	h.expect()
	// Never set → getWait returns nil after the bound, doesn't hang.
	start := time.Now()
	if m := h.getWait(50 * time.Millisecond); m != nil {
		t.Errorf("expected nil on timeout, got %+v", m)
	}
	if d := time.Since(start); d < 40*time.Millisecond || d > 500*time.Millisecond {
		t.Errorf("getWait timeout took %v, want ~50ms", d)
	}
}

func TestExpectAndStampViaContext(t *testing.T) {
	h := &reductionMeasurementHolder{}
	ctx := withReductionMeasurement(context.Background(), h)
	ExpectReductionMeasurement(ctx)
	want := &events.ReductionMeasurement{Mode: "shadow", Sampled: true, OriginalBytes: 10}
	StampReductionMeasurement(ctx, want)
	if got := h.getWait(time.Second); got != want {
		t.Fatalf("context stamp/get mismatch: %+v", got)
	}
}

func TestStampNilStopsWaiting(t *testing.T) {
	h := &reductionMeasurementHolder{}
	h.expect()
	go func() {
		time.Sleep(10 * time.Millisecond)
		h.set(nil) // "ran but empty" — must unblock getWait
	}()
	start := time.Now()
	if m := h.getWait(time.Second); m != nil {
		t.Errorf("expected nil, got %+v", m)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Error("set(nil) did not unblock getWait promptly")
	}
}
