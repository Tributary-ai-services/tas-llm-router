package registry

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// Registry is the in-memory authority over available models. Reads are served
// from a lock-guarded snapshot so the routing hot path never makes a store
// round trip; writes (Upsert / PutAlias) update both the store and the snapshot,
// and Load rehydrates the snapshot from the store at startup and after each
// sync pass.
type Registry struct {
	store  Store
	logger *logrus.Logger

	mu      sync.RWMutex
	models  map[string]map[string]types.ModelInfo // provider -> model name -> info
	aliases map[string]map[string]string          // provider -> alias -> target
}

var _ ModelRegistry = (*Registry)(nil)

// New returns a Registry backed by store. Call Load to populate it.
func New(store Store, logger *logrus.Logger) *Registry {
	if logger == nil {
		logger = logrus.New()
	}
	return &Registry{
		store:   store,
		logger:  logger,
		models:  map[string]map[string]types.ModelInfo{},
		aliases: map[string]map[string]string{},
	}
}

// Load pulls the durable snapshot from the store into memory, replacing the
// current one. Call at startup; the sync engine (Phase 3, #4) calls it after a
// discovery pass.
func (r *Registry) Load(ctx context.Context) error {
	snap, err := r.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("registry: load: %w", err)
	}
	r.mu.Lock()
	r.models = snap.Models
	r.aliases = snap.Aliases
	r.mu.Unlock()
	return nil
}

// Upsert writes a model to the store and the in-memory snapshot and registers
// any aliases the ModelInfo declares. Discovery (Phase 2/3) lands models this
// way; static seeding and tests use it too.
func (r *Registry) Upsert(ctx context.Context, provider string, m types.ModelInfo) error {
	if provider == "" || m.Name == "" {
		return fmt.Errorf("registry: Upsert requires provider and model name")
	}
	if err := r.store.PutModel(ctx, provider, m); err != nil {
		return err
	}
	r.mu.Lock()
	if r.models[provider] == nil {
		r.models[provider] = map[string]types.ModelInfo{}
	}
	r.models[provider][m.Name] = m
	r.mu.Unlock()

	for _, alias := range m.Aliases {
		if err := r.PutAlias(ctx, provider, alias, m.Name); err != nil {
			return err
		}
	}
	return nil
}

// PutAlias records that alias resolves to target for provider, in the store and
// the snapshot.
func (r *Registry) PutAlias(ctx context.Context, provider, alias, target string) error {
	if err := r.store.PutAlias(ctx, provider, alias, target); err != nil {
		return err
	}
	r.mu.Lock()
	if r.aliases[provider] == nil {
		r.aliases[provider] = map[string]string{}
	}
	r.aliases[provider][alias] = target
	r.mu.Unlock()
	return nil
}

// GetModel returns a copy of the model info for (provider, model), or
// ErrModelNotFound.
func (r *Registry) GetModel(provider, model string) (*types.ModelInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if ms := r.models[provider]; ms != nil {
		if m, ok := ms[model]; ok {
			cp := m
			return &cp, nil
		}
	}
	return nil, ErrModelNotFound
}

// ListModels returns the provider's models sorted by name (nil provider → empty).
func (r *Registry) ListModels(provider string) ([]types.ModelInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ms := r.models[provider]
	out := make([]types.ModelInfo, 0, len(ms))
	for _, m := range ms {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ResolveAlias maps a provider alias to its canonical model name. An unknown
// name is returned unchanged — it is presumably already a real model id — so a
// caller can resolve every requested name unconditionally without special-
// casing "is this an alias".
func (r *Registry) ResolveAlias(provider, alias string) (string, error) {
	if alias == "" {
		return "", fmt.Errorf("registry: empty alias")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if as := r.aliases[provider]; as != nil {
		if target, ok := as[alias]; ok {
			return target, nil
		}
	}
	return alias, nil
}

// GetFallback returns a serviceable replacement for a model that is deprecated
// or unavailable: its declared ReplacementModel if that is itself active, else
// the first active model of the same provider (by name, for determinism).
// ErrModelNotFound when the provider has nothing serviceable.
func (r *Registry) GetFallback(provider, model string) (*types.ModelInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ms := r.models[provider]
	if len(ms) == 0 {
		return nil, ErrModelNotFound
	}
	if cur, ok := ms[model]; ok && cur.ReplacementModel != "" {
		if repl, ok := ms[cur.ReplacementModel]; ok && repl.IsActive() {
			cp := repl
			return &cp, nil
		}
	}
	names := make([]string, 0, len(ms))
	for name := range ms {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == model {
			continue
		}
		if m := ms[name]; m.IsActive() {
			cp := m
			return &cp, nil
		}
	}
	return nil, ErrModelNotFound
}

// --- Skeletons filled in later phases -------------------------------------
//
// These reach provider APIs and so belong with the discovery adapters (Phase 2,
// #3) and the sync engine (Phase 3, #4). Defined now so the ModelRegistry
// contract is complete for the router integration (Phase 4, #5) to compile
// against; they return ErrNotImplemented until then.

func (r *Registry) SyncAll(ctx context.Context) error { return ErrNotImplemented }

func (r *Registry) SyncProvider(ctx context.Context, provider string) error {
	return ErrNotImplemented
}

func (r *Registry) ValidateModel(ctx context.Context, provider, model string) (bool, error) {
	return false, ErrNotImplemented
}
