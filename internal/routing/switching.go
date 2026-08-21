package routing

import (
	"fmt"
	"time"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"
)

// Switching hysteresis — routing-decision.md §5.7, step 5.
//
// # Why this ships with expected_cost rather than after it
//
// An expected-cost router without hysteresis flaps, and flapping pays the
// prompt-cache re-warm twice. Anthropic prices a cache write at 1.25x base input
// and a read at 0.10x, so abandoning a warm prefix costs
// (1.25 - 0.10) x p_in x prefix_tokens as a one-off. At a measured 3,324-token
// RAG prefix that is $0.00306, against a cached request costing $0.000486:
//
//	per-request saving   further requests to break even
//	5%                   126
//	10%                  63
//
// A router free to chase a 5% improvement every minute never reaches request
// 126. It just pays the write, over and over, and reports a saving on each
// individual decision while the bill goes up. Shipping expected_cost without
// this would be shipping precisely that machine.
//
// # Three terms, doing different jobs
//
//	min_improvement    is the alternative enough better to be worth ANY switch?
//	warm_cache_bias    …and enough better to also cover the re-warm it causes?
//	dwell              …and has enough time passed that this is a trend rather
//	                   than a tick?
//
// The warm-cache term is what makes hysteresis honest rather than arbitrary. A
// bare "must be 25% better" is a number someone picked; "must beat the
// alternative plus the cache you are about to throw away" is the actual
// economics.

// SwitchDecision records whether a switch was allowed and why not.
type SwitchDecision struct {
	Allow bool
	// Reason explains a refusal in terms an operator can act on, and rides on
	// the event. "We stayed put" and "there was nothing better" look identical
	// in a cost chart otherwise.
	Reason string
}

// DwellStore records when a routing context last switched, shared across
// replicas.
//
// Per-replica dwell would be no dwell at all: with N replicas a conversation
// could switch N times within one dwell window, each replica believing it was
// the first.
type DwellStore interface {
	// LastSwitch returns when this context last switched, and whether a record
	// exists.
	LastSwitch(key string) (time.Time, bool)
	// RecordSwitch marks a switch as having happened now.
	RecordSwitch(key string, at time.Time, ttl time.Duration)
}

// ShouldSwitch decides whether to move from current to candidate.
//
// currentCost and candidateCost are per-request estimates. warmCache reports
// whether the CURRENT provider holds a warm prompt cache that switching would
// discard.
func ShouldSwitch(
	cfg resilience.Switching,
	key string,
	current, candidate costEstimate,
	warmCache bool,
	dwell DwellStore,
	now time.Time,
) SwitchDecision {
	// No alternative, or the same target: nothing to decide.
	if candidate.Provider == "" || (candidate.Provider == current.Provider && candidate.Model == current.Model) {
		return SwitchDecision{Allow: false, Reason: "no alternative to switch to"}
	}
	// An unpriced side cannot be compared. Staying put is the conservative
	// answer: the current provider is at least known to work.
	if current.Total <= 0 || candidate.Total <= 0 {
		return SwitchDecision{Allow: false, Reason: "cannot compare: one side has no price"}
	}

	minImp := cfg.MinImprovementPct
	if minImp <= 0 {
		minImp = resilience.DefaultMinImprovementPct
	}
	bias := cfg.WarmCacheBiasPct
	if bias <= 0 {
		bias = resilience.DefaultWarmCacheBiasPct
	}

	// Required improvement rises when a warm cache is at stake, because the
	// switch must beat the alternative AND the rebuild it triggers.
	required := minImp
	if warmCache {
		required += bias
	}

	improvementPct := (current.Total - candidate.Total) / current.Total * 100
	if improvementPct < float64(required) {
		return SwitchDecision{Allow: false, Reason: reasonBelowThreshold(improvementPct, required, warmCache)}
	}

	// Dwell last: it is the cheapest check to state but the most confusing to
	// hit, so it should only ever explain a refusal for a switch that was
	// otherwise justified.
	if dwell != nil && cfg.DwellSeconds > 0 {
		if last, ok := dwell.LastSwitch(key); ok {
			if elapsed := now.Sub(last); elapsed < time.Duration(cfg.DwellSeconds)*time.Second {
				return SwitchDecision{Allow: false, Reason: "within the dwell window; last switch was " +
					elapsed.Truncate(time.Second).String() + " ago"}
			}
		}
	}
	return SwitchDecision{Allow: true}
}

func reasonBelowThreshold(got float64, required int, warmCache bool) string {
	base := fmt.Sprintf("improvement %.1f%% is below the %d%% threshold", got, required)
	if warmCache {
		// Naming the warm cache matters: an operator who set a 25% threshold
		// and sees a 30% improvement refused needs to know where the extra
		// came from, or the router looks broken.
		return base + " (raised because switching would discard a warm prompt cache)"
	}
	return base
}
