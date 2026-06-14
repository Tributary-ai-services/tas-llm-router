package judge

import (
	"context"
	"errors"
	"testing"
)

// fakeLLM returns a canned reply (or error) and records what it was asked.
type fakeLLM struct {
	reply     string
	err       error
	gotModel  string
	gotSystem string
	gotUser   string
}

func (f *fakeLLM) Complete(_ context.Context, model, system, user string) (string, error) {
	f.gotModel, f.gotSystem, f.gotUser = model, system, user
	return f.reply, f.err
}

func TestScore_ParsesAndClamps(t *testing.T) {
	f := &fakeLLM{reply: `{"dimensions":{"correctness":0.9,"helpfulness":1.4,"completeness":-0.2},"overall":0.8,"abstain":false}`}
	j := &Judge{LLM: f, Model: "judge-model"}
	s, err := j.Score(context.Background(), "single_turn_qa", "What is 2+2?", "4")
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if s.Dimensions["correctness"] != 0.9 {
		t.Errorf("correctness = %v", s.Dimensions["correctness"])
	}
	if s.Dimensions["helpfulness"] != 1.0 {
		t.Errorf("helpfulness should clamp to 1.0, got %v", s.Dimensions["helpfulness"])
	}
	if s.Dimensions["completeness"] != 0.0 {
		t.Errorf("completeness should clamp to 0.0, got %v", s.Dimensions["completeness"])
	}
	if s.Overall != 0.8 || s.Abstain {
		t.Errorf("overall=%v abstain=%v", s.Overall, s.Abstain)
	}
	if s.RubricVersion != RubricVersion || s.Workflow != "single_turn_qa" {
		t.Errorf("metadata: %+v", s)
	}
}

func TestScore_JudgeModelUsed(t *testing.T) {
	// Bias control: the judge calls its OWN model, not the variant's.
	f := &fakeLLM{reply: `{"overall":0.5}`}
	j := &Judge{LLM: f, Model: "claude-haiku-4-5"}
	_, _ = j.Score(context.Background(), "rag", "p", "r")
	if f.gotModel != "claude-haiku-4-5" {
		t.Errorf("judge used model %q, want the configured judge model", f.gotModel)
	}
	// RAG rubric must mention faithfulness in the system prompt.
	if !contains(f.gotSystem, "faithfulness") {
		t.Errorf("rag system prompt missing faithfulness dimension: %s", f.gotSystem)
	}
}

func TestScore_OverallAveragedWhenMissing(t *testing.T) {
	f := &fakeLLM{reply: "```json\n{\"dimensions\":{\"correctness\":0.6,\"helpfulness\":0.8,\"completeness\":1.0}}\n```"}
	j := &Judge{LLM: f, Model: "m"}
	s, _ := j.Score(context.Background(), "single_turn_qa", "p", "r")
	// (0.6+0.8+1.0)/3 = 0.8
	if s.Overall < 0.79 || s.Overall > 0.81 {
		t.Errorf("overall should average dims to ~0.8, got %v", s.Overall)
	}
}

func TestScore_FencedAndProseTolerant(t *testing.T) {
	f := &fakeLLM{reply: "Sure! Here is my assessment:\n```json\n{\"overall\":0.7,\"abstain\":false}\n```\nHope that helps."}
	j := &Judge{LLM: f, Model: "m"}
	s, _ := j.Score(context.Background(), "single_turn_qa", "p", "r")
	if s.Overall != 0.7 {
		t.Errorf("should extract JSON from prose+fences, got overall=%v", s.Overall)
	}
}

func TestScore_GarbledReplyAbstains(t *testing.T) {
	f := &fakeLLM{reply: "I think it's pretty good honestly"}
	j := &Judge{LLM: f, Model: "m"}
	s, err := j.Score(context.Background(), "single_turn_qa", "p", "r")
	if err != nil {
		t.Fatalf("garbled reply must not error: %v", err)
	}
	if !s.Abstain {
		t.Error("unparseable judge reply should abstain, not score")
	}
}

func TestScore_LLMErrorPropagates(t *testing.T) {
	f := &fakeLLM{err: errors.New("boom")}
	j := &Judge{LLM: f, Model: "m"}
	if _, err := j.Score(context.Background(), "rag", "p", "r"); err == nil {
		t.Error("LLM transport error should propagate")
	}
}

func TestRubricFor_FallsBack(t *testing.T) {
	if got := rubricFor("nonsense").dims[0]; got != "correctness" {
		t.Errorf("unknown workflow should use default rubric, got first dim %q", got)
	}
	if len(rubricFor("code_generation").dims) != 2 {
		t.Error("code_generation rubric should have 2 dims")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
