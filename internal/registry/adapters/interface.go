// Package adapters discovers available models from each provider for the model
// registry (Dynamic Model Registry epic, Phase 2, issue #3).
//
// The two vendors differ fundamentally in what they expose:
//   - OpenAI has a ListModels API — we filter it to chat-capable models and
//     enrich each with the pricing/capability data from static config.
//   - Anthropic has NO list-models API — we keep a curated list of known models
//     and test each one's availability with a minimal (1-token) request.
//
// Adapters depend on small injected interfaces (a model lister / a model prober)
// rather than the SDK directly, so they are unit-testable with stubs and the
// real providers supply the live implementation when the sync engine (Phase 3,
// #4) wires them.
package adapters

import (
	"context"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// ProviderAdapter discovers and validates a single provider's models.
type ProviderAdapter interface {
	// DiscoverModels returns the provider's currently available models, stamped
	// with Status and LastValidated.
	DiscoverModels(ctx context.Context) ([]types.ModelInfo, error)
	// ValidateModel reports whether a specific model is currently servable.
	ValidateModel(ctx context.Context, model string) (bool, error)
	// ResolveAlias maps a "-latest"-style alias to a concrete model id, or
	// returns the input unchanged when it is not an alias this adapter knows.
	ResolveAlias(ctx context.Context, alias string) (string, error)
	// GetProviderName is the provider key ("openai", "anthropic").
	GetProviderName() string
}

// OpenAIModelLister is the minimal surface the OpenAI adapter needs: the set of
// model ids the account currently offers (the ListModels API). The real
// provider implements it; tests stub it.
type OpenAIModelLister interface {
	ListModelIDs(ctx context.Context) ([]string, error)
}

// AnthropicModelProber tests whether a model answers a minimal request, since
// Anthropic has no list-models API. Contract:
//   - (true, nil)  → the model answered; it is available.
//   - (false, nil) → the vendor definitively rejected it (e.g. 404 not_found);
//     it is unavailable.
//   - (false, err) → transient/unknown failure (network, rate limit); the
//     caller must NOT downgrade the model on this.
type AnthropicModelProber interface {
	ProbeModel(ctx context.Context, model string) (bool, error)
}
