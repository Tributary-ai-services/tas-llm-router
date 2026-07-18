package semcache

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"
)

// MemoryStore is a brute-force FLAT cosine VectorStore held in one process.
//
// FLAT (exact brute force) is what the design mandates at this scale — at
// 10k–100k entries it is exact, with no recall loss on a correctness-sensitive
// path (§7.2). This implementation is per-pod, so it is NOT production-viable
// (a per-replica cache divides the hit rate by replica count, §7). It exists to
// (a) unit-test the cascade + L2 gate without infra, and (b) drive S1 shadow
// mode on a single replica until the shared Redis 8 / pgvector store lands.
type MemoryStore struct {
	mu      sync.RWMutex
	entries map[string]storedEntry
	maxKeys int
}

type storedEntry struct {
	e       Entry
	expires time.Time
}

// NewMemoryStore bounds the map at maxKeys (<=0 → 8192).
func NewMemoryStore(maxKeys int) *MemoryStore {
	if maxKeys <= 0 {
		maxKeys = 8192
	}
	return &MemoryStore{entries: make(map[string]storedEntry), maxKeys: maxKeys}
}

func (s *MemoryStore) Put(_ context.Context, e *Entry, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[e.Key]; !exists && len(s.entries) >= s.maxKeys {
		for k := range s.entries { // evict one arbitrary entry (recompute cost only)
			delete(s.entries, k)
			break
		}
	}
	var exp time.Time
	if ttl > 0 {
		exp = timeNow().Add(ttl)
	}
	cp := *e
	s.entries[e.Key] = storedEntry{e: cp, expires: exp}
	return nil
}

func (s *MemoryStore) Search(_ context.Context, scope Scope, vec []float32, minSimilarity float64, k int) ([]Candidate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := timeNow()
	var out []Candidate
	for _, se := range s.entries {
		if !se.expires.IsZero() && now.After(se.expires) {
			continue // passively expired (a reaper prunes lazily; skip here)
		}
		// TAG pre-filter — scope isolation is a hard boundary, mirrors the Redis
		// index's @tenant/@model/@scoring_version filters (§7.3, §8).
		if se.e.TenantID != scope.TenantID || se.e.Model != scope.Model || se.e.ScoringVersion != scope.ScoringVersion {
			continue
		}
		sim := cosineSimilarity(vec, se.e.Embedding)
		if sim < minSimilarity {
			continue
		}
		cp := se.e
		out = append(out, Candidate{Entry: &cp, Similarity: sim})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Similarity > out[j].Similarity })
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out, nil
}

func (s *MemoryStore) PurgeTenant(_ context.Context, tenantID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, se := range s.entries {
		if se.e.TenantID == tenantID {
			delete(s.entries, k)
			n++
		}
	}
	return n, nil
}

// timeNow is overridable in tests (TTL expiry without sleeping).
var timeNow = time.Now

// cosineSimilarity returns cosine similarity in [0,1] for non-negative use,
// clamped. Mismatched or zero-norm vectors score 0 (never a hit).
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return clamp01(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}
