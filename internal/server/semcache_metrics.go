package server

import (
	routermetrics "github.com/tributary-ai/llm-router-waf/internal/metrics"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/semcache"
)

// recordSemCacheOutcome exports one cascade decision as metrics.
//
// Both lookup paths call this — the shadow probe and the serving path — so the
// histogram covers every comparison the cache makes, not only the ones a tenant
// is serving. That matters because the calibration it feeds has to be done in
// shadow, before anyone is exposed to the threshold being wrong.
//
// A lookup that did not run (cache disabled, request not eligible) records
// nothing: counting those as misses would make the hit rate a function of how
// much ineligible traffic arrived, which is not a fact about the cache.
func recordSemCacheOutcome(out semcache.Outcome) {
	switch out.State {
	case semcache.StateSemanticHit, semcache.StateShadowHit:
		routermetrics.SemCacheLookupsTotal.WithLabelValues(out.State).Inc()
		routermetrics.SemCacheTopSimilarity.WithLabelValues("passed").Observe(out.Similarity)

	case semcache.StateMiss:
		// Similarity > 0 means L1 produced a candidate and L2 threw it out.
		// Zero means the store had nothing close. Separated because the first
		// says the threshold is working and the second says the cache is cold,
		// and the fixes point in opposite directions.
		if out.Similarity > 0 {
			routermetrics.SemCacheLookupsTotal.WithLabelValues("miss_rejected").Inc()
			routermetrics.SemCacheTopSimilarity.WithLabelValues("rejected").Observe(out.Similarity)
			reason := out.RejectReason
			if reason == "" {
				reason = "below_threshold" // L1 floor, no named L2 guard fired
			}
			routermetrics.SemCacheRejectionsTotal.WithLabelValues(reason).Inc()
		} else {
			routermetrics.SemCacheLookupsTotal.WithLabelValues("miss_no_candidate").Inc()
		}
	}
}
