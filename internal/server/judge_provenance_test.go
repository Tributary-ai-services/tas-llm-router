package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/judge"
)

// A judged score without its provenance is unusable twice over: it cannot be
// joined to the candidate it grades (the join key lives in a different
// database), and it cannot be compared across a grader change (RubricVersion
// versions the rubric, not the grader). Neither is reconstructible after the
// fact, so these fields are pinned here rather than trusted to survive a
// refactor.

func captureRecord(t *testing.T, g gradedSubject, s judge.Score) map[string]any {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding the recorded payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rec := &judgeRecorder{http: srv.Client(), baseURL: srv.URL, auth: "test"}
	if err := rec.record(context.Background(), "tenant-1", "event-1", "", "", g, s); err != nil {
		t.Fatalf("record: %v", err)
	}
	return got
}

func TestRecordCarriesTheJoinKeyAndTheGrader(t *testing.T) {
	got := captureRecord(t,
		gradedSubject{Vendor: "openai", Model: "gpt-4o-mini", JudgeModel: "claude-haiku-4-5-20251001"},
		judge.Score{Overall: 0.77, Workflow: "single_turn_qa", RubricVersion: judge.RubricVersion})

	// The join key. aiqg.event_metrics.model is written from the same routing
	// snapshot field, so these must match exactly or the aggregate silently
	// produces nothing rather than failing.
	if got["model"] != "gpt-4o-mini" || got["vendor"] != "openai" {
		t.Fatalf("join key missing or wrong: vendor=%v model=%v", got["vendor"], got["model"])
	}
	// The grader. Without it a swap to a different judge is unmeasurable.
	if got["judge_model"] != "claude-haiku-4-5-20251001" {
		t.Fatalf("judge_model = %v, want the grading model", got["judge_model"])
	}
	// RubricVersion is NOT a substitute: it would read "v1" for both graders.
	if got["rubric_version"] == got["judge_model"] {
		t.Fatal("rubric_version is standing in for the grader identity")
	}
	if got["self_judged"] != false {
		t.Fatalf("self_judged = %v, want false for a cross-model grade", got["self_judged"])
	}
}

func TestRecordMarksASelfJudgedScore(t *testing.T) {
	got := captureRecord(t,
		gradedSubject{Vendor: "anthropic", Model: "claude-haiku-4-5-20251001",
			JudgeModel: "claude-haiku-4-5-20251001", SelfJudged: true},
		judge.Score{Overall: 0.9, Workflow: "single_turn_qa", RubricVersion: judge.RubricVersion})

	// Kept, not dropped — the score is still worth a dashboard. The routing
	// aggregate is where a candidate grading itself actually matters, and
	// dashboard-be filters on this flag.
	if got["self_judged"] != true {
		t.Fatalf("self_judged = %v, want true when the grader graded its own output", got["self_judged"])
	}
	if got["overall"] != 0.9 {
		t.Fatalf("a self-judged score was altered or dropped: overall=%v", got["overall"])
	}
}
