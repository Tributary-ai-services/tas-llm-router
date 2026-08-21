package routing

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Dwell state — how recently a routing context switched providers.
//
// Fleet-wide, for the same reason breaker and affinity state are: per-replica
// dwell is no dwell at all. With N replicas a conversation could switch N times
// inside one dwell window, each replica believing it was the first.

// RedisDwellStore keeps switch timestamps in Redis.
//
// Keys carry a TTL of the dwell window itself: once the window has passed the
// record has no meaning, and redis-shared runs maxmemory=0 with noeviction, so
// an un-TTL'd key is a permanent leak in a datastore shared with other services.
type RedisDwellStore struct {
	rdb redis.UniversalClient
	ctx context.Context
}

// NewRedisDwellStore returns a fleet-wide dwell store.
func NewRedisDwellStore(rdb redis.UniversalClient) *RedisDwellStore {
	return &RedisDwellStore{rdb: rdb, ctx: context.Background()}
}

func (s *RedisDwellStore) key(k string) string { return "aiqg:dwell:" + k }

func (s *RedisDwellStore) LastSwitch(k string) (time.Time, bool) {
	v, err := s.rdb.Get(s.ctx, s.key(k)).Result()
	if err != nil {
		// Fail open: an unreachable Redis must not pin routing to whatever it
		// last chose. Losing dwell costs some flapping; blocking every switch
		// would freeze routing on a cache outage.
		return time.Time{}, false
	}
	unix, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(unix, 0), true
}

func (s *RedisDwellStore) RecordSwitch(k string, at time.Time, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	s.rdb.Set(s.ctx, s.key(k), strconv.FormatInt(at.Unix(), 10), ttl)
}

// MemoryDwellStore is a single-process store for tests and single-replica
// running. See the package note on why that is the wrong choice for a fleet.
type MemoryDwellStore struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// NewMemoryDwellStore returns an in-process dwell store.
func NewMemoryDwellStore() *MemoryDwellStore {
	return &MemoryDwellStore{last: map[string]time.Time{}}
}

func (m *MemoryDwellStore) LastSwitch(k string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.last[k]
	return t, ok
}

func (m *MemoryDwellStore) RecordSwitch(k string, at time.Time, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last[k] = at
}
