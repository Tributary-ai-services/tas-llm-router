package events

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func i64(v int64) *int64 { return &v }
func bptr(v bool) *bool  { return &v }

func goldenToolCall() ToolCallEnvelope {
	return NewToolCallEnvelope(ToolCallEvent{
		TenantID:        "a689c0b2-02ca-46d1-9916-f9a30c00222a",
		TokenID:         "8f29197c-818b-4b69-bf59-200f5c3228f5",
		OwnerSub:        "kc-sub-123",
		UserID:          "u-42",
		AgentID:         "release-bot",
		SourceApp:       "claude-code",
		FlowID:          "6c1d2e4f-0a1b-4c3d-8e9f-001122334455",
		StepID:          "0b9e3f1a-7c55-4d2e-a1b0-5f6e7d8c9b0a",
		ToolCallID:      "toolu_01AbC",
		ToolName:        "crm_lookup",
		ObservedBy:      ObservedByGateway,
		Outcome:         ToolOutcomeOK,
		RoundTripMs:     i64(1830),
		ResultBytes:     i64(2048),
		ResultTokensEst: i64(512),
		Findings:        map[string]int{"pii-email": 1},
		ScanSkipped:     bptr(false),
		OccurredAt:      time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC),
	}, time.Date(2026, 11, 21, 10, 0, 1, 830000000, time.UTC))
}

// The golden payload pins the wire contract (aether-shared
// data-models/aiqg/tool-call-event.md §2.2, tas-spark-jobs schema.py
// TOOL_CALL_DATA). Regenerate with UPDATE_GOLDEN=1 only for a deliberate
// contract change.
func TestToolCallEnvelope_Golden(t *testing.T) {
	got, err := json.MarshalIndent(goldenToolCall(), "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')
	path := filepath.Join("testdata", "tool_call_gateway.golden.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(path, got, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
}

// Every key the gateway sends is one the writer reads; nothing the gateway
// cannot know (server_id, duration_ms, reduced_bytes) is sent, so the
// proxy's values win the merge.
func TestToolCallEnvelope_FieldsMatchWriterSchema(t *testing.T) {
	writer := map[string]bool{ // tas-spark-jobs TOOL_CALL_DATA + findings
		"tenant_id": true, "token_id": true, "owner_sub": true, "user_id": true, "agent_id": true,
		"source_app": true, "flow_id": true, "step_id": true, "tool_call_id": true, "tool_name": true,
		"server_id": true, "observed_by": true, "outcome": true, "duration_ms": true, "round_trip_ms": true,
		"result_bytes": true, "result_tokens_est": true, "reduced_bytes": true, "scan_skipped": true,
		"occurred_at": true, "findings": true,
	}
	b, _ := json.Marshal(goldenToolCall())
	var env map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &env))
	for _, k := range []string{"specversion", "type", "source", "id", "time", "datacontenttype", "data"} {
		assert.Contains(t, env, k)
	}
	var data map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(env["data"], &data))
	for k := range data {
		assert.True(t, writer[k], "field %q is not in the writer's schema", k)
	}
	for _, k := range []string{"server_id", "duration_ms", "reduced_bytes"} {
		assert.NotContains(t, data, k)
	}
	assert.JSONEq(t, `"com.tas.aiqg.tool_call.v1"`, string(env["type"]))
}

func TestToolCallEnvelope_FindingsNullVsEmpty(t *testing.T) {
	ev := goldenToolCall()
	ev.Data.Findings = nil
	b, _ := json.Marshal(ev)
	assert.Contains(t, string(b), `"findings":null`, "unattributable -> NULL in the table")
	ev.Data.Findings = map[string]int{}
	b, _ = json.Marshal(ev)
	assert.Contains(t, string(b), `"findings":{}`, "scanned, none found")
}

func TestToolCallEventID_DeterministicAndUUIDShaped(t *testing.T) {
	a := ToolCallEventID("t", "toolu_1", "step-1")
	assert.Equal(t, a, ToolCallEventID("t", "toolu_1", "step-1"), "a re-publish is the same sighting")
	assert.NotEqual(t, a, ToolCallEventID("t", "toolu_1", "step-2"), "a reused id from another step is another call")
	assert.NotEqual(t, a, ToolCallEventID("t2", "toolu_1", "step-1"))
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`), a)
}

func TestToolCallPublisher_AllEmitters(t *testing.T) {
	ev := goldenToolCall()
	mem := &MemoryEmitter{}
	var buf bytes.Buffer
	lg := logrus.New()
	lg.SetOutput(&buf)
	lg.SetFormatter(&logrus.JSONFormatter{})
	multi := &MultiEmitter{Emitters: []Emitter{&LogEmitter{Logger: lg}, mem}}

	for _, em := range []Emitter{NoopEmitter{}, multi} {
		p := AsToolCallPublisher(em)
		require.NotNil(t, p)
		require.NoError(t, p.PublishToolCall(context.Background(), ev))
	}
	require.Len(t, mem.ToolCalls(), 1)
	assert.Equal(t, ev.ID, mem.ToolCalls()[0].ID)
	var line map[string]interface{}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	assert.Equal(t, TypeToolCall, line["ce_type"])
	assert.Equal(t, "toolu_01AbC", line["tool_call_id"])
	assert.EqualValues(t, 1830, line["round_trip_ms"])
}
