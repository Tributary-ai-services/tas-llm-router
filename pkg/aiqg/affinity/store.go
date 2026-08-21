package affinity

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore keeps affinity fleet-wide.
//
// # Keys, all TTL'd
//
//	aiqg:affinity:{tenant}:{key}:{epoch}  → the affine target
//	aiqg:affinity:seen:{tenant}:{key}     → "unix:bucket", for epoch derivation
//
// Every key carries a TTL. redis-shared runs maxmemory=0 with noeviction and is
// shared with other services, so an un-TTL'd key is a permanent leak in someone
// else's datastore.
//
// The epoch is part of the KEY rather than a field inside the value. That makes
// a new epoch a new key rather than a mutation, so there is no window in which
// a stale target can be read, and an abandoned epoch expires by itself with no
// sweeper.
//
// The seen key holds the bucket alongside the timestamp so the bucket survives
// a gateway restart. Deriving it from wall-clock time instead would be simpler
// and wrong — see ComputeEpoch: what matters is the GAP between requests, not
// the clock.
type RedisStore struct {
	rdb    redis.UniversalClient
	prefix string
}

// NewRedisStore returns a fleet-wide store.
func NewRedisStore(rdb redis.UniversalClient) *RedisStore {
	return &RedisStore{rdb: rdb, prefix: "aiqg:affinity:"}
}

func (s *RedisStore) targetKey(tenant, key string, e Epoch) string {
	return s.prefix + tenant + ":" + key + ":" + e.String()
}
func (s *RedisStore) seenKey(tenant, key string) string {
	return s.prefix + "seen:" + tenant + ":" + key
}

// seenTTL outlives the affinity TTL so an idle gap can still be MEASURED after
// the affinity itself has expired. If the seen key died with the affinity, a
// returning conversation would look brand new and the epoch would never advance
// for the right reason — the gap would be invisible.
func seenTTL(ttl time.Duration) time.Duration {
	d := ttl * 4
	if d < time.Hour {
		d = time.Hour
	}
	return d
}

func (s *RedisStore) Get(ctx context.Context, tenant, key string, e Epoch) (Target, bool) {
	raw, err := s.rdb.Get(ctx, s.targetKey(tenant, key, e)).Bytes()
	if err != nil {
		return Target{}, false // fail open, including redis.Nil
	}
	var t Target
	if err := json.Unmarshal(raw, &t); err != nil || t.Provider == "" {
		return Target{}, false
	}
	return t, true
}

func (s *RedisStore) Put(ctx context.Context, tenant, key string, e Epoch, t Target, ttl time.Duration) {
	raw, err := json.Marshal(t)
	if err != nil {
		return
	}
	s.rdb.Set(ctx, s.targetKey(tenant, key, e), raw, ttl)
}

// Touch records this request's time and returns the PREVIOUS last-seen time and
// bucket, so the caller can decide whether the gap advanced the epoch.
//
// Returning the previous values rather than the new ones is deliberate: the
// caller needs to compare against what came before, and having Touch also
// decide the bucket would split the epoch rule across two files.
func (s *RedisStore) Touch(ctx context.Context, tenant, key string, now time.Time, ttl time.Duration) (time.Time, int64) {
	k := s.seenKey(tenant, key)
	prev, err := s.rdb.Get(ctx, k).Result()

	var lastSeen time.Time
	var bucket int64
	if err == nil {
		if unix, b, ok := parseSeen(prev); ok {
			lastSeen, bucket = time.Unix(unix, 0), b
		}
	}

	// Write the value the NEXT request will read: this request's time, and the
	// bucket it belongs to.
	next := bucket
	if lastSeen.IsZero() || now.Sub(lastSeen) > ttl {
		next = bucket + 1
	}
	s.rdb.Set(ctx, k, formatSeen(now.Unix(), next), seenTTL(ttl))
	return lastSeen, bucket
}

func formatSeen(unix, bucket int64) string {
	return strconv.FormatInt(unix, 10) + ":" + strconv.FormatInt(bucket, 10)
}

func parseSeen(s string) (unix, bucket int64, ok bool) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return 0, 0, false
	}
	u, err1 := strconv.ParseInt(s[:i], 10, 64)
	b, err2 := strconv.ParseInt(s[i+1:], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return u, b, true
}

// MemoryStore is a single-process store for tests and single-replica running.
//
// The wrong choice for a multi-replica deployment: the same conversation would
// get a different affinity depending on which pod answered, rebuilding the
// vendor cache each time — precisely the cost affinity exists to avoid. Server
// wiring prefers Redis and logs the downgrade.
type MemoryStore struct {
	mu      sync.Mutex
	targets map[string]Target
	seen    map[string]seenEntry
}

type seenEntry struct {
	at     time.Time
	bucket int64
}

// NewMemoryStore returns an in-process store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{targets: map[string]Target{}, seen: map[string]seenEntry{}}
}

func (m *MemoryStore) Get(_ context.Context, tenant, key string, e Epoch) (Target, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.targets[tenant+":"+key+":"+e.String()]
	return t, ok
}

func (m *MemoryStore) Put(_ context.Context, tenant, key string, e Epoch, t Target, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targets[tenant+":"+key+":"+e.String()] = t
}

func (m *MemoryStore) Touch(_ context.Context, tenant, key string, now time.Time, ttl time.Duration) (time.Time, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := tenant + ":" + key
	prev := m.seen[k]
	next := prev.bucket
	if prev.at.IsZero() || now.Sub(prev.at) > ttl {
		next = prev.bucket + 1
	}
	m.seen[k] = seenEntry{at: now, bucket: next}
	return prev.at, prev.bucket
}
