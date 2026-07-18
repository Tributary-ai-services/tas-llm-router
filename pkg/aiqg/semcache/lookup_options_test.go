package semcache

import (
	"context"
	"testing"
)

// A per-request Shadow override flips a shadow-configured cache into serving
// (semantic_hit) — the seam per-tenant serve-enablement uses.
func TestLookupWithOptions_ShadowOverrideServes(t *testing.T) {
	c, _ := newCache(t, true /* global shadow */)
	sc := scopeOf("t1", "m1")
	seed(t, c, sc, "how do I reset my password", `{"ok":1}`)

	// Global shadow → shadow_hit.
	if out := c.Lookup(context.Background(), sc, "how can I reset my password"); out.State != StateShadowHit {
		t.Fatalf("global shadow: state=%q want shadow_hit", out.State)
	}
	// Override Shadow=false → semantic_hit (serve).
	serve := false
	out := c.LookupWithOptions(context.Background(), sc, "how can I reset my password", LookupOptions{Shadow: &serve})
	if out.State != StateSemanticHit {
		t.Errorf("shadow override: state=%q want semantic_hit", out.State)
	}
	if out.Entry == nil {
		t.Error("a semantic hit must carry the winning entry to serve")
	}
	if out.Threshold != 0.8 {
		t.Errorf("threshold should reflect the effective floor, got %v", out.Threshold)
	}
}

// A per-request MinSimilarity override raises the L1 floor above a candidate's
// similarity, turning a would-hit into a miss (no candidate clears the floor).
func TestLookupWithOptions_ThresholdOverrideTightens(t *testing.T) {
	c, _ := newCache(t, false)
	sc := scopeOf("t1", "m1")
	seed(t, c, sc, "how do I reset my password", `{"ok":1}`)

	// At the default 0.8 floor the paraphrase hits.
	if out := c.Lookup(context.Background(), sc, "how can I reset my password"); out.State != StateSemanticHit {
		t.Fatalf("baseline should hit, got %q (sim=%.3f)", out.State, out.Similarity)
	}
	// Raise the floor to ~1.0 → the paraphrase no longer clears L1 → miss.
	tight := 0.999
	out := c.LookupWithOptions(context.Background(), sc, "how can I reset my password", LookupOptions{MinSimilarity: &tight})
	if out.State != StateMiss {
		t.Errorf("tightened threshold should miss, got %q (sim=%.3f)", out.State, out.Similarity)
	}
	if out.Threshold != tight {
		t.Errorf("outcome threshold=%v want %v", out.Threshold, tight)
	}
}
