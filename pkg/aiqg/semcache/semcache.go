// Package semcache implements the AIQG semantic response cache (C4,
// docs/AIQG-SEMANTIC-CACHING.md) — serving a stored answer for a prompt that is
// *semantically similar*, not byte-identical, to one already answered.
//
// Semantic caching is the only AIQG lever that can return a WRONG answer, so the
// design is a CASCADE, not a threshold lookup (§1.1 — no single similarity
// threshold reaches an acceptable false-hit rate):
//
//	L0  exact match      → pkg/aiqg/responsecache (C1). Handled before this package.
//	L1  candidate gen     → embed + vector RANGE search. A shortlist, not a decision.
//	L2  verification gate → cheap deterministic guards; ALL must pass (this package).
//	L3  async judge       → off the critical path; learns. (later stage)
//
// This package is the store-agnostic core: the L1/L2 cascade, the L2 guards, the
// VectorStore + Embedder ports, and an in-process store for tests/shadow. The
// production vector store (Redis 8 FT.* or pgvector — §7) plugs in behind
// VectorStore once the infra lands; nothing here depends on that choice.
//
// L2 is "the part that makes it correct" (§5). Its guards are LEXICAL on purpose:
// the failure mode is embedding separation, so the gate must not be another
// embedding comparison (that's the same sensor twice). The discriminators are
// entities, numbers, dates and negation — exactly where cosine fails ("Chase
// Sapphire" vs "Chase Sapphire Reserve" sit at cosine 0.99).
package semcache

import (
	"context"
	"time"
)

// Entry is one stored semantic-cache record. Prompt is the POST-redaction prompt
// text (never raw PII — §11), kept so L2's lexical guards can run against it.
type Entry struct {
	// Key is the storage key (tenant-namespaced by the store).
	Key string
	// Prompt is the canonical post-redaction prompt text of the stored request.
	Prompt string
	// Response is the marshaled, post-outbound-scan response (safe to serve).
	Response []byte
	// Embedding is the prompt's vector (same model/dim as lookups).
	Embedding []float32
	// Scope — must match the incoming request exactly on a hit (L2 guard d).
	TenantID       string
	Model          string
	ScoringVersion string
	// CreatedAtUnix drives the freshness guard (L2 guard c).
	CreatedAtUnix int64
	// ClearScore is the cached CLEAR composite carried with the entry (a hit
	// serves the cached score, not a recompute — AIQG-CACHING.md §6).
	ClearScore float64
}

// Candidate is one L1 search result: an entry plus its similarity to the query.
// Similarity is cosine similarity in [0,1] (1 = identical); the store converts
// its native distance metric to this so the cascade speaks one scale.
type Candidate struct {
	Entry      *Entry
	Similarity float64
}

// VectorStore is the L1 port: a shared, tenant-scoped vector index with TTL.
// Search is a RANGE query ("everything within the similarity floor"), NOT
// unbounded KNN — KNN with no floor is a false-hit generator (§7.3).
type VectorStore interface {
	// Search returns candidates for (tenant, model, scoringVersion) whose cosine
	// similarity to vec is >= minSimilarity, most-similar first, capped at k.
	Search(ctx context.Context, scope Scope, vec []float32, minSimilarity float64, k int) ([]Candidate, error)
	// Put stores an entry with a TTL.
	Put(ctx context.Context, e *Entry, ttl time.Duration) error
	// PurgeTenant flushes a tenant's namespace (right-to-be-forgotten).
	PurgeTenant(ctx context.Context, tenantID string) (int, error)
}

// Embedder is the L1 embedding port. Implementations wrap TEI / Ollama; the
// cascade only needs a text→vector map. Deterministic per (model, text).
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	// Dim is the embedding dimensionality (must match the store's index).
	Dim() int
}

// Scope is the exact-match isolation tuple. tenant is the cardinal boundary — a
// cross-tenant semantic hit is the worst failure (§8), so it is always both a
// store filter AND an L2 guard.
type Scope struct {
	TenantID       string
	Model          string
	ScoringVersion string
}

// Config is the semantic-cache profile (§10). Default zero value is a disabled,
// shadow-safe cache: Enabled=false serves nothing.
type Config struct {
	// Enabled turns the cascade on at all. Default off.
	Enabled bool
	// Shadow: run the full cascade (embed, search, L2) and LOG what WOULD have
	// hit, but SERVE NOTHING (§15 S1). The zero-risk first mode. When false and
	// Enabled, a passing L2 hit is actually served.
	Shadow bool
	// MinSimilarity is the L1 candidate-generation floor (cosine, 0..1). NOT the
	// accept decision — that's L2. Starting value ~0.95 (§9.3), to be replaced by
	// calibration, not defended.
	MinSimilarity float64
	// CandidateK caps L1 results fed to L2.
	CandidateK int
	// TTL bounds entry lifetime (also the freshness ceiling).
	TTL time.Duration
}

// clamp01 bounds a similarity to [0,1].
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
