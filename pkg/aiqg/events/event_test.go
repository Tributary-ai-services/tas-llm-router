package events

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/tributary-ai/llm-router-waf/internal/instrumentation"
	"github.com/tributary-ai/llm-router-waf/pkg/clear"
)

func TestStatusFromHTTP(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{0, StatusGatewayError},
		{200, StatusSuccess},
		{201, StatusSuccess},
		{399, StatusSuccess},
		{400, StatusGatewayError},
		{401, StatusGatewayError},
		{403, StatusGatewayError},
		{408, StatusTimeout},
		{499, StatusGatewayError},
		{500, StatusVendorError},
		{502, StatusVendorError},
		{504, StatusTimeout},
	}
	for _, c := range cases {
		if got := StatusFromHTTP(c.code); got != c.want {
			t.Errorf("StatusFromHTTP(%d)=%q want=%q", c.code, got, c.want)
		}
	}
}

// UUID format check: 8-4-4-4-12 lowercase hex.
func TestNewUUIDShape(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]struct{}{}
	for i := 0; i < 200; i++ {
		u := newUUID()
		if !re.MatchString(u) {
			t.Fatalf("malformed uuid: %q", u)
		}
		if _, dup := seen[u]; dup {
			t.Fatalf("duplicate uuid in 200 tries: %q", u)
		}
		seen[u] = struct{}{}
	}
}

func TestBuild_RoundTrip(t *testing.T) {
	c := instrumentation.NewCollector()
	t0 := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	// Stamp a known schedule so the snapshot assertions are deterministic.
	stampSnapshot(t, c, t0)

	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions?stream=true", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	r.Header.Set("X-Request-ID", "client-req-abc")

	headers := AIQGHeadersView{
		Workflow:     "rag",
		Policy:       []string{"pii_redact", "no-cot"},
		PolicyBundle: "",
		Trace:        true,
		DryRun:       false,
		SourceApp:    "billing-api-prod",
	}

	reqEnv, respEnv := Build(r, headers, RoutingView{
		Vendor:       "openai",
		Model:        "gpt-4o-mini",
		Streaming:    true,
		StreamingSet: true,
	}, TokenView{}, c.Snapshot(), BuildOptions{
		HTTPStatus: 200,
		Region:     "us-east",
	})

	// Envelope shape
	if reqEnv.SpecVersion != "1.0" || respEnv.SpecVersion != "1.0" {
		t.Errorf("specversion wrong: req=%q resp=%q", reqEnv.SpecVersion, respEnv.SpecVersion)
	}
	if reqEnv.Type != TypeRequest {
		t.Errorf("req type=%q", reqEnv.Type)
	}
	if respEnv.Type != TypeResponse {
		t.Errorf("resp type=%q", respEnv.Type)
	}
	if reqEnv.Source != Source || respEnv.Source != Source {
		t.Errorf("source urn mismatch")
	}
	if reqEnv.DataContentType != "application/json" {
		t.Errorf("datacontenttype=%q", reqEnv.DataContentType)
	}

	// Pair correlation
	if reqEnv.ID != reqEnv.Data.RequestEventID {
		t.Errorf("envelope ID does not equal data.request_event_id")
	}
	if respEnv.ID != respEnv.Data.ResponseEventID {
		t.Errorf("envelope ID does not equal data.response_event_id")
	}
	if reqEnv.Data.CorrelatedResponseEventID != respEnv.Data.ResponseEventID {
		t.Errorf("correlated_response_event_id does not point to response")
	}
	if respEnv.Data.RequestEventID != reqEnv.Data.RequestEventID {
		t.Errorf("response.request_event_id does not point to request")
	}

	// Request-event payload
	d := reqEnv.Data
	if d.Endpoint != "/openai/v1/chat/completions" {
		t.Errorf("endpoint=%q", d.Endpoint)
	}
	if d.Method != "POST" {
		t.Errorf("method=%q", d.Method)
	}
	if d.SourceIP != "10.0.0.5" {
		t.Errorf("source_ip=%q (RemoteAddr port-strip failed)", d.SourceIP)
	}
	if d.ClientRequestID != "client-req-abc" {
		t.Errorf("client_request_id=%q", d.ClientRequestID)
	}
	if d.Region != "us-east" {
		t.Errorf("region=%q", d.Region)
	}
	if d.Vendor != "openai" {
		t.Errorf("vendor=%q want=openai (from RoutingView)", d.Vendor)
	}
	if d.Model != "gpt-4o-mini" {
		t.Errorf("model=%q want=gpt-4o-mini (from RoutingView)", d.Model)
	}
	if !d.Streaming {
		t.Errorf("streaming should be true (RoutingView authoritative)")
	}
	if !d.IsAIQGMode {
		t.Errorf("is_aiqg_mode should be true")
	}
	if d.Workflow != "rag" {
		t.Errorf("workflow=%q", d.Workflow)
	}
	if d.PolicyBundle != "" {
		t.Errorf("policy_bundle=%q want empty", d.PolicyBundle)
	}
	if len(d.PolicyNames) != 2 {
		t.Errorf("policy_names len=%d", len(d.PolicyNames))
	}
	if !d.TraceReturned {
		t.Errorf("trace_returned should be true")
	}
	if d.LifecycleState != LifecyclePairedWithResponse {
		t.Errorf("lifecycle_state=%q", d.LifecycleState)
	}
	if d.ScoringVersion != ScoringVersion {
		t.Errorf("scoring_version=%q", d.ScoringVersion)
	}

	// Response-event payload
	rd := respEnv.Data
	if rd.Status != StatusSuccess {
		t.Errorf("status=%q", rd.Status)
	}
	if rd.HTTPStatus != 200 {
		t.Errorf("http_status=%d", rd.HTTPStatus)
	}
	if !rd.Streamed {
		t.Errorf("streamed should be true (snapshot had chunks)")
	}
	if rd.ChunkCount != 3 {
		t.Errorf("chunk_count=%d want=3", rd.ChunkCount)
	}
	if rd.ContentChunkCount != 2 {
		t.Errorf("content_chunk_count=%d want=2", rd.ContentChunkCount)
	}
	if rd.EventTimestamps.GatewayOverheadMs == nil {
		t.Errorf("event_timestamps.gateway_overhead_ms not embedded")
	}
}

// Build with no timing stamps and no headers — degenerate but must not panic.
func TestBuild_EmptySnapshot(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	r.RemoteAddr = "10.0.0.5:1234"

	reqEnv, respEnv := Build(r, AIQGHeadersView{}, RoutingView{}, TokenView{}, instrumentation.Snapshot{}, BuildOptions{HTTPStatus: 0})

	if reqEnv.Data.LifecycleState != LifecyclePairedWithResponse {
		t.Errorf("lifecycle_state default missing")
	}
	if respEnv.Data.Status != StatusGatewayError {
		t.Errorf("status for HTTPStatus=0 should be gateway_error, got %q", respEnv.Data.Status)
	}
	// receivedAt and completeAt fall back to time.Now() — both should
	// be very recent (within last second).
	if time.Since(reqEnv.Time) > time.Second {
		t.Errorf("envelope Time too old: %v", reqEnv.Time)
	}
}

func TestBuild_XForwardedFor(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")

	reqEnv, _ := Build(r, AIQGHeadersView{}, RoutingView{}, TokenView{}, instrumentation.Snapshot{}, BuildOptions{HTTPStatus: 200})
	if reqEnv.Data.SourceIP != "203.0.113.7" {
		t.Errorf("source_ip=%q (XFF first-entry not picked)", reqEnv.Data.SourceIP)
	}
}

// JSON marshalling must produce a CloudEvents 1.0 structured-mode payload
// — top-level keys for envelope, `data` for the event body.
func TestEnvelopeMarshalsToCloudEventsShape(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	r.RemoteAddr = "10.0.0.5:80"
	reqEnv, respEnv := Build(r, AIQGHeadersView{}, RoutingView{}, TokenView{}, instrumentation.Snapshot{}, BuildOptions{HTTPStatus: 200})

	for _, env := range []any{reqEnv, respEnv} {
		b, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range []string{"specversion", "type", "source", "id", "time", "datacontenttype", "data"} {
			if _, ok := m[k]; !ok {
				t.Errorf("missing CloudEvents key %q in envelope %T", k, env)
			}
		}
	}
}

// RoutingView.Streaming must override the URL-query heuristic only
// when StreamingSet=true; otherwise the heuristic wins.
func TestBuild_StreamingPrecedence(t *testing.T) {
	cases := []struct {
		name string
		url  string
		view RoutingView
		want bool
	}{
		{"url_says_yes_view_unset", "/v1/chat/completions?stream=true", RoutingView{}, true},
		{"url_says_no_view_unset", "/v1/chat/completions", RoutingView{}, false},
		{"view_overrides_url_true_to_false", "/v1/chat/completions?stream=true", RoutingView{StreamingSet: true, Streaming: false}, false},
		{"view_overrides_url_false_to_true", "/v1/chat/completions", RoutingView{StreamingSet: true, Streaming: true}, true},
		{"view_set_matches_url", "/v1/chat/completions?stream=true", RoutingView{StreamingSet: true, Streaming: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, c.url, nil)
			r.RemoteAddr = "10.0.0.5:80"
			reqEnv, _ := Build(r, AIQGHeadersView{}, c.view, TokenView{}, instrumentation.Snapshot{}, BuildOptions{HTTPStatus: 200})
			if reqEnv.Data.Streaming != c.want {
				t.Errorf("Streaming=%v want=%v", reqEnv.Data.Streaming, c.want)
			}
		})
	}
}

// Vendor and model are emitted verbatim from RoutingView — no
// normalization, no inference. Empty values are emitted as omitted.
func TestBuild_VendorModelPassthrough(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.0.0.5:80"

	// Populated case
	reqEnv, _ := Build(r, AIQGHeadersView{}, RoutingView{Vendor: "anthropic", Model: "claude-3-7-sonnet"}, TokenView{}, instrumentation.Snapshot{}, BuildOptions{HTTPStatus: 200})
	if reqEnv.Data.Vendor != "anthropic" || reqEnv.Data.Model != "claude-3-7-sonnet" {
		t.Errorf("vendor/model not passed through: %q / %q", reqEnv.Data.Vendor, reqEnv.Data.Model)
	}

	// Empty case
	reqEnv2, _ := Build(r, AIQGHeadersView{}, RoutingView{}, TokenView{}, instrumentation.Snapshot{}, BuildOptions{HTTPStatus: 200})
	if reqEnv2.Data.Vendor != "" || reqEnv2.Data.Model != "" {
		t.Errorf("vendor/model not empty: %q / %q", reqEnv2.Data.Vendor, reqEnv2.Data.Model)
	}
}

func TestNoopEmitter(t *testing.T) {
	if err := (NoopEmitter{}).Emit(context.Background(), RequestEnvelope{}, ResponseEnvelope{}); err != nil {
		t.Errorf("NoopEmitter.Emit returned err: %v", err)
	}
}

func TestLogEmitter(t *testing.T) {
	l := logrus.New()
	l.SetOutput(testWriter{t})
	e := &LogEmitter{Logger: l}

	reqEnv := RequestEnvelope{Type: TypeRequest, ID: "req-1", Data: RequestEvent{RequestEventID: "req-1"}}
	respEnv := ResponseEnvelope{Type: TypeResponse, ID: "resp-1", Data: ResponseEvent{ResponseEventID: "resp-1", RequestEventID: "req-1", Status: StatusSuccess, HTTPStatus: 200}}

	if err := e.Emit(context.Background(), reqEnv, respEnv); err != nil {
		t.Fatalf("Emit err: %v", err)
	}

	// Nil logger must not panic.
	(&LogEmitter{Logger: nil}).Emit(context.Background(), reqEnv, respEnv)
}

// fieldCapture is a logrus hook that captures every emitted Entry's
// Data map for inspection. Used to verify the top-level field
// promotions on LogEmitter.
type fieldCapture struct {
	entries []map[string]any
}

func (f *fieldCapture) Levels() []logrus.Level { return logrus.AllLevels }
func (f *fieldCapture) Fire(e *logrus.Entry) error {
	row := map[string]any{"msg": e.Message}
	for k, v := range e.Data {
		row[k] = v
	}
	f.entries = append(f.entries, row)
	return nil
}

// Field-promotion test: nested values should ride as top-level logrus
// fields so LogQL can filter on them directly without parsing payload.
func TestLogEmitter_PromotesNestedFields(t *testing.T) {
	cap := &fieldCapture{}
	l := logrus.New()
	l.SetOutput(testWriter{t})
	l.AddHook(cap)
	e := &LogEmitter{Logger: l}

	cost, latency, composite := int16(80), int16(95), int16(88)
	reqEnv := RequestEnvelope{
		Type: TypeRequest, ID: "req-1",
		Data: RequestEvent{
			RequestEventID: "req-1",
			Vendor:         "openai",
			Model:          "gpt-4o-mini",
			Endpoint:       "/v1/chat/completions",
			Workflow:       "rag",
			TenantID:       "tenant-a",
			AIQGAccountID:  "account-a",
			SourceApp:      "billing",
			Streaming:      true,
			DryRun:         false,
			GatewayVersion: "aiqg-v5.5+test",
			PolicyBundle:   "prod-strict",
		},
	}
	respEnv := ResponseEnvelope{
		Type: TypeResponse, ID: "resp-1",
		Data: ResponseEvent{
			ResponseEventID: "resp-1",
			RequestEventID:  "req-1",
			Vendor:          "openai",
			Model:           "gpt-4o-mini",
			Status:          StatusSuccess,
			HTTPStatus:      200,
			Streamed:        true,
			ChunkCount:      42,
			FinishReason:    "stop",
			TenantID:        "tenant-a",
			AIQGAccountID:   "account-a",
			GatewayVersion:  "aiqg-v5.5+test",
			ScoringVersion:  "clear-v0.1-mvp",
			TokenAccounting: &TokenAccounting{
				PromptTokens:     100,
				CompletionTokens: 200,
				TotalTokens:      300,
				TotalCostUSD:     0.0012,
			},
			Assurance: &AssuranceSummary{
				InboundCount:  2,
				OutboundCount: 0,
				WorstSeverity: "medium",
			},
			// Timing decomposition — every field promoted to top-level
			// logrus so LogQL can unwrap each component independently.
			// Phase 2.3 latency-decomposition card on the Health Report
			// consumes these fields.
			EventTimestamps: instrumentation.Snapshot{
				EndToEndMs:         ptrI64(2500),
				GatewayIngressMs:   ptrI64(12),
				GatewayEgressMs:    ptrI64(8),
				GatewayOverheadMs:  ptrI64(20),
				VendorTTFBMs:       ptrI64(180),
				VendorTTFTMs:       ptrI64(220),
				VendorGenerationMs: ptrI64(2260),
				MedianInterTokenMs: ptrI64(35),
			},
		},
	}
	// Wire a clear.Scores manually — events imports pkg/clear so this
	// avoids re-importing it in the test by spinning up a minimal one.
	// We rely on the LogEmitter's nil-safe code path: if CLEAR is nil
	// the promoted fields are simply absent. For a population test
	// we set the field by writing the marshalled JSON; here we use
	// a helper that constructs the Scores directly.
	respEnv.Data.CLEAR = makeScoresForTest(&cost, &latency, &composite)

	if err := e.Emit(context.Background(), reqEnv, respEnv); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(cap.entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(cap.entries))
	}

	req := cap.entries[0]
	requireField(t, req, "vendor", "openai")
	requireField(t, req, "model", "gpt-4o-mini")
	requireField(t, req, "workflow", "rag")
	requireField(t, req, "tenant_id", "tenant-a")
	requireField(t, req, "source_app", "billing")
	requireField(t, req, "streaming", true)
	requireField(t, req, "policy_bundle", "prod-strict")

	resp := cap.entries[1]
	requireField(t, resp, "status", StatusSuccess)
	requireField(t, resp, "streamed", true)
	requireField(t, resp, "finish_reason", "stop")
	// Vendor/model are denormalized onto the response event and promoted
	// onto its log line so dashboards group cost/tokens by vendor/model.
	requireField(t, resp, "vendor", "openai")
	requireField(t, resp, "model", "gpt-4o-mini")
	requireField(t, resp, "prompt_tokens", 100)
	requireField(t, resp, "total_tokens", 300)
	requireField(t, resp, "total_cost_usd", 0.0012)
	requireField(t, resp, "clear_cost", int16(80))
	requireField(t, resp, "clear_latency", int16(95))
	requireField(t, resp, "clear_composite", int16(88))
	requireField(t, resp, "assurance_inbound_count", 2)
	requireField(t, resp, "assurance_outbound_count", 0)
	requireField(t, resp, "assurance_worst_severity", "medium")
	// Timing decomposition — every component promoted as int64 so LogQL
	// `unwrap` works directly. nil snapshot fields stay absent (separate
	// regression in TestLogEmitter_NilNestedStructs).
	requireField(t, resp, "end_to_end_ms", int64(2500))
	requireField(t, resp, "gateway_ingress_ms", int64(12))
	requireField(t, resp, "gateway_egress_ms", int64(8))
	requireField(t, resp, "gateway_overhead_ms", int64(20))
	requireField(t, resp, "vendor_ttfb_ms", int64(180))
	requireField(t, resp, "vendor_ttft_ms", int64(220))
	requireField(t, resp, "vendor_generation_ms", int64(2260))
	requireField(t, resp, "median_inter_token_ms", int64(35))
}

// ptrI64 returns a pointer to an int64 literal. Used only by tests
// that need to populate Snapshot fields without spinning up a full
// instrumentation.Collector run.
func ptrI64(v int64) *int64 { return &v }

// Empty/nil nested structs leave their promoted fields absent without
// panicking — important because not every request has TokenAccounting,
// CLEAR, or Assurance populated.
func TestLogEmitter_NilNestedStructs(t *testing.T) {
	cap := &fieldCapture{}
	l := logrus.New()
	l.SetOutput(testWriter{t})
	l.AddHook(cap)
	e := &LogEmitter{Logger: l}

	if err := e.Emit(context.Background(),
		RequestEnvelope{Type: TypeRequest, ID: "r"},
		ResponseEnvelope{Type: TypeResponse, ID: "s", Data: ResponseEvent{Status: StatusGatewayError}},
	); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	resp := cap.entries[1]
	// CLEAR / TokenAccounting / Assurance all nil — promoted fields absent.
	for _, absent := range []string{
		"clear_cost", "clear_composite", "prompt_tokens", "total_cost_usd",
		"assurance_inbound_count", "assurance_worst_severity",
	} {
		if _, ok := resp[absent]; ok {
			t.Errorf("field %q should be absent when nested struct is nil", absent)
		}
	}
}

func requireField(t *testing.T, fields map[string]any, key string, want any) {
	t.Helper()
	got, ok := fields[key]
	if !ok {
		t.Errorf("field %q missing", key)
		return
	}
	if got != want {
		t.Errorf("field %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}

func makeScoresForTest(cost, latency, composite *int16) *clear.Scores {
	return &clear.Scores{Cost: cost, Latency: latency, Composite: composite}
}

func TestMemoryEmitter(t *testing.T) {
	e := &MemoryEmitter{}
	if e.Len() != 0 {
		t.Errorf("Len()=%d want=0 initially", e.Len())
	}

	for i := 0; i < 5; i++ {
		err := e.Emit(context.Background(),
			RequestEnvelope{Data: RequestEvent{RequestEventID: "r"}},
			ResponseEnvelope{Data: ResponseEvent{ResponseEventID: "s"}})
		if err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}
	if e.Len() != 5 {
		t.Errorf("Len()=%d want=5", e.Len())
	}
	if len(e.Requests()) != 5 || len(e.Responses()) != 5 {
		t.Errorf("Requests/Responses len mismatch")
	}

	// Returned slice must be a copy — mutating it shouldn't affect future Emit calls.
	r := e.Requests()
	r[0] = RequestEnvelope{ID: "tampered"}
	if e.Requests()[0].ID == "tampered" {
		t.Errorf("Requests() returned live slice, not a copy")
	}

	e.Reset()
	if e.Len() != 0 {
		t.Errorf("after Reset() Len()=%d", e.Len())
	}
}

func TestMemoryEmitter_Concurrent(t *testing.T) {
	e := &MemoryEmitter{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.Emit(context.Background(),
				RequestEnvelope{Data: RequestEvent{RequestEventID: "r"}},
				ResponseEnvelope{Data: ResponseEvent{ResponseEventID: "s"}})
		}()
	}
	wg.Wait()
	if e.Len() != 50 {
		t.Errorf("Len()=%d want=50 after 50 concurrent emits", e.Len())
	}
}

// stampSnapshot drives a TimingCollector through a known schedule so
// subsequent .Snapshot() assertions are deterministic.
func stampSnapshot(t *testing.T, c *instrumentation.TimingCollector, t0 time.Time) {
	t.Helper()
	ctx := instrumentation.WithCollector(context.Background(), c)
	// We can't override time.Now() inside the package, but stamping in
	// rapid succession is enough — the test only checks counters and
	// presence of intervals, not absolute milliseconds.
	instrumentation.StampReceived(ctx)
	instrumentation.StampForwarded(ctx)
	instrumentation.StampChunk(ctx, false)
	instrumentation.StampChunk(ctx, true)
	instrumentation.StampChunk(ctx, true)
	instrumentation.StampLastChunk(ctx)
	instrumentation.StampComplete(ctx)
	_ = t0 // schedule arg reserved for future use; presently the helper just exercises the API
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Logf("log: %s", string(b))
	return len(b), nil
}
