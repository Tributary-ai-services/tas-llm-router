package semcache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestRedisStore_Integration exercises the real RediSearch FLAT vector index. It
// is skipped unless REDIS_SEMCACHE_ADDR points at a redis-stack instance (run it
// via a port-forward to redis-semcache). Uses a throwaway index name so it never
// collides with the production index.
func TestRedisStore_Integration(t *testing.T) {
	addr := os.Getenv("REDIS_SEMCACHE_ADDR")
	if addr == "" {
		t.Skip("set REDIS_SEMCACHE_ADDR to run the RediSearch integration test")
	}
	ctx := context.Background()
	rc := redis.NewClient(&redis.Options{Addr: addr})
	defer rc.Close()

	s := &RedisStore{rc: rc, index: "aiqg_scache_itest", dim: 4}
	_ = rc.FTDropIndex(ctx, s.index) // clean slate (ignore error)
	if err := s.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	// EnsureIndex must be idempotent.
	if err := s.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex idempotent: %v", err)
	}

	put := func(key, tenant, prompt string, vec []float32) {
		e := &Entry{Key: key, Prompt: prompt, Response: []byte("resp-" + key), Embedding: vec,
			TenantID: tenant, Model: "m1", ScoringVersion: "v1", CreatedAtUnix: time.Now().Unix()}
		if err := s.Put(ctx, e, time.Hour); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	// Two near-identical vectors under t1, one orthogonal, one under t2.
	put("a", "t1", "how do I reset my password", []float32{1, 0, 0, 0})
	put("b", "t1", "how can I reset my password", []float32{0.99, 0.05, 0, 0})
	put("c", "t1", "unrelated question about billing", []float32{0, 1, 0, 0})
	put("d", "t2", "how do I reset my password", []float32{1, 0, 0, 0})

	// Range search under t1 for a vector close to a/b, floor 0.9.
	cands, err := s.Search(ctx, Scope{TenantID: "t1", Model: "m1", ScoringVersion: "v1"},
		[]float32{1, 0, 0, 0}, 0.9, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(cands) < 2 {
		t.Fatalf("expected >=2 similar candidates (a,b), got %d: %+v", len(cands), cands)
	}
	// Top candidate is the exact match, and the orthogonal 'c' must be excluded.
	if cands[0].Similarity < 0.99 {
		t.Errorf("top similarity should be ~1.0, got %.3f", cands[0].Similarity)
	}
	for _, c := range cands {
		if c.Entry.Key == s.redisKey("t1", "c") {
			t.Error("orthogonal entry c must be below the 0.9 floor")
		}
		if c.Entry.TenantID != "t1" {
			t.Errorf("tenant isolation violated: got %s", c.Entry.TenantID)
		}
	}

	// Tenant isolation: t2's identical vector must not appear for t1.
	for _, c := range cands {
		if c.Entry.Key == s.redisKey("t2", "d") {
			t.Fatal("cross-tenant leak: t2 entry returned for t1 search")
		}
	}

	// Purge scoping.
	n, err := s.PurgeTenant(ctx, "t1")
	if err != nil || n < 3 {
		t.Fatalf("purge t1: n=%d err=%v", n, err)
	}
	if got, _ := s.Search(ctx, Scope{TenantID: "t2", Model: "m1", ScoringVersion: "v1"}, []float32{1, 0, 0, 0}, 0.9, 5); len(got) != 1 {
		t.Errorf("purge of t1 must leave t2 intact, got %d", len(got))
	}
	_ = rc.FTDropIndex(ctx, s.index)
}
