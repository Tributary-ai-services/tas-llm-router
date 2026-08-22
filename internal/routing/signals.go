package routing

import (
	"context"
	"strconv"
	"strings"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"
)

// Quality gating — routing-decision.md §5.8, step 6.
//
// # Why this runs BEFORE selection, not inside it
//
// Gates FILTER the candidate set; cost and latency choose among the survivors.
// That ordering is the whole design, and it is what makes the gate a real guard
// on step 5's expected_cost — which optimises for fewer output tokens, not
// better answers, and will happily prefer a model that replies badly in half the
// words.
//
// A quality TERM inside the cost function would simply be traded against: a
// large enough price advantage would buy its way past any weight. A FLOOR
// cannot be. Lexicographic ordering means there is never a 1:1
// dollar-versus-quality trade to argue about — a candidate either clears the
// floor or it does not.
//
// # Dimensions, never composite
//
// Gates read efficacy and assurance directly. Composite renormalises over
// whichever dimensions were present and is dominated by cost — a model scoring
// 100 on every quality dimension lands at 67 composite. Routing on it would
// mean a dashboard weighting change silently re-routes production traffic.

// signalsKey carries the rule's floors and the measured evidence.
type signalsKey struct{}

type signalsCtx struct {
	Signals resilience.Signals
	Quality []resilience.QualitySignal
}

// WithSignals returns a context carrying quality floors and their evidence.
func WithSignals(ctx context.Context, sig resilience.Signals, quality []resilience.QualitySignal) context.Context {
	if sig.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, signalsKey{}, signalsCtx{Signals: sig, Quality: quality})
}

// SignalsFrom returns the request's quality gating.
func SignalsFrom(ctx context.Context) (resilience.Signals, []resilience.QualitySignal, bool) {
	s, ok := ctx.Value(signalsKey{}).(signalsCtx)
	if !ok {
		return resilience.Signals{}, nil, false
	}
	return s.Signals, s.Quality, true
}

// ExcludedCandidate records a candidate the gate removed, for the event.
type ExcludedCandidate struct {
	Provider  string
	Model     string
	Dimension string
	Reason    string
}

// gateCandidates filters a candidate set by the rule's quality floors.
//
// Returns the survivors and what was removed. The exclusions are returned
// rather than merely logged because "candidate excluded by signals" has to be a
// first-class outcome on the event: a model quietly vanishing from
// consideration, with the request still succeeding on another, is invisible
// otherwise — and it is exactly the kind of change someone needs to see when
// asking why their traffic moved.
//
// NEVER RETURNS AN EMPTY SET. If every candidate fails, the gate yields and
// records that it did. A quality control that removes the last provider takes
// the service down to avoid a possible quality problem, which is a worse
// outcome than the one it prevents — and unlike constraints, a quality floor is
// a judgement about aggregate past behaviour rather than a rule about what is
// permitted.
func gateCandidates(ctx context.Context, candidates []string, modelFor func(string) string, workflow string) ([]string, []ExcludedCandidate, string) {
	sig, quality, ok := SignalsFrom(ctx)
	if !ok || sig.IsZero() || len(candidates) == 0 {
		return candidates, nil, ""
	}
	idx := indexQuality(quality)

	var kept []string
	var excluded []ExcludedCandidate
	abstained := 0
	for _, provider := range candidates {
		model := modelFor(provider)
		q, found := idx.lookup(model, workflow)
		res := sig.Gate(q, found)
		if res.Eligible {
			kept = append(kept, provider)
			if res.Reason != "" {
				abstained++
			}
			continue
		}
		excluded = append(excluded, ExcludedCandidate{
			Provider: provider, Model: model,
			Dimension: res.Dimension, Reason: res.Reason,
		})
	}

	if len(kept) == 0 {
		return candidates, nil, "quality gates would have excluded every candidate; gate yielded rather than fail the request"
	}
	note := ""
	if abstained > 0 && len(excluded) == 0 {
		// Distinguishes "everything passed" from "we could not judge" — they
		// look identical on the event otherwise, and only one of them is
		// evidence that the gate is working.
		note = "quality gate abstained on " + strconv.Itoa(abstained) + " candidate(s): insufficient evidence"
	}
	return kept, excluded, note
}

// qualityIndex looks up aggregates by (model, workflow).
type qualityIndex struct {
	rows map[string]resilience.QualitySignal
}

func indexQuality(rows []resilience.QualitySignal) *qualityIndex {
	idx := &qualityIndex{rows: make(map[string]resilience.QualitySignal, len(rows))}
	for _, r := range rows {
		idx.rows[qualityKey(r.Model, r.Workflow)] = r
	}
	return idx
}

func qualityKey(model, workflow string) string {
	return strings.ToLower(strings.TrimSpace(model)) + "\x00" + strings.ToLower(strings.TrimSpace(workflow))
}

// lookup prefers the workflow-specific aggregate and falls back to the
// workflow-agnostic one, for the same reason verbosity does: workflow is
// optional on a request, and a tenant that never sets it would otherwise have
// no evidence at all.
func (q *qualityIndex) lookup(model, workflow string) (resilience.QualitySignal, bool) {
	if workflow != "" {
		if r, ok := q.rows[qualityKey(model, workflow)]; ok {
			return r, true
		}
	}
	r, ok := q.rows[qualityKey(model, "")]
	return r, ok
}
