package semcache

import (
	"context"
	"testing"
	"time"
)

func TestDailyBudget_CapEnforced(t *testing.T) {
	b := NewDailyBudget(0.50)
	// Under the cap → allowed.
	if !b.Allow() {
		t.Fatal("fresh budget should allow")
	}
	b.Add(0.30)
	if !b.Allow() {
		t.Fatal("0.30 of 0.50 spent should still allow")
	}
	// Reach the cap → blocked.
	b.Add(0.25) // 0.55 total, over 0.50
	if b.Allow() {
		t.Fatal("over-cap budget should block")
	}
	capUSD, spent, remaining, _ := b.Snapshot()
	if capUSD != 0.50 {
		t.Errorf("cap=%v, want 0.50", capUSD)
	}
	if spent < 0.549 || spent > 0.551 {
		t.Errorf("spent=%v, want ~0.55", spent)
	}
	if remaining != 0 {
		t.Errorf("remaining=%v, want 0 (never negative)", remaining)
	}
}

func TestDailyBudget_Unlimited(t *testing.T) {
	for _, capUSD := range []float64{0, -1} {
		b := NewDailyBudget(capUSD)
		b.Add(1000)
		if !b.Allow() {
			t.Errorf("cap %v should be unlimited (always allow)", capUSD)
		}
	}
	// A nil budget is unlimited too (the judge runs ungoverned only if wired so).
	var nilB *DailyBudget
	if !nilB.Allow() {
		t.Error("nil budget should allow")
	}
	nilB.Add(5) // must not panic
}

func TestDailyBudget_RollsOverAtUTCMidnight(t *testing.T) {
	// Drive the package clock across a UTC day boundary.
	base := time.Date(2026, 7, 18, 23, 59, 0, 0, time.UTC)
	orig := timeNow
	timeNow = func() time.Time { return base }
	defer func() { timeNow = orig }()

	b := NewDailyBudget(0.50)
	b.Add(0.50)
	if b.Allow() {
		t.Fatal("cap reached on day 1 should block")
	}
	_, _, _, day1 := b.Snapshot()

	// Advance past midnight → fresh budget.
	timeNow = func() time.Time { return base.Add(2 * time.Minute) } // 2026-07-19 00:01 UTC
	if !b.Allow() {
		t.Fatal("budget should reset after UTC midnight")
	}
	_, spent, _, day2 := b.Snapshot()
	if spent != 0 {
		t.Errorf("spend should reset to 0 on the new day, got %v", spent)
	}
	if day1 == day2 {
		t.Errorf("day stamp should have rolled over: %q -> %q", day1, day2)
	}
}

// TestLoop_StopsGradingAtDailyCap proves the governor stops the judge once the
// day's cap is reached: with a tiny cap and a per-grade cost that exceeds it, the
// first sample is graded (pre-call check, post-call charge — one grade may
// overshoot) and the rest are BudgetSkipped, never graded.
func TestLoop_StopsGradingAtDailyCap(t *testing.T) {
	graded := 0
	grader := GraderFunc(func(_ context.Context, _ Sample) (Verdict, error) {
		graded++
		return Verdict{Correct: true, CostUSD: 0.50}, nil // one grade consumes the whole cap
	})
	loop := NewLoop(grader,
		PairSinkFunc(func(context.Context, JudgedPair) error { return nil }),
		NewSimCalibrator(0, 0),
		NewDailyBudget(0.50), 32)
	go loop.Run(t.Context())

	for i := range 10 {
		loop.Enqueue(Sample{Query: itoa(i), CachedAnswer: "x", Similarity: 0.98, Observed: StateShadowHit})
	}
	waitFor(t, func() bool {
		st := loop.Stats()
		return st.Graded+st.BudgetSkipped >= 10
	})
	st := loop.Stats()
	// Exactly one grade lands (it spends 0.50, hitting the cap); the other nine are
	// skipped for budget, not graded.
	if st.Graded != 1 {
		t.Errorf("Graded=%d, want 1 (the cap stops the rest)", st.Graded)
	}
	if st.BudgetSkipped != 9 {
		t.Errorf("BudgetSkipped=%d, want 9", st.BudgetSkipped)
	}
	if st.SpentUSD < 0.49 || st.SpentUSD > 0.51 {
		t.Errorf("SpentUSD=%.3f, want ~0.50", st.SpentUSD)
	}
}

// TestLoop_PerTenantBudget: a per-tenant cap binds one tenant independently while
// the global ceiling is generous — a capped tenant stops after ~1 grade, an
// uncapped tenant keeps grading.
func TestLoop_PerTenantBudget(t *testing.T) {
	grader := GraderFunc(func(_ context.Context, _ Sample) (Verdict, error) {
		return Verdict{Correct: true, CostUSD: 0.50}, nil // each grade spends $0.50
	})
	loop := NewLoop(grader,
		PairSinkFunc(func(context.Context, JudgedPair) error { return nil }),
		NewSimCalibrator(0, 0),
		NewDailyBudget(100), // generous GLOBAL ceiling
		64)
	go loop.Run(t.Context())

	// Tenant A: $0.50 cap → one grade consumes it. Tenant B: no cap.
	for range 6 {
		loop.Enqueue(Sample{Scope: scopeOf("A", "m1"), Query: "qa", CachedAnswer: "x", Similarity: 0.98, Observed: StateShadowHit, DailyUSD: 0.50})
		loop.Enqueue(Sample{Scope: scopeOf("B", "m1"), Query: "qb", CachedAnswer: "x", Similarity: 0.98, Observed: StateShadowHit, DailyUSD: 0})
	}
	waitFor(t, func() bool { return loop.Stats().Graded+loop.Stats().BudgetSkipped >= 12 })

	// Per-tenant accounting isn't in JudgeStats, but the global tallies prove the
	// cap bound SOMETHING: tenant A's 6 → 1 graded + 5 skipped; tenant B's 6 all
	// graded. So Graded == 7, BudgetSkipped == 5.
	st := loop.Stats()
	if st.Graded != 7 {
		t.Errorf("Graded=%d, want 7 (1 for capped A + 6 for uncapped B)", st.Graded)
	}
	if st.BudgetSkipped != 5 {
		t.Errorf("BudgetSkipped=%d, want 5 (A's over-cap samples)", st.BudgetSkipped)
	}
	// The global budget spent only $3.50 (7 grades) — nowhere near its $100 ceiling.
	if _, spent, _, _ := loop.Budget(); spent < 3.4 || spent > 3.6 {
		t.Errorf("global spent=%.2f, want ~3.50", spent)
	}
}
