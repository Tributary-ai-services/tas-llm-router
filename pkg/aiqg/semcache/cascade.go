package semcache

import (
	"context"
	"time"
)

// Outcome is what the cascade decided for one lookup — the record that drives
// the event fields (cache_state / cache_similarity / cache_threshold) and, in
// shadow mode, the "what would have hit" log (§14, §15 S1).
type Outcome struct {
	// State is "semantic_hit" (L2 passed AND not shadow), "shadow_hit" (L2 passed
	// but shadow mode suppressed serving), "miss" (no L1 candidate or L2 rejected
	// all), or "" when the cache didn't run (disabled / not eligible).
	State string
	// Entry is the winning entry — set only for semantic_hit and shadow_hit.
	Entry *Entry
	// TopCandidate is the closest L1 candidate regardless of the L2 verdict (set
	// whenever any candidate was generated). On a miss it carries the near-miss
	// that L2 rejected, so the L3 judge can grade whether the rejection was right.
	TopCandidate *Entry
	// Similarity is the L1 cosine similarity of the top candidate considered.
	Similarity float64
	// Threshold is the L1 floor in force (for the event / danger-band chart).
	Threshold float64
	// RejectReason is the L2 guard that rejected the top candidate (empty when a
	// candidate passed or none was generated). Feeds the shadow-mode analysis.
	RejectReason string
}

const (
	StateSemanticHit = "semantic_hit"
	StateShadowHit   = "shadow_hit"
	StateMiss        = "miss"
)

// Cache is the store-agnostic semantic cascade (L1 candidate gen → L2 gate). L0
// (exact match) is handled upstream by C1 before this runs; L3 (async judge) is
// a later stage. Nil-safe: a nil *Cache Lookups to a no-op miss so callers need
// no branch.
type Cache struct {
	cfg   Config
	store VectorStore
	embed Embedder
}

// New builds a semantic cache. A nil store or embedder, or Enabled=false, yields
// a cache whose Lookup always returns an empty (didn't-run) Outcome.
func New(cfg Config, store VectorStore, embed Embedder) *Cache {
	return &Cache{cfg: cfg, store: store, embed: embed}
}

func (c *Cache) ready() bool {
	return c != nil && c.cfg.Enabled && c.store != nil && c.embed != nil
}

// Serving reports whether a passing L2 hit would actually be served (vs shadow-
// logged). False in shadow mode.
func (c *Cache) Serving() bool { return c.ready() && !c.cfg.Shadow }

// Lookup runs the cascade for one post-redaction prompt. It NEVER mutates the
// request and NEVER serves in shadow mode. Errors from the embedder/store are
// swallowed into a miss (a cache must fail open — a broken cache is a slow path,
// never a wrong answer).
func (c *Cache) Lookup(ctx context.Context, scope Scope, prompt string) Outcome {
	if !c.ready() || prompt == "" {
		return Outcome{}
	}
	out := Outcome{State: StateMiss, Threshold: c.cfg.MinSimilarity}

	// L1 — embed + range search. A shortlist, not a decision.
	vec, err := c.embed.Embed(ctx, prompt)
	if err != nil || len(vec) == 0 {
		return out
	}
	k := c.cfg.CandidateK
	if k <= 0 {
		k = 5
	}
	cands, err := c.store.Search(ctx, scope, vec, c.cfg.MinSimilarity, k)
	if err != nil || len(cands) == 0 {
		return out
	}

	// L2 — verification gate. Candidates are similarity-sorted; the first that
	// passes ALL guards wins. Record the top candidate's similarity + the reason
	// the strongest rejected candidate failed (for shadow analysis).
	now := timeNow()
	out.Similarity = cands[0].Similarity
	out.TopCandidate = cands[0].Entry // the closest match, hit or miss (for L3)
	out.RejectReason = ""             // set below if the top candidate fails
	for i, cand := range cands {
		res := Verify(prompt, cand.Entry, scope, now, c.cfg.TTL)
		if res.Pass {
			if c.cfg.Shadow {
				out.State = StateShadowHit // would have hit — logged, not served
			} else {
				out.State = StateSemanticHit
			}
			out.Entry = cand.Entry
			out.Similarity = cand.Similarity
			return out
		}
		if i == 0 {
			out.RejectReason = res.Reason // why the closest match was not a hit
		}
	}
	return out // miss: candidates existed but none survived L2
}

// Store embeds and persists a (prompt, response) pair for future semantic hits.
// Called on a miss after the vendor responds (post-outbound-scan). Best-effort;
// a store failure is not a request error.
func (c *Cache) Store(ctx context.Context, scope Scope, key, prompt string, response []byte, clearScore float64) error {
	if !c.ready() || prompt == "" {
		return nil
	}
	vec, err := c.embed.Embed(ctx, prompt)
	if err != nil || len(vec) == 0 {
		return err
	}
	return c.store.Put(ctx, &Entry{
		Key:            key,
		Prompt:         prompt,
		Response:       response,
		Embedding:      vec,
		TenantID:       scope.TenantID,
		Model:          scope.Model,
		ScoringVersion: scope.ScoringVersion,
		CreatedAtUnix:  now().Unix(),
		ClearScore:     clearScore,
	}, c.cfg.TTL)
}

// now is a package indirection so Store + tests share the clock with the store.
func now() time.Time { return timeNow() }
