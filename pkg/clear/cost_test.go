package clear

import "testing"

func intP(v int) *int { return &v }

func TestLookupPricing_Known(t *testing.T) {
	in, out, ok := LookupPricing("openai", "gpt-4o-mini")
	if !ok {
		t.Fatalf("expected gpt-4o-mini to be priced")
	}
	if in != 0.00015 || out != 0.00060 {
		t.Errorf("gpt-4o-mini rates: in=%v out=%v", in, out)
	}
}

func TestLookupPricing_UnknownReturnsFalse(t *testing.T) {
	for _, c := range []struct{ vendor, model string }{
		{"openai", "gpt-99-imaginary"},
		{"unknown-vendor", "gpt-4o-mini"},
		{"", "gpt-4o-mini"},
		{"openai", ""},
		{"", ""},
	} {
		_, _, ok := LookupPricing(c.vendor, c.model)
		if ok {
			t.Errorf("expected !ok for %q:%q", c.vendor, c.model)
		}
	}
}

func TestDollarCost(t *testing.T) {
	// gpt-4o-mini: input $0.00015/1k, output $0.00060/1k
	// 1000 prompt + 500 completion = $0.00015 + $0.00030 = $0.00045
	cost, ok := DollarCost("openai", "gpt-4o-mini", 1000, 500)
	if !ok {
		t.Fatalf("expected priced")
	}
	want := 0.00045
	if cost < want*0.999 || cost > want*1.001 {
		t.Errorf("DollarCost=%v want=%v", cost, want)
	}

	if _, ok := DollarCost("unknown", "model", 100, 100); ok {
		t.Errorf("unknown model should not price")
	}
}

func TestScoreCost_Curve(t *testing.T) {
	// Boundary checks for the documented curve points using gpt-4o-mini
	// rates. We feed prompt-only tokens (output rate 4× input) so the
	// math stays predictable: cost = prompt_tokens × 0.00015/1000.
	//
	//   cost $0.001   → 100  → need promptTokens ≈ 6,667
	//   cost $0.01    → 75   → ≈ 66,667
	//   cost $0.10    → 50   → ≈ 666,667
	//   cost $1.00    → 25   → ≈ 6,666,667
	//   cost $10+     → 0    → ≥ 66,666,667
	cases := []struct {
		name   string
		prompt int
		wantLo Score
		wantHi Score
	}{
		{"sub_001_dollar_clamps_to_100", 100, 100, 100},
		{"around_001_dollar", 6667, 99, 100},
		{"around_01_dollar", 66_667, 74, 76},
		{"around_10_cents", 666_667, 49, 51},
		{"around_1_dollar", 6_666_667, 24, 26},
		{"around_10_dollars_clamps_to_0", 66_666_667, 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scoreCost(Input{
				Vendor:       "openai",
				Model:        "gpt-4o-mini",
				PromptTokens: intP(c.prompt),
				HTTPStatus:   200,
			})
			if s == nil {
				t.Fatalf("score was nil")
			}
			if *s < c.wantLo || *s > c.wantHi {
				t.Errorf("score=%d want in [%d,%d]", *s, c.wantLo, c.wantHi)
			}
		})
	}
}

func TestScoreCost_NilCases(t *testing.T) {
	// HTTPStatus=0 (gateway-blocked) — must skip.
	if s := scoreCost(Input{Vendor: "openai", Model: "gpt-4o-mini", PromptTokens: intP(100), HTTPStatus: 0}); s != nil {
		t.Errorf("HTTPStatus=0 should nil out cost score, got %d", *s)
	}
	// Both token counts nil — must skip.
	if s := scoreCost(Input{Vendor: "openai", Model: "gpt-4o-mini", HTTPStatus: 200}); s != nil {
		t.Errorf("nil token counts should nil out, got %d", *s)
	}
	// Both counts zero — also skip (no usage to score).
	zero := 0
	if s := scoreCost(Input{Vendor: "openai", Model: "gpt-4o-mini", PromptTokens: &zero, CompletionTokens: &zero, HTTPStatus: 200}); s != nil {
		t.Errorf("0+0 tokens should nil out, got %d", *s)
	}
	// Unknown vendor:model — must skip (don't guess).
	if s := scoreCost(Input{Vendor: "openai", Model: "made-up-2026", PromptTokens: intP(1000), HTTPStatus: 200}); s != nil {
		t.Errorf("unknown model should nil out, got %d", *s)
	}
}

func TestPricingVersionPresent(t *testing.T) {
	if PricingVersion == "" {
		t.Fatalf("PricingVersion must not be empty — re-score jobs filter on it")
	}
}

// Compute end-to-end now populates Cost when inputs are present.
func TestCompute_PopulatesCostAndComposite(t *testing.T) {
	s := Compute(Input{
		EndToEndMs:       ptr64(2500), // under rag SLA → Latency=100
		Workflow:         "rag",
		HTTPStatus:       200,
		Vendor:           "openai",
		Model:            "gpt-4o-mini",
		PromptTokens:     intP(1000),
		CompletionTokens: intP(500),
	})
	if s.Latency == nil || *s.Latency != 100 {
		t.Errorf("Latency=%v want=100", s.Latency)
	}
	if s.Cost == nil {
		t.Fatalf("Cost nil despite vendor/model/tokens")
	}
	// 1k+0.5k for gpt-4o-mini = $0.00045 → ~$0.0005, well in Healthy.
	if *s.Cost < 90 {
		t.Errorf("Cost=%d, expected >=90 for cheap request", *s.Cost)
	}
	if s.Composite == nil {
		t.Fatalf("Composite nil")
	}
	// (Latency + Cost) / 2 ≈ (100 + ~108-clamped-to-100) / 2 = 100
	if *s.Composite != 100 {
		t.Errorf("Composite=%d want=100", *s.Composite)
	}
}
