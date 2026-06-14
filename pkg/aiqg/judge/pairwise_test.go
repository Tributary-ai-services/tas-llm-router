package judge

import (
	"context"
	"testing"
)

func TestScorePairwise_VariantWinsEitherSlot(t *testing.T) {
	// variantFirst=true → variant is slot A; judge picks A → variant wins.
	f := &fakeLLM{reply: `{"winner":"A"}`}
	j := &Judge{LLM: f, Model: "judge"}
	r, err := j.ScorePairwise(context.Background(), "single_turn_qa", "p", "control resp", "variant resp", true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Winner != "variant" || r.VariantPreference != 1.0 {
		t.Errorf("variantFirst + winner A should be variant win, got %+v", r)
	}

	// variantFirst=false → variant is slot B; judge picks B → variant wins.
	f2 := &fakeLLM{reply: `{"winner":"B"}`}
	j2 := &Judge{LLM: f2, Model: "judge"}
	r2, _ := j2.ScorePairwise(context.Background(), "single_turn_qa", "p", "control resp", "variant resp", false)
	if r2.Winner != "variant" || r2.VariantPreference != 1.0 {
		t.Errorf("!variantFirst + winner B should be variant win, got %+v", r2)
	}
}

func TestScorePairwise_ControlWins(t *testing.T) {
	// variantFirst=true → control is slot B; judge picks B → control wins.
	f := &fakeLLM{reply: `{"winner":"B"}`}
	j := &Judge{LLM: f, Model: "judge"}
	r, _ := j.ScorePairwise(context.Background(), "rag", "p", "control resp", "variant resp", true)
	if r.Winner != "control" || r.VariantPreference != 0.0 {
		t.Errorf("expected control win (pref 0), got %+v", r)
	}
}

func TestScorePairwise_Tie(t *testing.T) {
	f := &fakeLLM{reply: `{"winner":"tie"}`}
	j := &Judge{LLM: f, Model: "judge"}
	r, _ := j.ScorePairwise(context.Background(), "single_turn_qa", "p", "c", "v", false)
	if r.Winner != "tie" || r.VariantPreference != 0.5 {
		t.Errorf("expected tie (pref 0.5), got %+v", r)
	}
}

func TestScorePairwise_AbstainAndGarble(t *testing.T) {
	for _, reply := range []string{`{"abstain":true}`, "no idea honestly", `{}`} {
		f := &fakeLLM{reply: reply}
		j := &Judge{LLM: f, Model: "judge"}
		r, err := j.ScorePairwise(context.Background(), "single_turn_qa", "p", "c", "v", true)
		if err != nil {
			t.Fatalf("reply %q errored: %v", reply, err)
		}
		if !r.Abstain {
			t.Errorf("reply %q should abstain, got %+v", reply, r)
		}
	}
}

func TestParsePairwise_SlotMapping(t *testing.T) {
	// Direct table over (winner, variantFirst) → preference.
	cases := []struct {
		winner       string
		variantFirst bool
		wantPref     float64
		wantWinner   string
	}{
		{"A", true, 1.0, "variant"},
		{"A", false, 0.0, "control"},
		{"B", true, 0.0, "control"},
		{"B", false, 1.0, "variant"},
	}
	for _, c := range cases {
		r := parsePairwise(`{"winner":"`+c.winner+`"}`, "single_turn_qa", c.variantFirst)
		if r.VariantPreference != c.wantPref || r.Winner != c.wantWinner {
			t.Errorf("winner=%s variantFirst=%v → %+v, want pref=%v %s", c.winner, c.variantFirst, r, c.wantPref, c.wantWinner)
		}
	}
}
