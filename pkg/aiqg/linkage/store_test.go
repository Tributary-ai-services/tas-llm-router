package linkage

import (
	"context"
	"testing"
)

func TestMemoryStore_IndexThenResolve(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	want := Link{FlowID: "flow-1", StepID: "step-A"}

	if err := s.IndexToolCalls(ctx, "t1", []string{"call_aaa", "call_bbb"}, want); err != nil {
		t.Fatalf("index: %v", err)
	}

	// A later request echoing one of the served ids resolves to the parent step.
	got, ok, err := s.ResolveToolCalls(ctx, "t1", []string{"call_bbb"})
	if err != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, err)
	}
	if got != want {
		t.Errorf("resolved %+v, want %+v", got, want)
	}
}

func TestMemoryStore_MissAndTenantIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	_ = s.IndexToolCalls(ctx, "t1", []string{"call_x"}, Link{FlowID: "f", StepID: "s"})

	// Unknown id → miss.
	if _, ok, _ := s.ResolveToolCalls(ctx, "t1", []string{"nope"}); ok {
		t.Error("unknown id should not resolve")
	}
	// Same id, different tenant → miss (flows never cross tenants).
	if _, ok, _ := s.ResolveToolCalls(ctx, "t2", []string{"call_x"}); ok {
		t.Error("tool_call_id must not resolve across tenants")
	}
	// Empty ids → miss, no panic.
	if _, ok, _ := s.ResolveToolCalls(ctx, "t1", []string{"", ""}); ok {
		t.Error("empty ids should not resolve")
	}
}

func TestMemoryStore_FirstMatchWins(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	_ = s.IndexToolCalls(ctx, "t1", []string{"call_2"}, Link{FlowID: "flow-2", StepID: "step-2"})

	// Request carries several tool results; resolution returns the first hit.
	got, ok, _ := s.ResolveToolCalls(ctx, "t1", []string{"unindexed", "call_2"})
	if !ok || got.FlowID != "flow-2" {
		t.Errorf("expected flow-2 from the indexed id, got ok=%v %+v", ok, got)
	}
}

func TestRedisStore_NilSafe(t *testing.T) {
	// A nil/unconfigured RedisStore degrades to no-op rather than panicking,
	// so the request path never breaks when Redis is down.
	var s *RedisStore
	if err := s.IndexToolCalls(context.Background(), "t", []string{"a"}, Link{}); err != nil {
		t.Errorf("nil index should be a no-op, got %v", err)
	}
	if _, ok, err := s.ResolveToolCalls(context.Background(), "t", []string{"a"}); ok || err != nil {
		t.Errorf("nil resolve should be (false,nil), got ok=%v err=%v", ok, err)
	}
}
