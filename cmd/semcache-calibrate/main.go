// Command semcache-calibrate is the offline threshold-sweep tool for the AIQG
// semantic cache (docs/AIQG-SEMANTIC-CACHING.md §9.2, §16 #3).
//
// It embeds a LABELED pair set once, then sweeps the L1 similarity threshold —
// running the full cascade (with the L2 verification gate) AND a threshold-only
// baseline — and reports, at each threshold, the hit rate and the false-hit rate
// (FPR). It recommends the threshold that MAXIMIZES hit rate subject to an FPR
// budget (a false hit is a correctness bug, not a miss — §9.2 step 4), and shows
// how much L2 lets you safely loosen that threshold.
//
// Usage:
//
//	semcache-calibrate --ollama-url http://localhost:11434 --model all-minilm \
//	    --dim 384 --fpr-budget 0.01 [--dataset pairs.json]
//
// pairs.json is a JSON array of {"query","candidate","match"} objects; when
// omitted, a built-in seed set (paraphrases vs entity/number/negation near-misses
// and unrelated look-alikes) is used so the tool runs out of the box.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/semcache"
)

func main() {
	var (
		ollamaURL = flag.String("ollama-url", "http://ollama.tas-shared:11434", "Ollama base URL")
		model     = flag.String("model", "all-minilm", "embedding model")
		dim       = flag.Int("dim", 384, "embedding dimensionality")
		datasetF  = flag.String("dataset", "", "labeled pairs JSON file (default: built-in seed set)")
		fprBudget = flag.Float64("fpr-budget", 0.01, "max acceptable false-hit rate (0..1)")
	)
	flag.Parse()

	pairs, err := loadPairs(*datasetF)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load dataset:", err)
		os.Exit(1)
	}
	matches, nonMatches := 0, 0
	for _, p := range pairs {
		if p.Match {
			matches++
		} else {
			nonMatches++
		}
	}
	fmt.Printf("dataset: %d pairs (%d match, %d non-match)  encoder=%s dim=%d\n",
		len(pairs), matches, nonMatches, *model, *dim)
	if nonMatches == 0 {
		fmt.Fprintln(os.Stderr, "WARNING: no non-match pairs — calibrating on positives alone tunes to threshold 0 (§9.2 step 1)")
	}

	// Embed each unique text ONCE, reused across every threshold (§9.2 step 3).
	embed := semcache.NewOllamaEmbedder(*ollamaURL, *model, *dim)
	emb := map[string][]float32{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, p := range pairs {
		for _, txt := range []string{p.Query, p.Candidate} {
			if _, ok := emb[txt]; ok {
				continue
			}
			v, err := embed.Embed(ctx, txt)
			if err != nil {
				fmt.Fprintf(os.Stderr, "embed %q: %v\n", txt, err)
				os.Exit(1)
			}
			emb[txt] = v
		}
	}

	scope := semcache.Scope{TenantID: "calib", Model: *model, ScoringVersion: "calib"}
	ths := semcache.DefaultThresholds()
	cascade := semcache.Sweep(pairs, emb, ths, true, scope)
	baseline := semcache.Sweep(pairs, emb, ths, false, scope)

	fmt.Println("\nthreshold |   CASCADE (L1+L2)        |  THRESHOLD-ONLY")
	fmt.Println("          |  hit%   fpr%   fp        |  hit%   fpr%   fp")
	fmt.Println("----------+--------------------------+------------------------")
	for i := range ths {
		c, b := cascade[i], baseline[i]
		// Print a readable subset: every 0.02 step plus the recommendation region.
		if int(ths[i]*1000)%20 != 0 {
			continue
		}
		fmt.Printf("  %.3f   |  %5.1f  %5.1f   %-3d      |  %5.1f  %5.1f   %-3d\n",
			ths[i], c.HitRate*100, c.FPR*100, c.FP, b.HitRate*100, b.FPR*100, b.FP)
	}

	fmt.Printf("\nFPR budget: %.1f%%\n", *fprBudget*100)
	recC, okC := semcache.Recommend(cascade, *fprBudget)
	recB, okB := semcache.Recommend(baseline, *fprBudget)
	report := func(name string, r semcache.ThresholdResult, ok bool, results []semcache.ThresholdResult) {
		if !ok {
			fmt.Printf("  %-16s no threshold meets the budget — FPR floors at %.1f%% (model separation limit; fix the model/L2/scope, not the number — §9.2 step 5)\n",
				name, semcache.FPRFloor(results)*100)
			return
		}
		fmt.Printf("  %-16s threshold=%.3f  ->  hit=%.1f%%  fpr=%.1f%%  precision=%.1f%%\n",
			name, r.Threshold, r.HitRate*100, r.FPR*100, r.Precision*100)
	}
	report("cascade (L1+L2):", recC, okC, cascade)
	report("threshold-only:", recB, okB, baseline)
	if okC && okB && recC.HitRate > recB.HitRate {
		fmt.Printf("\n  => L2 lets you serve at threshold %.3f (hit %.1f%%) vs %.3f (hit %.1f%%) threshold-only,\n"+
			"     at the same FPR budget — L2's value, quantified.\n",
			recC.Threshold, recC.HitRate*100, recB.Threshold, recB.HitRate*100)
	}
	if okC {
		fmt.Printf("\nRECOMMENDED global default: AIQG_SEMCACHE_MIN_SIMILARITY=%.3f  (then start SHADOW-verify before serving)\n", recC.Threshold)
	}
}

func loadPairs(path string) ([]semcache.LabeledPair, error) {
	if path == "" {
		return builtinPairs(), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []semcache.LabeledPair
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// builtinPairs is a seed labeled set: genuine paraphrases (match) vs three
// non-match classes — L2-catchable near-misses (entity/number/version/negation),
// and unrelated look-alikes only the threshold can separate.
func builtinPairs() []semcache.LabeledPair {
	m := func(q, c string) semcache.LabeledPair {
		return semcache.LabeledPair{Query: q, Candidate: c, Match: true}
	}
	n := func(q, c string) semcache.LabeledPair {
		return semcache.LabeledPair{Query: q, Candidate: c, Match: false}
	}
	return []semcache.LabeledPair{
		// matches — same intent, different wording
		m("How do I reset my account password", "How can I reset my account password"),
		m("How do I reset my account password", "What is the way to reset my account password"),
		m("What is the refund policy for annual subscriptions", "How does refunding work for annual subscriptions"),
		m("How long does standard shipping take", "What is the delivery time for standard shipping"),
		m("How do I cancel my subscription", "What is the way to cancel my subscription"),
		m("Where can I download my invoice", "How do I get a copy of my invoice"),
		m("Is the API rate limited", "Does the API have rate limits"),
		m("How do I enable two factor authentication", "How can I turn on two-factor authentication"),
		// non-match: entity / number / version / negation (L2 must catch)
		n("What are the annual fees on the Chase Sapphire card", "What are the annual fees on the Chase Sapphire Reserve card"),
		n("What is the invoice total for 5 seats", "What is the invoice total for 8 seats"),
		n("How do I use function calling with GPT-4", "How do I use function calling with GPT-5"),
		n("Is aspirin safe for young children", "Is aspirin not safe for young children"),
		n("What changed in the 2026-07-01 release", "What changed in the 2026-08-01 release"),
		n("Can I return this within 30 days", "Can I return this within 90 days"),
		// non-match: unrelated look-alikes (only the threshold separates these)
		n("cheap flights to Paris", "cheap flights to London"),
		n("best restaurants in Rome", "best restaurants in Milan"),
		n("how to bake a chocolate cake", "how to bake a vanilla cake"),
	}
}
