package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Tool-call events (Plan #22 T.5, design §9.5). The contract is fixed by
// aether-shared data-models/aiqg/tool-call-event.md and enforced by the
// tas-spark-jobs writer jobs/aiqg_aggregator/tool_calls.py: the gateway sends
// observed_by="gateway", one event per call, and the writer merges it with
// the MCP proxy's sighting of the same (tenant_id, tool_call_id).
const (
	TypeToolCall = "com.tas.aiqg.tool_call.v1"

	// ObservedByGateway is this source's observed_by value. "both" is written
	// only by the aggregator.
	ObservedByGateway = "gateway"

	// Outcomes the gateway can see. The other four (tool_error, timeout,
	// unreachable, blocked) are the proxy's: the gateway cannot tell why a
	// tool failed, only whether its result came back.
	ToolOutcomeOK       = "ok"
	ToolOutcomeNoResult = "no_result"
)

// ToolCallEvent is the `data` of a com.tas.aiqg.tool_call.v1 CloudEvent.
// Field names are the contract's (§2.2). Fields the gateway cannot know
// (server_id, duration_ms, reduced_bytes) are never set, so they marshal as
// absent and the proxy's values win on merge.
type ToolCallEvent struct {
	TenantID  string `json:"tenant_id"`
	TokenID   string `json:"token_id,omitempty"`
	OwnerSub  string `json:"owner_sub,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	SourceApp string `json:"source_app,omitempty"`
	FlowID    string `json:"flow_id,omitempty"`
	StepID    string `json:"step_id,omitempty"`

	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	ObservedBy string `json:"observed_by"`
	Outcome    string `json:"outcome"`

	// RoundTripMs runs from the response that made the call to the request
	// that echoed its result. Includes the client. Nil for no_result.
	RoundTripMs *int64 `json:"round_trip_ms,omitempty"`
	// ResultBytes / ResultTokensEst are the gateway's estimate of the tool
	// result as it re-entered the prompt. The proxy's values win on merge.
	ResultBytes     *int64 `json:"result_bytes,omitempty"`
	ResultTokensEst *int64 `json:"result_tokens_est,omitempty"`

	// Findings are the inbound scan's COUNTABLE findings in this call's tool
	// result, by Gatekeeper pattern id (the event_metrics.tags keys). Nil
	// (JSON null, stored NULL) when the gateway could not attribute findings
	// to the message; {} when it could and there were none. Counts only.
	Findings map[string]int `json:"findings"`
	// ScanSkipped is true when scan-once remembered the result as clean and
	// did not scan it again. Nil when unknown.
	ScanSkipped *bool `json:"scan_skipped,omitempty"`

	// OccurredAt is when the call happened: the completion of the response
	// that made it.
	OccurredAt time.Time `json:"occurred_at"`
}

// ToolCallEnvelope wraps a ToolCallEvent in a CloudEvents 1.0 envelope.
type ToolCallEnvelope struct {
	SpecVersion     string        `json:"specversion"`
	Type            string        `json:"type"`
	Source          string        `json:"source"`
	ID              string        `json:"id"`
	Time            time.Time     `json:"time"`
	DataContentType string        `json:"datacontenttype"`
	Data            ToolCallEvent `json:"data"`
}

// NewToolCallEnvelope wraps d. The CloudEvent id is DERIVED from
// (tenant, tool_call_id, step_id) rather than random, so a sighting that is
// published twice (a retry after a partial failure) is the same gateway event
// to the writer, which then treats it as one sighting instead of a reused id.
// step_id is part of the key because some clients reuse ids across turns
// ("call_0"): those are different calls made by different steps.
func NewToolCallEnvelope(d ToolCallEvent, emitAt time.Time) ToolCallEnvelope {
	return ToolCallEnvelope{
		SpecVersion:     SpecVersion,
		Type:            TypeToolCall,
		Source:          Source,
		ID:              ToolCallEventID(d.TenantID, d.ToolCallID, d.StepID),
		Time:            emitAt.UTC(),
		DataContentType: DataContentType,
		Data:            d,
	}
}

// ToolCallEventID is the deterministic UUID-shaped id of a gateway sighting.
func ToolCallEventID(tenantID, toolCallID, stepID string) string {
	h := sha256.Sum256([]byte("tool_call.v1\x00" + tenantID + "\x00" + toolCallID + "\x00" + stepID))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x50 // version 5 (name-based, SHA)
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// ToolCallPublisher publishes single tool-call events. It is implemented by
// every Emitter in this package, so the tracker publishes through the same
// sink (and the same Kafka producer + topic) the paired request/response
// events use. Use AsToolCallPublisher to obtain one from an Emitter.
type ToolCallPublisher interface {
	PublishToolCall(ctx context.Context, ev ToolCallEnvelope) error
}

// AsToolCallPublisher returns em's tool-call publisher, or nil when em does
// not implement one (a caller-supplied Emitter from outside this package).
func AsToolCallPublisher(em Emitter) ToolCallPublisher {
	if p, ok := em.(ToolCallPublisher); ok {
		return p
	}
	return nil
}

// PublishToolCall drops the event.
func (NoopEmitter) PublishToolCall(context.Context, ToolCallEnvelope) error { return nil }

// PublishToolCall logs the event at INFO with its identity promoted, so it is
// queryable in Loki ({...} | json | ce_type="com.tas.aiqg.tool_call.v1").
func (e *LogEmitter) PublishToolCall(_ context.Context, ev ToolCallEnvelope) error {
	if e == nil || e.Logger == nil {
		return nil
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f := map[string]interface{}{
		"ce_type":      ev.Type,
		"ce_id":        ev.ID,
		"tenant_id":    ev.Data.TenantID,
		"tool_call_id": ev.Data.ToolCallID,
		"tool_name":    ev.Data.ToolName,
		"outcome":      ev.Data.Outcome,
		"observed_by":  ev.Data.ObservedBy,
		"payload":      string(body),
	}
	if ev.Data.RoundTripMs != nil {
		f["round_trip_ms"] = *ev.Data.RoundTripMs
	}
	if ev.Data.FlowID != "" {
		f["flow_id"] = ev.Data.FlowID
	}
	e.Logger.WithFields(f).Info("aiqg tool_call event")
	return nil
}

// PublishToolCall stores the event for tests.
func (e *MemoryEmitter) PublishToolCall(_ context.Context, ev ToolCallEnvelope) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolCalls = append(e.toolCalls, ev)
	return nil
}

// ToolCalls returns a copy of the published tool-call envelopes.
func (e *MemoryEmitter) ToolCalls() []ToolCallEnvelope {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]ToolCallEnvelope, len(e.toolCalls))
	copy(out, e.toolCalls)
	return out
}

// PublishToolCall produces the event to the AIQG topic, keyed by
// tenant_id + tool_call_id so both sightings of one call hash to the same
// partition.
func (e *KafkaEmitter) PublishToolCall(_ context.Context, ev ToolCallEnvelope) error {
	if e == nil || e.Producer == nil {
		return errors.New("events.KafkaEmitter: nil producer")
	}
	msg, err := buildKafkaMessage(e.Topic, ev.Type, ev.ID, ev.Data.TenantID+"/"+ev.Data.ToolCallID, ev)
	if err != nil {
		return err
	}
	if _, _, err := e.Producer.SendMessage(msg); err != nil {
		return fmt.Errorf("kafka send tool_call event: %w", err)
	}
	return nil
}

// PublishToolCall fans out to every wrapped emitter that can publish one.
// First error wins; every emitter is still tried.
func (m *MultiEmitter) PublishToolCall(ctx context.Context, ev ToolCallEnvelope) error {
	var firstErr error
	for _, inner := range m.Emitters {
		p := AsToolCallPublisher(inner)
		if p == nil {
			continue
		}
		if err := p.PublishToolCall(ctx, ev); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// OfferedTool is one tool a request offered the model, for the gateway rows
// of aiqg.tool_catalogue. SchemaBytes is the size of the tool's JSON
// definition (name, description, parameters): what offering it costs in
// input on every request.
type OfferedTool struct {
	Name        string `json:"name"`
	SchemaBytes int    `json:"schema_bytes"`
}
