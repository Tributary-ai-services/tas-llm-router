package routing

import (
	"strings"
	"testing"
	"time"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// Real prices, so the arithmetic below is checkable against the vendors'
// published rates rather than against invented numbers.
func testPrices(provider, model string) (float64, float64, bool) {
	switch model {
	case "claude-haiku-4-5":
		return 0.001, 0.005, true
	case "claude-opus-4-6":
		return 0.015, 0.075, true
	case "gpt-4o-mini":
		return 0.00015, 0.0006, true
	}
	return 0, 0, false
}

func reqWithMax(n int) *types.ChatRequest {
	return &types.ChatRequest{MaxTokens: &n}
}

// ---------------------------------------------------------------------------
// The point of expected_cost: list price and expected price rank differently.
// ---------------------------------------------------------------------------

// Step 5's acceptance criterion. Opus is 15x Haiku on output price, but the
// measured verbosity gap is what decides a real bill — and on our own data Opus
// emits 451 tokens where Haiku emits 131.
func TestExpectedCostUsesMeasuredVerbosityNotTheCeiling(t *testing.T) {
	idx := newVerbosityIndex([]resilience.Verbosity{
		{Model: "claude-haiku-4-5", Workflow: "single_turn_qa", MeanOutputTokens: 131, Samples: 412},
		{Model: "claude-opus-4-6", Workflow: "single_turn_qa", MeanOutputTokens: 451, Samples: 300},
	}, 100)

	req := reqWithMax(4096)
	haiku := idx.estimate("anthropic", "claude-haiku-4-5", "single_turn_qa", req, 32, testPrices)
	opus := idx.estimate("anthropic", "claude-opus-4-6", "single_turn_qa", req, 32, testPrices)

	if !haiku.Measured || !opus.Measured {
		t.Fatal("estimates did not use the measured table")
	}
	// Priced at max_tokens both would use 4096 output tokens; the measured
	// figures are what make these comparable to a real bill.
	if haiku.ExpectedOut != 131 || opus.ExpectedOut != 451 {
		t.Fatalf("expected-out haiku=%v opus=%v, want the measured means", haiku.ExpectedOut, opus.ExpectedOut)
	}
	if !(haiku.Total < opus.Total) {
		t.Fatalf("haiku %v should be cheaper than opus %v", haiku.Total, opus.Total)
	}
}

// max_tokens stays in as a CAP even on the measured path: a model whose
// measured mean exceeds this request's cap cannot emit that many tokens here.
func TestMaxTokensCapsTheMeasuredExpectation(t *testing.T) {
	idx := newVerbosityIndex([]resilience.Verbosity{
		{Model: "claude-opus-4-6", MeanOutputTokens: 451, Samples: 300},
	}, 100)
	est := idx.estimate("anthropic", "claude-opus-4-6", "", reqWithMax(50), 32, testPrices)
	if est.ExpectedOut != 50 {
		t.Fatalf("expected-out = %v, want it capped at max_tokens 50", est.ExpectedOut)
	}
}

// ---------------------------------------------------------------------------
// Abstention. The failure this guards is a router that silently reverts to the
// behaviour it was meant to replace while an operator believes it is running.
// ---------------------------------------------------------------------------

func TestAbstainsBelowTheSampleFloorAndSaysSo(t *testing.T) {
	// 13 samples: the real gpt-4o-mini figure, which was entirely probe
	// traffic averaging three output tokens.
	idx := newVerbosityIndex([]resilience.Verbosity{
		{Model: "gpt-4o-mini", Workflow: "single_turn_qa", MeanOutputTokens: 3, Samples: 13},
	}, 100)
	est := idx.estimate("openai", "gpt-4o-mini", "single_turn_qa", reqWithMax(200), 14, testPrices)

	if est.Measured {
		t.Fatal("a 13-sample measurement was treated as usable")
	}
	if est.ExpectedOut != 200 {
		t.Fatalf("fallback expected-out = %v, want max_tokens", est.ExpectedOut)
	}
	// The reason must reach the event: silent fallback is the failure mode.
	if !strings.Contains(est.Reason, "no usable verbosity measurement") {
		t.Fatalf("reason %q does not record the abstention", est.Reason)
	}
}

func TestStaleMeasurementIsNotUsed(t *testing.T) {
	idx := newVerbosityIndex([]resilience.Verbosity{
		{Model: "claude-haiku-4-5", MeanOutputTokens: 131, Samples: 100000, Stale: true},
	}, 100)
	est := idx.estimate("anthropic", "claude-haiku-4-5", "", reqWithMax(300), 32, testPrices)
	if est.Measured {
		t.Fatal("a stale measurement was used; sample size does not make an old number current")
	}
}

func TestUnpricedCandidateIsReportedNotTreatedAsFree(t *testing.T) {
	idx := newVerbosityIndex(nil, 100)
	est := idx.estimate("cohere", "command", "", reqWithMax(100), 10, testPrices)
	if est.Total != 0 || !strings.Contains(est.Reason, "no price configured") {
		t.Fatalf("unpriced candidate: total=%v reason=%q", est.Total, est.Reason)
	}
	// And cheapest must skip it rather than picking it as the cheapest option.
	best, _ := cheapest([]costEstimate{
		est,
		{Provider: "anthropic", Model: "claude-haiku-4-5", Total: 0.001},
	})
	if best.Provider != "anthropic" {
		t.Fatalf("cheapest picked %q; an unpriced candidate must not read as free", best.Provider)
	}
}

// Workflow varies verbosity more than anything else — the same model measured
// 131 tokens on single_turn_qa and 285 on code_generation — but workflow is
// optional, so a tenant that never sets it must still get measurements.
func TestFallsBackToTheWorkflowAgnosticRow(t *testing.T) {
	idx := newVerbosityIndex([]resilience.Verbosity{
		{Model: "claude-haiku-4-5", Workflow: "", MeanOutputTokens: 200, Samples: 500},
		{Model: "claude-haiku-4-5", Workflow: "rag", MeanOutputTokens: 43, Samples: 400},
	}, 100)
	if got, _ := idx.lookup("claude-haiku-4-5", "rag"); got.MeanOutputTokens != 43 {
		t.Fatalf("workflow-specific row not preferred: %v", got.MeanOutputTokens)
	}
	if got, _ := idx.lookup("claude-haiku-4-5", "summarization"); got.MeanOutputTokens != 200 {
		t.Fatalf("did not fall back to the workflow-agnostic row: %v", got.MeanOutputTokens)
	}
}

// ---------------------------------------------------------------------------
// Hysteresis. Without it, expected_cost is the flapping machine §3.3 describes.
// ---------------------------------------------------------------------------

var now0 = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

func est(provider string, total float64) costEstimate {
	return costEstimate{Provider: provider, Model: "m", Total: total}
}

func TestSmallImprovementIsRefused(t *testing.T) {
	// 5% better. At a measured RAG prefix that needs 126 further requests to
	// repay one re-warm — a router chasing this never gets there.
	d := ShouldSwitch(resilience.Switching{MinImprovementPct: 25}, "k",
		est("openai", 1.00), est("anthropic", 0.95), false, nil, now0)
	if d.Allow {
		t.Fatal("a 5% improvement was allowed against a 25% threshold")
	}
	if !strings.Contains(d.Reason, "below the 25% threshold") {
		t.Fatalf("reason %q should state the threshold", d.Reason)
	}
}

func TestLargeImprovementIsAllowed(t *testing.T) {
	d := ShouldSwitch(resilience.Switching{MinImprovementPct: 25}, "k",
		est("openai", 1.00), est("anthropic", 0.50), false, nil, now0)
	if !d.Allow {
		t.Fatalf("a 50%% improvement was refused: %s", d.Reason)
	}
}

// The term that makes hysteresis honest: a switch must beat the alternative AND
// the cache rebuild it causes.
func TestWarmCacheRaisesTheBar(t *testing.T) {
	cfg := resilience.Switching{MinImprovementPct: 25, WarmCacheBiasPct: 15}
	// 30% better clears 25 but not 25+15.
	cold := ShouldSwitch(cfg, "k", est("openai", 1.00), est("anthropic", 0.70), false, nil, now0)
	warm := ShouldSwitch(cfg, "k", est("openai", 1.00), est("anthropic", 0.70), true, nil, now0)
	if !cold.Allow {
		t.Fatal("30% was refused with no warm cache at stake")
	}
	if warm.Allow {
		t.Fatal("30% was allowed while discarding a warm cache; the bar must rise to 40%")
	}
	// An operator who set 25% and sees 30% refused needs to know where the
	// extra came from, or the router looks broken.
	if !strings.Contains(warm.Reason, "warm prompt cache") {
		t.Fatalf("reason %q does not explain the raised threshold", warm.Reason)
	}
}

func TestDwellBlocksARapidSecondSwitch(t *testing.T) {
	cfg := resilience.Switching{MinImprovementPct: 10, DwellSeconds: 60}
	dwell := NewMemoryDwellStore()
	dwell.RecordSwitch("k", now0, time.Minute)

	// Overwhelmingly better, but 30 seconds after the last switch.
	d := ShouldSwitch(cfg, "k", est("openai", 1.00), est("anthropic", 0.10), false, dwell, now0.Add(30*time.Second))
	if d.Allow {
		t.Fatal("a switch was allowed inside the dwell window")
	}
	if !strings.Contains(d.Reason, "dwell window") {
		t.Fatalf("reason %q should name the dwell window", d.Reason)
	}
	// And permitted once the window has passed.
	if !ShouldSwitch(cfg, "k", est("openai", 1.00), est("anthropic", 0.10), false, dwell, now0.Add(90*time.Second)).Allow {
		t.Fatal("a switch was still blocked after the dwell window elapsed")
	}
}

// Staying put is the conservative answer when a comparison is impossible: the
// current provider is at least known to work.
func TestUnpricedSideRefusesToSwitch(t *testing.T) {
	d := ShouldSwitch(resilience.Switching{}, "k", est("openai", 0), est("anthropic", 0.5), false, nil, now0)
	if d.Allow || !strings.Contains(d.Reason, "no price") {
		t.Fatalf("allow=%v reason=%q", d.Allow, d.Reason)
	}
}

// ---------------------------------------------------------------------------
// Weighted / canary.
// ---------------------------------------------------------------------------

func TestWeightedRespectsTheSplit(t *testing.T) {
	weights := map[string]int{"anthropic": 90, "openai": 10}
	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		p, ok := weightedPick(weights, "conv-"+string(rune(i)), nil)
		if !ok {
			t.Fatal("no provider selected")
		}
		counts[p]++
	}
	share := float64(counts["openai"]) / 2000 * 100
	// Hash-based allocation is not a sampler, so allow a wide band; the point
	// is that a 10% arm gets roughly a tenth, not a half.
	if share < 4 || share > 18 {
		t.Fatalf("openai share = %.1f%%, want roughly 10%%", share)
	}
}

// A 90/10 split that reshuffles every turn gives every conversation a 10%
// chance of a cold cache on every request, instead of 10% of conversations
// running on the canary.
func TestWeightedIsStableForOneKey(t *testing.T) {
	weights := map[string]int{"anthropic": 90, "openai": 10}
	first, _ := weightedPick(weights, "conversation-42", nil)
	for i := 0; i < 50; i++ {
		got, _ := weightedPick(weights, "conversation-42", nil)
		if got != first {
			t.Fatal("the same key produced different providers; the split must be deterministic")
		}
	}
}

func TestWeightedSkipsIneligibleProviders(t *testing.T) {
	weights := map[string]int{"anthropic": 90, "openai": 10}
	only := func(p string) bool { return p == "openai" }
	for i := 0; i < 20; i++ {
		got, ok := weightedPick(weights, "k"+string(rune(i)), only)
		if !ok || got != "openai" {
			t.Fatalf("got %q; an ineligible provider must never be selected", got)
		}
	}
	// Nothing eligible must report failure rather than picking anyway.
	if _, ok := weightedPick(weights, "k", func(string) bool { return false }); ok {
		t.Fatal("a provider was selected with nothing eligible")
	}
}

func TestZeroWeightsSelectNothing(t *testing.T) {
	if _, ok := weightedPick(map[string]int{"a": 0, "b": 0}, "k", nil); ok {
		t.Fatal("all-zero weights selected a provider")
	}
	if _, ok := weightedPick(nil, "k", nil); ok {
		t.Fatal("empty weights selected a provider")
	}
}
