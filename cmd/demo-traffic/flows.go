// Enterprise demo flows for payload reduction + semantic caching (Plan #16).
//
// These are NOT the 7 Quickstart scenarios in gateway.go. Those exercise the
// CLEAR dimensions with one-shot prompts. These are multi-step flows whose
// step *sequence* is the demonstration: a seed request populates the cache,
// paraphrases of it should return as semantic hits, and deliberately-crafted
// near-miss probes must NOT hit — they exercise the C4 L2 guards
// (pkg/aiqg/semcache/verify.go: scope · freshness · negation ·
// entity_number_date).
//
// Why these six, and why some of them lose:
//
// Reduction and semantic caching fire under different preconditions.
// Reduction wants input-heavy, few-turn (uncached), MCP-routed traffic whose
// retrieved context is mostly irrelevant. Semantic caching wants requests
// that repeat in *meaning*, are stateless, and tolerate a slightly stale
// answer. The set below spans that space on purpose and includes a flow
// where AIQG wins almost nothing (coding), because a demo that shows 40% on
// everything is a sales deck rather than a product. See
// AIQG_REDUCTION_SUMMARY.md and AIQG_CACHING_PRIMER.md §5.
//
// Cacheability preconditions baked in here (from responsecache.Decide):
//   - temperature MUST be 0 (or an explicit seed) or the request is rejected
//     as nondeterministic and never reaches C1 or C4. The generator did not
//     set temperature at all before this, so none of its traffic was ever
//     cacheable.
//   - no tools/functions and no streaming, or the request is not cacheable.
//     That is why the reduction stage of a real dual-lever workload happens
//     upstream at the MCP proxy, and the flow's own call is tools-free.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// stepKind labels what a step is demonstrating. The runner asserts on it:
// paraphrase/repeat steps are *expected* to hit, probe steps are expected to
// MISS, and a probe that hits is reported as a false hit (a correctness
// failure, not a savings win).
type stepKind string

const (
	stepSeed       stepKind = "seed"       // populates the cache; expected miss
	stepRepeat     stepKind = "repeat"     // byte-identical; expected C1 exact hit
	stepParaphrase stepKind = "paraphrase" // same meaning, new wording; expected C4 semantic hit
	stepProbe      stepKind = "probe"      // near-miss; MUST NOT hit
	stepUnique     stepKind = "unique"     // genuinely new work; expected miss by design
)

// flowStep is one request in a demo flow.
type flowStep struct {
	Kind   stepKind `json:"kind"`
	Prompt string   `json:"prompt"`
	// Note explains what this step proves, for the UI's step-by-step trace.
	Note string `json:"note"`
}

// demoFlow is one enterprise workload shape.
//
// JSON tags are deliberate: the catalog is currently defined in Go but is
// hand-mirrored nowhere yet, and the open question (Plan #16) is whether
// aiqg-dashboard-be should serve it so the Go generator and aiqg-ui stop
// duplicating prompt lists the way gateway.go and QuickstartPage.tsx already
// do. Keeping this serializable means `--print-catalog` can feed that
// decision without a rewrite.
type demoFlow struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	WhatItShows string `json:"what_it_shows"`

	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// Temperature is a pointer so 0 is transmitted rather than omitted —
	// omitting it is exactly the bug that made every request nondeterministic.
	Temperature *float64 `json:"temperature"`

	// Modeled cache-aware expectations, so the UI can show measured-vs-modeled
	// instead of an unfalsifiable number. Derived with the same math as
	// aiqg-analysis/workload_savings.py.
	ExpectedReductionPct float64 `json:"expected_reduction_pct"`
	ExpectedCacheHitPct  float64 `json:"expected_cache_hit_pct"`

	Steps []flowStep `json:"steps"`
}

func f64(v float64) *float64 { return &v }

// zeroTemp is shared by every flow: cache eligibility requires it.
var zeroTemp = f64(0)

// ---- The six committed flows --------------------------------------------

var flowCatalog = []demoFlow{
	// F1 — dual-lever canonical. Modeled ~37% reduction, ~55% cache.
	{
		ID:          "it-helpdesk",
		Label:       "IT helpdesk / self-service",
		WhatItShows: "Both levers. A handful of questions dominate real helpdesk volume, so paraphrases collapse onto one cached answer — while the retrieved KB context is mostly irrelevant to any single question, which is what reduction removes.",
		Model:       "claude-haiku-4-5-20251001", MaxTokens: 256, Temperature: zeroTemp,
		ExpectedReductionPct: 37, ExpectedCacheHitPct: 55,
		Steps: []flowStep{
			{stepSeed, "How do I reset my VPN certificate?", "Cold cache — populates C1 and C4."},
			{stepRepeat, "How do I reset my VPN certificate?", "Byte-identical: C1 exact hit, no embedding needed."},
			{stepParaphrase, "What's the process for resetting my VPN cert?", "Same intent, different wording: C4 semantic hit."},
			{stepParaphrase, "How can I reset the VPN certificate on my laptop?", "Still the same answer: C4 semantic hit."},
			{stepProbe, "How do I reset my VPN certificate under the 2024 access policy?", "Near-miss with a date. MUST miss — L2 entity_number_date guard."},
			// Uses the literal word "not": negationParity counts only
			// not/no/never/without/cannot/none/nor/neither/nothing and "n't".
			// Lexical negations like "avoid"/"prevent"/"disable"/"skip" are NOT
			// covered, so a probe phrased that way would clear both L2 guards
			// (same discriminative token set — VPN — in each) and fall through
			// to raw similarity. Keep this probe on a word the guard actually
			// knows, so the assertion tests the mechanism it names.
			{stepProbe, "How do I make sure I do not reset my VPN certificate?", "Negated intent. MUST miss — L2 negation-parity guard."},
		},
	},

	// F2 — highest combined; best B2B story. Modeled ~36% / ~60%.
	{
		ID:          "security-questionnaire",
		Label:       "Security questionnaire / RFP",
		WhatItShows: "The strongest dual-lever case: the same due-diligence questions recur across every deal, and each answer is retrieved from a large policy corpus of which only a fraction is relevant.",
		Model:       "claude-haiku-4-5-20251001", MaxTokens: 384, Temperature: zeroTemp,
		ExpectedReductionPct: 36, ExpectedCacheHitPct: 60,
		Steps: []flowStep{
			{stepSeed, "Do you encrypt customer data at rest?", "Cold cache — first deal asks it."},
			{stepParaphrase, "Is customer data encrypted while stored?", "Second deal, same question reworded: C4 semantic hit."},
			{stepParaphrase, "Confirm whether customer data is encrypted at rest.", "Third deal, imperative phrasing: C4 semantic hit."},
			{stepSeed, "What is your recovery point objective, in hours?", "New question — cold."},
			{stepProbe, "Is your recovery point objective 4 hours or less?", "Numeric qualifier added. MUST miss — L2 entity_number_date."},
			{stepProbe, "Do you comply with SOC 2 Type II?", "Distinct acronym/standard. MUST miss — different question, different answer."},
		},
	},

	// F3 — reduction-only. Proves reduction is not caching in disguise.
	{
		ID:          "contract-review",
		Label:       "Contract clause review",
		WhatItShows: "Reduction ONLY. Every contract is different, so nothing ever cache-hits — but each request ships a long document to answer a narrow question, which is the single most reducible shape there is.",
		Model:       "claude-haiku-4-5-20251001", MaxTokens: 384, Temperature: zeroTemp,
		ExpectedReductionPct: 47, ExpectedCacheHitPct: 3,
		Steps: []flowStep{
			{stepUnique, contractPrompt("Northwind Logistics", "45 days", "2.5%"), "Long doc, narrow ask. Expected miss — the value here is payload reduction, not caching."},
			{stepUnique, contractPrompt("Acme Robotics", "30 days", "1.75%"), "Different counterparty: still a miss, still highly reducible."},
			{stepUnique, contractPrompt("Cobalt Health Systems", "90 days", "3.0%"), "Third unique contract. A cache hit here would be WRONG."},
		},
	},

	// F4 — cache-only. Proves caching is not reduction in disguise.
	//
	// MEASURED 0% semantic hits against a modeled 65% (live run 2026-08-16).
	// That gap is a real product finding, not a broken flow, and the prompts
	// are deliberately left as-is rather than tuned to make the demo look
	// better. Two independent L2 guards over-reject here, both confirmed
	// directly against pkg/aiqg/semcache:
	//
	//  1. discriminativeTokens() only exempts a capital at index 0 of the
	//     whole prompt, so a word that merely OPENS A QUOTED STRING is read as
	//     a proper-noun entity. The seed yields {}, "Unable to log in…" yields
	//     {@unable}, "Login is not working…" yields {@login} — token sets
	//     differ, so discriminativeTokensMatch fails. Lowercasing the same
	//     text makes all three match.
	//  2. negationParity() counts "can't" but not "unable", so two phrasings
	//     with identical meaning get parity 1 vs 0 and are rejected.
	//
	// Both fail in the SAFE direction (no wrong answers), but they zero out
	// the hit rate on quoted user content — ticket text, email subjects, log
	// lines — which is exactly the classification workload semantic caching
	// is supposed to be strongest on.
	{
		ID:          "ticket-triage",
		Label:       "Ticket triage / routing",
		WhatItShows: "Caching ONLY. Inputs are short so there is essentially nothing to reduce, but they are near-duplicates of each other — the classic high-hit-rate, tight-threshold classification case. Currently measures 0% live: two L2 guards over-reject on capitalized words inside quoted ticket text.",
		Model:       "claude-haiku-4-5-20251001", MaxTokens: 64, Temperature: zeroTemp,
		ExpectedReductionPct: 2, ExpectedCacheHitPct: 65,
		Steps: []flowStep{
			{stepSeed, "Classify this support ticket into one category: \"I can't log in to my account\"", "Cold cache."},
			{stepParaphrase, "Classify this support ticket into one category: \"Unable to log in to my account\"", "Near-duplicate input: C4 semantic hit."},
			{stepParaphrase, "Classify this support ticket into one category: \"Login is not working for my account\"", "Still the same label: C4 semantic hit."},
			{stepProbe, "Classify this support ticket into one category: \"I can't log in to the VPN\"", "Different system (VPN) — plausibly a different queue. MUST miss."},
			{stepProbe, "Classify this support ticket into one category: \"I can log in, but billing is wrong\"", "Negation plus a different topic. MUST miss."},
		},
	},

	// F5 — temporal. The point is the hit-rate curve, not a steady-state number.
	{
		ID:          "incident-burst",
		Label:       "Incident burst (thundering herd)",
		WhatItShows: "A time-series demo, not a steady-state one: an incident drives many people to ask the same thing within minutes. Watch the hit rate climb after the first request absorbs the cost for everyone else.",
		Model:       "claude-haiku-4-5-20251001", MaxTokens: 256, Temperature: zeroTemp,
		ExpectedReductionPct: 32, ExpectedCacheHitPct: 85,
		Steps: burstSteps(),
	},

	// F6 — the honesty exhibit. Modeled ~0.2% / 0%.
	{
		ID:          "coding-agent",
		Label:       "Coding agent (negative control)",
		WhatItShows: "Where AIQG wins almost nothing, shown deliberately. Every request carries different repo state, so no two mean the same thing and a cache hit would be WRONG. Reduction is near-zero too — coding agents route ~98% of tool output through built-in Read/Grep/Bash rather than MCP, so the proxy never sees it.",
		Model:       "claude-haiku-4-5-20251001", MaxTokens: 512, Temperature: zeroTemp,
		ExpectedReductionPct: 0.2, ExpectedCacheHitPct: 0,
		Steps: []flowStep{
			{stepUnique, "Here is the failing test output for internal/server/router_test.go:\n\n--- FAIL: TestRouteCostOptimized (0.02s)\n    router_test.go:88: expected provider \"anthropic\", got \"openai\"\n\nWhat is the likely cause?", "Unique repo state. Expected miss — correctly."},
			{stepUnique, "Refactor this function to return an error instead of panicking:\n\nfunc mustParseWindow(s string) time.Duration {\n\td, err := time.ParseDuration(s)\n\tif err != nil { panic(err) }\n\treturn d\n}", "Different task entirely. Expected miss."},
			{stepUnique, "The migration 021_notification_pref.sql added a unique index on unsubscribe_token. Write the down-migration.", "Third unique task. A hit here would return someone else's answer."},
		},
	},
}

// contractPrompt builds a long-document / narrow-question request — the shape
// reduction pays most on (input-heavy, few-turn, mostly-irrelevant context).
func contractPrompt(counterparty, notice, cap string) string {
	clause := "Each party shall maintain in force adequate insurance coverage with reputable insurers " +
		"in respect of its obligations under this Agreement. Neither party shall be liable for any " +
		"indirect, incidental, special, or consequential damages arising out of or in connection with " +
		"this Agreement, whether in contract, tort, or otherwise, even if advised of the possibility " +
		"of such damages. The parties agree that the limitations set out in this clause are reasonable " +
		"having regard to the commercial context in which this Agreement is made. "
	return fmt.Sprintf(
		"You are reviewing a master services agreement with %s.\n\n"+
			"Question: what is the termination notice period, and is there an annual uplift cap?\n\n"+
			"AGREEMENT TEXT:\n%s\n"+
			"Clause 14.2 (Termination for convenience): either party may terminate on %s written notice.\n"+
			"Clause 9.4 (Charges): annual uplift shall not exceed %s.\n%s",
		counterparty, strings.Repeat(clause, 12), notice, cap, strings.Repeat(clause, 12),
	)
}

// burstSteps models a thundering herd: one cold request, then a rush of
// paraphrases of it, then one probe to prove the burst didn't make the cache
// sloppy.
func burstSteps() []flowStep {
	phrasings := []string{
		"Is the payments API down right now?",
		"Are we having an outage with the payments API?",
		"Payments API seems to be failing — is there a known issue?",
		"Is there an incident affecting the payments API?",
		"Why is the payments API returning errors?",
		"Is the payment service currently unavailable?",
		"Are payments broken at the moment?",
		"Known problem with the payments API?",
	}
	steps := []flowStep{{stepSeed, phrasings[0], "First person to ask absorbs the full cost."}}
	for _, p := range phrasings[1:] {
		steps = append(steps, flowStep{stepParaphrase, p, "Same question, different words — served from cache, no vendor call."})
	}
	steps = append(steps, flowStep{
		stepProbe,
		"Is the payments API down in the EU region specifically?",
		"Scoped to a named region. MUST miss — a burst must not make near-misses sloppy.",
	})
	return steps
}

// splitFlowIDs parses --flow=a,b,c. Empty means "run the whole catalog".
func splitFlowIDs(csv string) []string {
	var out []string
	for _, id := range strings.Split(csv, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func flowByID(id string) (demoFlow, bool) {
	for _, f := range flowCatalog {
		if f.ID == id {
			return f, true
		}
	}
	return demoFlow{}, false
}

// ---- Runner --------------------------------------------------------------

// stepOutcome is one executed step, for the summary and for the UI later.
type stepOutcome struct {
	Step       flowStep      `json:"step"`
	Status     int           `json:"status"`
	CacheState string        `json:"cache_state"`
	Latency    time.Duration `json:"latency_ms"`
	Err        string        `json:"error,omitempty"`
}

// hit reports whether the gateway served this step from either cache.
func (o stepOutcome) hit() bool {
	return o.CacheState == "hit" || o.CacheState == "semantic_hit"
}

// falseHit is the one outcome that matters more than any savings number: a
// probe designed to be semantically close but factually different was matched
// to a DIFFERENT question and served that answer.
//
// Only semantic_hit counts. A probe returning "hit" is a C1 exact match,
// which means the identical prompt was asked before — on a re-run inside the
// C1 TTL that is the probe matching *itself*, which is correct behaviour, not
// a false positive. Conflating the two made a clean run report two spurious
// correctness failures. Use --cache-bust for a cold run.
func (o stepOutcome) falseHit() bool {
	return o.Step.Kind == stepProbe && o.CacheState == "semantic_hit"
}

// exactHit / semanticHit split the two caches apart, because they mean very
// different things in a demo: C1 proves nothing about semantic matching.
func (o stepOutcome) exactHit() bool    { return o.CacheState == "hit" }
func (o stepOutcome) semanticHit() bool { return o.CacheState == "semantic_hit" }

// cacheBustSuffix returns a per-RUN marker appended to every prompt in the
// run. It makes a re-run start from a cold cache — without it, a second run
// inside the C1 TTL sees even the seed step come back as an exact hit, which
// is correct but tells you nothing.
//
// One shared nonce for the whole run (not per step) is the point: prompts stay
// distinct from previous runs while the within-run seed→paraphrase→probe
// relationships that the demo depends on are preserved.
func cacheBustSuffix(g rng) string { return " (ref: " + g.hex(4) + ")" }

// runFlow executes one flow's steps in order against the gateway.
func runFlow(ctx context.Context, g rng, c *gatewayClient, f demoFlow, users []string, nonce string, dryRun bool) []stepOutcome {
	flowID := g.uuid()
	user := users[g.r.Intn(len(users))]
	headers := map[string]string{
		"TAS-Agent-Id":      agentIDFor("Demo Flow Runner"),
		"TAS-Agent-Name":    "Demo Flow Runner",
		"TAS-Agent-Version": "1.0.0",
		"TAS-Flow-Id":       flowID,
		"TAS-Source-App":    "aiqg-demo-flows",
		"baggage":           "user.id=" + user,
	}

	outcomes := make([]stepOutcome, 0, len(f.Steps))
	for i, s := range f.Steps {
		select {
		case <-ctx.Done():
			return outcomes
		default:
		}

		prompt := s.Prompt + nonce

		if dryRun {
			fmt.Printf("  %2d. %-10s %s\n", i+1, s.Kind, truncate(prompt, 80))
			outcomes = append(outcomes, stepOutcome{Step: s, CacheState: "(dry-run)"})
			continue
		}

		start := time.Now()
		res, err := c.do(ctx, sendOpts{
			Headers:     headers,
			Model:       f.Model,
			Prompt:      prompt,
			MaxTokens:   f.MaxTokens,
			Temperature: f.Temperature,
		})
		out := stepOutcome{Step: s, Status: res.Status, CacheState: res.CacheState, Latency: time.Since(start)}
		if err != nil {
			out.Err = err.Error()
		}
		if out.CacheState == "" {
			out.CacheState = "miss"
		}
		outcomes = append(outcomes, out)

		fmt.Printf("  %2d. %-10s %-13s %4dms  http=%d  %s\n",
			i+1, s.Kind, out.CacheState, out.Latency.Milliseconds(), out.Status, truncate(s.Prompt, 60))
	}
	return outcomes
}

// runFlowsTarget is the --target=flows entry point.
func runFlowsTarget(ctx context.Context, g rng, c *gatewayClient, ids []string, users []string, interval time.Duration, cacheBust, dryRun bool) {
	selected := flowCatalog
	if len(ids) > 0 {
		selected = nil
		for _, id := range ids {
			f, ok := flowByID(id)
			if !ok {
				fmt.Fprintf(os.Stderr, "error: unknown flow %q (see --print-catalog)\n", id)
				os.Exit(2)
			}
			selected = append(selected, f)
		}
	}

	pass := func() {
		nonce := ""
		if cacheBust {
			nonce = cacheBustSuffix(g)
			fmt.Printf("cold-start: appending %q to every prompt this run\n", nonce)
		}
		all := map[string][]stepOutcome{}
		for _, f := range selected {
			fmt.Printf("\n▶ %s — %s\n", f.Label, f.ID)
			all[f.ID] = runFlow(ctx, g, c, f, users, nonce, dryRun)
		}
		if !dryRun {
			printFlowSummary(selected, all)
		}
	}

	pass()
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nstopped.")
			return
		case <-ticker.C:
			pass()
		}
	}
}

// printFlowSummary reports per-flow hit rate against the modeled expectation,
// and — separately and loudly — any false hits. A false hit is not a smaller
// win, it is a wrong answer served to a user, so it never nets off against
// the savings.
func printFlowSummary(flows []demoFlow, all map[string][]stepOutcome) {
	// Exact and semantic hits are reported separately on purpose: C1 only ever
	// proves the identical bytes were seen before, so folding it into one
	// number would let a re-run inside the C1 TTL masquerade as semantic
	// matching working.
	fmt.Printf("\n%-28s %6s %6s %6s %9s %9s %s\n",
		"flow", "steps", "exact", "seman", "sem rate", "modeled", "probes")
	falseHits := 0
	for _, f := range flows {
		outs := all[f.ID]
		exact, semantic, probes, probesOK := 0, 0, 0, 0
		for _, o := range outs {
			if o.Step.Kind == stepProbe {
				probes++
				if o.falseHit() {
					falseHits++
				} else {
					probesOK++
				}
				continue // probes are correctness checks, not cache-rate samples
			}
			switch {
			case o.exactHit():
				exact++
			case o.semanticHit():
				semantic++
			}
		}
		cacheable := len(outs) - probes
		rate := 0.0
		if cacheable > 0 {
			rate = float64(exact+semantic) / float64(cacheable) * 100
		}
		fmt.Printf("%-28s %6d %6d %6d %8.0f%% %8.0f%% %d/%d rejected\n",
			f.ID, len(outs), exact, semantic, rate, f.ExpectedCacheHitPct, probesOK, probes)
	}

	if falseHits > 0 {
		fmt.Printf("\n!! %d FALSE HIT(S): a near-miss probe was semantically matched to a DIFFERENT question.\n", falseHits)
		fmt.Println("   This is a correctness failure, not a savings result — tighten")
		fmt.Println("   semantic.min_similarity for this tenant and re-run.")
		return
	}
	fmt.Println("\nAll near-miss probes correctly rejected.")
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// printCatalog dumps the flow catalog as JSON. This is the seam for the open
// question of whether aiqg-dashboard-be should serve the catalog so the Go
// generator and aiqg-ui stop hand-mirroring prompt lists.
func printCatalog() {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(flowCatalog)
}
