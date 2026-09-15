package server

import "testing"

// A shadow replay spends the tenant's money on an evaluation they never
// directly asked for, so the rate is a consent question, not a tuning knob
// (#184). These pin the effective-rate rule and the gate itself.

func TestShadowSampled_ZeroOrNegativeIsOff(t *testing.T) {
	for _, pct := range []int{0, -1} {
		if shadowSampled("evt-1", pct) {
			t.Errorf("pct=%d must disable shadow-eval entirely", pct)
		}
	}
}

func TestShadowSampled_HundredTakesEverything(t *testing.T) {
	for _, id := range []string{"evt-1", "evt-2", "evt-3"} {
		if !shadowSampled(id, 100) {
			t.Errorf("pct=100 should sample %q", id)
		}
	}
}

// Deterministic on the event id: the same event must always land the same way,
// or a retried emit would double-bill a replay.
func TestShadowSampled_IsDeterministic(t *testing.T) {
	first := shadowSampled("evt-stable", 50)
	for i := 0; i < 20; i++ {
		if shadowSampled("evt-stable", 50) != first {
			t.Fatal("sampling is not stable for a fixed event id")
		}
	}
}

// The rate must actually vary the population, not just the extremes — a gate
// that ignored pct would still pass the 0 and 100 cases above.
func TestShadowSampled_RateChangesTheSampledSet(t *testing.T) {
	ids := make([]string, 0, 400)
	for i := 0; i < 400; i++ {
		ids = append(ids, string(rune('a'+i%26))+string(rune('0'+i%10))+"-"+string(rune('A'+i%7)))
	}
	count := func(pct int) int {
		n := 0
		for _, id := range ids {
			if shadowSampled(id, pct) {
				n++
			}
		}
		return n
	}
	low, high := count(10), count(90)
	if low >= high {
		t.Errorf("a higher rate must sample more: 10%%=%d, 90%%=%d", low, high)
	}
	// Sanity: 10% should be well under half, 90% well over.
	if low > len(ids)/2 {
		t.Errorf("10%% sampled %d of %d — far too many", low, len(ids))
	}
	if high < len(ids)/2 {
		t.Errorf("90%% sampled %d of %d — far too few", high, len(ids))
	}
}

// effectiveShadowPct is the rule stated in shadowEval: a declared guardrail
// wins, an absent one inherits the gateway default. Asserted here as a table
// so the precedence can't drift silently.
func TestEffectiveShadowPct_DeclaredWinsAbsentInherits(t *testing.T) {
	ptrTo := func(i int) *int { return &i }
	cases := []struct {
		name     string
		declared *int
		global   int
		want     int
	}{
		{"declared overrides the global", ptrTo(25), 5, 25},
		{"declared zero disables even when the global is on", ptrTo(0), 50, 0},
		{"absent inherits the global", nil, 5, 5},
		{"absent with global off stays off", nil, 0, 0},
		{"declared raises above a zero global", ptrTo(10), 0, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.global
			if tc.declared != nil {
				got = *tc.declared
			}
			if got != tc.want {
				t.Errorf("effective pct = %d, want %d", got, tc.want)
			}
		})
	}
}
