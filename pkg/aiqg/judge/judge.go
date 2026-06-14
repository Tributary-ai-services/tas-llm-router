// Package judge implements the AIQG LLM-as-judge quality layer
// (docs/AIQG-EXPERIMENTS-RUNNER.md §6.6): a *different* model rubric-scores a
// sampled response on correctness / helpfulness / completeness (dimensions
// keyed to workflow_type), reference-free and pointwise. It's a shared
// capability — the Experiments runner consumes per-variant judge scores, and
// it augments CLEAR Efficacy (which is structural-only).
//
// Bias controls (§6.6): the judge model MUST differ from the model that
// produced the response (the caller enforces this via Model); the rubric is
// versioned (RubricVersion) so scores compare over time; the judge may
// ABSTAIN rather than guess. Pointwise scoring (no A/B position bias inline);
// pairwise shadow-eval is a future mode.
package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RubricVersion stamps every score so a rubric change is comparable over time.
// Bump when the dimensions or instructions below change.
const RubricVersion = "v1"

// Score is one judged response: per-dimension 0–1, an overall 0–1, and an
// abstain flag (the judge declined — exclude from aggregates rather than
// scoring 0).
type Score struct {
	Dimensions    map[string]float64 `json:"dimensions"`
	Overall       float64            `json:"overall"`
	Abstain       bool               `json:"abstain"`
	RubricVersion string             `json:"rubric_version"`
	Workflow      string             `json:"workflow"`
}

// Completion is the minimal LLM call the judge needs. Decouples the judge from
// internal/routing so it's unit-testable and so the judge model is explicit.
type Completion interface {
	// Complete sends system + user prompts to model and returns the text.
	Complete(ctx context.Context, model, system, user string) (string, error)
}

// Judge rubric-scores responses with a fixed judge model.
type Judge struct {
	LLM   Completion
	Model string // the judge model — caller guarantees it ≠ the response's model
}

// rubric is a workflow_type's judged dimensions + a one-line framing the
// system prompt embeds. Where a cheaper automatic signal exists (code
// execution, schema match, groundedness), prefer it over the judge — the
// caller decides; this package always offers the semantic fallback.
type rubric struct {
	dims    []string
	framing string
}

var rubrics = map[string]rubric{
	"single_turn_qa": {
		dims:    []string{"correctness", "helpfulness", "completeness"},
		framing: "a direct question-answering response",
	},
	"rag": {
		dims:    []string{"faithfulness", "answer_relevance"},
		framing: "a retrieval-augmented answer; faithfulness = supported by the provided context, not fabricated",
	},
	"summarization": {
		dims:    []string{"faithfulness", "key_point_coverage", "concision"},
		framing: "a summary; faithfulness = no hallucinated facts beyond the source",
	},
	"code_generation": {
		dims:    []string{"solves_the_prompt", "idiomatic"},
		framing: "generated code (prefer execution/tests over this judgment when available)",
	},
	"classification_extraction": {
		dims:    []string{"label_correctness"},
		framing: "a classification/extraction result (prefer schema+label match when labels are known)",
	},
	"agentic": {
		dims:    []string{"goal_completion", "correct_tool_use"},
		framing: "an agent turn that may call tools across steps",
	},
}

// defaultRubric is used for unknown/empty workflow types.
var defaultRubric = rubric{
	dims:    []string{"correctness", "helpfulness", "completeness"},
	framing: "an assistant response",
}

func rubricFor(workflow string) rubric {
	if r, ok := rubrics[workflow]; ok {
		return r
	}
	return defaultRubric
}

// Score rubric-scores one (prompt, response) pair for the given workflow. The
// judge is instructed to return strict JSON; parsing is tolerant of code
// fences and surrounding prose, clamps each score to [0,1], and treats a
// missing/garbled body as an abstain rather than an error (so a flaky judge
// never poisons aggregates with a hard failure — it just doesn't contribute).
func (j *Judge) Score(ctx context.Context, workflow, prompt, response string) (Score, error) {
	r := rubricFor(workflow)
	sys := buildSystemPrompt(r)
	user := buildUserPrompt(prompt, response)

	raw, err := j.LLM.Complete(ctx, j.Model, sys, user)
	if err != nil {
		return Score{}, fmt.Errorf("judge.Score: %w", err)
	}
	return parseScore(raw, workflow, r), nil
}

func buildSystemPrompt(r rubric) string {
	var b strings.Builder
	b.WriteString("You are an impartial evaluator scoring ")
	b.WriteString(r.framing)
	b.WriteString(".\nScore each dimension from 0.0 (poor) to 1.0 (excellent). Dimensions: ")
	b.WriteString(strings.Join(r.dims, ", "))
	b.WriteString(".\nIf you cannot fairly judge (insufficient context, refusal, off-topic), set \"abstain\": true.\n")
	b.WriteString("Respond with ONLY a JSON object, no prose:\n")
	b.WriteString(`{"dimensions":{`)
	for i, d := range r.dims {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(fmt.Sprintf("%q:0.0", d))
	}
	b.WriteString(`},"overall":0.0,"abstain":false}`)
	return b.String()
}

func buildUserPrompt(prompt, response string) string {
	return "PROMPT:\n" + truncate(prompt, 6000) + "\n\nRESPONSE:\n" + truncate(response, 6000)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…[truncated]"
}

// parseScore extracts the JSON object from the judge's reply, clamps scores,
// and fills RubricVersion/Workflow. A missing overall is averaged from the
// dimensions; an unparseable reply becomes an abstain.
func parseScore(raw, workflow string, r rubric) Score {
	out := Score{Dimensions: map[string]float64{}, RubricVersion: RubricVersion, Workflow: workflow}
	body := extractJSON(raw)
	if body == "" {
		out.Abstain = true
		return out
	}
	var parsed struct {
		Dimensions map[string]float64 `json:"dimensions"`
		Overall    *float64           `json:"overall"`
		Abstain    bool               `json:"abstain"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		out.Abstain = true
		return out
	}
	out.Abstain = parsed.Abstain
	for _, d := range r.dims {
		if v, ok := parsed.Dimensions[d]; ok {
			out.Dimensions[d] = clamp01(v)
		}
	}
	switch {
	case parsed.Overall != nil:
		out.Overall = clamp01(*parsed.Overall)
	case len(out.Dimensions) > 0:
		sum := 0.0
		for _, v := range out.Dimensions {
			sum += v
		}
		out.Overall = sum / float64(len(out.Dimensions))
	}
	return out
}

// extractJSON pulls the first {...} object out of a reply that may be wrapped
// in ```json fences or surrounded by prose.
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
