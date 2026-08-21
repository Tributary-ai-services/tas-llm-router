package routing

import (
	"hash/fnv"
	"sort"
	"strings"
)

// Weighted selection — routing-decision.md §5.7.
//
// A canary rollout and a load split are the SAME mechanism, differing only in
// who watches the result — which is the experiment runner's job, not this one's.
// Modelling them separately would duplicate the machinery and then require
// keeping two copies in step.
//
// # Deterministic, not random
//
// Selection hashes a stability key rather than calling a random source. Two
// requests from the same conversation land on the same provider, which matters
// for exactly the reason affinity exists: a 90/10 split that reshuffles every
// turn gives every conversation a 10% chance of a cold cache on every request,
// rather than 10% of conversations running on the canary.
//
// It also makes the split reproducible. "Why did this request go to the canary"
// is answerable from the request itself rather than from a coin we no longer
// have.

// weightedPick chooses a provider from relative weights, deterministically for
// a given key.
//
// Providers are sorted before allocation so the same weights always produce the
// same bands. Map iteration order in Go is randomised, and without sorting the
// same config would allocate different providers to the same key from one
// process start to the next — a canary that silently reassigns its cohort on
// every deploy.
func weightedPick(weights map[string]int, key string, eligible func(string) bool) (string, bool) {
	type band struct {
		provider string
		upper    int
	}
	names := make([]string, 0, len(weights))
	for p := range weights {
		if w := weights[p]; w > 0 && (eligible == nil || eligible(p)) {
			names = append(names, p)
		}
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Strings(names)

	total := 0
	bands := make([]band, 0, len(names))
	for _, p := range names {
		total += weights[p]
		bands = append(bands, band{provider: p, upper: total})
	}
	if total <= 0 {
		return "", false
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(key)))
	point := int(h.Sum32() % uint32(total))
	for _, b := range bands {
		if point < b.upper {
			return b.provider, true
		}
	}
	return bands[len(bands)-1].provider, true
}
