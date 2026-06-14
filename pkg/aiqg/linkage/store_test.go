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
	// Prefix-chaining methods are nil-safe too.
	if err := s.IndexPrefix(context.Background(), "t", "h", "c"); err != nil {
		t.Errorf("nil prefix index should be a no-op, got %v", err)
	}
	if _, ok, err := s.ResolvePrefix(context.Background(), "t", "h"); ok || err != nil {
		t.Errorf("nil prefix resolve should be (false,nil), got ok=%v err=%v", ok, err)
	}
}

func TestMemoryStore_PrefixIndexThenResolve(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	// Serving turn 1 leaves the conversation in state hash "state-1",
	// belonging to conversation "conv-1".
	if err := s.IndexPrefix(ctx, "t1", "state-1", "conv-1"); err != nil {
		t.Fatalf("index prefix: %v", err)
	}
	// Turn 2's message prefix hashes to that same state → same conversation.
	got, ok, err := s.ResolvePrefix(ctx, "t1", "state-1")
	if err != nil || !ok {
		t.Fatalf("resolve prefix: ok=%v err=%v", ok, err)
	}
	if got != "conv-1" {
		t.Errorf("resolved conversation %q, want conv-1", got)
	}
}

func TestMemoryStore_PrefixMissTenantAndNamespace(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	_ = s.IndexPrefix(ctx, "t1", "state-x", "conv-x")

	// Unknown prefix hash → miss.
	if _, ok, _ := s.ResolvePrefix(ctx, "t1", "state-nope"); ok {
		t.Error("unknown prefix should not resolve")
	}
	// Same hash, different tenant → miss (conversations never cross tenants).
	if _, ok, _ := s.ResolvePrefix(ctx, "t2", "state-x"); ok {
		t.Error("prefix must not resolve across tenants")
	}
	// Empty hash → miss, no panic.
	if _, ok, _ := s.ResolvePrefix(ctx, "t1", ""); ok {
		t.Error("empty prefix should not resolve")
	}
	// The prefix namespace must not collide with the tool_call_id namespace:
	// a tool_call_id equal to a state hash string must not cross-resolve.
	_ = s.IndexToolCalls(ctx, "t1", []string{"state-x"}, Link{FlowID: "f", StepID: "s"})
	if c, ok, _ := s.ResolvePrefix(ctx, "t1", "state-x"); !ok || c != "conv-x" {
		t.Errorf("prefix namespace collided with tool_call index: ok=%v c=%q", ok, c)
	}
}
