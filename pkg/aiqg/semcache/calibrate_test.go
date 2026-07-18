package semcache

import (
	"context"
	"testing"
)

// embedAll builds the text→vector map the sweep reuses (embed once, §9.2 step 3).
func embedAll(pairs []LabeledPair) map[string][]float32 {
	e := bagEmbedder{dim: 256}
	m := map[string][]float32{}
	for _, p := range pairs {
		for _, t := range []string{p.Query, p.Candidate} {
			if _, ok := m[t]; !ok {
				v, _ := e.Embed(context.Background(), t)
				m[t] = v
			}
		}
	}
	return m
}

func TestSweep_L2ReducesFalseHits(t *testing.T) {
	scope := scopeOf("t1", "m1")
	pairs := []LabeledPair{
		// Matches — genuine paraphrases (no discriminative difference).
		{Query: "How do I reset my account password", Candidate: "How can I reset my account password", Match: true},
		{Query: "What is the refund policy for subscriptions", Candidate: "How does refunding work for subscriptions", Match: true},
		// Non-matches — high lexical overlap (so high bag cosine) but a discriminative
		// entity/number/negation difference. A threshold-only cache serves these as
		// FALSE HITS; the cascade's L2 gate must reject them.
		{Query: "What are the fees on the Chase Sapphire card", Candidate: "What are the fees on the Chase Sapphire Reserve card", Match: false},
		{Query: "What is the invoice total for 5 seats", Candidate: "What is the invoice total for 8 seats", Match: false},
		{Query: "Is aspirin safe for children", Candidate: "Is aspirin not safe for children", Match: false},
	}
	emb := embedAll(pairs)
	ths := DefaultThresholds()

	cascade := Sweep(pairs, emb, ths, true /* L2 */, scope)
	threshOnly := Sweep(pairs, emb, ths, false, scope)

	// At a permissive threshold (0.8) the near-misses are all above it. Without
	// L2 they are false hits; with L2 they are rejected.
	find := func(rs []ThresholdResult, th float64) ThresholdResult {
		for _, r := range rs {
			if r.Threshold == th {
				return r
			}
		}
		t.Fatalf("threshold %.3f not in sweep", th)
		return ThresholdResult{}
	}
	c08, o08 := find(cascade, 0.8), find(threshOnly, 0.8)
	if o08.FP == 0 {
		t.Fatal("threshold-only at 0.8 should have false hits (the near-misses)")
	}
	if c08.FP >= o08.FP {
		t.Errorf("L2 must reduce false hits: cascade FP=%d vs threshold-only FP=%d", c08.FP, o08.FP)
	}
	if c08.FP != 0 {
		t.Errorf("L2 should reject all discriminative near-misses here, got FP=%d", c08.FP)
	}
}

func TestRecommend_HonorsFPRBudget(t *testing.T) {
	scope := scopeOf("t1", "m1")
	pairs := []LabeledPair{
		{Query: "reset my password", Candidate: "reset my password now", Match: true},
		{Query: "reset my password", Candidate: "change my password", Match: true},
		// A near-miss the L2 gate will NOT catch (no entity/number/negation diff),
		// so it survives to test the FPR-budget logic: only the threshold can
		// separate it, and only above its similarity.
		{Query: "cheap flights to paris", Candidate: "cheap flights to london", Match: false},
	}
	emb := embedAll(pairs)
	res := Sweep(pairs, emb, DefaultThresholds(), true, scope)

	// With a 0 FPR budget, the recommended threshold must have zero false hits.
	rec, ok := Recommend(res, 0.0)
	if ok && rec.FP != 0 {
		t.Errorf("0%% FPR budget must not pick a threshold with false hits: %+v", rec)
	}
	// A generous budget should still return a valid recommendation.
	if _, ok := Recommend(res, 0.5); !ok {
		t.Error("a generous FPR budget should yield a recommendation")
	}
	// Monotonicity: FPR is non-increasing as threshold rises.
	for i := 1; i < len(res); i++ {
		if res[i].FPR > res[i-1].FPR+1e-9 {
			t.Errorf("FPR should not increase with threshold: %.3f (%.3f) > %.3f (%.3f)",
				res[i].FPR, res[i].Threshold, res[i-1].FPR, res[i-1].Threshold)
		}
	}
}

func TestFPRFloor(t *testing.T) {
	scope := scopeOf("t1", "m1")
	// A non-match L2 can't catch and that's near-identical in embedding space →
	// the FPR floors above 0 (the model's separation limit, §9.2 step 5).
	pairs := []LabeledPair{
		{Query: "the cat sat on the mat", Candidate: "the cat sat on the mat", Match: false},
	}
	emb := embedAll(pairs)
	res := Sweep(pairs, emb, DefaultThresholds(), true, scope)
	if FPRFloor(res) < 0.99 {
		t.Errorf("an identical-embedding non-match should floor FPR near 1.0, got %.3f", FPRFloor(res))
	}
}
