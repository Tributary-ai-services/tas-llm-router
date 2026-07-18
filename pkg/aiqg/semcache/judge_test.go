package semcache

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantCorrect bool
		wantConf    float64
	}{
		{"strict json true", `{"correct": true, "confidence": 0.9, "reason": "match"}`, true, 0.9},
		{"strict json false", `{"correct": false, "confidence": 0.8, "reason": "wrong tier"}`, false, 0.8},
		{"json with prose around", "Sure!\n{\"correct\": false, \"confidence\": 0.7}\nHope that helps", false, 0.7},
		{"confidence clamped", `{"correct": true, "confidence": 5}`, true, 1.0},
		{"non-json affirmative", "true, the answer is correct", true, 0},
		{"non-json negative", "No — the answer is about a different plan", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parseVerdict(tc.in)
			if err != nil {
				t.Fatalf("parseVerdict: %v", err)
			}
			if v.Correct != tc.wantCorrect {
				t.Errorf("Correct = %v, want %v", v.Correct, tc.wantCorrect)
			}
			if v.Confidence != tc.wantConf {
				t.Errorf("Confidence = %v, want %v", v.Confidence, tc.wantConf)
			}
		})
	}
}

func TestPromptGrader_BuildsBlindPrompt(t *testing.T) {
	var gotSystem, gotUser string
	g := NewPromptGrader(func(_ context.Context, system, user string) (string, float64, error) {
		gotSystem, gotUser = system, user
		return `{"correct": false, "confidence": 0.6, "reason": "different card tier"}`, 0.0007, nil
	})
	v, err := g.Grade(context.Background(), Sample{
		Query:        "What are the fees on the Chase Sapphire Reserve card",
		CachedAnswer: "The Chase Sapphire card has a $95 annual fee.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Correct {
		t.Error("expected incorrect verdict for a tier mismatch")
	}
	if v.CostUSD != 0.0007 {
		t.Errorf("CostUSD = %v, want 0.0007 (propagated from the transport)", v.CostUSD)
	}
	// Blind: the grading prompt must carry the question + candidate answer, and
	// must NOT reveal that the answer came from a *different* cached question.
	if gotSystem == "" || gotUser == "" {
		t.Fatal("grader did not build a prompt")
	}
	if !contains(gotUser, "Reserve") || !contains(gotUser, "$95") {
		t.Errorf("user prompt missing question/answer content: %q", gotUser)
	}
	if contains(gotUser, "cached") || contains(gotUser, "similar") {
		t.Errorf("prompt leaks cache framing (not blind): %q", gotUser)
	}
}

func TestPromptGrader_NoTransport(t *testing.T) {
	var g *PromptGrader
	if _, err := g.Grade(context.Background(), Sample{}); err == nil {
		t.Error("expected error from a grader with no chat transport")
	}
}

func TestShouldSample(t *testing.T) {
	sc := SampleConfig{BandLo: 0.88, BandHi: 0.97, Rate: 1.0}
	// In band, rate 1.0 → always sampled.
	if !sc.ShouldSample(0.90, StateMiss, "k") {
		t.Error("in-band near-miss should sample at rate 1.0")
	}
	// Below band → never.
	if sc.ShouldSample(0.80, StateMiss, "k") {
		t.Error("below-band should not sample")
	}
	// Above band, IncludeHits off → not sampled even for a would-hit.
	if sc.ShouldSample(0.99, StateShadowHit, "k") {
		t.Error("above-band hit should not sample when IncludeHits is off")
	}
	// Above band, IncludeHits on + would-serve → sampled.
	sc.IncludeHits = true
	if !sc.ShouldSample(0.99, StateShadowHit, "k") {
		t.Error("above-band would-hit should sample when IncludeHits is on")
	}
	// Above band but a MISS (L2 rejected) is not a served hit → not sampled by the hit rule.
	if sc.ShouldSample(0.99, StateMiss, "k") {
		t.Error("above-band L2-rejected should not sample via the hit rule")
	}
	// Rate 0 → never.
	sc.Rate = 0
	if sc.ShouldSample(0.90, StateMiss, "k") {
		t.Error("rate 0 should never sample")
	}
}

func TestRateGate_DeterministicAndProportional(t *testing.T) {
	// Same key → same decision (idempotent under retries).
	first := rateGate("stable-key", 0.5)
	for range 100 {
		if rateGate("stable-key", 0.5) != first {
			t.Fatal("rateGate not deterministic for a fixed key")
		}
	}
	// Rate ~0.2 over many distinct keys lands near 20%.
	kept := 0
	const n = 10000
	for i := range n {
		if rateGate(itoa(i), 0.2) {
			kept++
		}
	}
	frac := float64(kept) / n
	if frac < 0.17 || frac > 0.23 {
		t.Errorf("rate 0.2 kept %.3f, expected ~0.20", frac)
	}
	// Rate >= 1 keeps everything.
	if !rateGate("x", 1.0) {
		t.Error("rate 1.0 must keep")
	}
}

// TestLoop_EndToEnd drives the full loop: a stub grader that fails would-hits
// whose answer mentions the wrong tier, and confirms the sampled FPR + labeled
// pairs come out right, and that a full queue drops rather than blocks.
func TestLoop_EndToEnd(t *testing.T) {
	var (
		mu    sync.Mutex
		pairs []JudgedPair
	)
	sink := PairSinkFunc(func(_ context.Context, p JudgedPair) error {
		mu.Lock()
		pairs = append(pairs, p)
		mu.Unlock()
		return nil
	})
	grader := GraderFunc(func(_ context.Context, s Sample) (Verdict, error) {
		// Incorrect when the cached answer is about the wrong thing: the base card
		// (a tier mismatch) or a different seat count (an entity mismatch).
		wrong := contains(s.CachedAnswer, "base") || contains(s.CachedAnswer, "5 seats")
		return Verdict{Correct: !wrong, Confidence: 0.9, CostUSD: 0.001}, nil
	})
	loop := NewLoop(grader, sink, NewSimCalibrator(0.5, 1), NewDailyBudget(1.0), 8)

	go loop.Run(t.Context())

	// 3 would-hits: 2 correct, 1 false hit (base-card answer).
	loop.Enqueue(Sample{Query: "reserve fees", CachedAnswer: "reserve fee is $550", Similarity: 0.98, Observed: StateShadowHit})
	loop.Enqueue(Sample{Query: "shipping time", CachedAnswer: "3-5 business days", Similarity: 0.97, Observed: StateShadowHit})
	loop.Enqueue(Sample{Query: "reserve fees", CachedAnswer: "the base card fee is $95", Similarity: 0.96, Observed: StateShadowHit})
	// 1 L2-rejected near-miss the judge agrees is wrong.
	loop.Enqueue(Sample{Query: "8 seats total", CachedAnswer: "for 5 seats it's $50", Similarity: 0.95, Observed: StateMiss, RejectReason: "entity_number_date"})

	waitFor(t, func() bool { return loop.Stats().Graded >= 4 })

	st := loop.Stats()
	if st.WouldServe != 3 || st.FalseHits != 1 {
		t.Errorf("WouldServe=%d FalseHits=%d, want 3/1", st.WouldServe, st.FalseHits)
	}
	if got := st.SampledFPR(); got < 0.33 || got > 0.34 {
		t.Errorf("SampledFPR=%.3f, want ~0.333", got)
	}
	if st.L2Rejected != 1 || st.L2Correct != 1 {
		t.Errorf("L2Rejected=%d L2Correct=%d, want 1/1", st.L2Rejected, st.L2Correct)
	}
	if p := st.L2Precision(); p != 1.0 {
		t.Errorf("L2Precision=%.3f, want 1.0", p)
	}
	// 4 grades × $0.001 metered against the budget.
	if st.SpentUSD < 0.0039 || st.SpentUSD > 0.0041 {
		t.Errorf("SpentUSD=%.4f, want ~0.004", st.SpentUSD)
	}
	if _, spent, _, _ := loop.Budget(); spent < 0.0039 || spent > 0.0041 {
		t.Errorf("budget spent=%.4f, want ~0.004", spent)
	}

	mu.Lock()
	np := len(pairs)
	mu.Unlock()
	if np != 4 {
		t.Errorf("recorded %d labeled pairs, want 4", np)
	}
}

func TestLoop_EnqueueDropsWhenFull(t *testing.T) {
	// A grader that blocks so the queue can fill.
	release := make(chan struct{})
	grader := GraderFunc(func(_ context.Context, _ Sample) (Verdict, error) {
		<-release
		return Verdict{Correct: true}, nil
	})
	loop := NewLoop(grader, PairSinkFunc(func(context.Context, JudgedPair) error { return nil }), NewSimCalibrator(0, 0), nil, 2)
	go loop.Run(t.Context())

	// Enqueue far more than capacity while the worker is blocked; Enqueue must
	// never block, and the excess must be counted as dropped.
	accepted := 0
	for i := range 50 {
		if loop.Enqueue(Sample{Query: itoa(i), Observed: StateShadowHit}) {
			accepted++
		}
	}
	close(release)
	if accepted >= 50 {
		t.Error("expected some samples to be dropped when the queue is full")
	}
	if loop.Stats().Dropped == 0 {
		t.Error("expected Dropped > 0")
	}
}

func TestLoop_NilSafe(t *testing.T) {
	var l *Loop
	if l.Enqueue(Sample{}) {
		t.Error("nil loop Enqueue should return false")
	}
	if _, ok := l.RecommendThreshold(0.01); ok {
		t.Error("nil loop should not recommend")
	}
	l.Run(context.Background()) // must not panic
	// A loop missing a grader/sink is not ready.
	partial := NewLoop(nil, nil, nil, nil, 4)
	if partial.Enqueue(Sample{}) {
		t.Error("loop without grader/sink should drop")
	}
}

// helpers

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := timeNow().Add(2 * time.Second)
	for timeNow().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}
