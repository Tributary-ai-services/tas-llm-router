package events

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// EmitMetricsRecorder is the minimal interface emitters use to push
// per-emit metrics. Lives here (not in pkg/aiqg/metrics) so this
// package doesn't take a build-time dep on prometheus client_golang
// just to instrument. Production wires pkg/aiqg/metrics; tests can
// inject a no-op or capture-recorder.
type EmitMetricsRecorder interface {
	ObserveEmit(emitter string, start time.Time, errPtr *error)
	RecordEvent(resp ResponseEnvelope)
}

// MetricsRecorder is the active recorder. Nil means metrics are
// silently dropped — useful for tests and for non-AIQG callers that
// happen to use these types. Wire production via
// events.MetricsRecorder = metricsAdapter{} at server startup.
var MetricsRecorder EmitMetricsRecorder

func recordEmit(emitter string, start time.Time, errPtr *error, resp ResponseEnvelope) {
	if MetricsRecorder == nil {
		return
	}
	MetricsRecorder.ObserveEmit(emitter, start, errPtr)
	MetricsRecorder.RecordEvent(resp)
}

// Emitter publishes paired (request, response) AIQG events. The pair
// is emitted atomically from the caller's perspective — concrete
// implementations may stagger their actual writes (e.g. two separate
// Kafka producer Send calls) but must not return until both have
// committed or been queued.
//
// Implementations:
//   - NoopEmitter: drops events on the floor (default for non-AIQG paths)
//   - LogEmitter: writes structured JSON to a logrus logger (default
//     for MVP; works without infrastructure)
//   - MemoryEmitter: stores events in memory for tests
//   - (future) KafkaEmitter: produces to the aiqg.events topic
type Emitter interface {
	Emit(ctx context.Context, req RequestEnvelope, resp ResponseEnvelope) error
}

// NoopEmitter drops events. Use when AIQG mode is off or when an
// emitter is unconfigured — caller-side code should not have to nil-check.
type NoopEmitter struct{}

// Emit returns nil. Always. Intentionally not instrumented — noop
// emitter is the "AIQG disabled" path and shouldn't register as
// traffic in the metrics.
func (NoopEmitter) Emit(_ context.Context, _ RequestEnvelope, _ ResponseEnvelope) error {
	return nil
}

// LogEmitter writes each event as a structured logrus entry. Useful as
// the MVP backend: events show up in Loki via the standard log pipeline
// with no Kafka topic to provision. Switch to KafkaEmitter when the
// downstream consumer (Spark / dashboard) lands.
type LogEmitter struct {
	Logger *logrus.Logger
}

// Emit logs both envelopes at INFO with the CloudEvents type as a field
// for downstream filtering (LogQL: `{...} | json | type="com.tas.aiqg.request.v1"`).
//
// Top-level fields are promoted via WithFields so LogQL can query them
// directly (e.g. `{...} | json | clear_composite > 75`) without
// double-parsing the embedded `payload` string. The payload itself is
// kept verbatim for consumers that want the full envelope.
func (e *LogEmitter) Emit(_ context.Context, req RequestEnvelope, resp ResponseEnvelope) (err error) {
	defer recordEmit("log", time.Now(), &err, resp)
	if e.Logger == nil {
		return nil
	}
	reqJSON, mErr := json.Marshal(req)
	if mErr != nil {
		err = mErr
		return err
	}
	respJSON, mErr := json.Marshal(resp)
	if mErr != nil {
		err = mErr
		return err
	}

	reqFields := logrus.Fields{
		"ce_type":          req.Type,
		"ce_id":            req.ID,
		"request_event_id": req.Data.RequestEventID,
		"payload":          string(reqJSON),
		// Routing + attribution — empty strings stay empty in JSON but
		// LogQL can still filter on them when populated.
		"vendor":           req.Data.Vendor,
		"model":            req.Data.Model,
		"endpoint":         req.Data.Endpoint,
		"workflow":         req.Data.Workflow,
		"streaming":        req.Data.Streaming,
		"region":           req.Data.Region,
		"tenant_id":        req.Data.TenantID,
		"aiqg_account_id":  req.Data.AIQGAccountID,
		"source_app":       req.Data.SourceApp,
		"dry_run":          req.Data.DryRun,
		"gateway_version":  req.Data.GatewayVersion,
	}
	if req.Data.PolicyBundle != "" {
		reqFields["policy_bundle"] = req.Data.PolicyBundle
	}
	e.Logger.WithFields(reqFields).Info("aiqg request event")

	respFields := logrus.Fields{
		"ce_type":           resp.Type,
		"ce_id":             resp.ID,
		"response_event_id": resp.Data.ResponseEventID,
		"request_event_id":  resp.Data.RequestEventID,
		"http_status":       resp.Data.HTTPStatus,
		"status":            resp.Data.Status,
		"chunk_count":       resp.Data.ChunkCount,
		"streamed":          resp.Data.Streamed,
		"finish_reason":     resp.Data.FinishReason,
		"tenant_id":         resp.Data.TenantID,
		"aiqg_account_id":   resp.Data.AIQGAccountID,
		"gateway_version":   resp.Data.GatewayVersion,
		"scoring_version":   resp.Data.ScoringVersion,
		"payload":           string(respJSON),
	}
	// Token accounting — nil-safe; either fully populated or omitted.
	if ta := resp.Data.TokenAccounting; ta != nil {
		respFields["prompt_tokens"] = ta.PromptTokens
		respFields["completion_tokens"] = ta.CompletionTokens
		respFields["total_tokens"] = ta.TotalTokens
		respFields["total_cost_usd"] = ta.TotalCostUSD
	}
	// Promote end_to_end_ms so LogQL can unwrap it directly — needed
	// for the dashboard /reports/preview p50/p95 latency queries.
	// Without this, latency is only reachable by parsing the embedded
	// `payload` JSON, which LogQL handles awkwardly. Skipped when nil
	// (request never completed) so dashboards distinguish "not
	// captured" from "captured as zero".
	if ms := resp.Data.EventTimestamps.EndToEndMs; ms != nil {
		respFields["end_to_end_ms"] = *ms
	}
	// CLEAR scores — pointer-fielded; promote each non-nil dimension.
	if c := resp.Data.CLEAR; c != nil {
		if c.Cost != nil {
			respFields["clear_cost"] = *c.Cost
		}
		if c.Latency != nil {
			respFields["clear_latency"] = *c.Latency
		}
		if c.Efficacy != nil {
			respFields["clear_efficacy"] = *c.Efficacy
		}
		if c.Assurance != nil {
			respFields["clear_assurance"] = *c.Assurance
		}
		if c.Reliability != nil {
			respFields["clear_reliability"] = *c.Reliability
		}
		if c.Composite != nil {
			respFields["clear_composite"] = *c.Composite
		}
	}
	// Assurance summary — counts always emit, worst severity when set.
	if a := resp.Data.Assurance; a != nil {
		respFields["assurance_inbound_count"] = a.InboundCount
		respFields["assurance_outbound_count"] = a.OutboundCount
		if a.WorstSeverity != "" {
			respFields["assurance_worst_severity"] = a.WorstSeverity
		}
	}
	e.Logger.WithFields(respFields).Info("aiqg response event")
	return nil
}

// MemoryEmitter stores emitted events in memory for tests. Concurrent
// emits are serialized via the embedded mutex.
type MemoryEmitter struct {
	mu        sync.Mutex
	requests  []RequestEnvelope
	responses []ResponseEnvelope
}

// Emit appends both envelopes to the internal slices.
func (e *MemoryEmitter) Emit(_ context.Context, req RequestEnvelope, resp ResponseEnvelope) (err error) {
	defer recordEmit("memory", time.Now(), &err, resp)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.requests = append(e.requests, req)
	e.responses = append(e.responses, resp)
	return nil
}

// Requests returns a copy of the emitted request envelopes. The copy
// prevents test mutation from racing future Emit calls.
func (e *MemoryEmitter) Requests() []RequestEnvelope {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]RequestEnvelope, len(e.requests))
	copy(out, e.requests)
	return out
}

// Responses returns a copy of the emitted response envelopes.
func (e *MemoryEmitter) Responses() []ResponseEnvelope {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]ResponseEnvelope, len(e.responses))
	copy(out, e.responses)
	return out
}

// Len returns the number of paired (req, resp) emits so far. Useful in
// test assertions: `if e.Len() != 1 { ... }`.
func (e *MemoryEmitter) Len() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.requests)
}

// Reset clears all stored events.
func (e *MemoryEmitter) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.requests = nil
	e.responses = nil
}
