package registry

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"
)

// SyncConfig configures periodic model discovery (Phase 3, #4). Present now so
// the config surface and lifecycle wiring are stable; the discovery body lands
// in Phase 3.
type SyncConfig struct {
	Enabled       bool          `yaml:"enabled"`
	SyncInterval  time.Duration `yaml:"sync_interval"`  // how often to discover; default 1h
	ValidationTTL time.Duration `yaml:"validation_ttl"` // how long a validation result is trusted
	StartupSync   bool          `yaml:"startup_sync"`   // discover once at startup
}

// SyncEngine periodically discovers and validates models from provider adapters
// and writes them into the Registry.
//
// Phase-1 skeleton: the loop and the on-demand trigger exist so the lifecycle
// can be wired, but syncAll performs no discovery yet — the adapters (Phase 2,
// #3) and the discovery pass (Phase 3, #4) fill it. Running this engine today
// is inert beyond a debug log, so it is safe to wire early.
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

// Run drives the sync loop until ctx is cancelled. It is a no-op when the engine
// is disabled. In Phase 1 the ticks and triggers call a stub syncAll; the
// discovery body lands in Phase 3 (#4).
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

// syncAll is the discovery pass — a stub in Phase 1. Phase 3 (#4) will fan out
// to the provider adapters, upsert discovered models, then Registry.Load.
func (s *SyncEngine) syncAll(_ context.Context) {
	s.logger.Debug("registry sync: skeleton no-op (discovery lands in Phase 3, #4)")
}
