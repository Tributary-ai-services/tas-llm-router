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

	reqEnv, respEnv := Build(r, headers, c.Snapshot(), BuildOptions{
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
	if !d.Streaming {
		t.Errorf("streaming should be true (?stream=true)")
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

	reqEnv, respEnv := Build(r, AIQGHeadersView{}, instrumentation.Snapshot{}, BuildOptions{HTTPStatus: 0})

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

	reqEnv, _ := Build(r, AIQGHeadersView{}, instrumentation.Snapshot{}, BuildOptions{HTTPStatus: 200})
	if reqEnv.Data.SourceIP != "203.0.113.7" {
		t.Errorf("source_ip=%q (XFF first-entry not picked)", reqEnv.Data.SourceIP)
	}
}

// JSON marshalling must produce a CloudEvents 1.0 structured-mode payload
// — top-level keys for envelope, `data` for the event body.
func TestEnvelopeMarshalsToCloudEventsShape(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	r.RemoteAddr = "10.0.0.5:80"
	reqEnv, respEnv := Build(r, AIQGHeadersView{}, instrumentation.Snapshot{}, BuildOptions{HTTPStatus: 200})

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
