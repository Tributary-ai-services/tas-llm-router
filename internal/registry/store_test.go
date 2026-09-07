package registry

import (
	"context"
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func TestMemoryStore_PutAndLoad(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	if err := s.PutModel(ctx, "openai", types.ModelInfo{Name: "gpt-4o"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutModel(ctx, "anthropic", types.ModelInfo{Name: "claude-opus-4-8"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAlias(ctx, "openai", "cheap", "gpt-4o-mini"); err != nil {
		t.Fatal(err)
	}

	snap, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.Models["openai"]["gpt-4o"]; !ok {
		t.Error("openai/gpt-4o missing from snapshot")
	}
	if _, ok := snap.Models["anthropic"]["claude-opus-4-8"]; !ok {
		t.Error("anthropic/claude-opus-4-8 missing from snapshot")
	}
	if snap.Aliases["openai"]["cheap"] != "gpt-4o-mini" {
		t.Error("alias missing from snapshot")
	}
}

// Load returns a deep copy: mutating it must not reach back into the store.
func TestMemoryStore_LoadReturnsIndependentCopy(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	_ = s.PutModel(ctx, "openai", types.ModelInfo{Name: "gpt-4o", InputCostPer1K: 0.005})

	snap, _ := s.Load(ctx)
	snap.Models["openai"]["gpt-4o"] = types.ModelInfo{Name: "gpt-4o", InputCostPer1K: 999}
	snap.Models["injected"] = map[string]types.ModelInfo{"x": {Name: "x"}}

	again, _ := s.Load(ctx)
	if again.Models["openai"]["gpt-4o"].InputCostPer1K != 0.005 {
		t.Error("store mutated through the returned snapshot")
	}
	if _, ok := again.Models["injected"]; ok {
		t.Error("a provider injected into the returned snapshot leaked into the store")
	}
}

func TestMemoryStore_Delete(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	_ = s.PutModel(ctx, "openai", types.ModelInfo{Name: "gpt-4o"})
	if err := s.DeleteModel(ctx, "openai", "gpt-4o"); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Load(ctx)
	if _, ok := snap.Models["openai"]["gpt-4o"]; ok {
		t.Error("model still present after delete")
	}
}

func TestMemoryStore_Validation(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.PutModel(ctx, "", types.ModelInfo{Name: "x"}); err == nil {
		t.Error("empty provider should error")
	}
	if err := s.PutModel(ctx, "openai", types.ModelInfo{}); err == nil {
		t.Error("empty model name should error")
	}
	if err := s.PutAlias(ctx, "openai", "", "target"); err == nil {
		t.Error("empty alias should error")
	}
	if err := s.PutAlias(ctx, "openai", "alias", ""); err == nil {
		t.Error("empty target should error")
	}
}
