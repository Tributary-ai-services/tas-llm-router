package server

import (
	"context"
	routermetrics "github.com/tributary-ai/llm-router-waf/internal/metrics"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"

	"github.com/Tributary-ai-services/Gatekeeper/pkg/scan"
	"github.com/tributary-ai/llm-router-waf/internal/middleware"
)

// Policy enforcement — routing-decision.md step 8 (Stage 4.1).
//
// # What this replaces, which is not "nothing"
//
// The gateway already enforced before this existed, in a way that ignored
// policy entirely: ShouldBlock rejects any request carrying a CRITICAL finding
// (gated on BlockOnCritical), and GATEKEEPER_REDACT_ENABLED redacts
// unconditionally. Both operate outside the bundle system meant to govern them,
// so a bundle could display `block` on a rule while something else decided what
// actually happened.
//
// # observe does not mean off, and that is the whole safety story
//
// In observe mode the pre-existing controls keep running EXACTLY as before and
// policy decides nothing. It still records what it WOULD have done, which makes
// the enforcement dry run live rather than retrospective.
//
// The alternative — observe superseding and therefore disabling the existing
// critical-block and redaction — would make adopting this feature quietly
// REDUCE protection on every tenant that had not configured enforcement. A
// security feature whose rollout weakens security is the wrong shape however
// clean the resulting architecture.
//
// # Why the decision is per finding, not per request severity
//
// The rule the operator wrote is the authority. A `critical` finding for a
// pattern the bundle only logs should NOT block just because it is critical —
// that is the hardcoded behaviour this replaces. Severity informs the rule; it
// does not override it.

// enforcementDecision is what policy concluded for one request.
type enforcementDecision struct {
	Outcome resilience.Outcome
	// Patterns that drove the outcome, so the event can say WHICH rule acted
	// rather than only that something did.
	Patterns []string
	Mode     resilience.Mode
}

// decideEnforcement applies a bundle's rule actions to a scan result.
//
// Returns the outcome policy reaches. The CALLER decides whether to act on it:
// in observe mode the outcome is recorded and discarded, in enforce mode it is
// applied. Keeping the decision separate from the action is what lets both
// modes share one code path — and therefore what makes the dry run's numbers
// and enforcement's behaviour provably the same logic rather than two
// implementations that agree by inspection.
func decideEnforcement(ctx context.Context, findings []scan.Finding) enforcementDecision {
	mode, rules := middleware.ResolvedEnforcement(ctx)
	d := enforcementDecision{Outcome: resilience.OutcomeAllowed, Mode: mode}
	if len(findings) == 0 || len(rules) == 0 {
		return d
	}

	worst := resilience.OutcomeAllowed
	seen := map[string]bool{}
	for _, f := range findings {
		action, governed := rules[strings.ToLower(strings.TrimSpace(f.PatternID))]
		if !governed || action == "log" {
			continue
		}
		// Strongest action wins: a request carrying both a redactable phone
		// number and a blockable secret is blocked. Reporting it as a
		// redaction would understate what enforcement did.
		if action == "block" {
			worst = resilience.OutcomeBlocked
		} else if worst != resilience.OutcomeBlocked {
			worst = resilience.OutcomeRedacted
		}
		if !seen[f.PatternID] {
			seen[f.PatternID] = true
			d.Patterns = append(d.Patterns, f.PatternID)
		}
	}
	d.Outcome = worst
	return d
}

// applyEnforcement records the decision and reports whether the caller should
// refuse the request.
//
// In observe mode this ALWAYS returns false — policy changes nothing — but the
// outcome is still stamped as would_block / would_redact so the difference
// between "we blocked this" and "we would have" survives onto the event.
func (s *Server) applyEnforcement(w http.ResponseWriter, r *http.Request, findings []scan.Finding, direction string) (blocked bool) {
	d := decideEnforcement(r.Context(), findings)
	if d.Outcome == resilience.OutcomeAllowed {
		middleware.StampEnforcement(r.Context(), string(d.Mode), string(resilience.OutcomeAllowed), nil, direction)
		return false
	}

	if d.Mode != resilience.ModeEnforce {
		// Observe: record what enforcing would have done, change nothing.
		middleware.StampEnforcement(r.Context(), string(d.Mode), string(d.Outcome.Observed()), d.Patterns, direction)
		s.logger.WithFields(logrus.Fields{
			"would":     string(d.Outcome),
			"patterns":  strings.Join(d.Patterns, ","),
			"direction": direction,
		}).Info("Policy would have acted (observe mode)")
		return false
	}

	middleware.StampEnforcement(r.Context(), string(d.Mode), string(d.Outcome), d.Patterns, direction)
	if d.Outcome != resilience.OutcomeBlocked {
		// Redaction is performed by the existing Gatekeeper redact path; policy
		// only decides that it should happen. Duplicating the redaction logic
		// here would give two implementations of the same transformation.
		return false
	}

	routermetrics.BlockedRequestsTotal.WithLabelValues(direction).Inc()
	s.logger.WithFields(logrus.Fields{
		"patterns":  strings.Join(d.Patterns, ","),
		"direction": direction,
	}).Warn("Policy blocked the request")
	// The refusal names the patterns, not the matched values: a block message
	// that quoted the secret would leak it to whoever triggered the block.
	s.writeErrorCtx(w, r, http.StatusUnprocessableEntity,
		"blocked by policy: "+strings.Join(d.Patterns, ", "))
	return true
}
