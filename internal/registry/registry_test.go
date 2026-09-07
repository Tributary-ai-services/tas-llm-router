package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func quietRegistry(t *testing.T) *Registry {
	t.Helper()
	log := logrus.New()
	log.SetLevel(logrus.FatalLevel)
	return New(NewMemoryStore(), log)
}

func mustUpsert(t *testing.T, r *Registry, provider string, m types.ModelInfo) {
	t.Helper()
	if err := r.Upsert(context.Background(), provider, m); err != nil {
		t.Fatalf("Upsert(%s/%s): %v", provider, m.Name, err)
	}
}

func TestGetModel_FoundAndNotFound(t *testing.T) {
	r := quietRegistry(t)
	mustUpsert(t, r, "anthropic", types.ModelInfo{Name: "claude-sonnet-4-5", Status: types.ModelStatusActive})

	got, err := r.GetModel("anthropic", "claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if got.Name != "claude-sonnet-4-5" {
		t.Errorf("got %q", got.Name)
	}

	if _, err := r.GetModel("anthropic", "nope"); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("unknown model err = %v, want ErrModelNotFound", err)
	}
	if _, err := r.GetModel("openai", "anything"); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("unknown provider err = %v, want ErrModelNotFound", err)
	}
}

// A returned model is a copy — mutating it must not corrupt the registry.
func TestGetModel_ReturnsCopy(t *testing.T) {
	r := quietRegistry(t)
	mustUpsert(t, r, "openai", types.ModelInfo{Name: "gpt-4o", InputCostPer1K: 0.005})
	got, _ := r.GetModel("openai", "gpt-4o")
	got.InputCostPer1K = 999
	again, _ := r.GetModel("openai", "gpt-4o")
	if again.InputCostPer1K != 0.005 {
		t.Errorf("registry mutated through a returned copy: %v", again.InputCostPer1K)
	}
}

func TestListModels_SortedByName(t *testing.T) {
	r := quietRegistry(t)
	mustUpsert(t, r, "openai", types.ModelInfo{Name: "gpt-4o"})
	mustUpsert(t, r, "openai", types.ModelInfo{Name: "gpt-3.5-turbo"})
	mustUpsert(t, r, "openai", types.ModelInfo{Name: "gpt-4o-mini"})

	list, err := r.ListModels("openai")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gpt-3.5-turbo", "gpt-4o", "gpt-4o-mini"}
	if len(list) != len(want) {
		t.Fatalf("got %d models, want %d", len(list), len(want))
	}
	for i, m := range list {
		if m.Name != want[i] {
			t.Errorf("position %d = %q, want %q", i, m.Name, want[i])
		}
	}
	if got, _ := r.ListModels("unknown"); len(got) != 0 {
		t.Errorf("unknown provider returned %d models, want 0", len(got))
	}
}

func TestResolveAlias(t *testing.T) {
	r := quietRegistry(t)
	// Declared on the model itself.
	mustUpsert(t, r, "anthropic", types.ModelInfo{
		Name:    "claude-sonnet-4-5",
		Aliases: []string{"smart", "claude-sonnet-latest"},
	})
	// Declared out of band.
	if err := r.PutAlias(context.Background(), "anthropic", "cheap", "claude-haiku-4-5"); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"smart":                 "claude-sonnet-4-5",
		"claude-sonnet-latest":  "claude-sonnet-4-5",
		"cheap":                 "claude-haiku-4-5",
		"claude-sonnet-4-5":     "claude-sonnet-4-5", // a real id is returned unchanged
		"some-unknown-model-id": "some-unknown-model-id",
	}
	for in, want := range cases {
		got, err := r.ResolveAlias("anthropic", in)
		if err != nil {
			t.Errorf("ResolveAlias(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ResolveAlias(%q) = %q, want %q", in, got, want)
		}
	}

	if _, err := r.ResolveAlias("anthropic", ""); err == nil {
		t.Error("empty alias should error")
	}
}

func TestGetFallback_ReplacementThenFirstActive(t *testing.T) {
	r := quietRegistry(t)
	mustUpsert(t, r, "anthropic", types.ModelInfo{Name: "claude-3-5-sonnet", Status: types.ModelStatusDeprecated, ReplacementModel: "claude-sonnet-4-5"})
	mustUpsert(t, r, "anthropic", types.ModelInfo{Name: "claude-sonnet-4-5", Status: types.ModelStatusActive})

	// Deprecated model with a valid replacement → the replacement.
	fb, err := r.GetFallback("anthropic", "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("GetFallback: %v", err)
	}
	if fb.Name != "claude-sonnet-4-5" {
		t.Errorf("fallback = %q, want claude-sonnet-4-5", fb.Name)
	}
}

func TestGetFallback_SkipsUnavailableAndSelf(t *testing.T) {
	r := quietRegistry(t)
	// The requested model has no replacement; the only other model is
	// unavailable, so there is nothing serviceable to fall back to.
	mustUpsert(t, r, "openai", types.ModelInfo{Name: "gpt-4o", Status: types.ModelStatusDeprecated})
	mustUpsert(t, r, "openai", types.ModelInfo{Name: "gpt-4-old", Status: types.ModelStatusUnavailable})
	if _, err := r.GetFallback("openai", "gpt-4o"); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("err = %v, want ErrModelNotFound (no active alternative)", err)
	}

	// Add an active alternative → it is chosen.
	mustUpsert(t, r, "openai", types.ModelInfo{Name: "gpt-4o-mini", Status: types.ModelStatusActive})
	fb, err := r.GetFallback("openai", "gpt-4o")
	if err != nil {
		t.Fatalf("GetFallback: %v", err)
	}
	if fb.Name != "gpt-4o-mini" {
		t.Errorf("fallback = %q, want gpt-4o-mini", fb.Name)
	}

	if _, err := r.GetFallback("unknown", "x"); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("unknown provider err = %v, want ErrModelNotFound", err)
	}
}

// An empty status counts as active, so a statically-configured model needs no
// migration to be serviceable.
func TestIsActive_EmptyStatusIsActive(t *testing.T) {
	if !(types.ModelInfo{}).IsActive() {
		t.Error("empty status should be active")
	}
	if !(types.ModelInfo{Status: types.ModelStatusActive}).IsActive() {
		t.Error("active status should be active")
	}
	if (types.ModelInfo{Status: types.ModelStatusUnavailable}).IsActive() {
		t.Error("unavailable status must not be active")
	}
}

// Load rehydrates the in-memory snapshot from the store — the startup path.
func TestLoad_RehydratesFromStore(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	if err := store.PutModel(ctx, "anthropic", types.ModelInfo{Name: "claude-opus-4-8", Status: types.ModelStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAlias(ctx, "anthropic", "best", "claude-opus-4-8"); err != nil {
		t.Fatal(err)
	}

	log := logrus.New()
	log.SetLevel(logrus.FatalLevel)
	r := New(store, log)

	// Before Load the registry is empty.
	if _, err := r.GetModel("anthropic", "claude-opus-4-8"); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("registry should be empty before Load, got %v", err)
	}
	if err := r.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := r.GetModel("anthropic", "claude-opus-4-8"); err != nil {
		t.Errorf("model missing after Load: %v", err)
	}
	if got, _ := r.ResolveAlias("anthropic", "best"); got != "claude-opus-4-8" {
		t.Errorf("alias not loaded: got %q", got)
	}
}

func TestSkeletonMethodsReturnNotImplemented(t *testing.T) {
	r := quietRegistry(t)
	ctx := context.Background()
	if err := r.SyncAll(ctx); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("SyncAll err = %v, want ErrNotImplemented", err)
	}
	if err := r.SyncProvider(ctx, "anthropic"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("SyncProvider err = %v, want ErrNotImplemented", err)
	}
	if _, err := r.ValidateModel(ctx, "anthropic", "x"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("ValidateModel err = %v, want ErrNotImplemented", err)
	}
}

func TestUpsert_Validation(t *testing.T) {
	r := quietRegistry(t)
	ctx := context.Background()
	if err := r.Upsert(ctx, "", types.ModelInfo{Name: "x"}); err == nil {
		t.Error("empty provider should error")
	}
	if err := r.Upsert(ctx, "anthropic", types.ModelInfo{}); err == nil {
		t.Error("empty model name should error")
	}
}
