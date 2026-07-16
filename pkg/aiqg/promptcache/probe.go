// Package promptcache measures the *opportunity* for vendor prompt caching
// without changing a single request (docs/AIQG-PROMPT-CACHE-CONTROL.md, P0).
//
// Vendor prompt caching pays only when a cacheable prefix recurs while a cache
// entry for it is still alive. We cannot ask the vendor "would this have hit?",
// but we can answer it ourselves: hash the cacheable prefix, remember the hash
// for one cache lifetime, and see whether it comes back. A recurrence inside
// the window is a read we are currently not taking.
//
// This is deliberately measure-only. Nothing here touches the outbound request,
// so it can run on live traffic at zero correctness risk — and it answers the
// question the design defers to data: is `auto` worth the write premium on this
// route? (§5, §9.)
//
// Sliding, not fixed. Observe refreshes the window on a hit, mirroring the
// vendor: a cache entry's TTL is measured from its last *use*, so a prefix
// touched every 4 minutes stays cached indefinitely. A fixed window would
// under-count exactly the steady traffic that benefits most.
//
// This is an upper bound on reads, not a promise of them: a prefix can recur
// inside the window and still miss at the vendor (eviction, a model switch —
// §5.1). It is a bound worth having, because if it is ~0 the feature cannot pay
// and no amount of placement cleverness changes that.
package promptcache

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultTTL is one ephemeral cache lifetime. Anthropic's default cache_control
// entry lives 5 minutes from last use, so a recurrence inside 5 minutes is the
// recurrence that would have been served from cache. Matching the vendor's
// window is the whole point — a longer one measures reuse that would have
// expired and overstates the case for enabling the feature.
const DefaultTTL = 5 * time.Minute

// probeKey namespaces the probe index away from the linkage tiers
// (aiqg:tcid: / aiqg:pfx:) so measurement can never collide with, or be
// mistaken for, attribution state. tenantID is in the key because a prefix
// recurring across tenants is not a cache hit — vendor caches are per-account,
// and conflating them would inflate the number that decides the rollout.
func probeKey(tenantID, hash string) string { return "aiqg:pcp:" + tenantID + ":" + hash }

// Probe records that a cacheable prefix was seen and reports whether it was
// already live. Implementations are safe for concurrent use.
type Probe interface {
	// Observe reports whether hash was seen within the TTL window, and marks it
	// seen (sliding the window). A true return means a vendor cache entry for
	// this prefix would plausibly have been readable — i.e. a read we are
	// currently forgoing.
	//
	// Best-effort by contract: on error it returns seen=false alongside the
	// error, so a caller that only logs can ignore the error and still be
	// correct — measurement must never be able to fail a served request.
	Observe(ctx context.Context, tenantID, hash string) (seen bool, err error)
}

// RedisProbe is the production Probe. Keys are TTL'd so the index self-prunes
// and holds nothing durable: a value is a constant, never prompt content — the
// hash is the key, and it is one-way. Nothing here is a new privacy surface.
type RedisProbe struct {
	R   redis.UniversalClient
	TTL time.Duration
}

// NewRedisProbe wraps a go-redis client; ttl<=0 uses DefaultTTL.
func NewRedisProbe(r redis.UniversalClient, ttl time.Duration) *RedisProbe {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &RedisProbe{R: r, TTL: ttl}
}

// Observe uses SetNX as the test-and-set: it succeeds only when the key was
// absent, so !ok is exactly "already live" — one round trip on the cold path,
// with no read-then-write race that could double-count a burst of concurrent
// identical requests. On a hit we slide the window with Expire; a failed slide
// is not an error worth surfacing (the observation itself already succeeded,
// and the key still expires on its original schedule).
func (p *RedisProbe) Observe(ctx context.Context, tenantID, hash string) (bool, error) {
	if p == nil || p.R == nil || hash == "" {
		return false, nil
	}
	key := probeKey(tenantID, hash)
	ok, err := p.R.SetNX(ctx, key, "1", p.TTL).Result()
	if err != nil {
		return false, err
	}
	if ok {
		return false, nil // key was absent: first sighting in this window
	}
	p.R.Expire(ctx, key, p.TTL)
	return true, nil
}

// MemoryProbe is an in-process Probe for tests and as a Redis-less fallback.
//
// Unlike RedisProbe it cannot be shared across replicas, so in a multi-replica
// deployment it under-counts: two sightings of one prefix that land on
// different pods both read as cold. It is a correctness-preserving fallback for
// tests and single-process runs, NOT a substitute for Redis when measuring —
// a fleet-wide reuse rate needs a fleet-wide index.
type MemoryProbe struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
	// now is injectable so tests can advance the clock without sleeping.
	now func() time.Time
}

// NewMemoryProbe returns an empty in-process probe; ttl<=0 uses DefaultTTL.
func NewMemoryProbe(ttl time.Duration) *MemoryProbe {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &MemoryProbe{seen: make(map[string]time.Time), ttl: ttl, now: time.Now}
}

// Observe mirrors RedisProbe: expired entries read as cold, and a live hit
// slides the window. Expired keys are dropped as they are encountered; a
// long-lived process that never re-observes a prefix would otherwise retain it
// forever (see the doc comment — prefer RedisProbe in production).
func (p *MemoryProbe) Observe(_ context.Context, tenantID, hash string) (bool, error) {
	if p == nil || hash == "" {
		return false, nil
	}
	key := probeKey(tenantID, hash)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	last, ok := p.seen[key]
	p.seen[key] = now
	if !ok {
		return false, nil
	}
	if now.Sub(last) >= p.ttl {
		return false, nil // stale: the vendor entry would have expired
	}
	return true, nil
}

// Ensure both satisfy Probe.
var (
	_ Probe = (*RedisProbe)(nil)
	_ Probe = (*MemoryProbe)(nil)
)
