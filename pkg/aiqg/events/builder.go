package events

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"time"

	"github.com/tributary-ai/llm-router-waf/internal/instrumentation"
	"github.com/tributary-ai/llm-router-waf/pkg/clear"
)

// AIQGHeadersView is the subset of parsed TAS-* headers the event
// builder needs. Defined here as a tiny interface-shaped struct so the
// builder doesn't import internal/middleware (which would create a
// cycle: middleware → events → middleware). Callers in middleware
// construct one from their AIQGHeaders before invoking Build.
type AIQGHeadersView struct {
	SourceApp    string
	Workflow     string
	Policy       []string
	PolicyBundle string
	DryRun       bool
	Trace        bool
}

// GatewayVersion is the build version stamped on every event. Override
// at build time via -ldflags "-X .../events.GatewayVersion=v1.4.2".
var GatewayVersion = "dev"

// ScoringVersion is the pkg/clear version stamped on every event.
// Re-exported from pkg/clear so call sites don't need to import both
// packages just to read the version string. When pkg/clear bumps Version,
// every emitted event reflects the new value automatically.
var ScoringVersion = clear.Version

// BuildOptions captures inputs the middleware passes through that
// aren't on the request context (HTTP status, response status string,
// region). Fields default to sensible MVP values when zero.
type BuildOptions struct {
	HTTPStatus   int    // 0 if no response was written (gateway blocked)
	Status       string // explicit status; if empty, derived from HTTPStatus
	Region       string // deployment region; if empty, omitted from event
	FinishReason string
}

// Build constructs the paired (RequestEnvelope, ResponseEnvelope) from
// request, parsed headers, timing snapshot, and outcome. Returns the
// correlated IDs already cross-linked (request.CorrelatedResponseEventID,
// response.RequestEventID).
//
// Pure function: no context access, no I/O, no clock side effects beyond
// time.Now() as the fallback when the snapshot has no stamps. Callers
// (Path A middleware) extract the snapshot + headers from ctx and pass
// them in — this keeps the builder importable without cycling back into
// internal/middleware.
//
// Fields the gateway can't populate at MVP today (tenant_id, vendor,
// model, CLEAR scores) are left empty — downstream slices fill them
// either by enriching the returned envelopes before Emit, or by adding
// new args to Build.
func Build(r *http.Request, headers AIQGHeadersView, snap instrumentation.Snapshot, opts BuildOptions) (RequestEnvelope, ResponseEnvelope) {
	reqID := newUUID()
	respID := newUUID()
	now := time.Now().UTC()

	receivedAt := now
	completeAt := now
	if snap.RequestReceivedAt != nil {
		receivedAt = snap.RequestReceivedAt.UTC()
	}
	if snap.ResponseCompleteAt != nil {
		completeAt = snap.ResponseCompleteAt.UTC()
	}

	status := opts.Status
	if status == "" {
		status = StatusFromHTTP(opts.HTTPStatus)
	}

	reqEvent := RequestEvent{
		RequestEventID:            reqID,
		ReceivedAt:                receivedAt,
		Endpoint:                  r.URL.Path,
		Method:                    r.Method,
		SourceIP:                  clientIP(r),
		SourceApp:                 headers.SourceApp,
		ClientRequestID:           r.Header.Get("X-Request-ID"),
		Region:                    opts.Region,
		Streaming:                 isStreaming(r),
		IsAIQGMode:                true,
		DryRun:                    headers.DryRun,
		TraceReturned:             headers.Trace,
		Workflow:                  headers.Workflow,
		PolicyNames:               headers.Policy,
		PolicyBundle:              headers.PolicyBundle,
		// SourceApp override from header was already captured above; the
		// AIQGHeadersView populates it for the field assignment below.
		CorrelatedResponseEventID: respID,
		ScoringVersion:            ScoringVersion,
		GatewayVersion:            GatewayVersion,
		LifecycleState:            LifecyclePairedWithResponse,
	}

	respEvent := ResponseEvent{
		ResponseEventID:   respID,
		RequestEventID:    reqID,
		CompleteAt:        completeAt,
		Status:            status,
		HTTPStatus:        opts.HTTPStatus,
		FinishReason:      opts.FinishReason,
		Streamed:          snap.ChunkCount > 0,
		ChunkCount:        snap.ChunkCount,
		ContentChunkCount: snap.ContentChunkCount,
		EventTimestamps:   snap,
		CLEAR: clear.Compute(clear.Input{
			EndToEndMs:        snap.EndToEndMs,
			GatewayOverheadMs: snap.GatewayOverheadMs,
			VendorTTFTMs:      snap.VendorTTFTMs,
			Workflow:          headers.Workflow,
			HTTPStatus:        opts.HTTPStatus,
		}),
		ScoringVersion: ScoringVersion,
		GatewayVersion: GatewayVersion,
	}

	reqEnv := RequestEnvelope{
		SpecVersion:     SpecVersion,
		Type:            TypeRequest,
		Source:          Source,
		ID:              reqID,
		Time:            receivedAt,
		DataContentType: DataContentType,
		Data:            reqEvent,
	}
	respEnv := ResponseEnvelope{
		SpecVersion:     SpecVersion,
		Type:            TypeResponse,
		Source:          Source,
		ID:              respID,
		Time:            completeAt,
		DataContentType: DataContentType,
		Data:            respEvent,
	}

	return reqEnv, respEnv
}

// newUUID returns a v4-shaped 128-bit random ID, formatted as the
// canonical 8-4-4-4-12 hex string. We don't pull in a UUID dep because
// the format is mechanical and we don't need RFC-strict version bits.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is catastrophic — every consumer of this
		// ID would silently get duplicates. Panic so the deployment
		// fails fast on a misconfigured entropy source.
		panic("aiqg/events: crypto/rand.Read failed: " + err.Error())
	}
	// Apply RFC 4122 version 4 + variant bits for compatibility.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexstr := hex.EncodeToString(b[:])
	return hexstr[0:8] + "-" + hexstr[8:12] + "-" + hexstr[12:16] + "-" +
		hexstr[16:20] + "-" + hexstr[20:32]
}

// clientIP applies trusted-proxy normalization to derive the original
// client address. Today this is a thin wrapper around X-Forwarded-For;
// when the gateway is fronted by a real LB, this will read a trusted-
// proxy chain from config. For MVP, take the first XFF entry, falling
// back to RemoteAddr's host (port-stripped).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// First entry is the original client.
		for i, c := range xff {
			if c == ',' {
				return string([]byte(xff)[:i])
			}
		}
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isStreaming inspects the request to decide whether it asked for a
// streamed response. Today this is a content-type / accept / query
// heuristic; the routing slice will replace it with payload-aware
// detection (OpenAI body has `"stream": true`, Anthropic same).
func isStreaming(r *http.Request) bool {
	if r.URL.Query().Get("stream") == "true" {
		return true
	}
	if accept := r.Header.Get("Accept"); accept != "" {
		// SSE accept header indicates streaming
		for i := 0; i < len(accept); i++ {
			if accept[i] == 't' && i+9 <= len(accept) && accept[i:i+9] == "text/even" {
				return true
			}
		}
	}
	return false
}
