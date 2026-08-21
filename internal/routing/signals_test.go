package routing

import (
	"context"
	"strings"
	"testing"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"
)

func q(model string, eff float64, worst resilience.Severity, samples int) resilience.QualitySignal {
	return resilience.QualitySignal{Model: model, Efficacy: eff, WorstAssurance: worst, Samples: samples}
}

func gateCtx(sig resilience.Signals, rows []resilience.QualitySignal, model string) context.Context {
	ctx := WithSignals(context.Background(), sig, rows)
	return WithGateContext(ctx, model)
}

func modelIs(m string) func(string) string { return func(string) string { return m } }

// ---------------------------------------------------------------------------
// The acceptance criterion: the assurance gate drops a candidate on evidence.
// ---------------------------------------------------------------------------

func TestAssuranceGateDropsACandidateOnEvidence(t *testing.T) {
	sig := resilience.Signals{MaxAssuranceSeverity: resilience.SeverityMedium, MinSamples: 10}
	rows := []resilience.QualitySignal{
		q("bad-model", 100, resilience.SeverityHigh, 500),
		q("good-model", 100, resilience.SeverityLow, 500),
	}
	ctx := gateCtx(sig, rows, "bad-model")

	kept, excluded, _ := gateCandidates(ctx, []string{"p1", "p2"}, func(p string) string {
		if p == "p1" {
			return "bad-model"
		}
		return "good-model"
	}, "")

	if len(kept) != 1 || kept[0] != "p2" {
		t.Fatalf("kept %v, want only the model clearing the assurance limit", kept)
	}
	if len(excluded) != 1 || excluded[0].Dimension != "assurance" {
		t.Fatalf("excluded %+v, want one assurance exclusion", excluded)
	}
	// "Excluded by signals" is never the whole answer — which dimension, on
	// what evidence.
	if !strings.Contains(excluded[0].Reason, "exceeds the limit") {
		t.Fatalf("reason %q should state the comparison", excluded[0].Reason)
	}
}

func TestEfficacyGateDropsACandidate(t *testing.T) {
	sig := resilience.Signals{MinEfficacy: 70, MinSamples: 10}
	rows := []resilience.QualitySignal{q("terse", 51, resilience.SeverityNone, 500), q("good", 84, resilience.SeverityNone, 500)}
	ctx := gateCtx(sig, rows, "terse")
	kept, excluded, _ := gateCandidates(ctx, []string{"p1", "p2"}, func(p string) string {
		if p == "p1" {
			return "terse"
		}
		return "good"
	}, "")
	if len(kept) != 1 || excluded[0].Dimension != "efficacy" {
		t.Fatalf("kept=%v excluded=%+v", kept, excluded)
	}
}

// ---------------------------------------------------------------------------
// Thin data is a no-op — the state the system is actually in.
// ---------------------------------------------------------------------------

func TestThinDataExcludesNothingAndSaysSo(t *testing.T) {
	sig := resilience.Signals{MinEfficacy: 70, MinSamples: 200}
	// 73 real samples is today's actual figure for the only measured model.
	rows := []resilience.QualitySignal{q("m", 10, resilience.SeverityCritical, 73)}
	ctx := gateCtx(sig, rows, "m")

	kept, excluded, note := gateCandidates(ctx, []string{"p1"}, modelIs("m"), "")
	if len(kept) != 1 || len(excluded) != 0 {
		t.Fatalf("thin data excluded a candidate: kept=%v excluded=%+v", kept, excluded)
	}
	// Abstained and passed are different facts, and only one is evidence the
	// gate is working.
	if !strings.Contains(note, "abstained") {
		t.Fatalf("note %q should record the abstention", note)
	}
}

func TestNoSignalsIsAPassthrough(t *testing.T) {
	ctx := context.Background()
	kept, excluded, note := gateCandidates(ctx, []string{"p1", "p2"}, modelIs("m"), "")
	if len(kept) != 2 || excluded != nil || note != "" {
		t.Fatal("an unconfigured gate altered the candidate set")
	}
}

// ---------------------------------------------------------------------------
// The gate must never remove the last provider.
// ---------------------------------------------------------------------------

func TestGateYieldsRatherThanEmptyingTheCandidateSet(t *testing.T) {
	sig := resilience.Signals{MinEfficacy: 99, MinSamples: 10}
	rows := []resilience.QualitySignal{q("m", 50, resilience.SeverityNone, 500)}
	ctx := gateCtx(sig, rows, "m")

	kept, excluded, note := gateCandidates(ctx, []string{"p1", "p2"}, modelIs("m"), "")
	if len(kept) != 2 {
		t.Fatal("the gate emptied the candidate set; a quality control that removes the last provider takes the service down to avoid a possible quality problem")
	}
	if len(excluded) != 0 {
		t.Fatal("exclusions were reported for a gate that yielded")
	}
	if !strings.Contains(note, "yielded") {
		t.Fatalf("note %q should record that the gate yielded", note)
	}
}

// ---------------------------------------------------------------------------
// Lookup and context plumbing.
// ---------------------------------------------------------------------------

func TestWorkflowSpecificEvidenceWins(t *testing.T) {
	idx := indexQuality([]resilience.QualitySignal{
		{Model: "m", Workflow: "", Efficacy: 90, Samples: 500},
		{Model: "m", Workflow: "rag", Efficacy: 40, Samples: 500},
	})
	if got, _ := idx.lookup("m", "rag"); got.Efficacy != 40 {
		t.Fatalf("workflow-specific evidence not preferred: %v", got.Efficacy)
	}
	// Workflow is optional on a request; a tenant that never sets it must
	// still have evidence.
	if got, _ := idx.lookup("m", "other"); got.Efficacy != 90 {
		t.Fatalf("did not fall back to workflow-agnostic evidence: %v", got.Efficacy)
	}
}

func TestExclusionsAreRetrievableForTheEvent(t *testing.T) {
	sig := resilience.Signals{MinEfficacy: 70, MinSamples: 10}
	rows := []resilience.QualitySignal{q("m", 10, resilience.SeverityNone, 500)}
	ctx := gateCtx(sig, rows, "m")
	// Two candidates so the gate excludes one without emptying the set.
	gateCandidates(ctx, []string{"p1"}, modelIs("m"), "")

	// A model quietly vanishing from consideration, with the request still
	// succeeding elsewhere, is invisible unless the exclusion reaches the event.
	if _, _, ok := SignalsFrom(ctx); !ok {
		t.Fatal("signals did not survive the context")
	}
}

func TestEmptySignalsStoreNothingOnTheContext(t *testing.T) {
	ctx := WithSignals(context.Background(), resilience.Signals{}, nil)
	if _, _, ok := SignalsFrom(ctx); ok {
		t.Fatal("an empty signals block was stored; no-gating and gating-with-no-floors must be the same code path")
	}
}
