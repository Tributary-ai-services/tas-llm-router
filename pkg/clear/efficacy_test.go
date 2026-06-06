package clear

import "testing"

func TestScoreEfficacy_FinishReasonMap(t *testing.T) {
	cases := []struct {
		reason string
		want   Score
	}{
		// OpenAI vocabulary
		{"stop", 100},
		{"tool_calls", 100},
		{"function_call", 100},
		{"length", 60},
		{"content_filter", 0},

		// Anthropic vocabulary (normalized onto the same table)
		{"end_turn", 100},
		{"stop_sequence", 100},
		{"tool_use", 100},
		{"max_tokens", 60},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			s := scoreEfficacy(Input{HTTPStatus: 200, FinishReason: c.reason})
			if s == nil {
				t.Fatalf("nil for %q", c.reason)
			}
			if *s != c.want {
				t.Errorf("%q → %d, want %d", c.reason, *s, c.want)
			}
		})
	}
}

// Empty finish_reason → nil (vendor didn't return one). Distinct from
// any of the documented values.
func TestScoreEfficacy_EmptyIsNil(t *testing.T) {
	if s := scoreEfficacy(Input{HTTPStatus: 200, FinishReason: ""}); s != nil {
		t.Errorf("empty FinishReason should produce nil, got %d", *s)
	}
}

// Unknown finish_reason (emerging vendor value we haven't mapped yet)
// → nil rather than a guess. Lets dashboards surface the unmapped
// value and prompt a code update.
//
// "STOP" (uppercase) is the Google Gemini convention — not yet
// normalized, kept here as a forward-compat reminder that the next
// vendor wiring needs to extend normalizeFinishReason.
func TestScoreEfficacy_UnknownIsNil(t *testing.T) {
	for _, r := range []string{"unknown_value", "STOP", "MAX_TOKENS", "SAFETY"} {
		if s := scoreEfficacy(Input{HTTPStatus: 200, FinishReason: r}); s != nil {
			t.Errorf("unknown %q should produce nil, got %d", r, *s)
		}
	}
}

// HTTPStatus=0 (gateway-blocked) — no vendor response, no finish_reason
// signal, nil score.
func TestScoreEfficacy_GatewayBlocked(t *testing.T) {
	if s := scoreEfficacy(Input{HTTPStatus: 0, FinishReason: "stop"}); s != nil {
		t.Errorf("HTTPStatus=0 should nil out, got %d", *s)
	}
}

// Compute end-to-end: FinishReason wires through.
func TestCompute_PopulatesEfficacy(t *testing.T) {
	s := Compute(Input{HTTPStatus: 200, FinishReason: "length"})
	if s.Efficacy == nil || *s.Efficacy != 60 {
		t.Errorf("Efficacy=%v want=60", s.Efficacy)
	}
}
