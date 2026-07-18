package semcache

import "sync"

// DailyBudget is a per-day USD spend cap for the L3 judge (docs §14.1 — judge
// tokens are recurring opex; this is the hard ceiling that keeps them bounded).
// Spend accumulates against the cap and resets at UTC midnight. A cap of 0 (or
// negative) means unlimited — the budget never blocks.
//
// The check is intentionally pre-call: Allow() is consulted before a grade, and
// the actual cost is recorded with Add() after. So spend can overshoot the cap by
// at most one grade (~$0.001) — acceptable for a cost governor, and it means a
// grade is never abandoned half-paid-for. Concurrency-safe.
type DailyBudget struct {
	mu     sync.Mutex
	capUSD float64
	spent  float64
	day    string // UTC "2006-01-02"; spend resets when this rolls over
}

// NewDailyBudget returns a budget with the given daily cap in USD. capUSD <= 0
// means unlimited.
func NewDailyBudget(capUSD float64) *DailyBudget {
	return &DailyBudget{capUSD: capUSD}
}

// today returns the current UTC date stamp (via the package clock, so tests can
// drive day rollover).
func today() string { return timeNow().UTC().Format("2006-01-02") }

// rollLocked resets spend when the UTC day has changed. Caller holds the lock.
func (b *DailyBudget) rollLocked() {
	if d := today(); d != b.day {
		b.day = d
		b.spent = 0
	}
}

// Allow reports whether the judge may spend more today: true while today's spend
// is under the cap (or the cap is unlimited). It rolls the day over first, so the
// first call after UTC midnight sees a fresh budget.
func (b *DailyBudget) Allow() bool {
	if b == nil || b.capUSD <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return b.spent < b.capUSD
}

// Add records actual spend (a grade's cost) against today's budget.
func (b *DailyBudget) Add(costUSD float64) {
	if b == nil || costUSD <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	b.spent += costUSD
}

// Snapshot returns today's cap, spend so far, remaining (never negative), and the
// UTC day stamp. capUSD 0 reports remaining 0 but Allow() still permits — read
// Cap to distinguish an unlimited budget.
func (b *DailyBudget) Snapshot() (capUSD, spent, remaining float64, day string) {
	if b == nil {
		return 0, 0, 0, ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	rem := b.capUSD - b.spent
	if rem < 0 {
		rem = 0
	}
	return b.capUSD, b.spent, rem, b.day
}
