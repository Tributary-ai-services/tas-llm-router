package clear

import "testing"

func TestScoreReliability_Table(t *testing.T) {
	cases := []struct {
		name     string
		attempts int
		fallback bool
		want     Score
	}{
		{"clean_first_try", 1, false, 100},
		{"fallback_after_one_attempt_unusual", 1, true, 75},
		{"one_in_provider_retry", 2, false, 75},
		{"retry_plus_fallback", 2, true, 50},
		{"two_retries", 3, false, 50},
		{"two_retries_plus_fallback", 3, true, 25},
		{"chronic_4", 4, false, 0},
		{"chronic_4_plus_fallback_clamped", 4, true, 0}, // base 0 stays 0
		{"chronic_10", 10, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scoreReliability(Input{HTTPStatus: 200, AttemptCount: c.attempts, FallbackUsed: c.fallback})
			if s == nil {
				t.Fatalf("nil score")
			}
			if *s != c.want {
				t.Errorf("attempts=%d fallback=%v → %d, want %d", c.attempts, c.fallback, *s, c.want)
			}
		})
	}
}

func TestScoreReliability_NilCases(t *testing.T) {
	// HTTPStatus=0 → no attempt was made
	if s := scoreReliability(Input{HTTPStatus: 0, AttemptCount: 1}); s != nil {
		t.Errorf("HTTPStatus=0 should nil out, got %d", *s)
	}
	// AttemptCount=0 → routing metadata not surfaced
	if s := scoreReliability(Input{HTTPStatus: 200, AttemptCount: 0}); s != nil {
		t.Errorf("AttemptCount=0 should nil out, got %d", *s)
	}
}

func TestCompute_PopulatesReliability(t *testing.T) {
	s := Compute(Input{HTTPStatus: 200, AttemptCount: 1})
	if s.Reliability == nil || *s.Reliability != 100 {
		t.Errorf("Reliability=%v want=100", s.Reliability)
	}
	// Verify all five dimensions can co-populate (with the right inputs).
	full := Compute(Input{
		EndToEndMs: ptr64(2500), Workflow: "rag",
		HTTPStatus: 200,
		Vendor:     "openai", Model: "gpt-4o-mini",
		PromptTokens: intP(100), CompletionTokens: intP(50),
		AssuranceScanRan:          true,
		InboundFindingsBySeverity: map[string]int{},
		FinishReason:              "stop",
		AttemptCount:              1,
	})
	for name, p := range map[string]*Score{
		"Latency": full.Latency, "Cost": full.Cost, "Efficacy": full.Efficacy,
		"Assurance": full.Assurance, "Reliability": full.Reliability, "Composite": full.Composite,
	} {
		if p == nil {
			t.Errorf("%s nil — should populate with full inputs", name)
		}
	}
}
