package adapters

import (
	"context"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// AnthropicAdapter discovers Anthropic models by probing a curated list, since
// Anthropic has no list-models API. Each known model is tested with a minimal
// request and stamped active/unavailable accordingly.
type AnthropicAdapter struct {
	prober AnthropicModelProber
	known  []types.ModelInfo // curated list with pricing/caps
	logger *logrus.Logger
}

var _ ProviderAdapter = (*AnthropicAdapter)(nil)

// NewAnthropicAdapter builds the adapter over a prober and the curated list of
// known Anthropic models (typically the provider's configured models).
func NewAnthropicAdapter(prober AnthropicModelProber, knownModels []types.ModelInfo, logger *logrus.Logger) *AnthropicAdapter {
	if logger == nil {
		logger = logrus.New()
	}
	return &AnthropicAdapter{prober: prober, known: knownModels, logger: logger}
}

func (a *AnthropicAdapter) GetProviderName() string { return "anthropic" }

// DiscoverModels probes every known model and returns them stamped with status.
// A transient probe error does NOT downgrade a model — it is carried through
// with its status unchanged and left unvalidated, so a blip in the vendor API
// can't wipe the registry.
func (a *AnthropicAdapter) DiscoverModels(ctx context.Context) ([]types.ModelInfo, error) {
	now := time.Now().UTC()
	out := make([]types.ModelInfo, 0, len(a.known))
	for _, km := range a.known {
		m := km
		ok, err := a.prober.ProbeModel(ctx, m.Name)
		if err != nil {
			a.logger.WithError(err).WithField("model", m.Name).
				Warn("anthropic probe failed; leaving model status unchanged")
			out = append(out, m) // status untouched (empty == active), not stamped
			continue
		}
		if ok {
			m.Status = types.ModelStatusActive
		} else {
			m.Status = types.ModelStatusUnavailable
		}
		m.LastValidated = now
		out = append(out, m)
	}
	return out, nil
}

// ValidateModel probes a single model.
func (a *AnthropicAdapter) ValidateModel(ctx context.Context, model string) (bool, error) {
	return a.prober.ProbeModel(ctx, model)
}

// ResolveAlias resolves a "<family>-latest" alias against the curated known
// list (e.g. "claude-sonnet-latest" → the newest known Claude Sonnet). Any
// other input is returned unchanged. Resolution is against the known list, not
// a live probe, so it is cheap and deterministic; the sync engine (Phase 3)
// keeps that list current.
func (a *AnthropicAdapter) ResolveAlias(_ context.Context, alias string) (string, error) {
	if !strings.HasSuffix(alias, latestSuffix) {
		return alias, nil
	}
	names := make([]string, 0, len(a.known))
	for _, m := range a.known {
		names = append(names, m.Name)
	}
	if resolved, ok := resolveLatest(alias, names); ok {
		return resolved, nil
	}
	return alias, nil
}
