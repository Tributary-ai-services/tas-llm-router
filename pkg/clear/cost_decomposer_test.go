package clear

import (
	"math"
	"testing"
)

func ip(v int) *int { return &v }

func TestActualCost(t *testing.T) {
	// gpt-4o: input 0.00250/1k, output 0.01000/1k.
	c := ActualCost("openai", "gpt-4o", 1000, 500, true)
	if !c.Priced {
		t.Fatal("expected priced")
	}
	if math.Abs(c.InputUSD-0.0025) > 1e-9 || math.Abs(c.OutputUSD-0.005) > 1e-9 {
		t.Errorf("input=%v output=%v", c.InputUSD, c.OutputUSD)
	}
	if math.Abs(c.TotalUSD-0.0075) > 1e-9 {
		t.Errorf("total=%v want 0.0075", c.TotalUSD)
	}
	if c.Source != "vendor_usage" {
		t.Errorf("source=%q want vendor_usage", c.Source)
	}
	if ActualCost("openai", "gpt-4o", 1, 1, false).Source != "computed" {
		t.Error("expected computed source when usageFromVendor=false")
	}
	if ActualCost("openai", "no-such-model", 1, 1, true).Priced {
		t.Error("unpriced model should return Priced=false")
	}
}

func TestDecompose_ModeAndInducedAndCER(t *testing.T) {
	in := Input{Vendor: "openai", Model: "gpt-4o", PromptTokens: ip(1000), CompletionTokens: ip(1000), HTTPStatus: 200}
	d := DecomposeCost(in, ActualCost("openai", "gpt-4o", 1000, 1000, true))
	if d.ReductionMode != "projected" {
		t.Errorf("mode=%q want projected", d.ReductionMode)
	}
	if d.InducedOutputWasteEstimatedUSD != 0 {
		t.Errorf("induced must be 0 in v1, got %v", d.InducedOutputWasteEstimatedUSD)
	}
	if d.ContextEfficiencyRatio == nil {
		t.Fatal("CER must be set")
	}
	// completion==prompt → r=1 → CER=1/(1+0.5)=0.667
	if got := *d.ContextEfficiencyRatio; math.Abs(got-0.6667) > 0.001 {
		t.Errorf("CER=%v want ~0.667", got)
	}
}

func TestDecompose_BloatDiscountsCER(t *testing.T) {
	base := Input{Vendor: "openai", Model: "gpt-4o", PromptTokens: ip(1000), CompletionTokens: ip(1000), HTTPStatus: 200}
	bloat := base
	bloat.InboundBloatFindings = 3
	a := ActualCost("openai", "gpt-4o", 1000, 1000, true)
	cerBase := *DecomposeCost(base, a).ContextEfficiencyRatio
	cerBloat := *DecomposeCost(bloat, a).ContextEfficiencyRatio
	if !(cerBloat < cerBase) {
		t.Errorf("bloat should lower CER: base=%v bloat=%v", cerBase, cerBloat)
	}
}

func TestDecompose_DirectWaste(t *testing.T) {
	// Low CER (tiny output) → most input is droppable.
	in := Input{Vendor: "openai", Model: "gpt-4o", PromptTokens: ip(10000), CompletionTokens: ip(10), HTTPStatus: 200}
	a := ActualCost("openai", "gpt-4o", 10000, 10, true)
	d := DecomposeCost(in, a)
	cer := *d.ContextEfficiencyRatio
	wantUSD := a.InputUSD * (1 - cer)
	if math.Abs(d.DirectPayloadWasteUSD-wantUSD) > 1e-9 {
		t.Errorf("direct usd=%v want %v", d.DirectPayloadWasteUSD, wantUSD)
	}
	wantTokens := int(math.Round(10000 * (1 - cer)))
	if d.DirectPayloadWasteTokens != wantTokens {
		t.Errorf("direct tokens=%d want %d", d.DirectPayloadWasteTokens, wantTokens)
	}
	// relevance == direct; combined >= relevance and >= slm.
	if math.Abs(d.ProjectedReductionRelevanceUSD-d.DirectPayloadWasteUSD) > 1e-9 {
		t.Errorf("relevance should equal direct waste")
	}
	if d.ProjectedReductionCombinedUSD < d.ProjectedReductionRelevanceUSD-1e-9 ||
		d.ProjectedReductionCombinedUSD < d.ProjectedReductionSLMUSD-1e-9 {
		t.Errorf("combined %v should dominate relevance %v / slm %v",
			d.ProjectedReductionCombinedUSD, d.ProjectedReductionRelevanceUSD, d.ProjectedReductionSLMUSD)
	}
}

func TestDecompose_GenuineOnFailure(t *testing.T) {
	// HTTP 500 → whole spend is genuine waste; direct clamps to 0.
	in := Input{Vendor: "openai", Model: "gpt-4o", PromptTokens: ip(1000), CompletionTokens: ip(100), HTTPStatus: 500}
	a := ActualCost("openai", "gpt-4o", 1000, 100, true)
	d := DecomposeCost(in, a)
	if math.Abs(d.GenuinePostModelWasteUSD-a.TotalUSD) > 1e-9 {
		t.Errorf("genuine=%v want total %v", d.GenuinePostModelWasteUSD, a.TotalUSD)
	}
	if d.DirectPayloadWasteUSD != 0 || d.DirectPayloadWasteTokens != 0 {
		t.Errorf("direct must clamp to 0 on full-genuine failure, got usd=%v tok=%d", d.DirectPayloadWasteUSD, d.DirectPayloadWasteTokens)
	}
}

func TestDecompose_GenuineOnOutboundFindings(t *testing.T) {
	in := Input{Vendor: "openai", Model: "gpt-4o", PromptTokens: ip(1000), CompletionTokens: ip(1000), HTTPStatus: 200,
		OutboundFindingsBySeverity: map[string]int{"high": 1}}
	a := ActualCost("openai", "gpt-4o", 1000, 1000, true)
	d := DecomposeCost(in, a)
	if math.Abs(d.GenuinePostModelWasteUSD-a.OutputUSD) > 1e-9 {
		t.Errorf("genuine=%v want output %v", d.GenuinePostModelWasteUSD, a.OutputUSD)
	}
}

// TestDecompose_BoundInvariant — the Go canary for the SQL CHECK:
// direct + induced + genuine ≤ actual.TotalUSD, across varied inputs.
func TestDecompose_BoundInvariant(t *testing.T) {
	prompts := []int{0, 1, 100, 5000, 100000}
	comps := []int{0, 1, 50, 5000}
	statuses := []int{200, 400, 500}
	for _, p := range prompts {
		for _, comp := range comps {
			for _, st := range statuses {
				for _, bloat := range []int{0, 5} {
					in := Input{Vendor: "openai", Model: "gpt-4o", PromptTokens: ip(p), CompletionTokens: ip(comp), HTTPStatus: st, InboundBloatFindings: bloat}
					a := ActualCost("openai", "gpt-4o", p, comp, true)
					d := DecomposeCost(in, a)
					sum := d.DirectPayloadWasteUSD + d.InducedOutputWasteEstimatedUSD + d.GenuinePostModelWasteUSD
					if sum > a.TotalUSD+1e-9 {
						t.Errorf("invariant violated p=%d c=%d st=%d bloat=%d: sum=%v > total=%v", p, comp, st, bloat, sum, a.TotalUSD)
					}
					if d.GatewayAddressablePct < 0 || d.GatewayAddressablePct > 100.0001 {
						t.Errorf("addressable pct out of range: %v", d.GatewayAddressablePct)
					}
				}
			}
		}
	}
}
