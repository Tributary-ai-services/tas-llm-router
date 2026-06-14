package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Pairwise shadow-eval (AIQG-EXPERIMENTS-RUNNER.md §6.3/§6.6): the strongest
// quality signal for an experiment. The same prompt is answered by control
// (the served response) and by the variant (replayed offline), and a third
// model judges which is better — controlling for prompt variance, which
// pointwise scoring can't. Position bias is countered by randomizing which
// response is shown as "A" (the caller passes variantFirst, flipped per call).

// PairwiseResult is one head-to-head comparison from the variant's point of
// view. VariantPreference: 1=variant better, 0.5=tie, 0=control better — so a
// per-variant mean is a win-rate (>0.5 = variant beats control).
type PairwiseResult struct {
	VariantPreference float64 `json:"variant_preference"`
	Winner            string  `json:"winner"` // variant | control | tie
	Abstain           bool    `json:"abstain"`
	RubricVersion     string  `json:"rubric_version"`
	Workflow          string  `json:"workflow"`
}

// ScorePairwise judges controlResp vs variantResp for the same prompt. The
// caller flips a coin into variantFirst so the variant isn't always in the
// same slot (position-bias control); ScorePairwise maps the judged winner slot
// back to a variant preference. An unparseable reply abstains.
func (j *Judge) ScorePairwise(ctx context.Context, workflow, prompt, controlResp, variantResp string, variantFirst bool) (PairwiseResult, error) {
	r := rubricFor(workflow)

	a, b := controlResp, variantResp
	if variantFirst {
		a, b = variantResp, controlResp
	}
	sys := buildPairwiseSystem(r)
	user := "PROMPT:\n" + truncate(prompt, 6000) +
		"\n\nRESPONSE A:\n" + truncate(a, 4000) +
		"\n\nRESPONSE B:\n" + truncate(b, 4000)

	raw, err := j.LLM.Complete(ctx, j.Model, sys, user)
	if err != nil {
		return PairwiseResult{}, fmt.Errorf("judge.ScorePairwise: %w", err)
	}
	return parsePairwise(raw, workflow, variantFirst), nil
}

func buildPairwiseSystem(r rubric) string {
	return "You are an impartial judge comparing two responses (A and B) to the same prompt — " +
		r.framing + ".\nDecide which response is better overall on: " + strings.Join(r.dims, ", ") +
		".\nIf they are equally good, answer \"tie\". If you cannot fairly judge, set \"abstain\": true.\n" +
		"Respond with ONLY a JSON object, no prose:\n" +
		`{"winner":"A","abstain":false}`
}

// parsePairwise maps the judged winner slot (A/B) back to a variant
// preference, given which slot the variant occupied.
func parsePairwise(raw, workflow string, variantFirst bool) PairwiseResult {
	out := PairwiseResult{RubricVersion: RubricVersion, Workflow: workflow}
	body := extractJSON(raw)
	if body == "" {
		out.Abstain = true
		return out
	}
	var parsed struct {
		Winner  string `json:"winner"`
		Abstain bool   `json:"abstain"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		out.Abstain = true
		return out
	}
	if parsed.Abstain {
		out.Abstain = true
		return out
	}
	// The variant is slot A when variantFirst, else slot B.
	variantSlot := "B"
	if variantFirst {
		variantSlot = "A"
	}
	switch strings.ToUpper(strings.TrimSpace(parsed.Winner)) {
	case "A", "B":
		if strings.EqualFold(parsed.Winner, variantSlot) {
			out.Winner, out.VariantPreference = "variant", 1.0
		} else {
			out.Winner, out.VariantPreference = "control", 0.0
		}
	case "TIE", "":
		if parsed.Winner == "" {
			out.Abstain = true
			return out
		}
		out.Winner, out.VariantPreference = "tie", 0.5
	default:
		out.Abstain = true
	}
	return out
}
