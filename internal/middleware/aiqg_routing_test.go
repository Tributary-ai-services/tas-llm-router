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

func TestRouting_StampTokenUsage(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	// Before stamping: UsageSet=false, counts=0 (distinguishable from "vendor returned 0").
	if s := r.Snapshot(); s.UsageSet {
		t.Errorf("UsageSet=true before stamping")
	}

	StampTokenUsage(ctx, 1000, 500, 200, 800)
	s := r.Snapshot()
	if !s.UsageSet {
		t.Errorf("UsageSet=false after stamping")
	}
	if s.PromptTokens != 1000 || s.CompletionTokens != 500 {
		t.Errorf("counts: prompt=%d completion=%d", s.PromptTokens, s.CompletionTokens)
	}
	if s.CacheCreationTokens != 200 || s.CacheReadTokens != 800 {
		t.Errorf("cache counts: creation=%d read=%d", s.CacheCreationTokens, s.CacheReadTokens)
	}

	// First-write-wins: a later fallback path must not overwrite (cache too).
	StampTokenUsage(ctx, 9999, 9999, 9999, 9999)
	s2 := r.Snapshot()
	if s2.PromptTokens != 1000 || s2.CompletionTokens != 500 ||
		s2.CacheCreationTokens != 200 || s2.CacheReadTokens != 800 {
		t.Errorf("StampTokenUsage not idempotent: %#v", s2)
	}
}

// Stamping (0, 0) is a valid stamp — represents the legitimate case of
// a content_filter response with no completion. UsageSet must flip true.
func TestRouting_StampTokenUsageZeroIsValid(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)
	StampTokenUsage(ctx, 0, 0, 0, 0)
	s := r.Snapshot()
	if !s.UsageSet {
		t.Errorf("UsageSet should be true after explicit (0,0) stamp")
	}
}

// StampGatekeeperFindings: ScanRan flips true even on empty counts,
// per-direction max-aggregates, and direction param selects target map.
func TestRouting_StampGatekeeperFindings(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	if r.Snapshot().ScanRan {
		t.Errorf("ScanRan=true before any stamp")
	}

	// Empty counts: scan ran, no findings — ScanRan flips true.
	StampGatekeeperFindings(ctx, GatekeeperDirectionInbound, map[string]int{})
	if !r.Snapshot().ScanRan {
		t.Errorf("ScanRan should be true after empty inbound stamp")
	}

	// Stamp inbound findings.
	StampGatekeeperFindings(ctx, GatekeeperDirectionInbound, map[string]int{"medium": 2, "high": 1})
	s := r.Snapshot()
	if s.InboundFindings["medium"] != 2 || s.InboundFindings["high"] != 1 {
		t.Errorf("inbound stamps: %v", s.InboundFindings)
	}
	if len(s.OutboundFindings) != 0 {
		t.Errorf("outbound should be empty: %v", s.OutboundFindings)
	}

	// Stamp outbound findings, separately.
	StampGatekeeperFindings(ctx, GatekeeperDirectionOutbound, map[string]int{"critical": 1})
	s2 := r.Snapshot()
	if s2.OutboundFindings["critical"] != 1 {
		t.Errorf("outbound stamp: %v", s2.OutboundFindings)
	}

	// Subsequent inbound stamp with HIGHER counts wins (max-aggregate).
	StampGatekeeperFindings(ctx, GatekeeperDirectionInbound, map[string]int{"medium": 5})
	if r.Snapshot().InboundFindings["medium"] != 5 {
		t.Errorf("max-aggregate failed")
	}
	// LOWER counts ignored.
	StampGatekeeperFindings(ctx, GatekeeperDirectionInbound, map[string]int{"medium": 1})
	if r.Snapshot().InboundFindings["medium"] != 5 {
		t.Errorf("lower count overwrote higher")
	}
}

// Snapshot must return a copy — mutating it should not race future stamps.
func TestRouting_SnapshotIsCopy(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)
	StampGatekeeperFindings(ctx, GatekeeperDirectionInbound, map[string]int{"low": 1})
	s := r.Snapshot()
	s.InboundFindings["low"] = 999
	if r.Snapshot().InboundFindings["low"] == 999 {
		t.Errorf("Snapshot returned live map, not a copy")
	}
}

func TestRouting_StampFindings_NilSafe(t *testing.T) {
	// No collector in ctx — must not panic.
	StampGatekeeperFindings(context.Background(), GatekeeperDirectionInbound, map[string]int{"high": 1})
}

func TestRouting_StampFinishReason(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	if r.Snapshot().FinishReason != "" {
		t.Errorf("FinishReason not empty before stamping")
	}

	StampFinishReason(ctx, "stop")
	if got := r.Snapshot().FinishReason; got != "stop" {
		t.Errorf("FinishReason=%q want=stop", got)
	}

	// First-write-wins.
	StampFinishReason(ctx, "length")
	if got := r.Snapshot().FinishReason; got != "stop" {
		t.Errorf("FinishReason=%q want=stop (idempotent)", got)
	}
}

// Empty-string stamps are no-ops (so an early loop iteration that hasn't
// seen the final chunk's finish_reason doesn't accidentally lock in "").
func TestRouting_StampFinishReasonEmptyIsNoop(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	StampFinishReason(ctx, "")
	StampFinishReason(ctx, "")
	StampFinishReason(ctx, "stop") // first real value wins
	if got := r.Snapshot().FinishReason; got != "stop" {
		t.Errorf("FinishReason=%q want=stop", got)
	}
}

func TestRouting_StampFinishReason_NilSafe(t *testing.T) {
	StampFinishReason(context.Background(), "stop") // no panic
}

func TestRouting_StampRetryMetadata(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	if r.Snapshot().RetrySet {
		t.Errorf("RetrySet true before stamp")
	}

	StampRetryMetadata(ctx, 2, true)
	s := r.Snapshot()
	if !s.RetrySet || s.AttemptCount != 2 || !s.FallbackUsed {
		t.Errorf("after stamp: %#v", s)
	}

	// First-write-wins.
	StampRetryMetadata(ctx, 99, false)
	if r.Snapshot().AttemptCount != 2 {
		t.Errorf("not idempotent")
	}
}

// attemptCount=0 is a sentinel for "router didn't surface metadata" —
// stamp must no-op so RetrySet stays false and Reliability stays nil.
func TestRouting_StampRetryMetadata_ZeroIsNoop(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)
	StampRetryMetadata(ctx, 0, false)
	if r.Snapshot().RetrySet {
		t.Errorf("RetrySet=true after attemptCount=0 stamp")
	}
}

func TestRouting_StampRetryMetadata_NilSafe(t *testing.T) {
	StampRetryMetadata(context.Background(), 1, false) // no panic
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

// TestRoutingView_CarriesSemanticCacheFields guards the sidecar → events.RoutingView
// copy (routingView) for the C4 fields. Regression: that copy carried cache_saved_*
// but DROPPED cache_similarity/cache_threshold, so served semantic hits lost their
// similarity/threshold on the event stream while savings survived.
func TestRoutingView_CarriesSemanticCacheFields(t *testing.T) {
	r := &Routing{}
	ctx := WithRouting(context.Background(), r)
	StampCacheState(ctx, "semantic_hit")
	StampCacheSemantic(ctx, 0.983, 0.9)
	StampCacheSavings(ctx, 20, 60, 0.00025)

	v := routingView(r)
	if v.CacheState != "semantic_hit" {
		t.Errorf("CacheState = %q, want semantic_hit", v.CacheState)
	}
	if v.CacheSimilarity != 0.983 {
		t.Errorf("CacheSimilarity dropped in routingView: got %v want 0.983", v.CacheSimilarity)
	}
	if v.CacheThreshold != 0.9 {
		t.Errorf("CacheThreshold dropped in routingView: got %v want 0.9", v.CacheThreshold)
	}
	// Sanity: the savings field that always propagated still does.
	if v.CacheSavedCostUSD != 0.00025 {
		t.Errorf("CacheSavedCostUSD = %v, want 0.00025", v.CacheSavedCostUSD)
	}
}
