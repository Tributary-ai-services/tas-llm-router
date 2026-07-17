package middleware

import (
	"context"
	"testing"
)

func TestStampRedaction_FirstWriteWinsAndNonPositiveNoop(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	if got := r.Snapshot().RedactionCount; got != 0 {
		t.Fatalf("unstamped RedactionCount = %d, want 0", got)
	}

	// Non-positive is a no-op.
	StampRedaction(ctx, 0)
	StampRedaction(ctx, -3)
	if got := r.Snapshot().RedactionCount; got != 0 {
		t.Fatalf("non-positive stamp set RedactionCount = %d, want 0", got)
	}

	// First positive write wins.
	StampRedaction(ctx, 4)
	StampRedaction(ctx, 9)
	if got := r.Snapshot().RedactionCount; got != 4 {
		t.Fatalf("RedactionCount = %d, want 4 (first-write-wins)", got)
	}
}

func TestStampRedaction_NoRoutingInContextIsSafe(t *testing.T) {
	StampRedaction(context.Background(), 2) // must not panic
}
