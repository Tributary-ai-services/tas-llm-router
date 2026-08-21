package server

import (
	"net/http"
	"time"

	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/routing"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/affinity"
)

// Provider affinity at the request boundary — routing-decision.md §5.5,
// cache-keys-and-sessions.md §4.
//
// Two halves, deliberately split around the vendor call: resolve BEFORE
// routing so the decision can prefer the affine provider, and record AFTER so
// the next turn sticks to what actually served this one — not to what we hoped
// would.

// resolveAffinity computes the epoch and looks up any affine target, returning
// a context carrying the decision.
//
// The usable predicate is what keeps affinity subordinate: it is consulted
// before a stored target is ever offered, so a warm cache on an ejected or
// denied provider is never proposed in the first place. Filtering afterwards
// would leave a window where the wrong provider had already been chosen.
func (s *Server) resolveAffinity(r *http.Request, req *types.ChatRequest) (*http.Request, affinity.Decision) {
	if s.affinity == nil || !s.affinity.Enabled() {
		return r, affinity.Decision{}
	}
	// Same accessor the response caches use — the tenant lives in the AIQG
	// Path-A token, not in security.GetAuthInfo.
	tenantID := middleware.ResolvedTenantFromContext(r.Context())
	key := affinityKeyFor(r)
	if key == "" {
		// No conversation identifier: the common case for single-shot API
		// traffic. Guessing an identity would be worse than having none.
		return r, affinity.Decision{}
	}

	chain := routing.ChainFrom(r.Context())
	usable := func(provider string) bool {
		if !chain.AllowedTarget(provider) {
			return false // compliance outranks economics, always
		}
		return s.router.ProviderHealthy(provider)
	}

	d := s.affinity.Resolve(r.Context(), tenantID, key, cachePrefixHash(req), time.Now(), usable)
	ctx := routing.WithAffinity(r.Context(), d)
	middleware.StampAffinity(ctx, d.Held, d.Epoch.String(), d.Reason)
	return r.WithContext(ctx), d
}

// recordAffinity remembers the provider that actually served this request.
//
// Recorded from the SERVED provider rather than the intended one, because a
// fallback or a breaker reselection may have moved it — and sticking the next
// turn to a provider that just failed would be worse than no affinity at all.
func (s *Server) recordAffinity(r *http.Request, d affinity.Decision, provider, model string) {
	if s.affinity == nil || !s.affinity.Enabled() || provider == "" {
		return
	}
	// Same accessor the response caches use — the tenant lives in the AIQG
	// Path-A token, not in security.GetAuthInfo.
	tenantID := middleware.ResolvedTenantFromContext(r.Context())
	key := affinityKeyFor(r)
	if key == "" {
		return
	}
	s.affinity.Record(r.Context(), tenantID, key, d.Epoch, provider, model, time.Now())
}

// affinityKeyFor derives the identity to stick on.
//
// Reuses the conversation identity the experiment runner already resolves —
// TAS-Conversation-Id, falling back to W3C baggage session.id — rather than
// inventing a second vocabulary with the same fallbacks and the same edge
// cases. Two vocabularies for one idea drift.
func affinityKeyFor(r *http.Request) string {
	if v := r.Header.Get("TAS-Conversation-Id"); v != "" {
		return v
	}
	return middleware.BaggageSessionID(r.Header.Get("baggage"))
}
