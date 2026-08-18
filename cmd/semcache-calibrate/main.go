// Command semcache-calibrate settles the embedder/threshold question from the
// L3 judge's REAL labels instead of hand-authored pairs.
//
// Why this exists: every threshold decision to date was made against pairs
// someone wrote to make a point. That is how the 0.85-is-safe conclusion got
// made and then inverted — a corpus you author encodes the failure modes you
// already thought of, and misses the ones you did not. The judged-pair corpus
// does not have that defect: the pairs are whatever real traffic collided in
// the sampling band, and the labels are the judge's, not the author's.
//
// The corpus is embedder-INDEPENDENT: it stores the two prompts and whether
// they want the same answer, not a vector. So the same real pairs can be
// re-embedded with any candidate model and swept on equal footing — which is
// exactly the comparison "is langcache better than all-minilm" needs, and
// exactly what cannot be done from production events (prompts are never
// retained, by design).
//
// Usage:
//
//	semcache-calibrate -redis <addr> -provider ollama -ollama-url … -model all-minilm
//	semcache-calibrate -redis <addr> -provider tei    -tei-url …
//
// Both runs read the same corpus, so their tables are directly comparable.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/semcache"
)

func main() {
	var (
		addr       = flag.String("redis", "localhost:6379", "redis-semcache address")
		key        = flag.String("key", semcache.LabeledPairsKey, "labeled-pairs list key")
		provider   = flag.String("provider", "ollama", "embedder: ollama | tei")
		ollamaURL  = flag.String("ollama-url", "http://ollama.tas-shared:11434", "Ollama base URL")
		ollamaMdl  = flag.String("model", "all-minilm", "Ollama embedding model")
		teiURL     = flag.String("tei-url", "http://tei.tas-shared:8080", "TEI base URL")
		dim        = flag.Int("dim", 384, "embedding dimensionality")
		useL2      = flag.Bool("l2", true, "apply the L2 guards, i.e. sweep the real cascade decision")
		fprBudget  = flag.Float64("fpr", 0.0, "false-hit budget for the recommendation")
		minPairs   = flag.Int("min-pairs", 40, "refuse to recommend below this many pairs")
		jsonOut    = flag.Bool("json", false, "emit JSON instead of a table")
		timeoutSec = flag.Int("timeout", 300, "overall timeout in seconds")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	pairs, judged, err := loadPairs(ctx, *addr, *key)
	if err != nil {
		fatal("load corpus: %v", err)
	}
	if len(pairs) == 0 {
		fatal("corpus %q is empty — nothing to calibrate.\n"+
			"The judge fills it from candidates in the sampling band; it needs traffic, not a rerun.", *key)
	}

	emb, err := newEmbedder(*provider, *ollamaURL, *ollamaMdl, *teiURL, *dim)
	if err != nil {
		fatal("%v", err)
	}

	vecs, err := embedAll(ctx, emb, pairs)
	if err != nil {
		fatal("embed: %v", err)
	}

	// Scope only matters to the L2 guards' scope check; the corpus records the
	// scope each pair was observed under, and mixing them would let a guard fire
	// for the wrong reason. Sweep under the dominant scope and say so.
	scope, mixed := dominantScope(judged)
	results := semcache.Sweep(pairs, vecs, semcache.DefaultThresholds(), *useL2, scope)

	rec, ok := semcache.Recommend(results, *fprBudget)
	floor := semcache.FPRFloor(results)

	if *jsonOut {
		out := map[string]any{
			"provider": *provider, "pairs": len(pairs), "results": results,
			"fpr_floor": floor, "recommended": rec, "recommendation_ok": ok,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}

	pos, neg := classCounts(pairs)
	fmt.Printf("corpus     %s  (%d pairs: %d match, %d near-miss)\n", *key, len(pairs), pos, neg)
	fmt.Printf("embedder   %s\n", describe(*provider, *ollamaMdl, *teiURL))
	fmt.Printf("decision   %s\n", map[bool]string{true: "full cascade (L1+L2)", false: "threshold only"}[*useL2])
	if mixed {
		fmt.Printf("⚠ corpus mixes scopes; swept under %s/%s\n", scope.Model, scope.ScoringVersion)
	}
	fmt.Println()
	fmt.Printf("  %-10s %5s %5s %5s %5s  %8s %8s\n", "threshold", "TP", "FP", "TN", "FN", "hit%", "FPR%")
	for _, r := range results {
		fmt.Printf("  %-10.2f %5d %5d %5d %5d  %7.1f%% %7.1f%%\n",
			r.Threshold, r.TP, r.FP, r.TN, r.FN, pct(r.HitRate), pct(r.FPR))
	}
	fmt.Println()

	// Guard the headline number. A confident recommendation off a handful of
	// pairs is how the last two calibrations went wrong; say so rather than
	// print a threshold that looks authoritative.
	switch {
	case len(pairs) < *minPairs:
		fmt.Printf("NO RECOMMENDATION — %d pairs is below the -min-pairs=%d floor.\n", len(pairs), *minPairs)
		fmt.Printf("  Accumulate more judged pairs before trusting any threshold here.\n")
	case pos == 0 || neg == 0:
		fmt.Printf("NO RECOMMENDATION — corpus has only one class (%d match / %d near-miss).\n", pos, neg)
		fmt.Printf("  A threshold cannot be separated from a single class.\n")
	case !ok:
		fmt.Printf("NO THRESHOLD meets an FPR budget of %.1f%% — the floor is %.1f%%.\n", pct(*fprBudget), pct(floor))
		fmt.Printf("  The fix is the embedder, the L2 guards, or the scope — not a number.\n")
	default:
		fmt.Printf("RECOMMENDED  threshold %.2f  →  hit %.1f%%, FPR %.1f%% (budget %.1f%%)\n",
			rec.Threshold, pct(rec.HitRate), pct(rec.FPR), pct(*fprBudget))
	}
}

func loadPairs(ctx context.Context, addr, key string) ([]semcache.LabeledPair, []semcache.JudgedPair, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	raw, err := rdb.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, nil, err
	}
	var pairs []semcache.LabeledPair
	var judged []semcache.JudgedPair
	for _, s := range raw {
		var jp semcache.JudgedPair
		if err := json.Unmarshal([]byte(s), &jp); err != nil {
			continue // a malformed entry must not sink the run
		}
		if jp.Query == "" || jp.Candidate == "" {
			continue
		}
		pairs = append(pairs, jp.LabeledPair)
		judged = append(judged, jp)
	}
	return pairs, judged, nil
}

func newEmbedder(provider, ollamaURL, model, teiURL string, dim int) (semcache.Embedder, error) {
	switch strings.ToLower(provider) {
	case "ollama":
		return semcache.NewOllamaEmbedder(ollamaURL, model, dim), nil
	case "tei":
		return semcache.NewTEIEmbedder(teiURL, dim), nil
	default:
		return nil, fmt.Errorf("unknown -provider %q (want ollama or tei)", provider)
	}
}

// embedAll embeds each distinct text once. Sweep keys embeddings by text, so
// deduping is both a speedup and required for the map to be consistent.
func embedAll(ctx context.Context, emb semcache.Embedder, pairs []semcache.LabeledPair) (map[string][]float32, error) {
	texts := make(map[string]struct{}, len(pairs)*2)
	for _, p := range pairs {
		texts[p.Query] = struct{}{}
		texts[p.Candidate] = struct{}{}
	}
	out := make(map[string][]float32, len(texts))
	for t := range texts {
		v, err := emb.Embed(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("embedding %.40q: %w", t, err)
		}
		out[t] = v
	}
	return out, nil
}

func dominantScope(judged []semcache.JudgedPair) (semcache.Scope, bool) {
	counts := map[semcache.Scope]int{}
	for _, j := range judged {
		counts[j.Scope]++
	}
	type kv struct {
		s semcache.Scope
		n int
	}
	var all []kv
	for s, n := range counts {
		all = append(all, kv{s, n})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n })
	if len(all) == 0 {
		return semcache.Scope{}, false
	}
	return all[0].s, len(all) > 1
}

func classCounts(pairs []semcache.LabeledPair) (pos, neg int) {
	for _, p := range pairs {
		if p.Match {
			pos++
		} else {
			neg++
		}
	}
	return
}

func describe(provider, model, teiURL string) string {
	if strings.EqualFold(provider, "tei") {
		return "tei " + teiURL
	}
	return "ollama " + model
}

func pct(v float64) float64 { return v * 100 }

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "semcache-calibrate: "+format+"\n", args...)
	os.Exit(1)
}
