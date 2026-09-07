package adapters

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// OpenAIAdapter discovers OpenAI models from the ListModels API, filters to the
// chat-capable ones, and enriches each with pricing/capability data from the
// provider's static config.
type OpenAIAdapter struct {
	lister OpenAIModelLister
	static map[string]types.ModelInfo // model id -> static pricing/caps
	logger *logrus.Logger
}

var _ ProviderAdapter = (*OpenAIAdapter)(nil)

// NewOpenAIAdapter builds the adapter over a model lister and the provider's
// statically-configured models (used to enrich discovered ids with pricing and
// capabilities that the ListModels API does not return).
func NewOpenAIAdapter(lister OpenAIModelLister, staticModels []types.ModelInfo, logger *logrus.Logger) *OpenAIAdapter {
	if logger == nil {
		logger = logrus.New()
	}
	idx := make(map[string]types.ModelInfo, len(staticModels)*2)
	for _, m := range staticModels {
		if m.Name != "" {
			idx[m.Name] = m
		}
		if m.ProviderModelID != "" {
			idx[m.ProviderModelID] = m
		}
	}
	return &OpenAIAdapter{lister: lister, static: idx, logger: logger}
}

func (a *OpenAIAdapter) GetProviderName() string { return "openai" }

// DiscoverModels lists the account's models and returns the chat-capable ones,
// enriched from static config and stamped active/now.
func (a *OpenAIAdapter) DiscoverModels(ctx context.Context) ([]types.ModelInfo, error) {
	ids, err := a.lister.ListModelIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("openai discover: %w", err)
	}
	now := time.Now().UTC()
	out := make([]types.ModelInfo, 0, len(ids))
	for _, id := range ids {
		if !isChatCapableOpenAI(id) {
			continue
		}
		m, ok := a.static[id]
		if !ok {
			// Discovered but not in config: carry it anyway (routing may still
			// use it) with just the id — pricing/caps stay zero until config or
			// a later enrichment fills them.
			m = types.ModelInfo{Name: id, ProviderModelID: id}
		}
		m.Status = types.ModelStatusActive
		m.LastValidated = now
		out = append(out, m)
	}
	return out, nil
}

// ValidateModel reports whether the model id appears in the ListModels output.
func (a *OpenAIAdapter) ValidateModel(ctx context.Context, model string) (bool, error) {
	ids, err := a.lister.ListModelIDs(ctx)
	if err != nil {
		return false, fmt.Errorf("openai validate: %w", err)
	}
	return slices.Contains(ids, model), nil
}

// ResolveAlias resolves a "<family>-latest" alias against the live model list;
// any other input is returned unchanged.
func (a *OpenAIAdapter) ResolveAlias(ctx context.Context, alias string) (string, error) {
	if !strings.HasSuffix(alias, latestSuffix) {
		return alias, nil
	}
	ids, err := a.lister.ListModelIDs(ctx)
	if err != nil {
		return alias, fmt.Errorf("openai resolve alias: %w", err)
	}
	chat := make([]string, 0, len(ids))
	for _, id := range ids {
		if isChatCapableOpenAI(id) {
			chat = append(chat, id)
		}
	}
	if resolved, ok := resolveLatest(alias, chat); ok {
		return resolved, nil
	}
	return alias, nil
}

// isChatCapableOpenAI keeps the chat/completions families and drops the
// non-chat ones the ListModels API also returns (embeddings, audio, image,
// moderation, etc.), which would otherwise pollute the registry.
func isChatCapableOpenAI(id string) bool {
	id = strings.ToLower(id)
	for _, bad := range []string{
		"embedding", "whisper", "tts", "audio", "realtime", "transcribe",
		"moderation", "dall-e", "image", "babbage", "davinci", "search", "similarity", "edit",
	} {
		if strings.Contains(id, bad) {
			return false
		}
	}
	return strings.HasPrefix(id, "gpt-") ||
		strings.HasPrefix(id, "chatgpt-") ||
		strings.HasPrefix(id, "o1") ||
		strings.HasPrefix(id, "o3") ||
		strings.HasPrefix(id, "o4")
}
