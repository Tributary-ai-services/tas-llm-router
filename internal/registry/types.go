// Package registry is the central authority for which LLM models are available
// across providers (Dynamic Model Registry epic, issue #2).
//
// Model configuration was static — YAML plus hardcoded Go defaults — so when a
// vendor deprecated a model (claude-3-5-sonnet-20241022) or shipped a new one,
// the router failed or missed it. This package holds an in-memory snapshot of
// known models (fast, lock-guarded reads on the routing hot path) backed by a
// durable Store that survives restarts and is shared across replicas.
//
// Phase 1 (this) is the foundation: the ModelRegistry contract, the Store with
// Redis and in-memory implementations, model/alias/fallback resolution, and the
// extended types.ModelInfo status fields. Discovery adapters (Phase 2, #3) and
// the periodic sync engine (Phase 3, #4) fill the skeleton methods; the router
// integration (Phase 4, #5) consumes this contract.
package registry

import (
	"context"
	"errors"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// ErrModelNotFound is returned when a (provider, model) is not in the registry.
var ErrModelNotFound = errors.New("registry: model not found")

// ErrNotImplemented marks the skeleton methods whose behaviour lands in later
// phases — model discovery (Phase 2, #3) and periodic sync (Phase 3, #4). They
// are defined now so the ModelRegistry contract is stable for the router
// integration (Phase 4, #5) to build against.
var ErrNotImplemented = errors.New("registry: not implemented until a later phase")

// ModelRegistry is the authority for available models across providers.
//
// The read methods (GetModel, ListModels, ResolveAlias, GetFallback) serve from
// the in-memory snapshot and take no context — they are non-blocking and safe
// to call per request. The sync/validate methods reach providers and take a
// context; they are skeletons in Phase 1.
type ModelRegistry interface {
	GetModel(provider, model string) (*types.ModelInfo, error)
	ListModels(provider string) ([]types.ModelInfo, error)
	ResolveAlias(provider, alias string) (string, error)
	GetFallback(provider, model string) (*types.ModelInfo, error)
	SyncAll(ctx context.Context) error
	SyncProvider(ctx context.Context, provider string) error
	ValidateModel(ctx context.Context, provider, model string) (bool, error)
}
