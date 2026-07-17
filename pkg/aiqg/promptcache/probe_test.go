package promptcache

import (
	"context"
	"testing"
	"time"
)

func TestMemoryProbe_FirstSightingIsCold(t *testing.T) {
	p := NewMemoryProbe(time.Minute)
	seen, err := p.Observe(context.Background(), "t1", "abc")
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if seen {
		t.Fatal("first sighting reported seen=true; a prefix we have never observed cannot be cached")
	}
}

func TestMemoryProbe_RecurrenceWithinWindowIsHit(t *testing.T) {
	p := NewMemoryProbe(time.Minute)
	ctx := context.Background()
	if _, err := p.Observe(ctx, "t1", "abc"); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	seen, err := p.Observe(ctx, "t1", "abc")
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !seen {
		t.Fatal("recurrence inside the window reported seen=false; this is the read we are forgoing")
	}
}

func TestMemoryProbe_RecurrenceAfterTTLIsCold(t *testing.T) {
	p := NewMemoryProbe(time.Minute)
	base := time.Now()
	p.now = func() time.Time { return base }
	ctx := context.Background()

	_, _ = p.Observe(ctx, "t1", "abc")
	// Exactly at the TTL boundary the vendor entry has expired: cold.
	p.now = func() time.Time { return base.Add(time.Minute) }
	seen, _ := p.Observe(ctx, "t1", "abc")
	if seen {
		t.Fatal("recurrence at/after TTL reported seen=true; the vendor entry would have expired, so counting it overstates the case for enabling caching")
	}
}

// The window slides on use, mirroring the vendor: an entry lives TTL from its
// last read, not from creation. Steady traffic under the TTL stays cached
// indefinitely, and a fixed window would under-count exactly that traffic.
func TestMemoryProbe_WindowSlidesOnHit(t *testing.T) {
	p := NewMemoryProbe(time.Minute)
	base := time.Now()
	cur := base
	p.now = func() time.Time { return cur }
	ctx := context.Background()

	_, _ = p.Observe(ctx, "t1", "abc") // t=0, cold

	// Touch every 40s for 4 minutes — always inside the 60s window.
	for i := 1; i <= 6; i++ {
		cur = base.Add(time.Duration(i) * 40 * time.Second)
		seen, _ := p.Observe(ctx, "t1", "abc")
		if !seen {
			t.Fatalf("touch #%d at t=%v reported cold; the window must slide on use", i, cur.Sub(base))
		}
	}
}

// A prefix recurring under a *different* tenant is not a cache hit — vendor
// caches are per-account. Conflating tenants would inflate the number that
// decides the rollout, and would leak cross-tenant traffic shape.
func TestMemoryProbe_TenantIsolation(t *testing.T) {
	p := NewMemoryProbe(time.Minute)
	ctx := context.Background()
	if _, err := p.Observe(ctx, "tenant-a", "same-hash"); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	seen, err := p.Observe(ctx, "tenant-b", "same-hash")
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if seen {
		t.Fatal("same prefix under a different tenant reported seen=true; vendor caches are per-account and must not be conflated")
	}
}

func TestMemoryProbe_DistinctHashesAreIndependent(t *testing.T) {
	p := NewMemoryProbe(time.Minute)
	ctx := context.Background()
	_, _ = p.Observe(ctx, "t1", "hash-a")
	seen, _ := p.Observe(ctx, "t1", "hash-b")
	if seen {
		t.Fatal("a different prefix reported seen=true")
	}
}

// Measurement must never be able to fail a served request: every degenerate
// input is a silent cold read, never a panic or an error.
func TestProbe_NilAndEmptyAreSafe(t *testing.T) {
	ctx := context.Background()

	var nilMem *MemoryProbe
	if seen, err := nilMem.Observe(ctx, "t1", "abc"); seen || err != nil {
		t.Fatalf("nil MemoryProbe: got (%v, %v), want (false, nil)", seen, err)
	}

	var nilRedis *RedisProbe
	if seen, err := nilRedis.Observe(ctx, "t1", "abc"); seen || err != nil {
		t.Fatalf("nil RedisProbe: got (%v, %v), want (false, nil)", seen, err)
	}

	// A RedisProbe with no client (Redis unconfigured) degrades, never panics.
	if seen, err := (&RedisProbe{}).Observe(ctx, "t1", "abc"); seen || err != nil {
		t.Fatalf("clientless RedisProbe: got (%v, %v), want (false, nil)", seen, err)
	}

	// An empty hash means "nothing cacheable in this request" (no system, no
	// tools) — it must not collapse every such request onto one shared key.
	p := NewMemoryProbe(time.Minute)
	_, _ = p.Observe(ctx, "t1", "")
	if seen, _ := p.Observe(ctx, "t1", ""); seen {
		t.Fatal("empty hash reported seen=true; requests with nothing cacheable must not alias onto a shared key")
	}
}

func TestNewProbe_TTLDefaults(t *testing.T) {
	if got := NewMemoryProbe(0).ttl; got != DefaultTTL {
		t.Fatalf("NewMemoryProbe(0).ttl = %v, want %v", got, DefaultTTL)
	}
	if got := NewMemoryProbe(-1).ttl; got != DefaultTTL {
		t.Fatalf("NewMemoryProbe(-1).ttl = %v, want %v", got, DefaultTTL)
	}
	if got := NewRedisProbe(nil, 0).TTL; got != DefaultTTL {
		t.Fatalf("NewRedisProbe(nil,0).TTL = %v, want %v", got, DefaultTTL)
	}
	// DefaultTTL must track the vendor's ephemeral cache lifetime; a mismatch
	// silently measures the wrong window.
	if DefaultTTL != 5*time.Minute {
		t.Fatalf("DefaultTTL = %v, want 5m (one Anthropic ephemeral cache lifetime)", DefaultTTL)
	}
}

func TestMemoryProbe_ConcurrentObserveIsRaceFree(t *testing.T) {
	p := NewMemoryProbe(time.Minute)
	ctx := context.Background()
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 200 {
				_, _ = p.Observe(ctx, "t1", "abc")
			}
		}()
	}
	for range 8 {
		<-done
	}
}
