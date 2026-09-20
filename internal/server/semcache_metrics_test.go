package server

import (
	"testing"

	routermetrics "github.com/tributary-ai/llm-router-waf/internal/metrics"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/semcache"
)

// counterValue reads one child of a CounterVec out of the shared registry.
func counterValue(t *testing.T, name string, label string) float64 {
	t.Helper()
	fams, err := routermetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetValue() == label {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return -1 // absent, which is distinct from zero and is the bug seed() exists to prevent
}

func histCount(t *testing.T, name, label string) uint64 {
	t.Helper()
	fams, err := routermetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetValue() == label {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

// The four outcomes must be distinguishable. A single "miss" would hide the
// difference between "the threshold rejected something" and "the store had
// nothing", which is the distinction the calibration depends on.
func TestRecordSemCacheOutcome_SeparatesMissKinds(t *testing.T) {
	before := map[string]float64{
		"miss_rejected":     counterValue(t, "llm_router_semcache_lookups_total", "miss_rejected"),
		"miss_no_candidate": counterValue(t, "llm_router_semcache_lookups_total", "miss_no_candidate"),
	}
	rejBefore := histCount(t, "llm_router_semcache_top_similarity", "rejected")

	// L1 found a candidate at 0.91; L2 threw it out.
	recordSemCacheOutcome(semcache.Outcome{
		State: semcache.StateMiss, Similarity: 0.91, Threshold: 0.87, RejectReason: "negation",
	})
	// Nothing close in the store at all.
	recordSemCacheOutcome(semcache.Outcome{State: semcache.StateMiss, Similarity: 0})

	if got := counterValue(t, "llm_router_semcache_lookups_total", "miss_rejected"); got != before["miss_rejected"]+1 {
		t.Errorf("miss_rejected = %v, want %v", got, before["miss_rejected"]+1)
	}
	if got := counterValue(t, "llm_router_semcache_lookups_total", "miss_no_candidate"); got != before["miss_no_candidate"]+1 {
		t.Errorf("miss_no_candidate = %v, want %v", got, before["miss_no_candidate"]+1)
	}
	// The rejected candidate must land in the histogram — it is the upper tail
	// of this distribution that sets the operating threshold.
	if got := histCount(t, "llm_router_semcache_top_similarity", "rejected"); got != rejBefore+1 {
		t.Errorf("rejected observations = %d, want %d", got, rejBefore+1)
	}
	if got := counterValue(t, "llm_router_semcache_rejections_total", "negation"); got < 1 {
		t.Errorf("rejection reason not recorded, got %v", got)
	}
}

// A shadow hit is a hit for calibration purposes even though nothing was
// served; if it were not counted, the shadow run this whole rollout depends on
// would report no signal.
func TestRecordSemCacheOutcome_CountsShadowHits(t *testing.T) {
	before := counterValue(t, "llm_router_semcache_lookups_total", "shadow_hit")
	passBefore := histCount(t, "llm_router_semcache_top_similarity", "passed")

	recordSemCacheOutcome(semcache.Outcome{
		State: semcache.StateShadowHit, Similarity: 0.955, Threshold: 0.87,
	})

	if got := counterValue(t, "llm_router_semcache_lookups_total", "shadow_hit"); got != before+1 {
		t.Errorf("shadow_hit = %v, want %v", got, before+1)
	}
	if got := histCount(t, "llm_router_semcache_top_similarity", "passed"); got != passBefore+1 {
		t.Errorf("passed observations = %d, want %d", got, passBefore+1)
	}
}

// A lookup that never ran records nothing. Counting it as a miss would make the
// hit rate depend on how much ineligible traffic arrived, which says nothing
// about the cache.
func TestRecordSemCacheOutcome_IgnoresDidNotRun(t *testing.T) {
	fams, _ := routermetrics.Registry.Gather()
	total := func() float64 {
		var sum float64
		for _, f := range fams {
			if f.GetName() != "llm_router_semcache_lookups_total" {
				continue
			}
			for _, m := range f.GetMetric() {
				sum += m.GetCounter().GetValue()
			}
		}
		return sum
	}
	before := total()

	recordSemCacheOutcome(semcache.Outcome{}) // State "" — cache did not run

	fams, _ = routermetrics.Registry.Gather()
	if after := total(); after != before {
		t.Errorf("a did-not-run lookup changed the counters: %v -> %v", before, after)
	}
}

// seed() must make the outcomes observable at zero from pod start. An absent
// series renders as a blank panel, which reads as "fine" — the exact failure
// mode that let a zero-recall cache run unnoticed for a month (#146).
func TestSemCacheOutcomesAreSeeded(t *testing.T) {
	for _, outcome := range []string{"semantic_hit", "shadow_hit", "miss_rejected", "miss_no_candidate"} {
		if got := counterValue(t, "llm_router_semcache_lookups_total", outcome); got < 0 {
			t.Errorf("outcome %q is absent from the registry; seed() should publish it at zero", outcome)
		}
	}
}
