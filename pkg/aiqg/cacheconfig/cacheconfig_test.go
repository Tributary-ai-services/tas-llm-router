package cacheconfig

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeLoader struct {
	calls int
	cfg   *Config
	err   error
}

func (f *fakeLoader) ForTenant(_ context.Context, _ string) (*Config, error) {
	f.calls++
	return f.cfg, f.err
}

func bptr(b bool) *bool       { return &b }
func fptr(f float64) *float64 { return &f }
func iptr(i int) *int         { return &i }

func TestEffectiveHelpers_NilConfigUsesGlobal(t *testing.T) {
	var c *Config
	if !c.ExactEnabled(true) {
		t.Error("nil config should defer to global true")
	}
	if c.SemanticShadow(true) != true {
		t.Error("nil config should defer to global shadow=true")
	}
	if c.SemanticMinSimilarity(0.95) != 0.95 {
		t.Error("nil config should defer to global threshold")
	}
	if c.ExactTTL(30*time.Second) != 30*time.Second {
		t.Error("nil config should defer to global TTL")
	}
}

func TestEffectiveHelpers_OverrideComposes(t *testing.T) {
	// Only semantic.shadow is set; everything else must fall back to global.
	c := &Config{Semantic: &SemanticConfig{Enabled: bptr(true), Shadow: bptr(false)}}
	if !c.SemanticEnabled(false) {
		t.Error("semantic.enabled override should win")
	}
	if c.SemanticShadow(true) {
		t.Error("semantic.shadow=false override should win (serving)")
	}
	// min_similarity unset → global.
	if c.SemanticMinSimilarity(0.97) != 0.97 {
		t.Error("unset threshold should fall back to global")
	}
	// exact unset → global.
	if !c.ExactEnabled(true) {
		t.Error("unset exact should fall back to global")
	}
}

func TestSemanticMinSimilarity_OnlyTightens(t *testing.T) {
	// A looser override (below the global floor) must NOT widen the FPR (§10.2).
	loose := &Config{Semantic: &SemanticConfig{MinSimilarity: fptr(0.80)}}
	if got := loose.SemanticMinSimilarity(0.95); got != 0.95 {
		t.Errorf("loose override should be clamped to global: got %v want 0.95", got)
	}
	// A tighter override raises the floor.
	tight := &Config{Semantic: &SemanticConfig{MinSimilarity: fptr(0.99)}}
	if got := tight.SemanticMinSimilarity(0.95); got != 0.99 {
		t.Errorf("tighter override should win: got %v want 0.99", got)
	}
}

func TestExactTTL_Override(t *testing.T) {
	c := &Config{Exact: &ExactConfig{TTLSeconds: iptr(600)}}
	if got := c.ExactTTL(30 * time.Second); got != 600*time.Second {
		t.Errorf("ttl override: got %v want 600s", got)
	}
	// Zero/absent → global.
	zero := &Config{Exact: &ExactConfig{TTLSeconds: iptr(0)}}
	if got := zero.ExactTTL(30 * time.Second); got != 30*time.Second {
		t.Errorf("zero ttl should fall back to global: got %v", got)
	}
}

func TestResolver_CachesWithinTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := &fakeLoader{cfg: &Config{Semantic: &SemanticConfig{Enabled: bptr(true)}}}
	r := NewResolver(f, time.Minute)
	r.now = func() time.Time { return now }

	_ = r.ForTenant(context.Background(), "t1")
	_ = r.ForTenant(context.Background(), "t1")
	if f.calls != 1 {
		t.Errorf("expected 1 loader call within TTL, got %d", f.calls)
	}
	// After TTL expiry it reloads.
	now = now.Add(2 * time.Minute)
	_ = r.ForTenant(context.Background(), "t1")
	if f.calls != 2 {
		t.Errorf("expected a reload after TTL, got %d calls", f.calls)
	}
}

func TestResolver_DegradesToStaleOnError(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	good := &Config{Semantic: &SemanticConfig{Shadow: bptr(false)}}
	f := &fakeLoader{cfg: good}
	r := NewResolver(f, time.Minute)
	r.now = func() time.Time { return now }

	if got := r.ForTenant(context.Background(), "t1"); got != good {
		t.Fatal("first load should return the good config")
	}
	// TTL expires and the loader now errors → serve the stale cached config.
	now = now.Add(2 * time.Minute)
	f.err = errors.New("dashboard down")
	if got := r.ForTenant(context.Background(), "t1"); got != good {
		t.Error("on loader error, resolver must degrade to the stale config, not fail")
	}
}

func TestResolver_NilSafeAndEmptyTenant(t *testing.T) {
	var r *Resolver
	if r.ForTenant(context.Background(), "t1") != nil {
		t.Error("nil resolver should return nil (global defaults)")
	}
	f := &fakeLoader{cfg: &Config{}}
	r2 := NewResolver(f, time.Minute)
	if r2.ForTenant(context.Background(), "") != nil {
		t.Error("empty tenant should return nil without calling the loader")
	}
	if f.calls != 0 {
		t.Error("empty tenant must not call the loader")
	}
}

func TestJudgeHelpers_Override(t *testing.T) {
	// nil / unset → global defaults.
	var nilC *Config
	if !nilC.JudgeEnabled(true) || nilC.JudgeSampleRate(0.5) != 0.5 {
		t.Error("nil config should defer judge knobs to global")
	}
	// A tenant opts OUT of grading and dials its own sample rate.
	c := &Config{Judge: &JudgeConfig{Enabled: bptr(false), SampleRate: fptr(0.1)}}
	if c.JudgeEnabled(true) {
		t.Error("judge.enabled=false override should win (opt out)")
	}
	if c.JudgeSampleRate(1.0) != 0.1 {
		t.Errorf("judge.sample_rate override should win: got %v want 0.1", c.JudgeSampleRate(1.0))
	}
	// Unset sample rate falls back to global even when enabled is set.
	c2 := &Config{Judge: &JudgeConfig{Enabled: bptr(true)}}
	if c2.JudgeSampleRate(0.25) != 0.25 {
		t.Error("unset sample rate should fall back to global")
	}
	// Per-tenant daily cap: override wins; unset falls back to the passed default.
	if c := (&Config{Judge: &JudgeConfig{DailyUSD: fptr(0.25)}}); c.JudgeDailyUSD(0) != 0.25 {
		t.Errorf("judge.daily_usd override should win: got %v", c.JudgeDailyUSD(0))
	}
	if nilC.JudgeDailyUSD(0) != 0 {
		t.Error("unset daily cap with global 0 should be 0 (no per-tenant cap)")
	}
}
