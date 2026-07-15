package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/tributary-ai/llm-router-waf/internal/instrumentation"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/tokens"
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

// Happy-path AIQG requests must produce a paired (request, response)
// event via the configured Emitter. This is the integration point
// between the AIQG middleware and pkg/aiqg/events.
func TestAIQG_EmitsPairedEvents(t *testing.T) {
	em := &events.MemoryEmitter{}
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{
		Strict:  true,
		Logger:  silentLogger(),
		Emitter: em,
		Region:  "us-east",
	})

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions?stream=true", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("TAS-Auth", "tas_qg_live_abc123")
	req.Header.Set("Authorization", "Bearer sk-customer")
	req.Header.Set("TAS-Workflow", "rag")
	req.Header.Set("TAS-Trace", "1")
	req.Header.Set("TAS-Source-App", "billing-api-prod")
	req.Header.Set("X-Request-ID", "client-req-xyz")

	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if !next.called {
		t.Fatalf("downstream not called")
	}
	if em.Len() != 1 {
		t.Fatalf("emit count=%d want=1", em.Len())
	}

	reqEnv := em.Requests()[0]
	respEnv := em.Responses()[0]

	// Pair correlation
	if reqEnv.Data.CorrelatedResponseEventID != respEnv.Data.ResponseEventID {
		t.Errorf("CorrelatedResponseEventID does not match ResponseEventID")
	}
	if respEnv.Data.RequestEventID != reqEnv.Data.RequestEventID {
		t.Errorf("response.RequestEventID does not match request.RequestEventID")
	}

	// Header reflection
	if reqEnv.Data.Workflow != "rag" {
		t.Errorf("workflow=%q", reqEnv.Data.Workflow)
	}
	if !reqEnv.Data.TraceReturned {
		t.Errorf("trace_returned not propagated")
	}
	if reqEnv.Data.SourceApp != "billing-api-prod" {
		t.Errorf("source_app=%q", reqEnv.Data.SourceApp)
	}
	if reqEnv.Data.ClientRequestID != "client-req-xyz" {
		t.Errorf("client_request_id=%q", reqEnv.Data.ClientRequestID)
	}
	if reqEnv.Data.Region != "us-east" {
		t.Errorf("region=%q", reqEnv.Data.Region)
	}
	if !reqEnv.Data.Streaming {
		t.Errorf("streaming should be true for ?stream=true URL")
	}

	// Timing snapshot must be embedded with both endpoints set —
	// proves StampReceived (entry) and StampComplete (defer LIFO order)
	// both fired before the emitter ran.
	if respEnv.Data.EventTimestamps.RequestReceivedAt == nil {
		t.Errorf("request_received_at not stamped before emit")
	}
	if respEnv.Data.EventTimestamps.ResponseCompleteAt == nil {
		t.Errorf("response_complete_at not stamped before emit (defer order broken)")
	}

	// HTTP status captured from the echo handler's WriteHeader(200).
	if respEnv.Data.HTTPStatus != http.StatusOK {
		t.Errorf("http_status=%d want=200", respEnv.Data.HTTPStatus)
	}
	if respEnv.Data.Status != events.StatusSuccess {
		t.Errorf("status=%q", respEnv.Data.Status)
	}
}

// A configured Resolver enriches the event with tenant_id /
// aiqg_account_id / tas_auth_token_id when the bearer resolves.
func TestAIQG_ResolverEnrichesEvent(t *testing.T) {
	em := &events.MemoryEmitter{}
	resolver := tokens.NewMapResolver([]tokens.ConfigToken{
		{
			TokenID:       "tok_uuid_1",
			Token:         "tas_qg_live_abc",
			TenantID:      "tenant-a",
			AIQGAccountID: "account-a",
			SourceApp:     "billing-api-prod",
		},
	})
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{
		Strict:   true,
		Logger:   silentLogger(),
		Emitter:  em,
		Resolver: resolver,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk-customer")
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if em.Len() != 1 {
		t.Fatalf("emit count=%d want=1", em.Len())
	}
	d := em.Requests()[0].Data
	if d.TenantID != "tenant-a" {
		t.Errorf("TenantID=%q", d.TenantID)
	}
	if d.AIQGAccountID != "account-a" {
		t.Errorf("AIQGAccountID=%q", d.AIQGAccountID)
	}
	if d.TASAuthTokenID != "tok_uuid_1" {
		t.Errorf("TASAuthTokenID=%q", d.TASAuthTokenID)
	}
	// SourceApp falls back to the token claim when no header was sent.
	if d.SourceApp != "billing-api-prod" {
		t.Errorf("SourceApp=%q want=billing-api-prod (token claim fallback)", d.SourceApp)
	}

	// Response event also carries the denormalized IDs.
	rd := em.Responses()[0].Data
	if rd.TenantID != "tenant-a" || rd.AIQGAccountID != "account-a" {
		t.Errorf("response denorm IDs not populated: tenant=%q account=%q", rd.TenantID, rd.AIQGAccountID)
	}
}

// TAS-Source-App header wins over the token's source_app claim per spec §80.
func TestAIQG_SourceAppHeaderOverridesTokenClaim(t *testing.T) {
	em := &events.MemoryEmitter{}
	resolver := tokens.NewMapResolver([]tokens.ConfigToken{
		{Token: "tas_qg_live_abc", TenantID: "t1", AIQGAccountID: "a1", SourceApp: "token-claim-app"},
	})
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em, Resolver: resolver})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set("TAS-Source-App", "header-override-app")
	rec := httptest.NewRecorder()
	mw(&echoHandler{}).ServeHTTP(rec, req)

	d := em.Requests()[0].Data
	if d.SourceApp != "header-override-app" {
		t.Errorf("SourceApp=%q; header should win over token claim", d.SourceApp)
	}
}

// Unknown token → 401 with reason=token_unknown; no event emitted.
func TestAIQG_UnknownTokenReturns401AndEmitsNothing(t *testing.T) {
	em := &events.MemoryEmitter{}
	resolver := tokens.NewMapResolver([]tokens.ConfigToken{
		{Token: "tas_qg_live_known", TenantID: "t1", AIQGAccountID: "a1"},
	})
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em, Resolver: resolver})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_unknown")
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want=401", rec.Code)
	}
	if next.called {
		t.Errorf("downstream called on unknown token")
	}
	if em.Len() != 0 {
		t.Errorf("emit count=%d want=0 (pre-validation auth failure per spec §273)", em.Len())
	}

	var body struct {
		Error struct {
			Code   string `json:"code"`
			Reason string `json:"reason"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error.Reason != "token_unknown" {
		t.Errorf("error.reason=%q want=token_unknown", body.Error.Reason)
	}
}

// Suspended account → 403 with code=account_suspended; no event emitted.
func TestAIQG_SuspendedAccountReturns403(t *testing.T) {
	em := &events.MemoryEmitter{}
	resolver := tokens.NewMapResolver([]tokens.ConfigToken{
		{Token: "tas_qg_live_suspended", TenantID: "t1", AIQGAccountID: "a1", Suspended: true},
	})
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em, Resolver: resolver})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_suspended")
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want=403", rec.Code)
	}
	if next.called {
		t.Errorf("downstream called on suspended account")
	}
	if em.Len() != 0 {
		t.Errorf("emit count=%d want=0", em.Len())
	}
	if !strings.Contains(rec.Body.String(), "account_suspended") {
		t.Errorf("body missing code: %s", rec.Body.String())
	}
}

// Resolver errors (e.g. backend store unavailable) → 503, no emit.
type errResolver struct{ err error }

func (e errResolver) Resolve(_ context.Context, _ string) (*tokens.Token, error) {
	return nil, e.err
}

func TestAIQG_ResolverErrorReturns503(t *testing.T) {
	em := &events.MemoryEmitter{}
	resolver := errResolver{err: errors.New("backend down")}
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em, Resolver: resolver})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_anything")
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	mw(&echoHandler{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want=503", rec.Code)
	}
	if em.Len() != 0 {
		t.Errorf("emit on resolver error")
	}
}

// No-resolver config (incremental rollout) accepts any TAS-Auth as
// opaque; events emit with empty tenant fields.
func TestAIQG_NoResolverAcceptsAnyToken(t *testing.T) {
	em := &events.MemoryEmitter{}
	mw := NewAIQG(AIQGConfig{
		Strict:   true,
		Logger:   silentLogger(),
		Emitter:  em,
		Resolver: nil, // no resolver configured
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_anything")
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	mw(&echoHandler{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want=200 (no-resolver path should accept)", rec.Code)
	}
	if em.Len() != 1 {
		t.Fatalf("emit count=%d want=1", em.Len())
	}
	d := em.Requests()[0].Data
	if d.TenantID != "" || d.AIQGAccountID != "" || d.TASAuthTokenID != "" {
		t.Errorf("expected empty tenant fields without resolver, got tenant=%q account=%q tokenID=%q",
			d.TenantID, d.AIQGAccountID, d.TASAuthTokenID)
	}
}

// Stamping vendor + model + token usage from the handler must produce
// a populated TokenAccounting on the response event and a non-nil
// CLEAR.Cost score. End-to-end test of the second CLEAR dimension.
func TestAIQG_TokenUsageStampedReachesEventAndCost(t *testing.T) {
	em := &events.MemoryEmitter{}
	stampingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StampVendor(r.Context(), "openai")
		StampModel(r.Context(), "gpt-4o-mini")
		StampTokenUsage(r.Context(), 1000, 500, 0, 0)
		w.WriteHeader(http.StatusOK)
	})

	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	mw(stampingHandler).ServeHTTP(rec, req)

	if em.Len() != 1 {
		t.Fatalf("emit count=%d want=1", em.Len())
	}
	rd := em.Responses()[0].Data

	if rd.TokenAccounting == nil {
		t.Fatalf("TokenAccounting nil despite StampTokenUsage")
	}
	if rd.TokenAccounting.PromptTokens != 1000 || rd.TokenAccounting.CompletionTokens != 500 {
		t.Errorf("token counts: prompt=%d completion=%d", rd.TokenAccounting.PromptTokens, rd.TokenAccounting.CompletionTokens)
	}
	if rd.TokenAccounting.TotalTokens != 1500 {
		t.Errorf("total_tokens=%d want=1500", rd.TokenAccounting.TotalTokens)
	}
	if rd.TokenAccounting.TotalCostUSD <= 0 {
		t.Errorf("TotalCostUSD=%v want>0 for priced model", rd.TokenAccounting.TotalCostUSD)
	}
	if rd.TokenAccounting.ModelPricingVersion == "" {
		t.Errorf("ModelPricingVersion empty")
	}
	if rd.CLEAR.Cost == nil {
		t.Fatalf("CLEAR.Cost nil despite priced model + usage stamp")
	}
}

// Gatekeeper findings stamped by the handler must produce both a
// CLEAR.Assurance score and an AssuranceSummary on the response event.
func TestAIQG_GatekeeperFindingsStampedReachEventAndAssurance(t *testing.T) {
	em := &events.MemoryEmitter{}
	stampingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StampGatekeeperFindings(r.Context(), GatekeeperDirectionInbound, map[string]int{"medium": 2})
		StampGatekeeperFindings(r.Context(), GatekeeperDirectionOutbound, map[string]int{})
		w.WriteHeader(http.StatusOK)
	})

	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	mw(stampingHandler).ServeHTTP(rec, req)

	if em.Len() != 1 {
		t.Fatalf("emit count=%d want=1", em.Len())
	}
	rd := em.Responses()[0].Data

	if rd.Assurance == nil {
		t.Fatalf("Assurance summary nil")
	}
	if rd.Assurance.InboundFindings["medium"] != 2 {
		t.Errorf("InboundFindings: %v", rd.Assurance.InboundFindings)
	}
	if rd.Assurance.WorstSeverity != "medium" {
		t.Errorf("WorstSeverity=%q want=medium", rd.Assurance.WorstSeverity)
	}
	if rd.CLEAR.Assurance == nil || *rd.CLEAR.Assurance != 80 {
		t.Errorf("CLEAR.Assurance=%v want=80 (medium bucket)", rd.CLEAR.Assurance)
	}
}

// Clean scan emits AssuranceSummary with concrete count=0 (not just
// the omitempty-stripped {}). Dashboards need a slice-able number.
func TestAIQG_CleanScanEmitsExplicitCounts(t *testing.T) {
	em := &events.MemoryEmitter{}
	stampingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StampGatekeeperFindings(r.Context(), GatekeeperDirectionInbound, map[string]int{})
		StampGatekeeperFindings(r.Context(), GatekeeperDirectionOutbound, map[string]int{})
		w.WriteHeader(http.StatusOK)
	})
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	mw(stampingHandler).ServeHTTP(rec, req)

	a := em.Responses()[0].Data.Assurance
	if a == nil {
		t.Fatalf("AssuranceSummary nil")
	}
	if a.InboundCount != 0 || a.OutboundCount != 0 {
		t.Errorf("clean scan counts: in=%d out=%d (want 0/0)", a.InboundCount, a.OutboundCount)
	}
	// JSON marshalling must produce the int fields, even with value 0
	// (omitempty would strip the maps but NOT named int fields per
	// Go's encoding/json semantics for typed int).
	js, _ := json.Marshal(a)
	if !strings.Contains(string(js), `"inbound_count":0`) {
		t.Errorf("inbound_count should always emit, got %s", js)
	}
}

// Counts populate correctly from findings.
func TestAIQG_AssuranceCountsSumFindings(t *testing.T) {
	em := &events.MemoryEmitter{}
	stampingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StampGatekeeperFindings(r.Context(), GatekeeperDirectionInbound, map[string]int{"low": 3, "medium": 2})
		StampGatekeeperFindings(r.Context(), GatekeeperDirectionOutbound, map[string]int{"high": 1})
		w.WriteHeader(http.StatusOK)
	})
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	mw(stampingHandler).ServeHTTP(rec, req)

	a := em.Responses()[0].Data.Assurance
	if a.InboundCount != 5 {
		t.Errorf("InboundCount=%d want=5 (3+2)", a.InboundCount)
	}
	if a.OutboundCount != 1 {
		t.Errorf("OutboundCount=%d want=1", a.OutboundCount)
	}
}

// Clean scan (ScanRan=true, no findings) → Assurance = 100, summary
// emitted with empty maps + no worst severity.
func TestAIQG_CleanScanScoresAssurance100(t *testing.T) {
	em := &events.MemoryEmitter{}
	stampingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StampGatekeeperFindings(r.Context(), GatekeeperDirectionInbound, map[string]int{})
		w.WriteHeader(http.StatusOK)
	})

	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	mw(stampingHandler).ServeHTTP(rec, req)

	rd := em.Responses()[0].Data
	if rd.CLEAR.Assurance == nil || *rd.CLEAR.Assurance != 100 {
		t.Errorf("clean scan Assurance=%v want=100", rd.CLEAR.Assurance)
	}
	if rd.Assurance == nil {
		t.Fatalf("Assurance summary nil despite ScanRan=true")
	}
	if rd.Assurance.WorstSeverity != "" {
		t.Errorf("WorstSeverity=%q want empty on clean scan", rd.Assurance.WorstSeverity)
	}
}

// No-scan path (handler never stamps Gatekeeper findings) → Assurance
// score nil + Assurance summary omitted. Distinct from clean scan.
func TestAIQG_NoScanLeavesAssuranceNil(t *testing.T) {
	em := &events.MemoryEmitter{}
	noStampHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	mw(noStampHandler).ServeHTTP(rec, req)

	rd := em.Responses()[0].Data
	if rd.CLEAR.Assurance != nil {
		t.Errorf("Assurance=%d want nil when no scan ran", *rd.CLEAR.Assurance)
	}
	if rd.Assurance != nil {
		t.Errorf("AssuranceSummary should be omitted when no scan ran")
	}
}

// finish_reason stamped by the handler reaches both ResponseEvent.
// FinishReason and CLEAR.Efficacy.
func TestAIQG_FinishReasonStampedReachesEventAndEfficacy(t *testing.T) {
	em := &events.MemoryEmitter{}
	stampingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StampFinishReason(r.Context(), "stop")
		w.WriteHeader(http.StatusOK)
	})
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	mw(stampingHandler).ServeHTTP(rec, req)

	rd := em.Responses()[0].Data
	if rd.FinishReason != "stop" {
		t.Errorf("FinishReason=%q want=stop", rd.FinishReason)
	}
	if rd.CLEAR.Efficacy == nil || *rd.CLEAR.Efficacy != 100 {
		t.Errorf("CLEAR.Efficacy=%v want=100", rd.CLEAR.Efficacy)
	}
}

// content_filter → Efficacy 0; the request still emits an event with
// status=success (HTTP 200) but Efficacy registers the policy block.
func TestAIQG_ContentFilterScoresEfficacy0(t *testing.T) {
	em := &events.MemoryEmitter{}
	stampingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StampFinishReason(r.Context(), "content_filter")
		w.WriteHeader(http.StatusOK)
	})
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	mw(stampingHandler).ServeHTTP(rec, req)

	rd := em.Responses()[0].Data
	if rd.CLEAR.Efficacy == nil || *rd.CLEAR.Efficacy != 0 {
		t.Errorf("CLEAR.Efficacy=%v want=0", rd.CLEAR.Efficacy)
	}
}

// Retry metadata stamped by the handler reaches CLEAR.Reliability.
// Clean first try → 100. Validates the four-stamp wire-up of the
// final CLEAR dimension.
func TestAIQG_RetryMetadataStampedReachesReliability(t *testing.T) {
	em := &events.MemoryEmitter{}
	stampingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StampRetryMetadata(r.Context(), 1, false) // clean first try
		w.WriteHeader(http.StatusOK)
	})
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	mw(stampingHandler).ServeHTTP(rec, req)

	rd := em.Responses()[0].Data
	if rd.CLEAR.Reliability == nil || *rd.CLEAR.Reliability != 100 {
		t.Errorf("Reliability=%v want=100", rd.CLEAR.Reliability)
	}
}

// Retry + fallback degrades Reliability.
func TestAIQG_RetryWithFallbackDegradesReliability(t *testing.T) {
	em := &events.MemoryEmitter{}
	stampingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StampRetryMetadata(r.Context(), 2, true) // 1 retry + fallback = 50
		w.WriteHeader(http.StatusOK)
	})
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	mw(stampingHandler).ServeHTTP(rec, req)

	rd := em.Responses()[0].Data
	if rd.CLEAR.Reliability == nil || *rd.CLEAR.Reliability != 50 {
		t.Errorf("Reliability=%v want=50", rd.CLEAR.Reliability)
	}
}

// Vendor/Model stamps made by the downstream handler must reach the
// emitted event. Proves the Routing sidecar (attached by middleware) +
// the deferred snapshot read both work end-to-end.
func TestAIQG_VendorModelStampedReachEmittedEvent(t *testing.T) {
	em := &events.MemoryEmitter{}
	stampingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StampVendor(r.Context(), "anthropic")
		StampModel(r.Context(), "claude-3-7-sonnet-20250219")
		StampStreaming(r.Context(), true)
		w.WriteHeader(http.StatusOK)
	})

	mw := NewAIQG(AIQGConfig{
		Strict:  true,
		Logger:  silentLogger(),
		Emitter: em,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk-customer")
	rec := httptest.NewRecorder()
	mw(stampingHandler).ServeHTTP(rec, req)

	if em.Len() != 1 {
		t.Fatalf("emit count=%d want=1", em.Len())
	}
	got := em.Requests()[0].Data
	if got.Vendor != "anthropic" {
		t.Errorf("Vendor=%q want=anthropic", got.Vendor)
	}
	if got.Model != "claude-3-7-sonnet-20250219" {
		t.Errorf("Model=%q want=claude-3-7-sonnet-20250219", got.Model)
	}
	if !got.Streaming {
		t.Errorf("Streaming=false despite explicit stamp")
	}
}

// The emitted response event must carry a populated CLEAR scores
// block — at MVP, that means Latency + Composite are non-nil and the
// other dimensions are nil. End-to-end check of the
// middleware → events.Build → clear.Compute chain.
func TestAIQG_EmittedEventCarriesCLEARScores(t *testing.T) {
	em := &events.MemoryEmitter{}
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{
		Strict:  true,
		Logger:  silentLogger(),
		Emitter: em,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk-customer")
	req.Header.Set("TAS-Workflow", "rag")
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if em.Len() != 1 {
		t.Fatalf("emit count=%d want=1", em.Len())
	}
	respEnv := em.Responses()[0]
	if respEnv.Data.CLEAR == nil {
		t.Fatalf("ResponseEvent.CLEAR is nil — scorer not wired into Build")
	}
	if respEnv.Data.CLEAR.Latency == nil {
		t.Errorf("CLEAR.Latency nil — should be scored from end_to_end_ms")
	}
	if respEnv.Data.CLEAR.Composite == nil {
		t.Errorf("CLEAR.Composite nil — should equal Latency when only Latency is scored")
	}
	// This handler doesn't stamp vendor/model/usage, so the Cost path
	// stays nil here even though it's wired. See
	// TestAIQG_TokenUsageStampedReachesEventAndCost for the populated case.
	if respEnv.Data.CLEAR.Cost != nil {
		t.Errorf("CLEAR.Cost should be nil when no usage stamped, got %d", *respEnv.Data.CLEAR.Cost)
	}
	if respEnv.Data.CLEAR.Efficacy != nil {
		t.Errorf("CLEAR.Efficacy should be nil when no finish_reason stamped, got %d", *respEnv.Data.CLEAR.Efficacy)
	}
	if respEnv.Data.CLEAR.Assurance != nil {
		t.Errorf("CLEAR.Assurance should be nil when no scan stamped, got %d", *respEnv.Data.CLEAR.Assurance)
	}
	if respEnv.Data.CLEAR.Reliability != nil {
		t.Errorf("CLEAR.Reliability should be nil when no retry metadata stamped, got %d", *respEnv.Data.CLEAR.Reliability)
	}
	if respEnv.Data.ScoringVersion == "" {
		t.Errorf("ScoringVersion must be present even with partial scoring")
	}
}

// Nil emitter must default to NoopEmitter (no panic, request succeeds).
func TestAIQG_NilEmitterDefaultsToNoop(t *testing.T) {
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: nil})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", "tas_qg_live_abc")
	req.Header.Set("Authorization", "Bearer sk-customer")
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("nil emitter caused %d", rec.Code)
	}
	if !next.called {
		t.Fatalf("downstream not called")
	}
}

// Pass-through requests (no TAS-Auth, permissive mode) must NOT emit
// events — internal-routing traffic stays out of the AIQG event stream.
func TestAIQG_PassThroughDoesNotEmit(t *testing.T) {
	em := &events.MemoryEmitter{}
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{Strict: false, Logger: silentLogger(), Emitter: em})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if em.Len() != 0 {
		t.Errorf("internal pass-through emitted %d events (want 0)", em.Len())
	}
}

// 401/400 rejections still need to be observable elsewhere, but they
// MUST NOT produce a paired AIQG event — the spec (request-event.md §273)
// says pre-validation auth failures emit no events.
func TestAIQG_StrictRejectionDoesNotEmit(t *testing.T) {
	em := &events.MemoryEmitter{}
	next := &echoHandler{}
	mw := NewAIQG(AIQGConfig{Strict: true, Logger: silentLogger(), Emitter: em})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if em.Len() != 0 {
		t.Errorf("pre-validation auth failure emitted %d events (want 0 per spec §273)", em.Len())
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
