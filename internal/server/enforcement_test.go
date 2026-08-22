package server

import (
	"context"
	"testing"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"

	"github.com/Tributary-ai-services/Gatekeeper/pkg/scan"
	"github.com/tributary-ai/llm-router-waf/internal/middleware"
)

func findings(patterns ...string) []scan.Finding {
	out := make([]scan.Finding, len(patterns))
	for i, p := range patterns {
		out[i] = scan.Finding{PatternID: p, Severity: scan.SeverityCritical}
	}
	return out
}

// ---------------------------------------------------------------------------
// The rule the operator wrote is the authority — severity informs it, and does
// not override it. This is exactly the hardcoded behaviour step 8 replaces.
// ---------------------------------------------------------------------------

func TestACriticalFindingDoesNotBlockWhenTheRuleOnlyLogs(t *testing.T) {
	ctx := middleware.WithResolvedEnforcementForTest(context.Background(),
		resilience.ModeEnforce, map[string]string{"pii-ssn": "log"})

	// SeverityCritical, but the bundle says log. The pre-existing hardcoded
	// path would have blocked this purely on severity.
	d := decideEnforcement(ctx, findings("pii-ssn"))
	if d.Outcome != resilience.OutcomeAllowed {
		t.Fatalf("outcome = %v; a log rule must not block, whatever the severity", d.Outcome)
	}
}

func TestBlockRuleBlocks(t *testing.T) {
	ctx := middleware.WithResolvedEnforcementForTest(context.Background(),
		resilience.ModeEnforce, map[string]string{"cred-private-key": "block"})
	d := decideEnforcement(ctx, findings("cred-private-key"))
	if d.Outcome != resilience.OutcomeBlocked {
		t.Fatalf("outcome = %v, want blocked", d.Outcome)
	}
	// The event must be able to say WHICH rule acted.
	if len(d.Patterns) != 1 || d.Patterns[0] != "cred-private-key" {
		t.Fatalf("patterns = %v, want the rule that acted", d.Patterns)
	}
}

// Strongest action wins: reporting a block as a redaction would understate what
// enforcement did.
func TestBlockBeatsRedactOnTheSameRequest(t *testing.T) {
	ctx := middleware.WithResolvedEnforcementForTest(context.Background(),
		resilience.ModeEnforce, map[string]string{"pii-phone": "redact", "cred-api-key": "block"})
	d := decideEnforcement(ctx, findings("pii-phone", "cred-api-key"))
	if d.Outcome != resilience.OutcomeBlocked {
		t.Fatalf("outcome = %v, want blocked", d.Outcome)
	}
}

// A finding for a pattern the bundle does not govern changes nothing —
// otherwise one bundle would act on another's rules.
func TestUngovernedPatternIsIgnored(t *testing.T) {
	ctx := middleware.WithResolvedEnforcementForTest(context.Background(),
		resilience.ModeEnforce, map[string]string{"pii-ssn": "block"})
	if d := decideEnforcement(ctx, findings("injection-xss")); d.Outcome != resilience.OutcomeAllowed {
		t.Fatalf("outcome = %v; an ungoverned pattern must not act", d.Outcome)
	}
}

// ---------------------------------------------------------------------------
// observe records without acting — the property that makes adoption safe.
// ---------------------------------------------------------------------------

func TestObserveReachesTheSameVerdictWithoutActing(t *testing.T) {
	rules := map[string]string{"cred-private-key": "block"}
	enforce := middleware.WithResolvedEnforcementForTest(context.Background(), resilience.ModeEnforce, rules)
	observe := middleware.WithResolvedEnforcementForTest(context.Background(), resilience.ModeObserve, rules)

	// One decision function, two modes — which is what makes the dry run's
	// numbers and enforcement's behaviour provably the same logic.
	e := decideEnforcement(enforce, findings("cred-private-key"))
	o := decideEnforcement(observe, findings("cred-private-key"))
	if e.Outcome != o.Outcome {
		t.Fatalf("observe reached a different verdict (%v) from enforce (%v)", o.Outcome, e.Outcome)
	}
	if o.Mode != resilience.ModeObserve {
		t.Fatalf("mode = %v, want observe", o.Mode)
	}
	// The caller renders it as would_block, so a report can never confuse
	// "we blocked this" with "we would have".
	if o.Outcome.Observed() != resilience.OutcomeWouldBlock {
		t.Fatalf("observed outcome = %v, want would_block", o.Outcome.Observed())
	}
}

// An unresolved or unconfigured bundle enforces nothing: an operator who has
// not chosen enforcement has not consented to it.
func TestNoPolicyEnforcesNothing(t *testing.T) {
	if d := decideEnforcement(context.Background(), findings("cred-private-key")); d.Outcome != resilience.OutcomeAllowed {
		t.Fatalf("outcome = %v with no resolved policy, want allowed", d.Outcome)
	}
}

func TestNoFindingsIsAllowed(t *testing.T) {
	ctx := middleware.WithResolvedEnforcementForTest(context.Background(),
		resilience.ModeEnforce, map[string]string{"pii-ssn": "block"})
	if d := decideEnforcement(ctx, nil); d.Outcome != resilience.OutcomeAllowed {
		t.Fatalf("outcome = %v with no findings", d.Outcome)
	}
}
