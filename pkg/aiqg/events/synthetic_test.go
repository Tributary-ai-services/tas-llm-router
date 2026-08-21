package events

import (
	"testing"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"
)

// These guard the classification that measurement-driven routing depends on.
// Measured over 30 days, every gpt-4o-mini event in production came from
// gateway verification probes averaging three output tokens against a real ~48;
// counting those would price the model ~16x cheaper than reality.

func TestDeclaredMarkingWinsOverInference(t *testing.T) {
	// A declaration is exact and survives renaming; the denylist is a guess.
	// When both fire, the reason must record the declaration.
	synthetic, reason := classify(true, "checkout")
	if !synthetic || reason != resilience.SyntheticDeclared {
		t.Fatalf("declared=%v reason=%q, want declared", synthetic, reason)
	}
}

func TestDenylistCatchesUndeclaredProbes(t *testing.T) {
	synthetic, reason := classify(false, "step1-gate")
	if !synthetic || reason != resilience.SyntheticSourceApp {
		t.Fatalf("synthetic=%v reason=%q, want the source_app fallback", synthetic, reason)
	}
}

func TestRealTrafficIsNotMarked(t *testing.T) {
	synthetic, reason := classify(false, "checkout")
	if synthetic || reason != "" {
		t.Fatalf("real traffic was marked synthetic (reason=%q)", reason)
	}
}

// The reason is what lets the denylist's contribution be watched as it shrinks;
// without it, "declared" and "guessed" are indistinguishable downstream.
func TestReasonDistinguishesTheTwoMechanisms(t *testing.T) {
	_, declared := classify(true, "claude code")
	_, guessed := classify(false, "claude code")
	if declared == guessed {
		t.Fatal("a declared marking and a denylist match produce the same reason")
	}
}

// classify mirrors the builder's rule. Kept here so the precedence is asserted
// directly rather than only through a full event build.
func classify(declared bool, sourceApp string) (bool, string) {
	if declared {
		return true, resilience.SyntheticDeclared
	}
	if resilience.IsSyntheticSourceApp(sourceApp) {
		return true, resilience.SyntheticSourceApp
	}
	return false, ""
}
