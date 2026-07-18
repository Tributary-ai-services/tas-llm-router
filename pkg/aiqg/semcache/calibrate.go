package semcache

import (
	"sort"
	"time"
)

// Calibration (docs/AIQG-SEMANTIC-CACHING.md §9). The threshold is a property of
// the (encoder, task) pair, never a constant — so it must be measured against a
// LABELED pair set, and the second class is the one that matters: pairs that look
// similar but must NOT share an answer (calibrating on positives alone tunes
// straight to threshold 0, §9.2).
//
// The metric is deliberate: fix an FPR budget and maximize hit rate subject to it
// (§9.2 step 4). Optimizing F1/accuracy is wrong for a cache — it prices a false
// hit like a miss, but a miss is a cost and a false hit is a correctness bug.

// LabeledPair is one calibration example: a live query and a candidate cache
// entry, labeled by whether they should share an answer.
type LabeledPair struct {
	Query     string `json:"query"`
	Candidate string `json:"candidate"`
	// Match is true when the two genuinely share an answer (a paraphrase), false
	// when they look similar but differ (the near-miss / false-hit class).
	Match bool `json:"match"`
}

// ThresholdResult is the confusion matrix + rates at one L1 similarity threshold,
// for the configured decision (cascade with L2, or threshold-only).
type ThresholdResult struct {
	Threshold float64 `json:"threshold"`
	TP        int     `json:"tp"`        // predicted hit & Match
	FP        int     `json:"fp"`        // predicted hit & !Match  (a FALSE HIT — the bug)
	FN        int     `json:"fn"`        // predicted miss & Match  (a missed opportunity)
	TN        int     `json:"tn"`        // predicted miss & !Match
	HitRate   float64 `json:"hit_rate"`  // TP/(TP+FN) — recall over the matches
	FPR       float64 `json:"fpr"`       // FP/(FP+TN) — false-hit rate over non-matches
	Precision float64 `json:"precision"` // TP/(TP+FP)
}

// DefaultThresholds is a fine sweep over the meaningful similarity band. Cheap:
// the vectors are embedded once and reused across every threshold (§9.2 step 3).
func DefaultThresholds() []float64 {
	var out []float64
	for t := 0.80; t <= 0.995+1e-9; t += 0.005 {
		out = append(out, round3(t))
	}
	return out
}

// Sweep evaluates every threshold against the pairs, reusing precomputed
// embeddings (emb maps text→vector; the caller embeds each unique text once).
// When useL2 is true the predicted-hit decision is the full cascade (similarity
// >= t AND the L2 verification gate passes); when false it's threshold-only —
// running both and comparing is how L2's value is shown.
//
// scope is the (tenant, model, scoring) the candidates are treated as sharing, so
// the L2 scope/freshness guards pass and only the discriminative guards
// (entity/number/date, negation) bite — exactly the effect being measured.
func Sweep(pairs []LabeledPair, emb map[string][]float32, thresholds []float64, useL2 bool, scope Scope) []ThresholdResult {
	now := time.Now()
	// Precompute per-pair similarity and (if used) the L2 verdict — both are
	// threshold-independent, so they're computed once, not per threshold.
	type ev struct {
		sim   float64
		l2ok  bool
		match bool
	}
	evs := make([]ev, 0, len(pairs))
	for _, p := range pairs {
		qv, ok1 := emb[p.Query]
		cv, ok2 := emb[p.Candidate]
		if !ok1 || !ok2 {
			continue
		}
		l2ok := true
		if useL2 {
			cand := &Entry{Prompt: p.Candidate, TenantID: scope.TenantID, Model: scope.Model,
				ScoringVersion: scope.ScoringVersion, CreatedAtUnix: now.Unix()}
			l2ok = Verify(p.Query, cand, scope, now, time.Hour).Pass
		}
		evs = append(evs, ev{sim: cosineSimilarity(qv, cv), l2ok: l2ok, match: p.Match})
	}

	out := make([]ThresholdResult, 0, len(thresholds))
	for _, t := range thresholds {
		var r ThresholdResult
		r.Threshold = t
		for _, e := range evs {
			hit := e.sim >= t && e.l2ok
			switch {
			case hit && e.match:
				r.TP++
			case hit && !e.match:
				r.FP++
			case !hit && e.match:
				r.FN++
			default:
				r.TN++
			}
		}
		if d := r.TP + r.FN; d > 0 {
			r.HitRate = float64(r.TP) / float64(d)
		}
		if d := r.FP + r.TN; d > 0 {
			r.FPR = float64(r.FP) / float64(d)
		}
		if d := r.TP + r.FP; d > 0 {
			r.Precision = float64(r.TP) / float64(d)
		}
		out = append(out, r)
	}
	return out
}

// Recommend picks the threshold with the highest hit rate whose FPR is within
// budget (§9.2 step 4: fix the FPR budget, maximize hits under it). Ties on hit
// rate break toward the LOWER threshold (more coverage). Returns ok=false when no
// threshold meets the budget — meaning the embedding model can't separate this
// task and the fix is a better model / tighter L2 / narrower scope (§9.2 step 5),
// not a number.
func Recommend(results []ThresholdResult, fprBudget float64) (ThresholdResult, bool) {
	best := ThresholdResult{}
	found := false
	// Sort by threshold ascending so ties prefer the lower threshold.
	sorted := append([]ThresholdResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Threshold < sorted[j].Threshold })
	for _, r := range sorted {
		if r.FPR <= fprBudget && (!found || r.HitRate > best.HitRate) {
			best, found = r, true
		}
	}
	return best, found
}

// FPRFloor returns the minimum FPR achievable over the sweep — when it stays
// above the budget even at the strictest threshold, the model has hit its
// separation limit (§9.2 step 5).
func FPRFloor(results []ThresholdResult) float64 {
	min := 1.0
	for _, r := range results {
		if r.FPR < min {
			min = r.FPR
		}
	}
	return min
}

func round3(v float64) float64 {
	return float64(int(v*1000+0.5)) / 1000
}
