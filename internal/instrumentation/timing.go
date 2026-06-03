// Package instrumentation captures per-request and per-chunk timing data
// for AIQG mode without modifying any existing exported types or signatures.
//
// The collector lives in request context; all stamping methods are no-ops
// when no collector is attached, so non-AIQG callers see zero behavior
// change. The corrected gateway_overhead_ms formula is documented in
// aether-shared/data-models/aiqg/event-timestamps.md and matches the SRE
// review fix (Risk #2): include all gateway processing time between
// request_received_at and request_forwarded_at, not just auth.
package instrumentation

import (
	"context"
	"sync"
	"time"
)

// ctxKey is unexported to prevent collisions with other packages putting
// values in the same context.
type ctxKey struct{}

var timingKey = ctxKey{}

// TimingCollector accumulates timestamps captured during a single request
// lifecycle. Safe for concurrent use because StampChunk runs in the stream
// goroutine while StampLastChunk/StampComplete may run from the caller.
type TimingCollector struct {
	mu sync.Mutex

	// Gateway ingress/egress
	requestReceivedAt  time.Time
	requestForwardedAt time.Time
	lastChunkAt        time.Time
	responseCompleteAt time.Time

	// HTTP client trace (vendor-side network)
	dnsStartAt          time.Time
	dnsDoneAt           time.Time
	connectStartAt      time.Time
	connectDoneAt       time.Time
	tlsHandshakeStartAt time.Time
	tlsHandshakeDoneAt  time.Time
	gotConnAt           time.Time

	// Vendor stream events
	firstByteAt time.Time // TTFB: first response header byte received
	firstTokenAt time.Time // TTFT: first non-empty content delta — distinct from TTFB
	chunkAt     []time.Time

	// Counters useful for the AIQG event payload
	chunkCount        int
	contentChunkCount int // chunks that contained non-empty content (excludes role announcements, message_start, etc.)
}

// NewCollector returns a fresh collector with no timestamps set.
func NewCollector() *TimingCollector {
	return &TimingCollector{
		chunkAt: make([]time.Time, 0, 256),
	}
}

// WithCollector attaches a collector to the context. Returns the new
// context. The provider goroutines read the collector via FromContext;
// callers without an AIQG-mode collector get nil and all stamps no-op.
func WithCollector(ctx context.Context, c *TimingCollector) context.Context {
	return context.WithValue(ctx, timingKey, c)
}

// FromContext returns the collector attached to ctx, or nil if none.
// All package-level Stamp* helpers below tolerate a nil collector so
// callers can stamp unconditionally.
func FromContext(ctx context.Context) *TimingCollector {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(timingKey).(*TimingCollector)
	return c
}

// StampReceived records the moment the gateway received the inbound request.
// Idempotent — first call wins.
func StampReceived(ctx context.Context) {
	c := FromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.requestReceivedAt.IsZero() {
		c.requestReceivedAt = time.Now()
	}
}

// StampForwarded records the moment the request was handed to the vendor
// HTTP client. The interval between Received and Forwarded is the gateway's
// pre-flight work (auth, scanning, classification, policy resolution,
// event emission). It is the bulk of gateway_overhead_ms on the ingress
// side. Idempotent.
func StampForwarded(ctx context.Context) {
	c := FromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.requestForwardedAt.IsZero() {
		c.requestForwardedAt = time.Now()
	}
}

// StampTTFT records the first non-empty content delta from the vendor
// stream. Callers MUST guard with their own hasContent check — this
// method only enforces idempotence. The architect-review (§4 Risk #5)
// established that stamping on the first stream.Recv() over-counts by
// 50-200ms because OpenAI sends a role announcement first and Anthropic
// sends message_start with no token content.
//
// This is distinct from StampTTFB which records the first response
// header byte; TTFT is the first byte of actual generated content.
func StampTTFT(ctx context.Context) {
	c := FromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.firstTokenAt.IsZero() {
		c.firstTokenAt = time.Now()
	}
}

// StampTTFB records the first response header byte received from the
// vendor. Captured by the httptrace.ClientTrace GotFirstResponseByte hook;
// most callers won't invoke this directly. Idempotent.
func StampTTFB(ctx context.Context) {
	c := FromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.firstByteAt.IsZero() {
		c.firstByteAt = time.Now()
	}
}

// StampChunk records the timestamp of each stream chunk and increments
// the chunk counter. hasContent should be true when the chunk contained
// a non-empty content delta — used to differentiate from role
// announcements, message_start, content_block_start, and ping events
// that don't carry tokens. The first chunk with hasContent=true also
// triggers TTFT capture.
func StampChunk(ctx context.Context, hasContent bool) {
	c := FromContext(ctx)
	if c == nil {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chunkCount++
	c.chunkAt = append(c.chunkAt, now)
	if hasContent {
		c.contentChunkCount++
		if c.firstTokenAt.IsZero() {
			c.firstTokenAt = now
		}
	}
}

// StampLastChunk records the moment the stream's last chunk arrived
// (EOF or final event). Idempotent — final-chunk emission can fire
// twice in some Anthropic stream patterns.
func StampLastChunk(ctx context.Context) {
	c := FromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastChunkAt.IsZero() {
		c.lastChunkAt = time.Now()
	}
}

// StampComplete records the moment the gateway finished its post-stream
// processing — scoring, event emission, response flush to the caller.
// The interval between LastChunk and Complete is the gateway's egress-side
// overhead. Idempotent.
func StampComplete(ctx context.Context) {
	c := FromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.responseCompleteAt.IsZero() {
		c.responseCompleteAt = time.Now()
	}
}

// HTTP trace stamps — wired from httptrace.go's ClientTrace hooks.
// All idempotent.

func (c *TimingCollector) stampDNSStart() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dnsStartAt.IsZero() {
		c.dnsStartAt = time.Now()
	}
}

func (c *TimingCollector) stampDNSDone() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dnsDoneAt.IsZero() {
		c.dnsDoneAt = time.Now()
	}
}

func (c *TimingCollector) stampConnectStart() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connectStartAt.IsZero() {
		c.connectStartAt = time.Now()
	}
}

func (c *TimingCollector) stampConnectDone() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connectDoneAt.IsZero() {
		c.connectDoneAt = time.Now()
	}
}

func (c *TimingCollector) stampTLSHandshakeStart() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tlsHandshakeStartAt.IsZero() {
		c.tlsHandshakeStartAt = time.Now()
	}
}

func (c *TimingCollector) stampTLSHandshakeDone() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tlsHandshakeDoneAt.IsZero() {
		c.tlsHandshakeDoneAt = time.Now()
	}
}

func (c *TimingCollector) stampGotConn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gotConnAt.IsZero() {
		c.gotConnAt = time.Now()
	}
}

// Snapshot returns a frozen, marshalable view of the collector at the
// moment of call. Fields that were never stamped are reported as nil
// duration pointers, distinguishing "not captured" from "captured as zero
// duration." This shape matches the AIQG CloudEvent payload defined in
// aether-shared/data-models/aiqg/event-timestamps.md.
func (c *TimingCollector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	s := Snapshot{
		RequestReceivedAt:  zeroToNilTime(c.requestReceivedAt),
		RequestForwardedAt: zeroToNilTime(c.requestForwardedAt),
		LastChunkAt:        zeroToNilTime(c.lastChunkAt),
		ResponseCompleteAt: zeroToNilTime(c.responseCompleteAt),
		FirstByteAt:        zeroToNilTime(c.firstByteAt),
		FirstTokenAt:       zeroToNilTime(c.firstTokenAt),
		DNSStartAt:         zeroToNilTime(c.dnsStartAt),
		DNSDoneAt:          zeroToNilTime(c.dnsDoneAt),
		ConnectStartAt:     zeroToNilTime(c.connectStartAt),
		ConnectDoneAt:      zeroToNilTime(c.connectDoneAt),
		TLSHandshakeStartAt: zeroToNilTime(c.tlsHandshakeStartAt),
		TLSHandshakeDoneAt:  zeroToNilTime(c.tlsHandshakeDoneAt),
		GotConnAt:          zeroToNilTime(c.gotConnAt),
		ChunkCount:         c.chunkCount,
		ContentChunkCount:  c.contentChunkCount,
	}

	// Derived intervals — only computed when both endpoints were stamped.
	if !c.requestReceivedAt.IsZero() && !c.requestForwardedAt.IsZero() {
		d := c.requestForwardedAt.Sub(c.requestReceivedAt)
		s.GatewayIngressMs = ptrInt64(d.Milliseconds())
	}
	if !c.lastChunkAt.IsZero() && !c.responseCompleteAt.IsZero() {
		d := c.responseCompleteAt.Sub(c.lastChunkAt)
		s.GatewayEgressMs = ptrInt64(d.Milliseconds())
	}
	// gateway_overhead_ms = ingress + egress per the corrected formula
	// (event-timestamps.md §4.3-§4.6 + SRE review Risk #2). The earlier
	// formula (auth_validated_at - request_received_at) excluded scanning,
	// classification, policy resolution, and event emission — all of which
	// are gateway-side work the customer perceives as overhead.
	if s.GatewayIngressMs != nil && s.GatewayEgressMs != nil {
		total := *s.GatewayIngressMs + *s.GatewayEgressMs
		s.GatewayOverheadMs = ptrInt64(total)
	}
	// Vendor-side derivations
	if !c.requestForwardedAt.IsZero() && !c.firstByteAt.IsZero() {
		d := c.firstByteAt.Sub(c.requestForwardedAt)
		s.VendorTTFBMs = ptrInt64(d.Milliseconds())
	}
	if !c.requestForwardedAt.IsZero() && !c.firstTokenAt.IsZero() {
		d := c.firstTokenAt.Sub(c.requestForwardedAt)
		s.VendorTTFTMs = ptrInt64(d.Milliseconds())
	}
	if !c.firstTokenAt.IsZero() && !c.lastChunkAt.IsZero() {
		d := c.lastChunkAt.Sub(c.firstTokenAt)
		s.VendorGenerationMs = ptrInt64(d.Milliseconds())
	}
	if !c.requestReceivedAt.IsZero() && !c.responseCompleteAt.IsZero() {
		d := c.responseCompleteAt.Sub(c.requestReceivedAt)
		s.EndToEndMs = ptrInt64(d.Milliseconds())
	}
	// Inter-token cadence: median ms between content chunks. Skipped when
	// fewer than 3 content chunks observed because the median is unstable.
	if c.contentChunkCount >= 3 {
		s.MedianInterTokenMs = ptrInt64(medianInterChunkMs(c.chunkAt))
	}

	return s
}

// Snapshot is the marshalable timing view. Duration fields are pointers
// so callers can distinguish "not captured" from "zero duration."
type Snapshot struct {
	// Absolute timestamps
	RequestReceivedAt   *time.Time `json:"request_received_at,omitempty"`
	RequestForwardedAt  *time.Time `json:"request_forwarded_at,omitempty"`
	LastChunkAt         *time.Time `json:"last_chunk_at,omitempty"`
	ResponseCompleteAt  *time.Time `json:"response_complete_at,omitempty"`
	FirstByteAt         *time.Time `json:"first_byte_at,omitempty"`
	FirstTokenAt        *time.Time `json:"first_token_at,omitempty"`
	DNSStartAt          *time.Time `json:"dns_start_at,omitempty"`
	DNSDoneAt           *time.Time `json:"dns_done_at,omitempty"`
	ConnectStartAt      *time.Time `json:"connect_start_at,omitempty"`
	ConnectDoneAt       *time.Time `json:"connect_done_at,omitempty"`
	TLSHandshakeStartAt *time.Time `json:"tls_handshake_start_at,omitempty"`
	TLSHandshakeDoneAt  *time.Time `json:"tls_handshake_done_at,omitempty"`
	GotConnAt           *time.Time `json:"got_conn_at,omitempty"`

	// Derived intervals (ms)
	GatewayIngressMs   *int64 `json:"gateway_ingress_ms,omitempty"`
	GatewayEgressMs    *int64 `json:"gateway_egress_ms,omitempty"`
	GatewayOverheadMs  *int64 `json:"gateway_overhead_ms,omitempty"`
	VendorTTFBMs       *int64 `json:"vendor_ttfb_ms,omitempty"`
	VendorTTFTMs       *int64 `json:"vendor_ttft_ms,omitempty"`
	VendorGenerationMs *int64 `json:"vendor_generation_ms,omitempty"`
	EndToEndMs         *int64 `json:"end_to_end_ms,omitempty"`
	MedianInterTokenMs *int64 `json:"median_inter_token_ms,omitempty"`

	// Counters
	ChunkCount        int `json:"chunk_count"`
	ContentChunkCount int `json:"content_chunk_count"`
}

func zeroToNilTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func ptrInt64(v int64) *int64 {
	return &v
}

// medianInterChunkMs returns the median gap between adjacent chunk
// timestamps, in milliseconds. Caller must hold the collector lock OR
// pass a defensive copy; we pass the live slice here only because the
// caller (Snapshot) holds the lock and the slice is not mutated after
// the goroutine has drained.
func medianInterChunkMs(ts []time.Time) int64 {
	if len(ts) < 2 {
		return 0
	}
	gaps := make([]int64, 0, len(ts)-1)
	for i := 1; i < len(ts); i++ {
		gaps = append(gaps, ts[i].Sub(ts[i-1]).Milliseconds())
	}
	// In-place insertion sort — gaps are typically <300 elements per
	// request and the overhead of importing sort just for this is silly.
	for i := 1; i < len(gaps); i++ {
		for j := i; j > 0 && gaps[j-1] > gaps[j]; j-- {
			gaps[j-1], gaps[j] = gaps[j], gaps[j-1]
		}
	}
	mid := len(gaps) / 2
	if len(gaps)%2 == 1 {
		return gaps[mid]
	}
	return (gaps[mid-1] + gaps[mid]) / 2
}
