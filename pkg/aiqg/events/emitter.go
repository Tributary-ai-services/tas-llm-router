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
		"vendor":          req.Data.Vendor,
		"model":           req.Data.Model,
		"endpoint":        req.Data.Endpoint,
		"workflow":        req.Data.Workflow,
		"streaming":       req.Data.Streaming,
		"region":          req.Data.Region,
		"tenant_id":       req.Data.TenantID,
		"aiqg_account_id": req.Data.AIQGAccountID,
		"source_app":      req.Data.SourceApp,
		"dry_run":         req.Data.DryRun,
		"gateway_version": req.Data.GatewayVersion,
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
		// Workflow is on the RequestEvent in the schema, but copy it
		// onto the response log line too so per-workflow dashboard
		// queries can filter the response stream directly (e.g.
		// {namespace="tas-llm-router"} | json | workflow="code_generation").
		"workflow": req.Data.Workflow,
		// Vendor/model are denormalized onto the ResponseEvent itself
		// (builder.go), so promote them from the response — dashboards
		// group cost/tokens by vendor/model directly off the response
		// stream (e.g. sum by (vendor) (sum_over_time(... | unwrap
		// total_cost_usd))). Empty strings stay empty in JSON but LogQL
		// filters when populated.
		"vendor":  resp.Data.Vendor,
		"model":   resp.Data.Model,
		"payload": string(respJSON),
	}
	// Experiment attribution (Phase D) — promote so dashboards/Loki can slice
	// per-variant directly; empty (omitted) for traffic no experiment claimed.
	if resp.Data.ExperimentID != "" {
		respFields["experiment_id"] = resp.Data.ExperimentID
		respFields["experiment_variant"] = resp.Data.ExperimentVariant
	}
	// Classification drift (Axis-1, Plan #8) — promote as FIELDS (never stream
	// labels: keep cardinality off the label set). Present only when a declared
	// signal existed; the Loki-fallback backend unwraps these until the Spark
	// event_metrics columns land. See classification-drift.md §3/§4.
	if resp.Data.WorkflowDeclared != "" {
		respFields["workflow_declared"] = resp.Data.WorkflowDeclared
		respFields["workflow_inferred"] = resp.Data.WorkflowInferred
	}
	if resp.Data.WorkflowDeclaredOp != "" {
		respFields["workflow_declared_op"] = resp.Data.WorkflowDeclaredOp
	}
	if resp.Data.WorkflowDrift != nil {
		respFields["workflow_drift"] = *resp.Data.WorkflowDrift
	}
	if resp.Data.OTelMapVersion != "" {
		respFields["otel_map_version"] = resp.Data.OTelMapVersion
	}
	// Token accounting — nil-safe; either fully populated or omitted.
	if ta := resp.Data.TokenAccounting; ta != nil {
		respFields["prompt_tokens"] = ta.PromptTokens
		respFields["completion_tokens"] = ta.CompletionTokens
		respFields["total_tokens"] = ta.TotalTokens
		respFields["total_cost_usd"] = ta.TotalCostUSD
		// Cost decomposition (CLEAR v0.2) — promote as FIELDS (never
		// stream labels: these are high-cardinality numerics). The
		// Loki-fallback backend unwraps these; only present on priced
		// traffic where the decomposer ran (ReductionMode set).
		if ta.ReductionMode != "" {
			respFields["reduction_mode"] = ta.ReductionMode
			respFields["actual_cost_usd"] = ta.ActualCostUSD
			respFields["projected_direct_payload_waste_usd"] = ta.ProjectedDirectPayloadWasteUSD
			respFields["direct_payload_waste_usd"] = ta.DirectPayloadWasteUSD
			respFields["projected_reduction_relevance_usd"] = ta.ProjectedReductionRelevanceUSD
			respFields["projected_reduction_slm_usd"] = ta.ProjectedReductionSLMUSD
			respFields["projected_reduction_combined_usd"] = ta.ProjectedReductionCombinedUSD
			respFields["induced_output_waste_estimated_usd"] = ta.InducedOutputWasteEstimatedUSD
			respFields["genuine_post_model_waste_usd"] = ta.GenuinePostModelWasteUSD
			respFields["gateway_addressable_pct"] = ta.GatewayAddressablePct
			if ta.ContextEfficiencyRatio != nil {
				respFields["context_efficiency_ratio"] = *ta.ContextEfficiencyRatio
			}
			// Measured reduction (Contract v2) — only when the real
			// extractor ran (shadow/active). Absent under projected.
			if ta.ReductionMode == "shadow" || ta.ReductionMode == "active" {
				respFields["reduction_sampled"] = ta.ReductionSampled
				respFields["actual_direct_payload_reduction_usd"] = ta.ActualDirectPayloadReductionUSD
				respFields["actual_reduction_relevance_usd"] = ta.ActualReductionRelevanceUSD
				respFields["actual_reduction_slm_usd"] = ta.ActualReductionSLMUSD
				if ta.ReductionEfficacyDelta != nil {
					respFields["reduction_efficacy_delta"] = *ta.ReductionEfficacyDelta
				}
				if ta.ReductionAssuranceDelta != nil {
					respFields["reduction_assurance_delta"] = *ta.ReductionAssuranceDelta
				}
			}
		}
	}
	// Promote the timing decomposition so LogQL can unwrap each
	// component directly. instrumentation.Snapshot captures the full
	// breakdown — gateway ingress → vendor TTFB → vendor TTFT →
	// vendor generation → gateway egress — but only end_to_end_ms
	// reaches dashboards unless we surface the rest here. Phase 2.3
	// (latency decomposition card) consumes these fields.
	//
	// Every duration is *int64; nil means "not captured" (skipped) so
	// dashboards distinguish "no data" from "0 ms".
	ts := resp.Data.EventTimestamps
	if ms := ts.EndToEndMs; ms != nil {
		respFields["end_to_end_ms"] = *ms
	}
	if ms := ts.GatewayIngressMs; ms != nil {
		respFields["gateway_ingress_ms"] = *ms
	}
	if ms := ts.GatewayEgressMs; ms != nil {
		respFields["gateway_egress_ms"] = *ms
	}
	if ms := ts.GatewayOverheadMs; ms != nil {
		respFields["gateway_overhead_ms"] = *ms
	}
	if ms := ts.VendorTTFBMs; ms != nil {
		respFields["vendor_ttfb_ms"] = *ms
	}
	if ms := ts.VendorTTFTMs; ms != nil {
		respFields["vendor_ttft_ms"] = *ms
	}
	if ms := ts.VendorGenerationMs; ms != nil {
		respFields["vendor_generation_ms"] = *ms
	}
	if ms := ts.MedianInterTokenMs; ms != nil {
		respFields["median_inter_token_ms"] = *ms
	}
	// Resolved policy bundle (Phase 4.0). Always populated when AIQG
	// middleware is wired; nil for non-AIQG callers (tests, the noop
	// path). Promote all three fields so LogQL can aggregate
	// "bundles in use this week" without parsing payload.
	if rpb := resp.Data.ResolvedPolicyBundle; rpb != nil {
		respFields["resolved_policy_bundle_id"] = rpb.BundleID
		respFields["resolved_policy_bundle_name"] = rpb.BundleName
		respFields["resolved_policy_bundle_source"] = rpb.Source
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
		// Promote NIST AI RMF per-characteristic counts as flat
		// top-level fields so LogQL can sum_over_time them directly
		// in the Day-1 Report Trustworthiness query.
		for k, v := range a.NISTFindings {
			respFields["nist_"+k] = v
		}
		// Promote per-pattern_id counts as `tag_<sanitized>` fields.
		// Hyphens become underscores because LogQL labels need to be
		// valid identifiers. /api/v1/metrics/tags performs the same
		// replacement when building its queries.
		for k, v := range a.TagFindings {
			respFields["tag_"+sanitizeTagKey(k)] = v
		}
	}
	// Promote self-asserted identity attribution so dashboards can
	// `| json | user_id="…"` / group by (agent_id) / by (flow_id)
	// without parsing the payload. Only non-empty fields are promoted.
	if ac := resp.Data.AgentContext; ac != nil {
		if ac.AgentID != "" {
			respFields["agent_id"] = ac.AgentID
		}
		if ac.AgentName != "" {
			respFields["agent_name"] = ac.AgentName
		}
		if ac.AgentVersion != "" {
			respFields["agent_version"] = ac.AgentVersion
		}
		if ac.UserID != "" {
			respFields["user_id"] = ac.UserID
		}
		if ac.ConversationID != "" {
			respFields["conversation_id"] = ac.ConversationID
		}
		if ac.FlowID != "" {
			respFields["flow_id"] = ac.FlowID
		}
		if ac.PrincipalID != "" {
			respFields["principal_id"] = ac.PrincipalID
		}
		if ac.ClientIP != "" {
			respFields["client_ip"] = ac.ClientIP
		}
		if ac.IdentitySource != "" {
			respFields["identity_source"] = ac.IdentitySource
		}
		if ac.StepID != "" {
			respFields["step_id"] = ac.StepID
		}
		if ac.ParentStepID != "" {
			respFields["parent_step_id"] = ac.ParentStepID
		}
		if ac.FlowStepSeq > 0 {
			respFields["flow_step_seq"] = ac.FlowStepSeq
		}
		// Attribution drift (Axis-1, Plan #8) — promote as fields; present
		// only when a declared agent existed. See classification-drift.md §3.
		if ac.AgentDeclared != "" {
			respFields["agent_declared"] = ac.AgentDeclared
			if ac.AgentInferred != "" {
				respFields["agent_inferred"] = ac.AgentInferred
			}
			if ac.DriftSource != "" {
				respFields["drift_source"] = ac.DriftSource
			}
		}
		if ac.AgentDrift != nil {
			respFields["agent_drift"] = *ac.AgentDrift
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

// sanitizeTagKey converts a Gatekeeper pattern_id ("aiqg-role-claim",
// "pii-ssn") into a Loki-label-safe identifier (hyphens become
// underscores). Kept in this package because the matching reverse
// lookup lives client-side in aiqg-dashboard-be's /metrics/tags
// handler — they MUST agree on the mapping.
func sanitizeTagKey(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' {
			b = append(b, '_')
			continue
		}
		b = append(b, c)
	}
	return string(b)
}
