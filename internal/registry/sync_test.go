package registry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// stubAdapter is a ProviderAdapter whose discovery output and validate result
// are scripted per test.
type stubAdapter struct {
	name        string
	discover    []types.ModelInfo
	discoverErr error
	valid       map[string]bool

	mu    sync.Mutex
	calls int
}

func (a *stubAdapter) GetProviderName() string { return a.name }

func (a *stubAdapter) DiscoverModels(_ context.Context) ([]types.ModelInfo, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	if a.discoverErr != nil {
		return nil, a.discoverErr
	}
	return a.discover, nil
}

func (a *stubAdapter) ValidateModel(_ context.Context, model string) (bool, error) {
	return a.valid[model], nil
}

func (a *stubAdapter) ResolveAlias(_ context.Context, alias string) (string, error) {
	return alias, nil
}

func (a *stubAdapter) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func syncRegistry(t *testing.T) *Registry {
	t.Helper()
	log := logrus.New()
	log.SetLevel(logrus.FatalLevel)
	return New(NewMemoryStore(), log)
}

func TestSyncAll_DiscoversAndUpserts(t *testing.T) {
	r := syncRegistry(t)
	r.RegisterAdapter("openai", &stubAdapter{name: "openai", discover: []types.ModelInfo{
		{Name: "gpt-4o", Status: types.ModelStatusActive},
	}})
	r.RegisterAdapter("anthropic", &stubAdapter{name: "anthropic", discover: []types.ModelInfo{
		{Name: "claude-sonnet-4-5", Status: types.ModelStatusActive},
	}})

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if _, err := r.GetModel("openai", "gpt-4o"); err != nil {
		t.Errorf("gpt-4o not upserted: %v", err)
	}
	if _, err := r.GetModel("anthropic", "claude-sonnet-4-5"); err != nil {
		t.Errorf("claude-sonnet-4-5 not upserted: %v", err)
	}
}

func TestSyncAll_NoAdaptersIsNotImplemented(t *testing.T) {
	r := syncRegistry(t)
	if err := r.SyncAll(context.Background()); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("SyncAll with no adapters = %v, want ErrNotImplemented", err)
	}
}

// One failing provider must not stop the others; its error is joined, not fatal.
func TestSyncAll_OneProviderFailsOthersSucceed(t *testing.T) {
	r := syncRegistry(t)
	boom := errors.New("openai down")
	r.RegisterAdapter("openai", &stubAdapter{name: "openai", discoverErr: boom})
	r.RegisterAdapter("anthropic", &stubAdapter{name: "anthropic", discover: []types.ModelInfo{
		{Name: "claude-sonnet-4-5", Status: types.ModelStatusActive},
	}})

	err := r.SyncAll(context.Background())
	if !errors.Is(err, boom) {
		t.Errorf("SyncAll err = %v, want it to include the openai failure", err)
	}
	// Anthropic still synced despite openai failing.
	if _, err := r.GetModel("anthropic", "claude-sonnet-4-5"); err != nil {
		t.Errorf("anthropic did not sync when openai failed: %v", err)
	}
}

// A status transition (active → unavailable) across two syncs is tracked in the
// store — the deprecation-visibility the registry exists for.
func TestSyncProvider_TracksStatusChange(t *testing.T) {
	r := syncRegistry(t)
	a := &stubAdapter{name: "anthropic", discover: []types.ModelInfo{
		{Name: "claude-old", Status: types.ModelStatusActive},
	}}
	r.RegisterAdapter("anthropic", a)

	if err := r.SyncProvider(context.Background(), "anthropic"); err != nil {
		t.Fatal(err)
	}
	if m, _ := r.GetModel("anthropic", "claude-old"); m.Status != types.ModelStatusActive {
		t.Fatalf("first sync status = %q, want active", m.Status)
	}

	// The model is deprecated upstream on the next pass.
	a.discover = []types.ModelInfo{{Name: "claude-old", Status: types.ModelStatusUnavailable}}
	if err := r.SyncProvider(context.Background(), "anthropic"); err != nil {
		t.Fatal(err)
	}
	m, _ := r.GetModel("anthropic", "claude-old")
	if m.Status != types.ModelStatusUnavailable {
		t.Errorf("second sync status = %q, want unavailable", m.Status)
	}
}

func TestValidateModel_DelegatesToAdapter(t *testing.T) {
	r := syncRegistry(t)
	r.RegisterAdapter("anthropic", &stubAdapter{name: "anthropic", valid: map[string]bool{"claude-sonnet-4-5": true}})
	ctx := context.Background()
	if ok, _ := r.ValidateModel(ctx, "anthropic", "claude-sonnet-4-5"); !ok {
		t.Error("known model should validate")
	}
	if ok, _ := r.ValidateModel(ctx, "anthropic", "claude-gone"); ok {
		t.Error("unknown model should not validate")
	}
	if _, err := r.ValidateModel(ctx, "openai", "gpt-4o"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("no adapter err = %v, want ErrNotImplemented", err)
	}
}

// GetFallback resolves against post-sync state — a deprecated model with an
// active replacement falls back to it.
func TestSync_ThenFallback(t *testing.T) {
	r := syncRegistry(t)
	r.RegisterAdapter("anthropic", &stubAdapter{name: "anthropic", discover: []types.ModelInfo{
		{Name: "claude-3-5-sonnet", Status: types.ModelStatusDeprecated, ReplacementModel: "claude-sonnet-4-5"},
		{Name: "claude-sonnet-4-5", Status: types.ModelStatusActive},
	}})
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	fb, err := r.GetFallback("anthropic", "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("GetFallback: %v", err)
	}
	if fb.Name != "claude-sonnet-4-5" {
		t.Errorf("fallback = %q, want claude-sonnet-4-5", fb.Name)
	}
}

// --- SyncEngine ------------------------------------------------------------

func quietSyncEngine(r *Registry, cfg SyncConfig) *SyncEngine {
	log := logrus.New()
	log.SetLevel(logrus.FatalLevel)
	return NewSyncEngine(r, cfg, log)
}

// Startup sync fires once and the loop stops on ctx cancel (graceful shutdown).
func TestEngine_StartupSyncAndGracefulShutdown(t *testing.T) {
	r := syncRegistry(t)
	a := &stubAdapter{name: "openai", discover: []types.ModelInfo{{Name: "gpt-4o", Status: types.ModelStatusActive}}}
	r.RegisterAdapter("openai", a)

	eng := quietSyncEngine(r, SyncConfig{Enabled: true, StartupSync: true, SyncInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()

	// The startup pass should upsert gpt-4o promptly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := r.GetModel("openai", "gpt-4o"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup sync did not upsert within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel (graceful shutdown failed)")
	}
}

// A disabled engine does nothing and returns immediately.
func TestEngine_DisabledIsInert(t *testing.T) {
	r := syncRegistry(t)
	a := &stubAdapter{name: "openai", discover: []types.ModelInfo{{Name: "gpt-4o"}}}
	r.RegisterAdapter("openai", a)
	eng := quietSyncEngine(r, SyncConfig{Enabled: false})

	eng.Run(context.Background()) // returns immediately
	if a.callCount() != 0 {
		t.Errorf("disabled engine ran discovery %d times, want 0", a.callCount())
	}
}

// Configured aliases are attached to the provider that owns the target, after
// discovery populates it.
func TestEngine_AppliesConfiguredAliases(t *testing.T) {
	r := syncRegistry(t)
	r.RegisterAdapter("anthropic", &stubAdapter{name: "anthropic", discover: []types.ModelInfo{
		{Name: "claude-haiku-4-5", Status: types.ModelStatusActive},
	}})
	r.RegisterAdapter("openai", &stubAdapter{name: "openai", discover: []types.ModelInfo{
		{Name: "gpt-4o-mini", Status: types.ModelStatusActive},
	}})
	eng := quietSyncEngine(r, SyncConfig{
		Enabled:      true,
		StartupSync:  true,
		SyncInterval: time.Hour,
		Aliases: map[string]string{
			"fast":    "claude-haiku-4-5",
			"cheap":   "gpt-4o-mini",
			"missing": "model-that-does-not-exist",
		},
	})

	// Drive a single pass synchronously via the unexported syncAll.
	eng.syncAll(context.Background())

	if got, _ := r.ResolveAlias("anthropic", "fast"); got != "claude-haiku-4-5" {
		t.Errorf("alias fast = %q, want claude-haiku-4-5", got)
	}
	if got, _ := r.ResolveAlias("openai", "cheap"); got != "gpt-4o-mini" {
		t.Errorf("alias cheap = %q, want gpt-4o-mini", got)
	}
	// A missing target is skipped, not registered (resolves to identity).
	if got, _ := r.ResolveAlias("openai", "missing"); got != "missing" {
		t.Errorf("missing-target alias should not resolve; got %q", got)
	}
}

func TestEngine_TriggerCoalesces(t *testing.T) {
	r := syncRegistry(t)
	eng := quietSyncEngine(r, SyncConfig{Enabled: true})
	// Fill the buffer, then a second Trigger must not block or panic.
	eng.Trigger()
	eng.Trigger()
}
