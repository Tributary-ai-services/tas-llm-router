package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/judge"
)

// Shadow-eval fidelity tests (#182, #183).
//
// The behaviour under test is not "does it post a row" but "does the row say
// enough to be trusted later": a preference with no price cannot support the
// claim shadow exists to support, and a preference produced under a cap we
// imposed is not a claim about the model at all.

// captureRecorder stands up an httptest server and returns the recorder
// pointed at it plus a channel carrying the decoded payload.
func captureRecorder(t *testing.T) (*judgeRecorder, <-chan map[string]any) {
	t.Helper()
	got := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("recorder posted invalid JSON: %v", err)
		}
		got <- payload
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return &judgeRecorder{
		http:    &http.Client{Timeout: 2 * time.Second},
		baseURL: srv.URL,
		auth:    "test-secret",
	}, got
}

func shadowBlock(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	raw, ok := payload["shadow"]
	if !ok {
		t.Fatalf("payload carries no shadow block: %v", payload)
	}
	block, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("shadow block is %T, want object", raw)
	}
	return block
}

// A recorded comparison must carry BOTH arms' token counts. This is the whole
// of #183: the replay is a live billed call on the customer's prompt, and a
// row that keeps the preference while discarding the price cannot answer
// whether the cheaper model was actually cheaper.
func TestRecordPairwiseCarriesBothArmsEconomics(t *testing.T) {
	rec, got := captureRecorder(t)
	rr := replayResult{
		Text:         "variant answer",
		Model:        "claude-haiku-4-5",
		Usage:        &types.Usage{PromptTokens: 1200, CompletionTokens: 340},
		FinishReason: "stop",
	}
	control := &types.Usage{PromptTokens: 1200, CompletionTokens: 410}
	pw := judge.PairwiseResult{VariantPreference: 1, Winner: "variant", RubricVersion: "v1"}

	if err := rec.recordPairwise(context.Background(), "t1", "evt1", "exp1", "cheap", "code_generation", pw, rr, control); err != nil {
		t.Fatalf("recordPairwise: %v", err)
	}

	payload := <-got
	if payload["signal_type"] != "judge_pairwise" {
		t.Errorf("signal_type = %v, want judge_pairwise", payload["signal_type"])
	}
	if payload["overall"] != float64(1) {
		t.Errorf("overall = %v, want 1 (the preference must survive)", payload["overall"])
	}

	sb := shadowBlock(t, payload)
	for field, want := range map[string]float64{
		"variant_input_tokens":  1200,
		"variant_output_tokens": 340,
		"control_input_tokens":  1200,
		"control_output_tokens": 410,
	} {
		if sb[field] != want {
			t.Errorf("%s = %v, want %v", field, sb[field], want)
		}
	}
	if sb["variant_model"] != "claude-haiku-4-5" {
		t.Errorf("variant_model = %v — without it the row cannot say what was priced", sb["variant_model"])
	}
}

// A provider that reports no usage must not produce zeros. A zero token count
// reads as "measured, and it was free"; absence reads as "not measured". The
// fit store draws that same NULL-vs-0 distinction, and it starts here.
func TestRecordPairwiseOmitsTokensWhenUsageAbsent(t *testing.T) {
	rec, got := captureRecorder(t)
	rr := replayResult{Text: "answer", Model: "gpt-4o-mini", FinishReason: "stop"} // Usage nil
	pw := judge.PairwiseResult{VariantPreference: 0.5, Winner: "tie", RubricVersion: "v1"}

	if err := rec.recordPairwise(context.Background(), "t1", "evt1", "exp1", "cheap", "rag", pw, rr, nil); err != nil {
		t.Fatalf("recordPairwise: %v", err)
	}

	sb := shadowBlock(t, <-got)
	for _, field := range []string{
		"variant_input_tokens", "variant_output_tokens",
		"control_input_tokens", "control_output_tokens",
	} {
		if _, present := sb[field]; present {
			t.Errorf("%s present with no usage reported — absent and zero are different facts", field)
		}
	}
}

// A truncated variant judged against a complete control response is a distorted
// comparison (#182). The flag has to reach the row so such comparisons can be
// excluded downstream rather than quietly averaged in.
func TestRecordPairwiseMarksTruncatedComparisons(t *testing.T) {
	rec, got := captureRecorder(t)
	rr := replayResult{
		Text:         "cut off mid-",
		Model:        "claude-haiku-4-5",
		Usage:        &types.Usage{PromptTokens: 900, CompletionTokens: 256},
		FinishReason: "length",
		Truncated:    true,
	}
	pw := judge.PairwiseResult{VariantPreference: 0, Winner: "control", RubricVersion: "v1"}

	if err := rec.recordPairwise(context.Background(), "t1", "evt1", "exp1", "cheap", "code_generation", pw, rr, nil); err != nil {
		t.Fatalf("recordPairwise: %v", err)
	}

	sb := shadowBlock(t, <-got)
	if sb["variant_truncated"] != true {
		t.Errorf("variant_truncated = %v, want true — an unmarked truncated row is indistinguishable from a fair loss", sb["variant_truncated"])
	}
	if sb["variant_finish_reason"] != "length" {
		t.Errorf("variant_finish_reason = %v, want length", sb["variant_finish_reason"])
	}
}

// finishReasonOf is what separates "the model stopped" from "we cut it off",
// so it must not panic on the degenerate responses providers actually return.
func TestFinishReasonOf(t *testing.T) {
	if got := finishReasonOf(nil); got != "" {
		t.Errorf("nil response: got %q, want empty", got)
	}
	if got := finishReasonOf(&types.ChatResponse{}); got != "" {
		t.Errorf("no choices: got %q, want empty", got)
	}
	resp := &types.ChatResponse{Choices: []types.Choice{{FinishReason: "length"}}}
	if got := finishReasonOf(resp); got != "length" {
		t.Errorf("got %q, want length", got)
	}
}

// The replay must not alias the caller's max_tokens pointer: the client still
// holds that request, and an experiment override replaces the pointer on the
// replay's copy. Cloning is what keeps that swap from being visible upstream.
func TestCloneIntPtrIsIndependent(t *testing.T) {
	if cloneIntPtr(nil) != nil {
		t.Error("nil must stay nil — the provider default is what control got too")
	}
	orig := 4096
	clone := cloneIntPtr(&orig)
	if clone == &orig {
		t.Fatal("clone aliases the caller's pointer")
	}
	if *clone != 4096 {
		t.Fatalf("clone = %d, want 4096", *clone)
	}
	*clone = 256
	if orig != 4096 {
		t.Errorf("mutating the clone changed the caller's value to %d", orig)
	}
}
