package registry

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"
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
		s.syncAll(ctx)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncAll(ctx)
		case <-s.triggerCh:
			s.syncAll(ctx)
		}
	}
}

// syncAll runs one discovery pass: the registry discovers + upserts every
// provider's models, then configured aliases are (re)applied. Best-effort —
// errors are logged, never fatal, so a provider outage degrades to stale data
// rather than a crash. Duration/result are logged; Prometheus metrics are the
// Observability phase (#6).
func (s *SyncEngine) syncAll(ctx context.Context) {
	start := time.Now()
	err := s.registry.SyncAll(ctx)
	if err != nil {
		s.logger.WithError(err).Warn("registry sync: completed with errors")
	}
	s.applyAliases(ctx)
	s.logger.WithFields(logrus.Fields{
		"duration_ms": time.Since(start).Milliseconds(),
		"ok":          err == nil,
	}).Debug("registry sync pass complete")
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
