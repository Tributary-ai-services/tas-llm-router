package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/tributary-ai/llm-router-waf/internal/instrumentation"
)

// silentLogger returns a logger that discards output — keeps test
// output clean while exercising every code path that logs.
func silentLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

// echoHandler is a downstream handler that records whether it ran and
// what the AIQG state in the request context looked like at the time.
type echoHandler struct {
	called      bool
	headers     AIQGHeaders
	headersOK   bool
	hasTimings  bool
	requestSeen *http.Request
}

func (e *echoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.called = true
	e.requestSeen = r
	e.headers, e.headersOK = FromContext(r.Context())
	if c := instrumentation.FromContext(r.Context()); c != nil {
		e.hasTimings = true
	}
	w.WriteHeader(http.StatusOK)
}

func TestNewAIQG_PanicsOnNilLogger(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil logger")
		}
	}()
	NewAIQG(AIQGConfig{Strict: true, Logger: nil})
}

// Permissive mode + no TAS-Auth = pass-through. The downstream handler
// runs, and there is NO AIQG state on the context — internal-routing
// callers must look identical to today.
func TestAIQG_PermissivePassThrough(t *testing.T) {
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{Strict: false, Logger: silentLogger()})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if !next.called {
		t.Fatalf("downstream handler not called in permissive pass-through")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want=200", rec.Code)
	}
	if next.headersOK {
		t.Errorf("AIQG headers attached on a pass-through request: %#v", next.headers)
	}
	if next.hasTimings {
		t.Errorf("TimingCollector attached on a pass-through request")
	}
}

// Strict mode + no TAS-Auth = 401 with diagnostic body.
func TestAIQG_StrictRejectsMissingTASAuth(t *testing.T) {
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger()})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-customer")
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if next.called {
		t.Fatalf("downstream called despite 401")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want=401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "aiqg") {
		t.Errorf("WWW-Authenticate=%q missing aiqg realm", got)
	}

	var body struct {
		Error struct {
			Code          string `json:"code"`
			Message       string `json:"message"`
			MissingHeader string `json:"missing_header"`
			Docs          string `json:"docs"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v\nbody=%s", err, rec.Body.String())
	}
	if body.Error.Code != "path_a_auth_required" {
		t.Errorf("error.code=%q", body.Error.Code)
	}
	if body.Error.MissingHeader != "TAS-Auth" {
		t.Errorf("missing_header=%q want=TAS-Auth", body.Error.MissingHeader)
	}
	if body.Error.Docs == "" {
		t.Errorf("docs link should be present for self-service diagnosis")
	}
}

// TAS-Auth set, Authorization missing = 401 regardless of strict — once
// a caller asserts TAS-Auth they've opted into Path A semantics, and
// Path A requires the customer's vendor key in Authorization.
func TestAIQG_RejectsTASAuthWithoutAuthorization(t *testing.T) {
	for _, strict := range []bool{true, false} {
		next := &echoHandler{}
		mw := NewAIQG(AIQGConfig{Strict: strict, Logger: silentLogger()})

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("TAS-Auth", "tas_qg_live_abc")
		rec := httptest.NewRecorder()
		mw(next).ServeHTTP(rec, req)

		if next.called {
			t.Fatalf("strict=%v: downstream called despite 401", strict)
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("strict=%v: code=%d want=401", strict, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Authorization") {
			t.Errorf("strict=%v: body should name Authorization as missing", strict)
		}
	}
}

// Happy path: both headers present, validation passes, downstream runs
// with AIQG state attached.
func TestAIQG_HappyPathAttachesContextAndStamps(t *testing.T) {
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger()})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc123")
	req.Header.Set("Authorization", "Bearer sk-customer")
	req.Header.Set("TAS-Workflow", "rag")
	req.Header.Set("TAS-Trace", "1")
	req.Header.Set("TAS-Policy-Bundle", "prod-strict")
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if !next.called {
		t.Fatalf("downstream not called on happy path")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want=200", rec.Code)
	}
	if !next.headersOK {
		t.Fatalf("AIQG headers not attached to ctx")
	}
	if next.headers.Workflow != "rag" {
		t.Errorf("Workflow=%q want=rag", next.headers.Workflow)
	}
	if !next.headers.Trace {
		t.Errorf("Trace not propagated")
	}
	if next.headers.PolicyBundle != "prod-strict" {
		t.Errorf("PolicyBundle=%q", next.headers.PolicyBundle)
	}
	if !next.hasTimings {
		t.Fatalf("TimingCollector not attached")
	}

	// StampReceived must have fired before downstream ran. We can verify
	// by snapshotting the collector after the middleware returns and
	// checking RequestReceivedAt is populated.
	c := instrumentation.FromContext(next.requestSeen.Context())
	if c == nil {
		t.Fatalf("no collector on captured request ctx")
	}
	s := c.Snapshot()
	if s.RequestReceivedAt == nil {
		t.Errorf("StampReceived did not fire on entry")
	}
	if s.ResponseCompleteAt == nil {
		t.Errorf("StampComplete did not fire on exit")
	}
}

// Validation errors (TAS-Policy + TAS-Policy-Bundle conflict) → 400.
func TestAIQG_PolicyConflictReturns400(t *testing.T) {
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger()})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk-customer")
	req.Header.Set("TAS-Policy", "pii_redact")
	req.Header.Set("TAS-Policy-Bundle", "prod-strict")
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if next.called {
		t.Fatalf("downstream called despite 400")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want=400", rec.Code)
	}

	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body.Error.Code != "aiqg_header_invalid" {
		t.Errorf("error.code=%q", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "mutually exclusive") {
		t.Errorf("message=%q should mention mutually exclusive", body.Error.Message)
	}
}

func TestAIQG_AuthMalformedReturns400(t *testing.T) {
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger()})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "not-a-valid-token-prefix")
	req.Header.Set("Authorization", "Bearer sk-customer")
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want=400", rec.Code)
	}
	if next.called {
		t.Errorf("downstream called despite malformed auth")
	}
}

// Soft error (unknown workflow): logged, field cleared, request proceeds.
func TestAIQG_UnknownWorkflowFallsThrough(t *testing.T) {
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger()})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk-customer")
	req.Header.Set("TAS-Workflow", "rag-bogus") // invalid
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want=200 (soft error should not block)", rec.Code)
	}
	if !next.called {
		t.Fatalf("downstream not called")
	}
	if !next.headersOK {
		t.Fatalf("headers not attached")
	}
	if next.headers.Workflow != "" {
		t.Errorf("Workflow=%q should be cleared on invalid override", next.headers.Workflow)
	}
}

// The middleware must not touch any non-TAS header on a pass-through
// request — internal callers' Authorization to the stored-key vendor
// path must survive verbatim.
func TestAIQG_PassThroughPreservesHeaders(t *testing.T) {
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{Strict: false, Logger: silentLogger()})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-internal")
	req.Header.Set("X-Tenant-ID", "tenant-7")
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if !next.called {
		t.Fatalf("downstream not called")
	}
	if got := next.requestSeen.Header.Get("Authorization"); got != "Bearer sk-internal" {
		t.Errorf("Authorization mangled in pass-through: %q", got)
	}
	if got := next.requestSeen.Header.Get("X-Tenant-ID"); got != "tenant-7" {
		t.Errorf("X-Tenant-ID mangled: %q", got)
	}
}
