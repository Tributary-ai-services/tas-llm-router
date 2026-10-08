package middleware

import (
	"context"
	"testing"
)

func TestStampOutcome_ReachesTheSnapshot(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)
	if s := r.Snapshot(); s.Outcome != "" {
		t.Fatalf("Outcome set before stamping: %q", s.Outcome)
	}
	StampOutcome(ctx, "vendor_error")
	if s := r.Snapshot(); s.Outcome != "vendor_error" {
		t.Errorf("Outcome = %q, want vendor_error", s.Outcome)
	}
}

// First terminal outcome wins -- it is the one that ended the request.
func TestStampOutcome_FirstWins(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)
	StampOutcome(ctx, "vendor_error")
	StampOutcome(ctx, "timeout")
	if s := r.Snapshot(); s.Outcome != "vendor_error" {
		t.Errorf("Outcome = %q, want vendor_error", s.Outcome)
	}
}

// An empty status is not a stamp: it must not latch and block a real one.
func TestStampOutcome_EmptyIsNotAStamp(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)
	StampOutcome(ctx, "")
	StampOutcome(ctx, "vendor_error")
	if s := r.Snapshot(); s.Outcome != "vendor_error" {
		t.Errorf("an empty stamp blocked a real one: %q", s.Outcome)
	}
}

func TestStampOutcome_NoRoutingIsSafe(t *testing.T) {
	StampOutcome(context.Background(), "vendor_error")
}
