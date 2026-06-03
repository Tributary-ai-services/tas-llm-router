package middleware

import (
	"context"
	"sync"
	"testing"
)

func TestRouting_NilSafeNoCollector(t *testing.T) {
	ctx := context.Background()

	// All stampers must tolerate a context with no Routing attached.
	StampVendor(ctx, "openai")
	StampModel(ctx, "gpt-4o-mini")
	StampStreaming(ctx, true)

	if got := RoutingFromContext(ctx); got != nil {
		t.Errorf("RoutingFromContext returned non-nil for bare context: %#v", got)
	}
	var nilCtx context.Context
	if got := RoutingFromContext(nilCtx); got != nil {
		t.Errorf("RoutingFromContext(nil ctx) returned non-nil")
	}
}

func TestRouting_StampAndSnapshot(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	StampVendor(ctx, "anthropic")
	StampModel(ctx, "claude-3-7-sonnet")
	StampStreaming(ctx, true)

	s := r.Snapshot()
	if s.Vendor != "anthropic" {
		t.Errorf("Vendor=%q", s.Vendor)
	}
	if s.Model != "claude-3-7-sonnet" {
		t.Errorf("Model=%q", s.Model)
	}
	if !s.Streaming || !s.StreamingSet {
		t.Errorf("Streaming=%v StreamingSet=%v", s.Streaming, s.StreamingSet)
	}
}

// First-write-wins: a fallback later in the request must not overwrite
// the authoritative value the handler stamped early.
func TestRouting_IdempotentStamps(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	StampVendor(ctx, "openai")
	StampVendor(ctx, "anthropic") // ignored
	StampModel(ctx, "gpt-4o-mini")
	StampModel(ctx, "fallback-model") // ignored
	StampStreaming(ctx, true)
	StampStreaming(ctx, false) // ignored

	s := r.Snapshot()
	if s.Vendor != "openai" {
		t.Errorf("Vendor not idempotent: %q", s.Vendor)
	}
	if s.Model != "gpt-4o-mini" {
		t.Errorf("Model not idempotent: %q", s.Model)
	}
	if !s.Streaming {
		t.Errorf("Streaming not idempotent: %v", s.Streaming)
	}
}

// StreamingSet must distinguish "never stamped" from "explicitly false".
func TestRouting_StreamingSetSemantic(t *testing.T) {
	r := NewRouting()
	// Never stamped.
	s := r.Snapshot()
	if s.StreamingSet {
		t.Errorf("StreamingSet=true before any stamp")
	}

	// Stamp false — StreamingSet must flip to true.
	r2 := NewRouting()
	ctx := WithRouting(context.Background(), r2)
	StampStreaming(ctx, false)
	s2 := r2.Snapshot()
	if !s2.StreamingSet {
		t.Errorf("StreamingSet=false after explicit StampStreaming(false)")
	}
	if s2.Streaming {
		t.Errorf("Streaming=true after StampStreaming(false)")
	}
}

func TestRouting_ConcurrentStamps(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); StampVendor(ctx, "openai") }()
		go func() { defer wg.Done(); StampModel(ctx, "gpt-4o-mini") }()
		go func() { defer wg.Done(); StampStreaming(ctx, true) }()
	}
	wg.Wait()
	s := r.Snapshot()
	if s.Vendor != "openai" || s.Model != "gpt-4o-mini" || !s.Streaming {
		t.Errorf("concurrent stamps produced inconsistent state: %#v", s)
	}
}
