package instrumentation

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// stamps run against a nil collector (no context value) must be no-ops.
func TestStampsAreNoopWithoutCollector(t *testing.T) {
	ctx := context.Background()

	// None of these may panic, and none may install state anywhere.
	StampReceived(ctx)
	StampForwarded(ctx)
	StampTTFB(ctx)
	StampTTFT(ctx)
	StampChunk(ctx, true)
	StampChunk(ctx, false)
	StampLastChunk(ctx)
	StampComplete(ctx)

	if c := FromContext(ctx); c != nil {
		t.Fatalf("FromContext returned non-nil for ctx with no collector: %#v", c)
	}
	// FromContext must also tolerate a literal-nil context — the helper
	// is called from goroutines that don't always have a request ctx in hand.
	var nilCtx context.Context
	if c := FromContext(nilCtx); c != nil {
		t.Fatalf("FromContext(nil ctx) returned non-nil: %#v", c)
	}
}

func TestWithCollectorAndFromContext(t *testing.T) {
	c := NewCollector()
	ctx := WithCollector(context.Background(), c)

	got := FromContext(ctx)
	if got != c {
		t.Fatalf("FromContext did not return the attached collector")
	}
}

// All stamp methods are documented as idempotent — first call wins.
func TestStampsAreIdempotent(t *testing.T) {
	c := NewCollector()
	ctx := WithCollector(context.Background(), c)

	StampReceived(ctx)
	first := c.requestReceivedAt
	time.Sleep(2 * time.Millisecond)
	StampReceived(ctx)
	if !c.requestReceivedAt.Equal(first) {
		t.Fatalf("StampReceived not idempotent: first=%v second=%v", first, c.requestReceivedAt)
	}

	StampForwarded(ctx)
	firstFwd := c.requestForwardedAt
	time.Sleep(2 * time.Millisecond)
	StampForwarded(ctx)
	if !c.requestForwardedAt.Equal(firstFwd) {
		t.Fatalf("StampForwarded not idempotent")
	}

	StampTTFB(ctx)
	firstTTFB := c.firstByteAt
	time.Sleep(2 * time.Millisecond)
	StampTTFB(ctx)
	if !c.firstByteAt.Equal(firstTTFB) {
		t.Fatalf("StampTTFB not idempotent")
	}

	StampTTFT(ctx)
	firstTTFT := c.firstTokenAt
	time.Sleep(2 * time.Millisecond)
	StampTTFT(ctx)
	if !c.firstTokenAt.Equal(firstTTFT) {
		t.Fatalf("StampTTFT not idempotent")
	}

	StampLastChunk(ctx)
	firstLast := c.lastChunkAt
	time.Sleep(2 * time.Millisecond)
	StampLastChunk(ctx)
	if !c.lastChunkAt.Equal(firstLast) {
		t.Fatalf("StampLastChunk not idempotent")
	}

	StampComplete(ctx)
	firstComplete := c.responseCompleteAt
	time.Sleep(2 * time.Millisecond)
	StampComplete(ctx)
	if !c.responseCompleteAt.Equal(firstComplete) {
		t.Fatalf("StampComplete not idempotent")
	}
}

// TTFT must NOT be stamped on a chunk whose hasContent=false (e.g.
// OpenAI's role announcement or Anthropic's message_start). This is
// the architect-review §4 Risk #5 fix.
func TestStampChunkContentFirstDetection(t *testing.T) {
	c := NewCollector()
	ctx := WithCollector(context.Background(), c)

	StampChunk(ctx, false) // role announcement
	if !c.firstTokenAt.IsZero() {
		t.Fatalf("firstTokenAt set on hasContent=false chunk")
	}
	if c.chunkCount != 1 {
		t.Fatalf("chunkCount=%d want=1", c.chunkCount)
	}
	if c.contentChunkCount != 0 {
		t.Fatalf("contentChunkCount=%d want=0", c.contentChunkCount)
	}

	StampChunk(ctx, false) // another empty chunk
	if !c.firstTokenAt.IsZero() {
		t.Fatalf("firstTokenAt set on second hasContent=false chunk")
	}

	StampChunk(ctx, true) // first actual content
	firstTokenAt := c.firstTokenAt
	if firstTokenAt.IsZero() {
		t.Fatalf("firstTokenAt not set on first hasContent=true chunk")
	}
	if c.contentChunkCount != 1 {
		t.Fatalf("contentChunkCount=%d want=1", c.contentChunkCount)
	}

	StampChunk(ctx, true) // subsequent content chunks must not move firstTokenAt
	if !c.firstTokenAt.Equal(firstTokenAt) {
		t.Fatalf("firstTokenAt moved on subsequent content chunk")
	}
	if c.contentChunkCount != 2 {
		t.Fatalf("contentChunkCount=%d want=2", c.contentChunkCount)
	}
	if c.chunkCount != 4 {
		t.Fatalf("chunkCount=%d want=4", c.chunkCount)
	}
}

// gateway_overhead_ms = (forwarded - received) + (complete - lastChunk).
// This is the corrected formula per event-timestamps.md §4.3-§4.6 and the
// SRE review Risk #2 — both endpoints of both windows must be set.
func TestGatewayOverheadFormula(t *testing.T) {
	c := NewCollector()

	// Manually set timestamps to a known schedule so the assertion is
	// not flaky. Wall-clock instants are arbitrary; only deltas matter.
	t0 := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	c.requestReceivedAt = t0
	c.requestForwardedAt = t0.Add(30 * time.Millisecond) // 30ms ingress
	c.firstByteAt = t0.Add(120 * time.Millisecond)
	c.firstTokenAt = t0.Add(180 * time.Millisecond)
	c.lastChunkAt = t0.Add(900 * time.Millisecond)
	c.responseCompleteAt = t0.Add(945 * time.Millisecond) // 45ms egress

	s := c.Snapshot()

	if s.GatewayIngressMs == nil || *s.GatewayIngressMs != 30 {
		t.Fatalf("GatewayIngressMs=%v want=30", s.GatewayIngressMs)
	}
	if s.GatewayEgressMs == nil || *s.GatewayEgressMs != 45 {
		t.Fatalf("GatewayEgressMs=%v want=45", s.GatewayEgressMs)
	}
	if s.GatewayOverheadMs == nil || *s.GatewayOverheadMs != 75 {
		t.Fatalf("GatewayOverheadMs=%v want=75 (ingress+egress)", s.GatewayOverheadMs)
	}
	if s.VendorTTFBMs == nil || *s.VendorTTFBMs != 90 {
		t.Fatalf("VendorTTFBMs=%v want=90 (firstByte - forwarded)", s.VendorTTFBMs)
	}
	if s.VendorTTFTMs == nil || *s.VendorTTFTMs != 150 {
		t.Fatalf("VendorTTFTMs=%v want=150 (firstToken - forwarded)", s.VendorTTFTMs)
	}
	if s.VendorGenerationMs == nil || *s.VendorGenerationMs != 720 {
		t.Fatalf("VendorGenerationMs=%v want=720 (lastChunk - firstToken)", s.VendorGenerationMs)
	}
	if s.EndToEndMs == nil || *s.EndToEndMs != 945 {
		t.Fatalf("EndToEndMs=%v want=945", s.EndToEndMs)
	}
}

// Snapshot must return nil pointers for any timestamp/interval where
// either endpoint wasn't stamped — "not captured" ≠ "captured as 0ms".
func TestSnapshotOmitsUnsetIntervals(t *testing.T) {
	c := NewCollector()
	// Only stamp received — nothing else.
	c.requestReceivedAt = time.Now()

	s := c.Snapshot()

	if s.GatewayIngressMs != nil {
		t.Errorf("GatewayIngressMs should be nil with no forwarded stamp: %v", *s.GatewayIngressMs)
	}
	if s.GatewayEgressMs != nil {
		t.Errorf("GatewayEgressMs should be nil: %v", *s.GatewayEgressMs)
	}
	if s.GatewayOverheadMs != nil {
		t.Errorf("GatewayOverheadMs should be nil when ingress/egress unset: %v", *s.GatewayOverheadMs)
	}
	if s.VendorTTFBMs != nil {
		t.Errorf("VendorTTFBMs should be nil: %v", *s.VendorTTFBMs)
	}
	if s.VendorTTFTMs != nil {
		t.Errorf("VendorTTFTMs should be nil: %v", *s.VendorTTFTMs)
	}
	if s.VendorGenerationMs != nil {
		t.Errorf("VendorGenerationMs should be nil: %v", *s.VendorGenerationMs)
	}
	if s.EndToEndMs != nil {
		t.Errorf("EndToEndMs should be nil: %v", *s.EndToEndMs)
	}
	if s.MedianInterTokenMs != nil {
		t.Errorf("MedianInterTokenMs should be nil: %v", *s.MedianInterTokenMs)
	}
	if s.RequestReceivedAt == nil {
		t.Errorf("RequestReceivedAt should be non-nil (was stamped)")
	}
	if s.RequestForwardedAt != nil {
		t.Errorf("RequestForwardedAt should be nil")
	}
}

func TestSnapshotMarshalsToExpectedJSON(t *testing.T) {
	c := NewCollector()
	t0 := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	c.requestReceivedAt = t0
	c.requestForwardedAt = t0.Add(10 * time.Millisecond)
	c.lastChunkAt = t0.Add(500 * time.Millisecond)
	c.responseCompleteAt = t0.Add(520 * time.Millisecond)
	c.chunkCount = 3
	c.contentChunkCount = 2

	s := c.Snapshot()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal Snapshot: %v", err)
	}

	// Unmarshal into a map and check the expected keys are present with
	// the expected shapes.
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	expectKey := func(k string) {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in JSON", k)
		}
	}
	expectKey("request_received_at")
	expectKey("request_forwarded_at")
	expectKey("last_chunk_at")
	expectKey("response_complete_at")
	expectKey("gateway_ingress_ms")
	expectKey("gateway_egress_ms")
	expectKey("gateway_overhead_ms")
	expectKey("end_to_end_ms")
	expectKey("chunk_count")
	expectKey("content_chunk_count")

	// Unset intervals must NOT appear in JSON (omitempty + nil pointers).
	if _, ok := m["vendor_ttfb_ms"]; ok {
		t.Errorf("vendor_ttfb_ms should be omitted when not stamped")
	}
	if _, ok := m["median_inter_token_ms"]; ok {
		t.Errorf("median_inter_token_ms should be omitted when contentChunkCount < 3")
	}
}

func TestMedianInterTokenMs(t *testing.T) {
	// Need ≥3 content chunks for MedianInterTokenMs to be reported.
	c := NewCollector()
	t0 := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	// 5 chunks at gaps 100ms, 50ms, 200ms, 50ms → gaps sorted: 50, 50, 100, 200 → median=(50+100)/2=75
	c.chunkAt = []time.Time{
		t0,
		t0.Add(100 * time.Millisecond),
		t0.Add(150 * time.Millisecond),
		t0.Add(350 * time.Millisecond),
		t0.Add(400 * time.Millisecond),
	}
	c.contentChunkCount = 5

	s := c.Snapshot()
	if s.MedianInterTokenMs == nil {
		t.Fatalf("MedianInterTokenMs nil")
	}
	if *s.MedianInterTokenMs != 75 {
		t.Fatalf("MedianInterTokenMs=%d want=75", *s.MedianInterTokenMs)
	}
}

func TestMedianInterTokenMsOddCount(t *testing.T) {
	c := NewCollector()
	t0 := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	// 4 chunks at gaps 10ms, 50ms, 200ms → sorted: 10, 50, 200 → median=50
	c.chunkAt = []time.Time{
		t0,
		t0.Add(10 * time.Millisecond),
		t0.Add(60 * time.Millisecond),
		t0.Add(260 * time.Millisecond),
	}
	c.contentChunkCount = 4

	s := c.Snapshot()
	if s.MedianInterTokenMs == nil || *s.MedianInterTokenMs != 50 {
		t.Fatalf("MedianInterTokenMs=%v want=50", s.MedianInterTokenMs)
	}
}

// medianInterChunkMs is skipped when contentChunkCount < 3 — even if
// chunkAt has plenty of entries (those could all be empty role/start events).
func TestMedianSkippedBelowThreshold(t *testing.T) {
	c := NewCollector()
	t0 := time.Now()
	c.chunkAt = []time.Time{t0, t0.Add(10 * time.Millisecond), t0.Add(20 * time.Millisecond)}
	c.contentChunkCount = 2 // below 3

	s := c.Snapshot()
	if s.MedianInterTokenMs != nil {
		t.Fatalf("MedianInterTokenMs should be nil when contentChunkCount<3, got=%v", *s.MedianInterTokenMs)
	}
}

// StampChunk runs in a vendor stream goroutine while StampLastChunk and
// StampComplete may run from the caller goroutine. Verify no data race
// (run with -race in CI).
func TestStampConcurrentSafety(t *testing.T) {
	c := NewCollector()
	ctx := WithCollector(context.Background(), c)
	StampReceived(ctx)
	StampForwarded(ctx)

	var wg sync.WaitGroup
	// 50 goroutines stamping chunks
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			StampChunk(ctx, i%2 == 0)
		}(i)
	}
	// 5 goroutines racing the terminal stamps
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			StampLastChunk(ctx)
			StampComplete(ctx)
		}()
	}
	wg.Wait()

	if c.chunkCount != 50 {
		t.Fatalf("chunkCount=%d want=50", c.chunkCount)
	}
	if c.contentChunkCount != 25 {
		t.Fatalf("contentChunkCount=%d want=25", c.contentChunkCount)
	}
	if c.lastChunkAt.IsZero() {
		t.Fatalf("lastChunkAt not stamped")
	}
	if c.responseCompleteAt.IsZero() {
		t.Fatalf("responseCompleteAt not stamped")
	}

	// Snapshot under concurrent access should not panic and should be coherent.
	s := c.Snapshot()
	if s.ChunkCount != 50 {
		t.Fatalf("Snapshot.ChunkCount=%d want=50", s.ChunkCount)
	}
}

// End-to-end happy path through the public API: every stamp flows
// through, derived intervals are produced, JSON round-trips.
func TestEndToEndHappyPath(t *testing.T) {
	c := NewCollector()
	ctx := WithCollector(context.Background(), c)

	StampReceived(ctx)
	time.Sleep(2 * time.Millisecond)
	StampForwarded(ctx)
	time.Sleep(2 * time.Millisecond)
	StampTTFB(ctx)
	StampChunk(ctx, false) // role announcement (no content)
	time.Sleep(1 * time.Millisecond)
	StampChunk(ctx, true) // first content -> TTFT
	time.Sleep(1 * time.Millisecond)
	StampChunk(ctx, true)
	time.Sleep(1 * time.Millisecond)
	StampChunk(ctx, true)
	StampLastChunk(ctx)
	StampComplete(ctx)

	s := c.Snapshot()
	if s.GatewayIngressMs == nil {
		t.Fatalf("GatewayIngressMs nil")
	}
	if s.GatewayEgressMs == nil {
		t.Fatalf("GatewayEgressMs nil")
	}
	if s.GatewayOverheadMs == nil {
		t.Fatalf("GatewayOverheadMs nil")
	}
	if *s.GatewayOverheadMs != *s.GatewayIngressMs+*s.GatewayEgressMs {
		t.Fatalf("overhead %d != ingress %d + egress %d",
			*s.GatewayOverheadMs, *s.GatewayIngressMs, *s.GatewayEgressMs)
	}
	if s.VendorTTFBMs == nil || s.VendorTTFTMs == nil {
		t.Fatalf("vendor TTFB/TTFT must be present")
	}
	// TTFT >= TTFB because TTFT requires content (a later chunk) while
	// TTFB is the response headers landing.
	if *s.VendorTTFTMs < *s.VendorTTFBMs {
		t.Fatalf("TTFT %d < TTFB %d (TTFT must come after TTFB)", *s.VendorTTFTMs, *s.VendorTTFBMs)
	}
	if s.ChunkCount != 4 {
		t.Fatalf("ChunkCount=%d want=4", s.ChunkCount)
	}
	if s.ContentChunkCount != 3 {
		t.Fatalf("ContentChunkCount=%d want=3", s.ContentChunkCount)
	}
}
