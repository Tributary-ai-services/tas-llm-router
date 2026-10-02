package middleware

import (
	"context"
	"testing"
)

// The tri-state is the whole point of this stamp: the number it exists to
// collect is the share of traffic a body-derived Efficacy sub-metric could
// apply to, and counting "we never looked" as "the caller declared nothing"
// would bias that share downward.
func TestStampEfficacyApplicability_TriState(t *testing.T) {
	t.Run("unstamped reports not-set", func(t *testing.T) {
		r := NewRouting()
		s := r.Snapshot()
		if s.ApplicabilitySet {
			t.Fatalf("ApplicabilitySet = true on a fresh Routing, want false")
		}
		if s.SchemaRequested || s.ToolsDeclared {
			t.Fatalf("unstamped flags = (%v, %v), want (false, false)", s.SchemaRequested, s.ToolsDeclared)
		}
	})

	t.Run("stamped false is an observation, not an absence", func(t *testing.T) {
		r := NewRouting()
		ctx := WithRouting(context.Background(), r)
		StampEfficacyApplicability(ctx, false, false)
		s := r.Snapshot()
		if !s.ApplicabilitySet {
			t.Fatalf("ApplicabilitySet = false after stamping, want true — a stamped false must be distinguishable from never looking")
		}
		if s.SchemaRequested || s.ToolsDeclared {
			t.Fatalf("flags = (%v, %v), want (false, false)", s.SchemaRequested, s.ToolsDeclared)
		}
	})

	t.Run("flags are independent", func(t *testing.T) {
		for _, tc := range []struct{ schema, tools bool }{
			{true, false}, {false, true}, {true, true},
		} {
			r := NewRouting()
			ctx := WithRouting(context.Background(), r)
			StampEfficacyApplicability(ctx, tc.schema, tc.tools)
			s := r.Snapshot()
			if s.SchemaRequested != tc.schema || s.ToolsDeclared != tc.tools {
				t.Errorf("stamped (%v, %v) -> snapshot (%v, %v)",
					tc.schema, tc.tools, s.SchemaRequested, s.ToolsDeclared)
			}
			if !s.ApplicabilitySet {
				t.Errorf("stamped (%v, %v) left ApplicabilitySet=false", tc.schema, tc.tools)
			}
		}
	})
}

// Deliberately NOT first-write-wins, unlike the finish_reason / prefix-hash
// stamps: the flags must describe the request shape actually served, so a later
// layer that rewrites it gets the last word.
func TestStampEfficacyApplicability_LastWriteWins(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	StampEfficacyApplicability(ctx, true, true)
	StampEfficacyApplicability(ctx, false, true)

	s := r.Snapshot()
	if s.SchemaRequested {
		t.Errorf("SchemaRequested = true, want false (last write should win so the flags match the served config)")
	}
	if !s.ToolsDeclared {
		t.Errorf("ToolsDeclared = false, want true")
	}
}

func TestStampEfficacyApplicability_NoRoutingInContextIsSafe(t *testing.T) {
	StampEfficacyApplicability(context.Background(), true, true) // must not panic
}
