package affinity

import (
	"context"
	"strings"
	"testing"
	"time"
)

const (
	tenant = "t1"
	conv   = "conv-1"
	prefix = "abc123def456789"
)

func mgr(cfg Config) (*Manager, *MemoryStore) {
	st := NewMemoryStore()
	if cfg.KeySource == "" {
		cfg.KeySource = "conversation"
	}
	return New(st, cfg), st
}

func alwaysUsable(string) bool { return true }

var t0 = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// The core behaviour: stick to the provider whose cache is warm.
// ---------------------------------------------------------------------------

func TestAffinityHoldsWithinAnEpoch(t *testing.T) {
	m, _ := mgr(Config{})
	ctx := context.Background()

	d1 := m.Resolve(ctx, tenant, conv, prefix, t0, alwaysUsable)
	if d1.Held {
		t.Fatal("first request of a conversation cannot hold affinity")
	}
	m.Record(ctx, tenant, conv, d1.Epoch, "anthropic", "claude-haiku-4-5", t0)

	d2 := m.Resolve(ctx, tenant, conv, prefix, t0.Add(30*time.Second), alwaysUsable)
	if !d2.Held {
		t.Fatal("affinity did not hold on the next turn within the TTL")
	}
	if d2.Target.Provider != "anthropic" || d2.Target.Model != "claude-haiku-4-5" {
		t.Fatalf("target = %+v, want the recorded provider and model", d2.Target)
	}
}

// The crux of the design: the vendor cache is keyed on prefix BYTES, not
// meaning. A long session covering many topics keeps its cache the whole time,
// so affinity must survive continued traffic indefinitely.
func TestAffinitySurvivesALongSessionOfFrequentRequests(t *testing.T) {
	m, _ := mgr(Config{TTL: 5 * time.Minute})
	ctx := context.Background()
	now := t0

	d := m.Resolve(ctx, tenant, conv, prefix, now, alwaysUsable)
	m.Record(ctx, tenant, conv, d.Epoch, "anthropic", "claude-haiku-4-5", now)

	// Three hours of traffic every 30 seconds — well inside the TTL each time.
	for i := 0; i < 360; i++ {
		now = now.Add(30 * time.Second)
		d = m.Resolve(ctx, tenant, conv, prefix, now, alwaysUsable)
		if !d.Held {
			t.Fatalf("affinity broke after %d turns of continuous traffic", i)
		}
	}
	// A wall-clock bucket (floor(now/ttl)) would have expired this ~36 times.
	if d.Epoch.IdleBucket != 1 {
		t.Fatalf("idle bucket = %d after continuous traffic, want 1 — the epoch must track GAPS, not the clock", d.Epoch.IdleBucket)
	}
}

func TestIdleBeyondTTLAdvancesTheEpoch(t *testing.T) {
	m, _ := mgr(Config{TTL: 5 * time.Minute})
	ctx := context.Background()

	d1 := m.Resolve(ctx, tenant, conv, prefix, t0, alwaysUsable)
	m.Record(ctx, tenant, conv, d1.Epoch, "anthropic", "claude-haiku-4-5", t0)

	d2 := m.Resolve(ctx, tenant, conv, prefix, t0.Add(6*time.Minute), alwaysUsable)
	if d2.Held {
		t.Fatal("affinity held across an idle gap longer than the TTL; the cache has expired")
	}
	if !d2.EpochAdvanced {
		t.Fatal("epoch did not advance on an idle gap")
	}
	// The two causes call for completely different responses, so they must be
	// distinguishable on the event.
	if d2.Reason == "" || !strings.Contains(d2.Reason, "idle") {
		t.Fatalf("reason = %q, should name the idle gap", d2.Reason)
	}
}

// A changed system prompt or tool set means the vendor cache is cold no matter
// where we route, so affinity to the old provider is worthless.
func TestPrefixChangeAdvancesTheEpoch(t *testing.T) {
	m, _ := mgr(Config{})
	ctx := context.Background()

	d1 := m.Resolve(ctx, tenant, conv, prefix, t0, alwaysUsable)
	m.Record(ctx, tenant, conv, d1.Epoch, "anthropic", "claude-haiku-4-5", t0)

	d2 := m.Resolve(ctx, tenant, conv, "totally-different-prefix", t0.Add(time.Second), alwaysUsable)
	if d2.Held {
		t.Fatal("affinity held across a changed stable prefix")
	}
	// Pointing at a deploy rather than at traffic patterns is the useful
	// distinction here.
	if !strings.Contains(d2.Reason, "prefix changed") {
		t.Fatalf("reason = %q, should name the prefix change", d2.Reason)
	}
}

// ---------------------------------------------------------------------------
// Precedence. Affinity is an economic optimisation and never outranks
// correctness.
// ---------------------------------------------------------------------------

func TestAffinityYieldsToHealthAndConstraints(t *testing.T) {
	m, _ := mgr(Config{})
	ctx := context.Background()

	d1 := m.Resolve(ctx, tenant, conv, prefix, t0, alwaysUsable)
	m.Record(ctx, tenant, conv, d1.Epoch, "anthropic", "claude-haiku-4-5", t0)

	// A warm cache on an ejected or denied provider is worthless at best and a
	// compliance breach at worst.
	unusable := func(string) bool { return false }
	d2 := m.Resolve(ctx, tenant, conv, prefix, t0.Add(time.Second), unusable)
	if d2.Held {
		t.Fatal("affinity overrode health/constraints")
	}
	// "Affinity existed but was not honoured" and "no affinity existed" look
	// identical downstream unless this is recorded.
	if !strings.Contains(d2.Reason, "unavailable or denied") {
		t.Fatalf("reason = %q, should record that a target existed and was rejected", d2.Reason)
	}
}

// ---------------------------------------------------------------------------
// Scope. Vendor prompt caches are model-scoped.
// ---------------------------------------------------------------------------

func TestVendorOnlyScopeOmitsTheModel(t *testing.T) {
	m, _ := mgr(Config{Scope: "vendor"})
	ctx := context.Background()
	d := m.Resolve(ctx, tenant, conv, prefix, t0, alwaysUsable)
	m.Record(ctx, tenant, conv, d.Epoch, "anthropic", "claude-haiku-4-5", t0)

	got := m.Resolve(ctx, tenant, conv, prefix, t0.Add(time.Second), alwaysUsable)
	if got.Target.Model != "" {
		t.Fatalf("vendor-scope affinity recorded a model (%q); it should pin the vendor only", got.Target.Model)
	}
}

func TestDefaultScopeIncludesTheModel(t *testing.T) {
	m, _ := mgr(Config{})
	ctx := context.Background()
	d := m.Resolve(ctx, tenant, conv, prefix, t0, alwaysUsable)
	m.Record(ctx, tenant, conv, d.Epoch, "anthropic", "claude-haiku-4-5", t0)

	got := m.Resolve(ctx, tenant, conv, prefix, t0.Add(time.Second), alwaysUsable)
	// Pinning the vendor alone lets the next turn land on a different model and
	// rebuild the cache anyway — affinity that appears to hold while buying
	// nothing.
	if got.Target.Model != "claude-haiku-4-5" {
		t.Fatalf("model = %q, want the recorded model", got.Target.Model)
	}
}

// ---------------------------------------------------------------------------
// on_break.
// ---------------------------------------------------------------------------

func TestFailOnBreakOnlyFiresWhenATargetExisted(t *testing.T) {
	m, _ := mgr(Config{OnBreak: Fail})
	ctx := context.Background()

	// First request of a conversation: no target yet. Failing here would fail
	// the first request of EVERY conversation.
	d1 := m.Resolve(ctx, tenant, conv, prefix, t0, alwaysUsable)
	if m.ShouldFail(d1) {
		t.Fatal("on_break=fail failed the first request, before any affinity existed")
	}
	m.Record(ctx, tenant, conv, d1.Epoch, "anthropic", "claude-haiku-4-5", t0)

	// Now a target exists and is unusable — the case fail is for.
	d2 := m.Resolve(ctx, tenant, conv, prefix, t0.Add(time.Second), func(string) bool { return false })
	if !m.ShouldFail(d2) {
		t.Fatal("on_break=fail did not fail when the affine provider was unusable")
	}
}

func TestPreferSameNeverFails(t *testing.T) {
	m, _ := mgr(Config{OnBreak: PreferSame})
	ctx := context.Background()
	d := m.Resolve(ctx, tenant, conv, prefix, t0, alwaysUsable)
	m.Record(ctx, tenant, conv, d.Epoch, "anthropic", "claude-haiku-4-5", t0)
	d2 := m.Resolve(ctx, tenant, conv, prefix, t0.Add(time.Second), func(string) bool { return false })
	if m.ShouldFail(d2) {
		t.Fatal("prefer_same failed a request; it must fall through silently")
	}
}

// ---------------------------------------------------------------------------
// Disabled / absent cases must be inert, not special-cased by callers.
// ---------------------------------------------------------------------------

func TestNoConversationIdMeansNoAffinity(t *testing.T) {
	m, _ := mgr(Config{})
	d := m.Resolve(context.Background(), tenant, "", prefix, t0, alwaysUsable)
	if d.Held || d.EpochAdvanced {
		t.Fatal("affinity engaged without a conversation identifier")
	}
}

func TestDisabledAndNilAreInert(t *testing.T) {
	off := New(NewMemoryStore(), Config{})
	if off.Enabled() {
		t.Fatal("affinity with no key source reported enabled")
	}
	if d := off.Resolve(context.Background(), tenant, conv, prefix, t0, alwaysUsable); d.Held {
		t.Fatal("disabled affinity held")
	}
	off.Record(context.Background(), tenant, conv, Epoch{}, "anthropic", "m", t0) // must not panic

	nilStore := New(nil, Config{KeySource: "conversation"})
	if nilStore.Enabled() {
		t.Fatal("nil store reported enabled")
	}
	if d := nilStore.Resolve(context.Background(), tenant, conv, prefix, t0, alwaysUsable); d.Held {
		t.Fatal("nil store held affinity")
	}
}

// ---------------------------------------------------------------------------
// Epoch mechanics.
// ---------------------------------------------------------------------------

func TestEpochStringIsStableAndBounded(t *testing.T) {
	e := Epoch{PrefixHash: "0123456789abcdef0123456789abcdef", IdleBucket: 3}
	got := e.String()
	if got != "0123456789ab:3" {
		t.Fatalf("Epoch.String() = %q, want a truncated hash and the bucket", got)
	}
	if (Epoch{IdleBucket: 1}).String() != "none:1" {
		t.Fatal("an empty prefix hash should render distinctly, not as an empty segment")
	}
}

func TestComputeEpochAdvancesOnlyOnGaps(t *testing.T) {
	ttl := 5 * time.Minute
	// First ever request: no last-seen, so the bucket advances.
	if got := ComputeEpoch(prefix, time.Time{}, 0, t0, ttl); got.IdleBucket != 1 {
		t.Fatalf("first request bucket = %d, want 1", got.IdleBucket)
	}
	// Inside the TTL: unchanged.
	if got := ComputeEpoch(prefix, t0, 1, t0.Add(ttl-time.Second), ttl); got.IdleBucket != 1 {
		t.Fatalf("bucket advanced inside the TTL: %d", got.IdleBucket)
	}
	// Beyond the TTL: advances.
	if got := ComputeEpoch(prefix, t0, 1, t0.Add(ttl+time.Second), ttl); got.IdleBucket != 2 {
		t.Fatalf("bucket did not advance past the TTL: %d", got.IdleBucket)
	}
}

func TestHashPrefixDistinguishesToolChanges(t *testing.T) {
	a := HashPrefix("sys", []string{"search", "fetch"})
	b := HashPrefix("sys", []string{"search"})
	if a == b {
		t.Fatal("removing a tool did not change the prefix hash; the vendor cache would be cold while affinity held")
	}
	// The separator matters: without it, ["ab","c"] and ["a","bc"] would hash
	// identically.
	if HashPrefix("s", []string{"ab", "c"}) == HashPrefix("s", []string{"a", "bc"}) {
		t.Fatal("tool names are concatenated without a separator")
	}
}
