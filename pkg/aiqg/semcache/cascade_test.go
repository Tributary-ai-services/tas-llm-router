package semcache

import (
	"context"
	"hash/fnv"
	"strings"
	"testing"
	"time"
)

// bagEmbedder is a deterministic bag-of-words embedder for tests: each lowercase
// word is hashed into a fixed-dim vector. Texts sharing words get high cosine —
// so paraphrases and near-duplicates (Sapphire vs Sapphire Reserve) both surface
// as L1 candidates, which is exactly when L2 has to do its job.
type bagEmbedder struct{ dim int }

func (b bagEmbedder) Dim() int { return b.dim }
func (b bagEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	v := make([]float32, b.dim)
	for w := range strings.FieldsSeq(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(w))
		v[h.Sum32()%uint32(b.dim)] += 1
	}
	return v, nil
}

func newCache(t *testing.T, shadow bool) (*Cache, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore(0)
	cfg := Config{Enabled: true, Shadow: shadow, MinSimilarity: 0.8, CandidateK: 5, TTL: time.Hour}
	return New(cfg, store, bagEmbedder{dim: 256}), store
}

func seed(t *testing.T, c *Cache, scope Scope, prompt, resp string) {
	t.Helper()
	if err := c.Store(context.Background(), scope, "k:"+prompt, prompt, []byte(resp), 100); err != nil {
		t.Fatalf("store: %v", err)
	}
}

// A clean paraphrase (same entities, same polarity, high cosine) is a hit.
func TestCascade_ParaphraseHit(t *testing.T) {
	c, _ := newCache(t, false /* serving */)
	sc := scopeOf("t1", "m1")
	seed(t, c, sc, "how do I reset my password", "click forgot password")

	out := c.Lookup(context.Background(), sc, "how do I reset my password")
	if out.State != StateSemanticHit {
		t.Fatalf("expected semantic_hit, got %q (sim=%.3f reason=%s)", out.State, out.Similarity, out.RejectReason)
	}
	if string(out.Entry.Response) != "click forgot password" {
		t.Errorf("wrong response served: %s", out.Entry.Response)
	}
}

// The Sapphire/Reserve near-duplicate surfaces at L1 (high cosine) but L2 rejects
// it → miss, not a wrong-answer hit. The whole reason the cascade exists.
func TestCascade_NearDuplicateRejectedByL2(t *testing.T) {
	c, _ := newCache(t, false)
	sc := scopeOf("t1", "m1")
	// Real prompts capitalize proper nouns; the embedder lowercases internally
	// (high cosine), but L2 sees the original casing (entity "Reserve" differs).
	seed(t, c, sc, "What are the fees on the Chase Sapphire Reserve card", "reserve: $550/yr")

	out := c.Lookup(context.Background(), sc, "What are the fees on the Chase Sapphire card")
	if out.State != StateMiss {
		t.Fatalf("near-duplicate must MISS, got %q (entry=%v)", out.State, out.Entry)
	}
	if out.Similarity < 0.8 {
		t.Errorf("expected a high-similarity L1 candidate (that L2 then rejects), sim=%.3f", out.Similarity)
	}
	if out.RejectReason != "entity_number_date" {
		t.Errorf("expected L2 entity rejection recorded, got %q", out.RejectReason)
	}
}

// Shadow mode: a passing L2 candidate is reported as shadow_hit but never served.
func TestCascade_ShadowMode(t *testing.T) {
	c, _ := newCache(t, true /* shadow */)
	if c.Serving() {
		t.Fatal("shadow cache must report Serving()=false")
	}
	sc := scopeOf("t1", "m1")
	seed(t, c, sc, "how do I reset my password", "click forgot password")
	out := c.Lookup(context.Background(), sc, "how can I reset my password")
	if out.State != StateShadowHit {
		t.Fatalf("expected shadow_hit, got %q (sim=%.3f)", out.State, out.Similarity)
	}
}

// Tenant isolation: an identical prompt under a different tenant never hits.
func TestCascade_TenantIsolation(t *testing.T) {
	c, _ := newCache(t, false)
	seed(t, c, scopeOf("t1", "m1"), "how do I reset my password", "t1 answer")

	out := c.Lookup(context.Background(), scopeOf("t2", "m1"), "how do I reset my password")
	if out.State != StateMiss || out.Entry != nil {
		t.Fatalf("cross-tenant must MISS with no entry, got %q %v", out.State, out.Entry)
	}
}

// Cross-model never cross-serves.
func TestCascade_ModelIsolation(t *testing.T) {
	c, _ := newCache(t, false)
	seed(t, c, scopeOf("t1", "m1"), "how do I reset my password", "m1 answer")
	if out := c.Lookup(context.Background(), scopeOf("t1", "m2"), "how do I reset my password"); out.State != StateMiss {
		t.Fatalf("cross-model must MISS, got %q", out.State)
	}
}

// A disabled / nil cache is a no-op miss with no panic.
func TestCascade_DisabledAndNil(t *testing.T) {
	var nilC *Cache
	if out := nilC.Lookup(context.Background(), scopeOf("t1", "m1"), "x"); out.State != "" {
		t.Errorf("nil cache should return empty outcome, got %q", out.State)
	}
	off := New(Config{Enabled: false}, NewMemoryStore(0), bagEmbedder{dim: 64})
	if out := off.Lookup(context.Background(), scopeOf("t1", "m1"), "x"); out.State != "" {
		t.Errorf("disabled cache should return empty outcome, got %q", out.State)
	}
}

func TestMemoryStore_TTLAndPurge(t *testing.T) {
	store := NewMemoryStore(0)
	sc := scopeOf("t1", "m1")
	base := time.Unix(1000, 0)
	timeNow = func() time.Time { return base }
	defer func() { timeNow = time.Now }()
	e := &Entry{Key: "k1", Prompt: "p", Embedding: []float32{1, 0}, TenantID: "t1", Model: "m1", ScoringVersion: "v1"}
	_ = store.Put(context.Background(), e, 30*time.Second)

	if got, _ := store.Search(context.Background(), sc, []float32{1, 0}, 0.9, 5); len(got) != 1 {
		t.Fatalf("expected 1 candidate before TTL, got %d", len(got))
	}
	base = base.Add(31 * time.Second)
	if got, _ := store.Search(context.Background(), sc, []float32{1, 0}, 0.9, 5); len(got) != 0 {
		t.Errorf("expected 0 candidates after TTL, got %d", len(got))
	}
	// Purge scoping.
	base = time.Unix(1000, 0)
	_ = store.Put(context.Background(), &Entry{Key: "k2", Prompt: "p", Embedding: []float32{1, 0}, TenantID: "t1", Model: "m1", ScoringVersion: "v1"}, time.Hour)
	_ = store.Put(context.Background(), &Entry{Key: "k3", Prompt: "p", Embedding: []float32{1, 0}, TenantID: "t2", Model: "m1", ScoringVersion: "v1"}, time.Hour)
	if n, _ := store.PurgeTenant(context.Background(), "t1"); n == 0 {
		t.Error("purge should remove t1 entries")
	}
	if got, _ := store.Search(context.Background(), scopeOf("t2", "m1"), []float32{1, 0}, 0.9, 5); len(got) != 1 {
		t.Error("purge of t1 must not touch t2")
	}
}
