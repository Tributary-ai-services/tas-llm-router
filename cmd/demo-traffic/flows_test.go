package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEveryFlowIsCacheEligible is the regression guard that matters most.
//
// responsecache.Decide refuses to cache a request unless it carries an
// explicit seed or temperature == 0. Before these flows existed the generator
// set neither, so every request it ever sent was classified nondeterministic
// and silently skipped both C1 and C4 — a caching demo that could never
// produce a hit. If someone drops Temperature from a flow, or switches it to
// a non-pointer field where 0 gets elided by omitempty, this fails.
func TestEveryFlowIsCacheEligible(t *testing.T) {
	for _, f := range flowCatalog {
		if f.Temperature == nil && f.Steps != nil {
			t.Errorf("flow %q: Temperature is nil — request would be rejected as nondeterministic and never cached", f.ID)
			continue
		}
		if *f.Temperature != 0 {
			t.Errorf("flow %q: Temperature = %v, want 0 (or an explicit seed) for cache eligibility", f.ID, *f.Temperature)
		}
	}
}

// TestTemperatureSurvivesJSON pins the pointer-vs-omitempty subtlety: a plain
// `float64` with omitempty would drop temperature=0 from the wire entirely,
// which is indistinguishable from never setting it.
func TestTemperatureSurvivesJSON(t *testing.T) {
	body, err := json.Marshal(chatRequest{
		Model:       "m",
		Messages:    []chatMessage{{Role: "user", Content: "hi"}},
		MaxTokens:   16,
		Temperature: f64(0),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"temperature":0`) {
		t.Errorf("temperature=0 was dropped from the request body: %s", body)
	}
}

func TestCatalogWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range flowCatalog {
		if f.ID == "" {
			t.Error("flow with empty ID")
		}
		if seen[f.ID] {
			t.Errorf("duplicate flow id %q", f.ID)
		}
		seen[f.ID] = true

		if f.Label == "" || f.WhatItShows == "" {
			t.Errorf("flow %q: Label and WhatItShows are user-facing and must be set", f.ID)
		}
		if f.Model == "" || f.MaxTokens <= 0 {
			t.Errorf("flow %q: Model/MaxTokens unset", f.ID)
		}
		if len(f.Steps) == 0 {
			t.Errorf("flow %q: no steps", f.ID)
		}
		for i, s := range f.Steps {
			if strings.TrimSpace(s.Prompt) == "" {
				t.Errorf("flow %q step %d: empty prompt", f.ID, i+1)
			}
			if strings.TrimSpace(s.Note) == "" {
				t.Errorf("flow %q step %d: empty note — the note is what the UI shows to explain the step", f.ID, i+1)
			}
		}
	}
}

// TestStepKindsAreCoherent checks that each step kind can actually do what it
// claims, since the whole demo rests on the sequence being meaningful:
//   - a `repeat` must be byte-identical to an earlier prompt, or it cannot
//     produce a C1 exact hit;
//   - a `paraphrase` must NOT be byte-identical to an earlier prompt, or it
//     would hit C1 and demonstrate nothing about semantic matching;
//   - a `probe` must not be byte-identical to an earlier prompt either, or it
//     would legitimately hit and the "must miss" assertion would be wrong.
func TestStepKindsAreCoherent(t *testing.T) {
	for _, f := range flowCatalog {
		seen := map[string]bool{}
		for i, s := range f.Steps {
			dup := seen[s.Prompt]
			switch s.Kind {
			case stepRepeat:
				if !dup {
					t.Errorf("flow %q step %d: kind=repeat but prompt is new — cannot be a C1 exact hit", f.ID, i+1)
				}
			case stepParaphrase:
				if dup {
					t.Errorf("flow %q step %d: kind=paraphrase but prompt is byte-identical to an earlier step — that is an exact hit, not a semantic one", f.ID, i+1)
				}
			case stepProbe:
				if dup {
					t.Errorf("flow %q step %d: kind=probe but prompt repeats an earlier step — it would legitimately hit", f.ID, i+1)
				}
			}
			seen[s.Prompt] = true
		}
	}
}

// TestCacheDemoFlowsHaveSeedAndProbe: any flow claiming cache value must both
// populate the cache and prove it does not over-fire. A savings demo without
// a near-miss probe is a demo you cannot trust.
func TestCacheDemoFlowsHaveSeedAndProbe(t *testing.T) {
	for _, f := range flowCatalog {
		if f.ExpectedCacheHitPct < 10 {
			continue // reduction-only or negative-control flows are exempt
		}
		var seeds, hitters, probes int
		for _, s := range f.Steps {
			switch s.Kind {
			case stepSeed:
				seeds++
			case stepParaphrase, stepRepeat:
				hitters++
			case stepProbe:
				probes++
			}
		}
		if seeds == 0 {
			t.Errorf("flow %q expects %.0f%% cache hits but has no seed step", f.ID, f.ExpectedCacheHitPct)
		}
		if hitters == 0 {
			t.Errorf("flow %q expects cache hits but has no repeat/paraphrase step", f.ID)
		}
		if probes == 0 {
			t.Errorf("flow %q expects cache hits but has no near-miss probe to prove it does not over-fire", f.ID)
		}
	}
}

// TestNegativeControlStaysNegative guards the honesty exhibit. If someone
// "improves" the coding flow by adding a repeat so it posts a nicer number,
// it stops being a negative control and the demo starts lying.
func TestNegativeControlStaysNegative(t *testing.T) {
	f, ok := flowByID("coding-agent")
	if !ok {
		t.Fatal("coding-agent flow missing — it is the negative control and must stay in the catalog")
	}
	if f.ExpectedCacheHitPct != 0 {
		t.Errorf("coding-agent ExpectedCacheHitPct = %v, want 0", f.ExpectedCacheHitPct)
	}
	if f.ExpectedReductionPct > 1 {
		t.Errorf("coding-agent ExpectedReductionPct = %v, want <=1 (coding is ~2%% MCP-addressable)", f.ExpectedReductionPct)
	}
	for i, s := range f.Steps {
		if s.Kind != stepUnique {
			t.Errorf("coding-agent step %d: kind=%s, want unique — every request must be genuinely new", i+1, s.Kind)
		}
	}
}

// TestReductionFlowIsInputHeavy: reduction pays on long-context, narrow-ask
// requests. If the contract prompts shrink, the flow stops demonstrating the
// thing it exists to demonstrate.
func TestReductionFlowIsInputHeavy(t *testing.T) {
	f, ok := flowByID("contract-review")
	if !ok {
		t.Fatal("contract-review flow missing")
	}
	for i, s := range f.Steps {
		if len(s.Prompt) < 4000 {
			t.Errorf("contract-review step %d: prompt is %d chars, want >=4000 — reduction needs an input-heavy shape", i+1, len(s.Prompt))
		}
	}
}

func TestContractPromptsAreDistinct(t *testing.T) {
	a := contractPrompt("Alpha", "30 days", "1%")
	b := contractPrompt("Beta", "60 days", "2%")
	if a == b {
		t.Error("contractPrompt returned identical text for different inputs — the flow would cache-hit itself")
	}
	if !strings.Contains(a, "Alpha") || !strings.Contains(a, "30 days") || !strings.Contains(a, "1%") {
		t.Error("contractPrompt dropped one of its parameters")
	}
}

func TestSplitFlowIDs(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{" a , b ,, c ", []string{"a", "b", "c"}},
	} {
		got := splitFlowIDs(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitFlowIDs(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitFlowIDs(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

func TestFlowByID(t *testing.T) {
	if _, ok := flowByID("nope"); ok {
		t.Error("flowByID found a flow that does not exist")
	}
	f, ok := flowByID("it-helpdesk")
	if !ok {
		t.Fatal("flowByID could not find it-helpdesk")
	}
	if f.ID != "it-helpdesk" {
		t.Errorf("flowByID returned %q", f.ID)
	}
}

func TestStepOutcomeClassification(t *testing.T) {
	probe := flowStep{Kind: stepProbe, Prompt: "p", Note: "n"}
	para := flowStep{Kind: stepParaphrase, Prompt: "p2", Note: "n"}

	for _, tc := range []struct {
		name          string
		out           stepOutcome
		hit, falseHit bool
	}{
		// An exact hit on a probe means the identical prompt was seen before —
		// on a re-run inside the C1 TTL that is the probe matching ITSELF,
		// which is correct. Only a semantic hit means it was matched to a
		// different question, which is the actual correctness failure.
		{"probe served from exact cache matched itself, not a false hit", stepOutcome{Step: probe, CacheState: "hit"}, true, false},
		{"probe served from semantic cache is a false hit", stepOutcome{Step: probe, CacheState: "semantic_hit"}, true, true},
		{"probe that missed is correct", stepOutcome{Step: probe, CacheState: "miss"}, false, false},
		{"paraphrase hit is a win, not a false hit", stepOutcome{Step: para, CacheState: "semantic_hit"}, true, false},
		{"bypass is not a hit", stepOutcome{Step: para, CacheState: "bypass"}, false, false},
	} {
		if got := tc.out.hit(); got != tc.hit {
			t.Errorf("%s: hit() = %v, want %v", tc.name, got, tc.hit)
		}
		if got := tc.out.falseHit(); got != tc.falseHit {
			t.Errorf("%s: falseHit() = %v, want %v", tc.name, got, tc.falseHit)
		}
	}
}

// The retrieval flow is the ONLY one that can demonstrate applied reduction,
// so its wiring is worth pinning: reduction happens at the MCP proxy on tool
// results, which means a flow with no retrieval spec can never apply any.
func TestRetrievalFlowIsWiredForAppliedReduction(t *testing.T) {
	f, ok := flowByID("research-rag")
	if !ok {
		t.Fatal("research-rag flow missing — it is the only applied-reduction demo")
	}
	if f.Retrieval == nil {
		t.Fatal("research-rag has no retrieval spec, so it cannot apply reduction")
	}
	if f.Retrieval.Server == "" || f.Retrieval.Tool == "" || f.Retrieval.QueryArg == "" {
		t.Errorf("retrieval spec incomplete: %+v", *f.Retrieval)
	}
	// spec.reduce is per FederatedMCPServer; naming a server without it would
	// silently pass tool results through unreduced.
	if f.Retrieval.Server != "paper-search-mcp" {
		t.Errorf("retrieval server = %q; confirm spec.reduce:true on it before changing", f.Retrieval.Server)
	}
}

// Every other flow must NOT claim retrieval it does not do.
func TestOnlyRetrievalFlowsDeclareRetrieval(t *testing.T) {
	for _, f := range flowCatalog {
		if f.ID == "research-rag" {
			continue
		}
		if f.Retrieval != nil {
			t.Errorf("flow %q declares retrieval but is not a retrieval flow", f.ID)
		}
	}
}
