package registry

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/metrics"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// SyncConfig configures periodic model discovery (the `registry:` YAML block).
type SyncConfig struct {
	Enabled        bool          `yaml:"enabled"`
	SyncInterval   time.Duration `yaml:"sync_interval"`    // how often to discover; default 1h
	ValidationTTL  time.Duration `yaml:"validation_ttl"`   // how long a validation result is trusted
	StartupSync    bool          `yaml:"startup_sync"`     // discover once at startup
	RetryOnFailure bool          `yaml:"retry_on_failure"` // router may Trigger() a sync when a request hits an unknown/failed model (honoured by the router integration, Phase 4/#5)

	// Fallback selects how the router (Phase 4, #5) replaces an unavailable
	// model; carried here so the config surface is complete. Registry.GetFallback
	// is the current resolver.
	Fallback FallbackConfig `yaml:"fallback"`

	// Aliases are user-defined name→target-model mappings applied after each
	// sync. Provider-agnostic: the engine attaches each to the provider that
	// owns its target.
	Aliases map[string]string `yaml:"aliases"`
}

// FallbackConfig configures replacement of an unavailable model.
type FallbackConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Strategy string `yaml:"strategy"` // "capability_match" | "configured_chain"
}

// SyncEngine periodically drives model discovery: on startup (optional), on a
// fixed interval, and on demand via Trigger. Each pass asks the Registry to
// discover + upsert every registered provider's models, then reapplies the
// configured aliases. It owns scheduling and lifecycle; the Registry owns the
// adapters and the actual discovery work.
type SyncEngine struct {
	registry  *Registry
	config    SyncConfig
	triggerCh chan struct{}
	logger    *logrus.Logger

	mu    sync.Mutex
	stats SyncStats // last completed pass; read by the admin status endpoint
}

// SyncStats summarises the most recent discovery pass for the status endpoint.
type SyncStats struct {
	LastRun    time.Time                   `json:"last_run"`
	DurationMS int64                       `json:"duration_ms"`
	OK         bool                        `json:"ok"`
	Runs       int                         `json:"runs"`
	Providers  map[string]ProviderSyncStat `json:"providers"`
}

// ProviderSyncStat is one provider's outcome in a pass.
type ProviderSyncStat struct {
	Discovered  int    `json:"discovered"`
	Active      int    `json:"active"`
	Deprecated  int    `json:"deprecated"`
	Unavailable int    `json:"unavailable"`
	DurationMS  int64  `json:"duration_ms"`
	Error       string `json:"error,omitempty"`
}

// Stats returns a copy of the most recent pass's summary (zero value before the
// first pass). Safe for concurrent use.
func (s *SyncEngine) Stats() SyncStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := s.stats
	cp.Providers = make(map[string]ProviderSyncStat, len(s.stats.Providers))
	for k, v := range s.stats.Providers {
		cp.Providers[k] = v
	}
	return cp
}

// NewSyncEngine builds an engine over reg. A nil logger gets a default.
func NewSyncEngine(reg *Registry, cfg SyncConfig, logger *logrus.Logger) *SyncEngine {
	if logger == nil {
		logger = logrus.New()
	}
	return &SyncEngine{
		registry:  reg,
		config:    cfg,
		triggerCh: make(chan struct{}, 1),
		logger:    logger,
	}
}

// Trigger requests an out-of-band sync. Non-blocking and coalesced: a trigger
// arriving while one is queued is dropped rather than stacking.
func (s *SyncEngine) Trigger() {
	select {
	case s.triggerCh <- struct{}{}:
	default:
	}
}

// Run drives the sync loop until ctx is cancelled (graceful shutdown). It is a
// no-op when the engine is disabled. Runs a startup pass when configured, then
// syncs on the interval and whenever Trigger fires.
func (s *SyncEngine) Run(ctx context.Context) {
	if !s.config.Enabled {
		s.logger.Debug("registry sync: disabled")
		return
	}
	interval := s.config.SyncInterval
	if interval <= 0 {
		interval = time.Hour
	}
	if s.config.StartupSync {
		s.syncOnce(ctx)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncOnce(ctx)
		case <-s.triggerCh:
			s.syncOnce(ctx)
		}
	}
}

// SyncNow runs one discovery pass synchronously and returns its summary. Used by
// the admin sync endpoint so the caller sees the result of the sync it asked
// for, rather than firing the async Trigger and polling.
func (s *SyncEngine) SyncNow(ctx context.Context) SyncStats {
	return s.syncOnce(ctx)
}

// syncOnce runs one discovery pass: per provider it times SyncProvider, emits
// the sync metrics, tallies the resulting models by status, and logs a
// structured summary; then it reapplies configured aliases. Best-effort —
// a provider error is recorded and logged, never fatal, so one provider's
// outage degrades to stale data rather than stopping the others or crashing.
// The pass summary is stored for the status endpoint and returned.
func (s *SyncEngine) syncOnce(ctx context.Context) SyncStats {
	start := time.Now()
	stats := SyncStats{LastRun: start, OK: true, Providers: map[string]ProviderSyncStat{}}

	for _, provider := range s.registry.Providers() {
		if err := ctx.Err(); err != nil { // graceful shutdown mid-pass
			break
		}
		pStart := time.Now()
		err := s.registry.SyncProvider(ctx, provider)
		dur := time.Since(pStart)
		metrics.ObserveRegistrySync(provider, dur, err == nil)

		ps := ProviderSyncStat{DurationMS: dur.Milliseconds()}
		for _, m := range mustList(s.registry, provider) {
			ps.Discovered++
			switch m.Status {
			case "", "active":
				ps.Active++
			case "deprecated":
				ps.Deprecated++
			case "unavailable":
				ps.Unavailable++
			}
		}
		if err != nil {
			stats.OK = false
			ps.Error = err.Error()
			s.logger.WithError(err).WithField("provider", provider).Error("registry: sync failed")
		} else {
			s.logger.WithFields(logrus.Fields{
				"provider":          provider,
				"models_found":      ps.Discovered,
				"models_active":     ps.Active,
				"models_deprecated": ps.Deprecated,
				"duration_ms":       ps.DurationMS,
			}).Info("registry: model sync completed")
		}
		stats.Providers[provider] = ps
	}

	s.applyAliases(ctx)

	stats.DurationMS = time.Since(start).Milliseconds()
	s.mu.Lock()
	stats.Runs = s.stats.Runs + 1
	s.stats = stats
	s.mu.Unlock()
	return stats
}

// mustList returns a provider's models, or nil on error (the tally then reads
// as zero for that provider — the error itself is already surfaced via the
// SyncProvider result).
func mustList(reg *Registry, provider string) []types.ModelInfo {
	models, err := reg.ListModels(provider)
	if err != nil {
		return nil
	}
	return models
}

// applyAliases attaches each configured name→target alias to the provider that
// owns the target model. A target not present in any provider is skipped with a
// warning rather than guessed at.
func (s *SyncEngine) applyAliases(ctx context.Context) {
	for alias, target := range s.config.Aliases {
		provider, ok := s.registry.FindProvider(target)
		if !ok {
			s.logger.WithFields(logrus.Fields{"alias": alias, "target": target}).
				Warn("registry: configured alias target not found in any provider; skipping")
			continue
		}
		if err := s.registry.PutAlias(ctx, provider, alias, target); err != nil {
			s.logger.WithError(err).WithField("alias", alias).Warn("registry: apply alias failed")
		}
	}
}
