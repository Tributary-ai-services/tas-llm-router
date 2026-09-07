package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// Snapshot is the whole registry state: models keyed by provider then model
// name, and per-provider alias→target maps. A Store.Load returns one; the
// Registry keeps it in memory and serves reads from it.
type Snapshot struct {
	Models  map[string]map[string]types.ModelInfo
	Aliases map[string]map[string]string
}

func newSnapshot() *Snapshot {
	return &Snapshot{
		Models:  map[string]map[string]types.ModelInfo{},
		Aliases: map[string]map[string]string{},
	}
}

// cloneSnapshot deep-copies so a caller can never mutate a store's live maps.
func cloneSnapshot(s *Snapshot) *Snapshot {
	out := newSnapshot()
	for p, ms := range s.Models {
		cp := make(map[string]types.ModelInfo, len(ms))
		maps.Copy(cp, ms)
		out.Models[p] = cp
	}
	for p, as := range s.Aliases {
		cp := make(map[string]string, len(as))
		maps.Copy(cp, as)
		out.Aliases[p] = cp
	}
	return out
}

// Store is durable model + alias storage behind the in-memory Registry. The
// Registry serves reads from its own snapshot; the Store is what survives a
// restart and what the sync engine (Phase 3, #4) writes discovered models into.
// Implementations are safe for concurrent use.
type Store interface {
	PutModel(ctx context.Context, provider string, m types.ModelInfo) error
	DeleteModel(ctx context.Context, provider, model string) error
	PutAlias(ctx context.Context, provider, alias, target string) error
	Load(ctx context.Context) (*Snapshot, error)
}

// MemoryStore is an in-process Store for tests and single-replica / no-Redis
// deployments.
type MemoryStore struct {
	mu   sync.RWMutex
	snap *Snapshot
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{snap: newSnapshot()} }

var _ Store = (*MemoryStore)(nil)

func (s *MemoryStore) PutModel(_ context.Context, provider string, m types.ModelInfo) error {
	if provider == "" || m.Name == "" {
		return fmt.Errorf("registry: PutModel requires provider and model name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap.Models[provider] == nil {
		s.snap.Models[provider] = map[string]types.ModelInfo{}
	}
	s.snap.Models[provider][m.Name] = m
	return nil
}

func (s *MemoryStore) DeleteModel(_ context.Context, provider, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ms := s.snap.Models[provider]; ms != nil {
		delete(ms, model)
	}
	return nil
}

func (s *MemoryStore) PutAlias(_ context.Context, provider, alias, target string) error {
	if provider == "" || alias == "" || target == "" {
		return fmt.Errorf("registry: PutAlias requires provider, alias and target")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap.Aliases[provider] == nil {
		s.snap.Aliases[provider] = map[string]string{}
	}
	s.snap.Aliases[provider][alias] = target
	return nil
}

func (s *MemoryStore) Load(_ context.Context) (*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSnapshot(s.snap), nil
}

// RedisStore persists models and aliases in Redis so the registry survives
// restarts and is shared across replicas. Per provider, models live in one hash
// (field = model name, value = JSON ModelInfo) and aliases in a second; a set
// of known providers makes Load enumerable. TTL bounds staleness — the sync
// engine refreshes within it; TTL<=0 means no expiry.
type RedisStore struct {
	R   redis.UniversalClient
	TTL time.Duration
}

// NewRedisStore wraps a go-redis client.
func NewRedisStore(r redis.UniversalClient, ttl time.Duration) *RedisStore {
	return &RedisStore{R: r, TTL: ttl}
}

var _ Store = (*RedisStore)(nil)

const registryKeyPrefix = "aiqg:registry:"

func modelsKey(provider string) string  { return registryKeyPrefix + "models:" + provider }
func aliasesKey(provider string) string { return registryKeyPrefix + "aliases:" + provider }
func providersKey() string              { return registryKeyPrefix + "providers" }

func (s *RedisStore) PutModel(ctx context.Context, provider string, m types.ModelInfo) error {
	if provider == "" || m.Name == "" {
		return fmt.Errorf("registry: PutModel requires provider and model name")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("registry: marshal model %s/%s: %w", provider, m.Name, err)
	}
	pipe := s.R.TxPipeline()
	pipe.HSet(ctx, modelsKey(provider), m.Name, b)
	pipe.SAdd(ctx, providersKey(), provider)
	if s.TTL > 0 {
		pipe.Expire(ctx, modelsKey(provider), s.TTL)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *RedisStore) DeleteModel(ctx context.Context, provider, model string) error {
	return s.R.HDel(ctx, modelsKey(provider), model).Err()
}

func (s *RedisStore) PutAlias(ctx context.Context, provider, alias, target string) error {
	if provider == "" || alias == "" || target == "" {
		return fmt.Errorf("registry: PutAlias requires provider, alias and target")
	}
	pipe := s.R.TxPipeline()
	pipe.HSet(ctx, aliasesKey(provider), alias, target)
	pipe.SAdd(ctx, providersKey(), provider)
	if s.TTL > 0 {
		pipe.Expire(ctx, aliasesKey(provider), s.TTL)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *RedisStore) Load(ctx context.Context) (*Snapshot, error) {
	snap := newSnapshot()
	providers, err := s.R.SMembers(ctx, providersKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("registry: list providers: %w", err)
	}
	for _, provider := range providers {
		raw, err := s.R.HGetAll(ctx, modelsKey(provider)).Result()
		if err != nil {
			return nil, fmt.Errorf("registry: load models for %s: %w", provider, err)
		}
		if len(raw) > 0 {
			ms := make(map[string]types.ModelInfo, len(raw))
			for name, blob := range raw {
				var m types.ModelInfo
				if err := json.Unmarshal([]byte(blob), &m); err != nil {
					// A single corrupt entry must not sink the whole load — skip
					// it; the next sync overwrites it.
					continue
				}
				ms[name] = m
			}
			snap.Models[provider] = ms
		}
		aliases, err := s.R.HGetAll(ctx, aliasesKey(provider)).Result()
		if err != nil {
			return nil, fmt.Errorf("registry: load aliases for %s: %w", provider, err)
		}
		if len(aliases) > 0 {
			snap.Aliases[provider] = aliases
		}
	}
	return snap, nil
}
