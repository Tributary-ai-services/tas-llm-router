package events

import (
	"encoding/json"
	"strings"
	"testing"
)

// Guards the Contract v2 (measured) JSON keys + pointer semantics. These
// are populated only when the real extractor runs (shadow/active); the
// dashboard-be reader and Spark aggregator depend on these exact names.
func TestTokenAccounting_MeasuredJSONContract(t *testing.T) {
	eff, asr := -0.01, 0.0
	ta := TokenAccounting{
		ReductionMode:                      "shadow",
		ReductionSampled:                   true,
		ActualDirectPayloadReductionTokens: 1200,
		ActualDirectPayloadReductionUSD:    0.0009,
		ActualReductionRelevanceUSD:        0.0008,
		ActualReductionSLMUSD:              0.0003,
		ReductionEfficacyDelta:             &eff,
		ReductionAssuranceDelta:            &asr,
	}
	b, _ := json.Marshal(ta)
	s := string(b)
	for _, k := range []string{
		"reduction_sampled", "actual_direct_payload_reduction_tokens",
		"actual_direct_payload_reduction_usd", "actual_reduction_relevance_usd",
		"actual_reduction_slm_usd", "reduction_efficacy_delta", "reduction_assurance_delta",
	} {
		if !strings.Contains(s, `"`+k+`"`) {
			t.Errorf("missing measured key %q in %s", k, s)
		}
	}
	// assurance delta is 0.0 (measured no change) — must round-trip via pointer.
	var back TokenAccounting
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ReductionAssuranceDelta == nil || *back.ReductionAssuranceDelta != 0.0 {
		t.Errorf("0.0 assurance delta must round-trip distinctly from absent")
	}

	// Projected-only (Contract v1) traffic omits all measured fields.
	v1, _ := json.Marshal(TokenAccounting{ReductionMode: "projected", ProjectedDirectPayloadWasteUSD: 0.001})
	if strings.Contains(string(v1), "actual_reduction") || strings.Contains(string(v1), "reduction_sampled") {
		t.Errorf("projected traffic must omit measured fields: %s", v1)
	}
}
