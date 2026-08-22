package server

import (
	"context"
	"net/http"
	"strconv"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/providers"
	"github.com/tributary-ai/llm-router-waf/internal/routing"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/breaker"
)

// Ordered failover at the completion boundary — routing-decision.md §5.4,
// step 3.
//
// The chain is walked HERE rather than inside the router because only this
// layer knows whether an attempt actually succeeded: Route() hands back a
// provider and never sees the result.
//
// # The two classifications are not the same question
//
// Each attempt is classified twice, on purpose:
//
//	breaker.ClassifyError   — should this failure count against the provider?
//	breaker.ClassifyFailure — should this failure move to the next tier?
//
// They disagree constantly, and both disagreements matter. A 429 must not
// eject a healthy vendor, yet another vendor may well have capacity. A context
// overflow is our request being too big for this model — never the provider's
// fault — yet a larger-window tier serves it unchanged, which is the single
// clearest reason chains exist. And a malformed request is neither: every
// provider rejects it identically, so advancing would multiply one client
// error across every vendor and burn the chain to reach the same answer.

// completeWithFallback performs the completion, records the outcome for
// passive outlier detection, and walks the failover chain when the failure
// warrants it.
//
// Returns the last error when every tier is exhausted, so the caller reports
// the failure the user actually hit rather than the first one.
func (s *Server) completeWithFallback(
	r *http.Request,
	req *types.ChatRequest,
	provider providers.LLMProvider,
	metadata *types.RouterMetadata,
) (*types.ChatResponse, error) {
	ctx := r.Context()
	chain := routing.ChainFrom(ctx)

	// Pre-flight limits (step 7). A breach is turned into the SAME failure the
	// chain already acts on rather than returned to the caller, because context
	// overflow is fallback-eligible: a larger-window tier can serve the
	// identical request. Returning an error here would make limits strictly
	// worse than having none — the vendor's own 400 would at least have
	// advanced the chain, while a tidy local rejection would not.
	//
	// Detecting the problem earlier must not mean recovering from it less.
	if b := s.router.CheckLimits(ctx, metadata.Provider, req.Model, req); b != nil {
		s.logger.WithFields(logrus.Fields{
			"provider": b.Provider, "model": b.Model,
			"window": b.Window, "overage": b.Overage,
		}).Warn("Request exceeds the effective context window; treating as context overflow")
		metadata.RoutingReason = append(metadata.RoutingReason, b.Error())
		return s.walkChainFrom(r, req, metadata, chain, errLimitBreach{b})
	}
	// Output caps bound the request rather than refusing it: asking for more
	// output than the tenant permits is still a serviceable request.
	if applied, changed := s.router.ApplyOutputCap(ctx, metadata.Provider, req.Model, req); changed {
		metadata.RoutingReason = append(metadata.RoutingReason,
			"max_tokens lowered to "+strconv.Itoa(applied)+" by this route's output limit")
	}

	resp, err := s.attempt(ctx, req, provider, metadata)
	if err == nil {
		s.recordServedAffinity(r, metadata)
		return resp, nil
	}
	if !chain.Configured() {
		return resp, err
	}

	return s.walkChainFrom(r, req, metadata, chain, err)
}

// errLimitBreach presents a pre-flight limit breach as a context-overflow
// failure, so the existing classification treats it exactly like the vendor's
// own — one code path, one behaviour.
type errLimitBreach struct{ b *routing.LimitBreach }

func (e errLimitBreach) Error() string {
	// Worded to classify as context_overflow: the classifier reads the
	// message, and a breach that failed to classify would silently stop
	// advancing the chain.
	return "context length exceeded: " + e.b.Error()
}

// walkChainFrom advances the fallback chain after an initial failure.
func (s *Server) walkChainFrom(
	r *http.Request,
	req *types.ChatRequest,
	metadata *types.RouterMetadata,
	chain *routing.Chain,
	err error,
) (*types.ChatResponse, error) {
	ctx := r.Context()
	var resp *types.ChatResponse
	if !chain.Configured() {
		return nil, err
	}
	for pos := 1; ; pos++ {
		class, eligible := breaker.ClassifyFailure(err)
		if !eligible {
			// Our error, not the provider's. Say so on the decision so the
			// event explains why a configured chain did not engage — silence
			// here reads as the chain being broken.
			metadata.RoutingReason = append(metadata.RoutingReason,
				"fallback not attempted: the failure is a client error, which every provider would reject identically")
			return nil, err
		}
		if !chain.Advances(class) {
			metadata.RoutingReason = append(metadata.RoutingReason,
				"fallback not attempted: "+string(class)+" is not in this rule's fallback.on")
			return nil, err
		}
		next, tier, ok := s.router.Tier(chain, pos)
		if !ok {
			metadata.RoutingReason = append(metadata.RoutingReason,
				"fallback chain exhausted after "+string(class))
			return nil, err
		}

		s.logger.WithFields(logrus.Fields{
			"from":     metadata.Provider,
			"to":       tier.Provider,
			"model":    tier.Model,
			"position": tier.Position,
			"class":    string(class),
		}).Warn("Falling back to the next chain tier")

		// The event must record HOW FAR the chain walked and WHY, not merely
		// that a fallback happened. "Which tier served this, and what drove it
		// there" is the question an operator asks afterwards.
		metadata.FailedProviders = append(metadata.FailedProviders, metadata.Provider)
		metadata.FallbackUsed = true
		metadata.RoutingReason = append(metadata.RoutingReason,
			"fallback tier "+strconv.Itoa(tier.Position)+": "+metadata.Provider+" → "+tier.Provider+"/"+tier.Model+" on "+string(class))

		// A chain tier always names its own model — carrying the caller's
		// forward would reproduce the failure that reached the chain, which is
		// exactly why the contract requires it.
		req.Model = tier.Model
		metadata.Provider, metadata.Model = tier.Provider, tier.Model
		metadata.AttemptCount++

		// Re-check limits against the new tier: the whole point of advancing
		// on a context overflow is that a different model has a different
		// window, so a tier that also cannot fit must be skipped rather than
		// attempted.
		if b := s.router.CheckLimits(ctx, tier.Provider, tier.Model, req); b != nil {
			metadata.RoutingReason = append(metadata.RoutingReason,
				"skipped tier "+strconv.Itoa(tier.Position)+": "+b.Error())
			err = errLimitBreach{b}
			continue
		}
		s.router.ApplyOutputCap(ctx, tier.Provider, tier.Model, req)

		resp, err = s.attempt(ctx, req, next, metadata)
		if err == nil {
			// Recorded from the tier that actually served, not the one we
			// intended: sticking the next turn to a provider that just failed
			// would be worse than no affinity at all.
			s.recordServedAffinity(r, metadata)
			return resp, nil
		}
	}
}

// attempt performs one provider call and records its outcome against passive
// outlier detection.
//
// Classification matters as much as recording: a 404 for a model this provider
// does not serve is OUR error, and counting it against the provider would let
// one bad route rule eject a healthy vendor for every tenant on the gateway
// (tas-llm-router#151).
func (s *Server) attempt(
	ctx context.Context,
	req *types.ChatRequest,
	provider providers.LLMProvider,
	metadata *types.RouterMetadata,
) (*types.ChatResponse, error) {
	resp, err := provider.ChatCompletion(ctx, req)
	s.router.RecordOutcome(ctx, metadata.Provider, req.Model, breaker.ClassifyError(err))
	if err != nil {
		s.logger.WithError(err).WithField("provider", metadata.Provider).Error("Chat completion failed")
	}
	return resp, err
}

// recordServedAffinity remembers the provider that actually served, so the next
// turn in this epoch sticks to it.
func (s *Server) recordServedAffinity(r *http.Request, metadata *types.RouterMetadata) {
	if metadata == nil {
		return
	}
	s.recordAffinity(r, routing.AffinityFrom(r.Context()), metadata.Provider, metadata.Model)
}
