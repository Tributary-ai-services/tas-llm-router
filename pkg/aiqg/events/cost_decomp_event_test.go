package events

import (
	"encoding/json"
	"strings"
	"testing"
)

// Guards the Contract v1 JSON keys + CER pointer semantics that the
// dashboard-be Loki queries and Spark aggregator depend on. A rename or
// retype here is a breaking contract change.
func TestTokenAccounting_CostDecompJSONContract(t *testing.T) {
	cer := 0.42
	ta := TokenAccounting{
		PromptTokens: 1000, CompletionTokens: 200, TotalTokens: 1200,
		TotalCostUSD: 0.0075, ActualCostUSD: 0.0075, ActualCostSource: "vendor_usage",
		ReductionMode:                     "projected",
		ProjectedDirectPayloadWasteTokens: 300, ProjectedDirectPayloadWasteUSD: 0.0007,
		DirectPayloadWasteTokens: 300, DirectPayloadWasteUSD: 0.0007,
		ProjectedReductionRelevanceUSD: 0.0007, ProjectedReductionRelevanceConfidence: "medium",
		ProjectedReductionSLMUSD: 0.0006, ProjectedReductionSLMConfidence: "low",
		ProjectedReductionCombinedUSD: 0.0011,
		GenuinePostModelWasteUSD:      0,
		GatewayAddressablePct:         9.3,
		ContextEfficiencyRatio:        &cer,
	}
	b, err := json.Marshal(ta)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, key := range []string{
		"reduction_mode", "actual_cost_usd", "actual_cost_source",
		"projected_direct_payload_waste_tokens", "projected_direct_payload_waste_usd",
		"direct_payload_waste_tokens", "direct_payload_waste_usd",
		"projected_reduction_relevance_usd", "projected_reduction_relevance_confidence",
		"projected_reduction_slm_usd", "projected_reduction_slm_confidence",
		"projected_reduction_combined_usd", "gateway_addressable_pct", "context_efficiency_ratio",
	} {
		if !strings.Contains(s, `"`+key+`"`) {
			t.Errorf("missing contract key %q in %s", key, s)
		}
	}

	// CER pointer: 0.0 is meaningful and must round-trip distinctly from absent.
	var back TokenAccounting
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ContextEfficiencyRatio == nil || *back.ContextEfficiencyRatio != 0.42 {
		t.Errorf("CER pointer did not round-trip: %v", back.ContextEfficiencyRatio)
	}

	// Empty decomposition (unpriced traffic) omits the fields entirely.
	empty, _ := json.Marshal(TokenAccounting{PromptTokens: 5, TotalCostUSD: 0})
	if strings.Contains(string(empty), "reduction_mode") || strings.Contains(string(empty), "context_efficiency_ratio") {
		t.Errorf("unpriced TokenAccounting must omit decomposition fields: %s", empty)
	}
}
