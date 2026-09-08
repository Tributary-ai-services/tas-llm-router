package registry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/registry/adapters"
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

	adaptersMu sync.RWMutex
	adapters   map[string]adapters.ProviderAdapter // provider -> discovery adapter
}

var _ ModelRegistry = (*Registry)(nil)

// New returns a Registry backed by store. Call Load to populate it.
func New(store Store, logger *logrus.Logger) *Registry {
	if logger == nil {
		logger = logrus.New()
	}
	return &Registry{
		store:    store,
		logger:   logger,
		models:   map[string]map[string]types.ModelInfo{},
		aliases:  map[string]map[string]string{},
		adapters: map[string]adapters.ProviderAdapter{},
	}
}

// RegisterAdapter wires a provider's discovery adapter (Phase 2, #3) so SyncAll
// / SyncProvider / ValidateModel can reach it. Call at startup before syncing.
func (r *Registry) RegisterAdapter(provider string, a adapters.ProviderAdapter) {
	r.adaptersMu.Lock()
	r.adapters[provider] = a
	r.adaptersMu.Unlock()
}

func (r *Registry) adapterFor(provider string) adapters.ProviderAdapter {
	r.adaptersMu.RLock()
	defer r.adaptersMu.RUnlock()
	return r.adapters[provider]
}

// FindProvider returns the provider serving a model name, searching every
// provider deterministically. Used to attach a global (provider-agnostic)
// config alias to the provider that actually owns its target.
func (r *Registry) FindProvider(model string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	provs := make([]string, 0, len(r.models))
	for p := range r.models {
		provs = append(provs, p)
	}
	sort.Strings(provs)
	for _, p := range provs {
		if _, ok := r.models[p][model]; ok {
			return p, true
		}
	}
	return "", false
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

// ResolveAliasAny resolves an alias WITHOUT a known provider, searching every
// provider's alias map in deterministic order. Returns the target model, the
// provider that owns it, and ok. The router needs this because it sees a model
// name before it knows which provider will serve it.
func (r *Registry) ResolveAliasAny(alias string) (model, provider string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	provs := make([]string, 0, len(r.aliases))
	for p := range r.aliases {
		provs = append(provs, p)
	}
	sort.Strings(provs)
	for _, p := range provs {
		if target, found := r.aliases[p][alias]; found {
			return target, p, true
		}
	}
	return "", "", false
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

// SyncAll discovers and upserts models for every registered adapter, in a
// deterministic provider order. It honours ctx cancellation between providers
// (graceful shutdown) and joins per-provider errors rather than aborting on the
// first, so one flaky provider does not stop the others from refreshing.
// ErrNotImplemented when no adapters are registered.
func (r *Registry) SyncAll(ctx context.Context) error {
	r.adaptersMu.RLock()
	names := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		names = append(names, name)
	}
	r.adaptersMu.RUnlock()
	if len(names) == 0 {
		return fmt.Errorf("%w: no adapters registered", ErrNotImplemented)
	}
	sort.Strings(names)

	var errs []error
	for _, provider := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.SyncProvider(ctx, provider); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SyncProvider discovers one provider's models via its adapter and upserts them,
// logging new models and status transitions so a deprecation or removal is
// visible rather than silent. ErrNotImplemented when the provider has no
// adapter.
func (r *Registry) SyncProvider(ctx context.Context, provider string) error {
	a := r.adapterFor(provider)
	if a == nil {
		return fmt.Errorf("%w: no adapter for provider %q", ErrNotImplemented, provider)
	}
	discovered, err := a.DiscoverModels(ctx)
	if err != nil {
		return fmt.Errorf("registry: discover %s: %w", provider, err)
	}
	for _, m := range discovered {
		prev, prevErr := r.GetModel(provider, m.Name)
		switch {
		case errors.Is(prevErr, ErrModelNotFound):
			r.logger.WithFields(logrus.Fields{"provider": provider, "model": m.Name, "status": m.Status}).
				Info("registry: new model discovered")
		case prevErr == nil && prev.Status != m.Status:
			r.logger.WithFields(logrus.Fields{"provider": provider, "model": m.Name, "from": prev.Status, "to": m.Status}).
				Info("registry: model status changed")
		}
		if err := r.Upsert(ctx, provider, m); err != nil {
			return fmt.Errorf("registry: upsert %s/%s: %w", provider, m.Name, err)
		}
	}
	return nil
}

// ValidateModel delegates to the provider's adapter. ErrNotImplemented when the
// provider has no adapter.
func (r *Registry) ValidateModel(ctx context.Context, provider, model string) (bool, error) {
	a := r.adapterFor(provider)
	if a == nil {
		return false, fmt.Errorf("%w: no adapter for provider %q", ErrNotImplemented, provider)
	}
	return a.ValidateModel(ctx, model)
}
