package experiments

import (
	"context"
	"testing"
	"time"
)

// A shadow replay bills the tenant for an evaluation no request of theirs asked
// for, so shadow_eval_pct is a consent declaration rather than a tuning knob
// (tas-llm-router#184). These pin what ShadowEvalPlan hands the caller.
//
// fakeLoader, ptr and twoArm live in experiments_test.go — same package.

func shadowExperiment(id string, guard Guardrails) Experiment {
	return Experiment{
		ID:         id,
		TenantID:   "tenant-1",
		Status:     "running",
		Guardrails: guard,
		Variants: []Variant{
			{Key: "control"},
			{Key: "cheap"},
		},
	}
}

func TestShadowEvalPlan_ReturnsVariantsAndDeclaredRate(t *testing.T) {
	e := shadowExperiment("exp-1", Guardrails{ShadowEvalPct: ptr(25)})
	r := NewResolver(&fakeLoader{exps: []Experiment{e}}, time.Minute)

	variants, pct := r.ShadowEvalPlan(context.Background(), "tenant-1", "exp-1")

	if len(variants) != 1 || variants[0].Key != "cheap" {
		t.Fatalf("want only the non-control arm, got %+v", variants)
	}
	if pct == nil {
		t.Fatal("declared shadow_eval_pct was dropped")
	}
	if *pct != 25 {
		t.Errorf("shadow_eval_pct = %d, want 25", *pct)
	}
}

// Undeclared must be nil, not 0 — the caller distinguishes "inherit the gateway
// default" from "explicitly declined", and collapsing them would silently
// disable shadow-eval for every experiment that simply didn't mention it.
func TestShadowEvalPlan_UndeclaredIsNilNotZero(t *testing.T) {
	e := shadowExperiment("exp-1", Guardrails{})
	r := NewResolver(&fakeLoader{exps: []Experiment{e}}, time.Minute)

	variants, pct := r.ShadowEvalPlan(context.Background(), "tenant-1", "exp-1")
	if len(variants) != 1 {
		t.Fatalf("variants = %+v", variants)
	}
	if pct != nil {
		t.Errorf("undeclared guardrail should be nil, got %d", *pct)
	}
}

// An explicit zero is a refusal and must survive as one.
func TestShadowEvalPlan_DeclaredZeroIsPreserved(t *testing.T) {
	e := shadowExperiment("exp-1", Guardrails{ShadowEvalPct: ptr(0)})
	r := NewResolver(&fakeLoader{exps: []Experiment{e}}, time.Minute)

	_, pct := r.ShadowEvalPlan(context.Background(), "tenant-1", "exp-1")
	if pct == nil {
		t.Fatal("an explicit 0 must not be indistinguishable from undeclared")
	}
	if *pct != 0 {
		t.Errorf("shadow_eval_pct = %d, want 0", *pct)
	}
}

func TestShadowEvalPlan_UnknownExperimentAndNilResolver(t *testing.T) {
	e := shadowExperiment("exp-1", Guardrails{ShadowEvalPct: ptr(10)})
	r := NewResolver(&fakeLoader{exps: []Experiment{e}}, time.Minute)

	if v, pct := r.ShadowEvalPlan(context.Background(), "tenant-1", "no-such-exp"); v != nil || pct != nil {
		t.Errorf("unknown experiment should yield nil/nil, got %+v/%v", v, pct)
	}

	var nilResolver *Resolver
	if v, pct := nilResolver.ShadowEvalPlan(context.Background(), "tenant-1", "exp-1"); v != nil || pct != nil {
		t.Errorf("nil resolver should yield nil/nil, got %+v/%v", v, pct)
	}
}

// The returned pointer must be a copy. The cache entry it came from is read
// concurrently by other requests, so handing out an interior pointer would let
// one caller mutate another's consent rate.
func TestShadowEvalPlan_GuardrailPointerIsCloned(t *testing.T) {
	e := shadowExperiment("exp-1", Guardrails{ShadowEvalPct: ptr(25)})
	r := NewResolver(&fakeLoader{exps: []Experiment{e}}, time.Minute)
	ctx := context.Background()

	_, first := r.ShadowEvalPlan(ctx, "tenant-1", "exp-1")
	if first == nil {
		t.Fatal("no guardrail returned")
	}
	*first = 99 // a caller scribbling on what it was handed

	_, second := r.ShadowEvalPlan(ctx, "tenant-1", "exp-1")
	if second == nil {
		t.Fatal("no guardrail on the second read")
	}
	if *second != 25 {
		t.Errorf("cached guardrail was mutated through the returned pointer: got %d, want 25", *second)
	}
}
