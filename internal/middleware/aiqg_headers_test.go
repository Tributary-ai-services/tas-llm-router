package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newReq(t *testing.T, headers map[string]string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://gateway.aiqg.tas.io/v1/chat/completions", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func TestParseHeaders_AllFields(t *testing.T) {
	req := newReq(t, map[string]string{
		"TAS-Auth":                   "tas_qg_live_abc123",
		"TAS-Policy-Bundle":          "prod-strict",
		"TAS-Workflow":               "rag",
		"TAS-Upstream-Authorization": "Bearer sk-real-vendor-key",
		"TAS-Trace":                  "1",
		"TAS-Dry-Run":                "1",
		"TAS-Source-App":             "billing-api-prod",
	})

	h, err := ParseHeaders(req)
	if err != nil {
		t.Fatalf("ParseHeaders err: %v", err)
	}
	if h.Auth != "tas_qg_live_abc123" {
		t.Errorf("Auth=%q", h.Auth)
	}
	if h.PolicyBundle != "prod-strict" {
		t.Errorf("PolicyBundle=%q", h.PolicyBundle)
	}
	if h.Workflow != "rag" {
		t.Errorf("Workflow=%q", h.Workflow)
	}
	if h.UpstreamAuthorization != "Bearer sk-real-vendor-key" {
		t.Errorf("UpstreamAuthorization=%q", h.UpstreamAuthorization)
	}
	if !h.Trace {
		t.Errorf("Trace not set")
	}
	if !h.DryRun {
		t.Errorf("DryRun not set")
	}
	if h.SourceApp != "billing-api-prod" {
		t.Errorf("SourceApp=%q", h.SourceApp)
	}
	if len(h.Policy) != 0 {
		t.Errorf("Policy should be empty when only PolicyBundle set, got %v", h.Policy)
	}
	if h.IsZero() {
		t.Errorf("IsZero=true with all fields populated")
	}
}

func TestParseHeaders_EmptyRequest(t *testing.T) {
	req := newReq(t, nil)
	h, err := ParseHeaders(req)
	if err != nil {
		t.Fatalf("ParseHeaders on empty: %v", err)
	}
	if !h.IsZero() {
		t.Errorf("IsZero=false with no headers, got %#v", h)
	}
}

func TestParseHeaders_PolicySplit(t *testing.T) {
	req := newReq(t, map[string]string{
		"TAS-Policy": "pii_redact, prompt_injection,   no-cot ",
	})
	h, err := ParseHeaders(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := []string{"pii_redact", "prompt_injection", "no-cot"}
	if len(h.Policy) != len(want) {
		t.Fatalf("len(Policy)=%d want=%d (%v)", len(h.Policy), len(want), h.Policy)
	}
	for i, v := range want {
		if h.Policy[i] != v {
			t.Errorf("Policy[%d]=%q want=%q", i, h.Policy[i], v)
		}
	}
}

func TestParseHeaders_PolicyAndBundleConflict(t *testing.T) {
	req := newReq(t, map[string]string{
		"TAS-Policy":        "pii_redact",
		"TAS-Policy-Bundle": "prod-strict",
	})
	h, err := ParseHeaders(req)
	if !errors.Is(err, ErrPolicyConflict) {
		t.Fatalf("expected ErrPolicyConflict, got %v", err)
	}
	// The returned struct should still be populated so callers can log
	// both values when constructing the 400 response.
	if h.PolicyBundle != "prod-strict" || len(h.Policy) != 1 {
		t.Errorf("expected both populated, got %#v", h)
	}
}

func TestParseHeaders_AuthMalformed(t *testing.T) {
	req := newReq(t, map[string]string{
		"TAS-Auth": "wrong-prefix-token",
	})
	h, err := ParseHeaders(req)
	if !errors.Is(err, ErrAuthMalformed) {
		t.Fatalf("expected ErrAuthMalformed, got %v", err)
	}
	if h.Auth != "wrong-prefix-token" {
		t.Errorf("Auth should be preserved for logging, got %q", h.Auth)
	}
}

func TestParseHeaders_AuthAccepted(t *testing.T) {
	req := newReq(t, map[string]string{
		"TAS-Auth": "tas_qg_live_abcdef0123456789",
	})
	h, err := ParseHeaders(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if h.Auth == "" {
		t.Errorf("Auth not parsed")
	}
}

func TestParseHeaders_SourceAppTooLong(t *testing.T) {
	long := strings.Repeat("a", maxSourceAppLen+1)
	req := newReq(t, map[string]string{"TAS-Source-App": long})
	_, err := ParseHeaders(req)
	if !errors.Is(err, ErrSourceAppTooLong) {
		t.Fatalf("expected ErrSourceAppTooLong, got %v", err)
	}

	atLimit := strings.Repeat("a", maxSourceAppLen)
	req = newReq(t, map[string]string{"TAS-Source-App": atLimit})
	h, err := ParseHeaders(req)
	if err != nil {
		t.Fatalf("at-limit length should pass: %v", err)
	}
	if h.SourceApp != atLimit {
		t.Errorf("SourceApp not preserved")
	}
}

func TestParseHeaders_WorkflowEnum(t *testing.T) {
	tests := []struct {
		name     string
		val      string
		wantSet  bool
		wantSoft bool
	}{
		{"valid_rag", "rag", true, false},
		{"valid_agentic", "agentic", true, false},
		{"valid_single_turn_qa", "single_turn_qa", true, false},
		{"valid_summarization", "summarization", true, false},
		{"valid_code_generation", "code_generation", true, false},
		{"valid_classification_extraction", "classification_extraction", true, false},
		// `unknown` is intentionally NOT accepted — customer should omit
		// the header rather than assert ignorance.
		{"reject_unknown", "unknown", false, true},
		{"reject_misspelled", "ragg", false, true},
		{"reject_empty_unset", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdrs := map[string]string{}
			if tt.val != "" {
				hdrs["TAS-Workflow"] = tt.val
			}
			req := newReq(t, hdrs)
			h, err := ParseHeaders(req)
			gotSoft := errors.Is(err, ErrUnknownWorkflowSilent)
			if gotSoft != tt.wantSoft {
				t.Errorf("ErrUnknownWorkflowSilent=%v want=%v (err=%v)", gotSoft, tt.wantSoft, err)
			}
			if err != nil && !tt.wantSoft {
				t.Errorf("unexpected hard error: %v", err)
			}
			gotSet := h.Workflow != ""
			if gotSet != tt.wantSet {
				t.Errorf("Workflow set=%v want=%v (val=%q)", gotSet, tt.wantSet, h.Workflow)
			}
			// On soft error the field MUST be cleared so downstream code
			// never trusts an unvalidated value.
			if tt.wantSoft && h.Workflow != "" {
				t.Errorf("invalid workflow not cleared: %q", h.Workflow)
			}
		})
	}
}

func TestParseHeaders_BoolNotOne(t *testing.T) {
	// Only literal "1" turns the bool on — "true", "yes", " 1" must not.
	tests := []struct {
		name     string
		hdr      string
		val      string
		readBack func(AIQGHeaders) bool
	}{
		{"trace_true_string", "TAS-Trace", "true", func(h AIQGHeaders) bool { return h.Trace }},
		{"trace_yes", "TAS-Trace", "yes", func(h AIQGHeaders) bool { return h.Trace }},
		{"trace_padded_one", "TAS-Trace", " 1", func(h AIQGHeaders) bool { return h.Trace }},
		{"dry_run_zero", "TAS-Dry-Run", "0", func(h AIQGHeaders) bool { return h.DryRun }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newReq(t, map[string]string{tt.hdr: tt.val})
			h, err := ParseHeaders(req)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if tt.readBack(h) {
				t.Errorf("bool=true for non-canonical %q=%q", tt.hdr, tt.val)
			}
		})
	}
}

func TestParseHeaders_TrimsAuthAndBundle(t *testing.T) {
	req := newReq(t, map[string]string{
		"TAS-Auth":          "  tas_qg_live_xyz  ",
		"TAS-Policy-Bundle": "  prod-strict  ",
		"TAS-Workflow":      "  rag  ",
		"TAS-Source-App":    "  billing  ",
	})
	h, err := ParseHeaders(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if h.Auth != "tas_qg_live_xyz" {
		t.Errorf("Auth not trimmed: %q", h.Auth)
	}
	if h.PolicyBundle != "prod-strict" {
		t.Errorf("PolicyBundle not trimmed: %q", h.PolicyBundle)
	}
	if h.Workflow != "rag" {
		t.Errorf("Workflow not trimmed: %q", h.Workflow)
	}
	if h.SourceApp != "billing" {
		t.Errorf("SourceApp not trimmed: %q", h.SourceApp)
	}
}

func TestParseHeaders_PolicyEmptyStringsDropped(t *testing.T) {
	req := newReq(t, map[string]string{
		"TAS-Policy": "a,,b,  ,c",
	})
	h, err := ParseHeaders(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := []string{"a", "b", "c"}
	if len(h.Policy) != len(want) {
		t.Fatalf("len=%d want=%d (%v)", len(h.Policy), len(want), h.Policy)
	}
}

func TestStripFromOutbound(t *testing.T) {
	req := newReq(t, map[string]string{
		"TAS-Auth":                   "tas_qg_live_abc",
		"TAS-Policy":                 "pii_redact",
		"TAS-Policy-Bundle":          "prod-strict",
		"TAS-Workflow":               "rag",
		"TAS-Upstream-Authorization": "Bearer sk-real",
		"TAS-Trace":                  "1",
		"TAS-Dry-Run":                "1",
		"TAS-Source-App":             "billing",
		// Non-TAS headers must survive.
		"Authorization": "Bearer sk-customer",
		"Content-Type":  "application/json",
		"X-Request-ID":  "req-123",
	})

	StripFromOutbound(req)

	for _, name := range canonicalHeaderNames {
		if v := req.Header.Get(name); v != "" {
			t.Errorf("%s not stripped (=%q)", name, v)
		}
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-customer" {
		t.Errorf("Authorization wiped: %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type wiped: %q", got)
	}
	if got := req.Header.Get("X-Request-ID"); got != "req-123" {
		t.Errorf("X-Request-ID wiped: %q", got)
	}
}

func TestStripFromOutbound_Idempotent(t *testing.T) {
	req := newReq(t, map[string]string{"TAS-Auth": "tas_qg_live_x"})
	StripFromOutbound(req)
	// Second call must not panic and must leave the request alone.
	StripFromOutbound(req)
	if v := req.Header.Get("TAS-Auth"); v != "" {
		t.Errorf("TAS-Auth survived second strip: %q", v)
	}
}

func TestStripFromOutbound_NoTASHeaders(t *testing.T) {
	req := newReq(t, map[string]string{"Authorization": "Bearer sk"})
	StripFromOutbound(req)
	if v := req.Header.Get("Authorization"); v != "Bearer sk" {
		t.Errorf("Authorization wiped on no-op strip: %q", v)
	}
}

func TestContextRoundTrip(t *testing.T) {
	src := AIQGHeaders{
		Auth:         "tas_qg_live_abc",
		PolicyBundle: "prod",
		Workflow:     "rag",
		Trace:        true,
	}
	ctx := WithHeaders(context.Background(), src)
	got, ok := FromContext(ctx)
	if !ok {
		t.Fatalf("FromContext returned ok=false after WithHeaders")
	}
	if got.Auth != src.Auth || got.PolicyBundle != src.PolicyBundle ||
		got.Workflow != src.Workflow || got.Trace != src.Trace {
		t.Errorf("round trip mismatch: got=%#v want=%#v", got, src)
	}
}

func TestContextMissing(t *testing.T) {
	_, ok := FromContext(context.Background())
	if ok {
		t.Errorf("FromContext returned ok=true on bare context")
	}
	var nilCtx context.Context
	_, ok = FromContext(nilCtx)
	if ok {
		t.Errorf("FromContext returned ok=true on nil context")
	}
}

func TestIsZero(t *testing.T) {
	if !(AIQGHeaders{}).IsZero() {
		t.Errorf("zero value not IsZero")
	}
	if (AIQGHeaders{Trace: true}).IsZero() {
		t.Errorf("Trace-only IsZero=true")
	}
	if (AIQGHeaders{Policy: []string{"a"}}).IsZero() {
		t.Errorf("Policy-only IsZero=true")
	}
	if (AIQGHeaders{SourceApp: "x"}).IsZero() {
		t.Errorf("SourceApp-only IsZero=true")
	}
}

// Canonical header list must not accidentally lose entries during refactors.
// If you legitimately add a header, update this test alongside the slice.
func TestCanonicalHeaderListExhaustive(t *testing.T) {
	expected := map[string]struct{}{
		"TAS-Auth":                   {},
		"TAS-Policy":                 {},
		"TAS-Policy-Bundle":          {},
		"TAS-Workflow":               {},
		"TAS-Upstream-Authorization": {},
		"TAS-Trace":                  {},
		"TAS-Dry-Run":                {},
		"TAS-Source-App":             {},
		// Identity headers (stripped before vendor).
		"TAS-Agent-Id":        {},
		"TAS-Agent-Name":      {},
		"TAS-Agent-Version":   {},
		"TAS-Flow-Id":         {},
		"TAS-Conversation-Id": {},
		"baggage":             {},
	}
	if len(canonicalHeaderNames) != len(expected) {
		t.Fatalf("canonicalHeaderNames len=%d want=%d", len(canonicalHeaderNames), len(expected))
	}
	for _, n := range canonicalHeaderNames {
		if _, ok := expected[n]; !ok {
			t.Errorf("unexpected header in canonical list: %q", n)
		}
	}
}

func TestParseHeaders_Identity(t *testing.T) {
	req := newReq(t, map[string]string{
		"TAS-Agent-Id":        "agent-7",
		"TAS-Agent-Name":      "Planner",
		"TAS-Conversation-Id": "conv-9",
		"traceparent":         "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"baggage":             "user.id=u_42,session.id=s_99,account.id=acct_1",
	})
	h, err := ParseHeaders(req)
	if err != nil {
		t.Fatalf("ParseHeaders: %v", err)
	}
	if h.AgentID != "agent-7" || h.AgentName != "Planner" {
		t.Errorf("agent fields: %+v", h)
	}
	if h.ConversationID != "conv-9" {
		t.Errorf("conversation: %q", h.ConversationID)
	}
	if h.TraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("trace id: %q", h.TraceID)
	}
	if h.Baggage["user.id"] != "u_42" || h.Baggage["session.id"] != "s_99" || h.Baggage["account.id"] != "acct_1" {
		t.Errorf("baggage: %+v", h.Baggage)
	}
}

func TestParseBaggage_Malformed(t *testing.T) {
	// Empty, junk, and ;-properties must not panic and parse defensively.
	if parseBaggage("") != nil {
		t.Errorf("empty baggage should be nil")
	}
	got := parseBaggage("user.id=u1;meta=x, =bad, novalue=, k=v")
	if got["user.id"] != "u1" || got["k"] != "v" {
		t.Errorf("baggage parse: %+v", got)
	}
	if _, ok := got["novalue"]; ok {
		t.Errorf("empty-value pair should be dropped: %+v", got)
	}
}

func TestStripFromOutbound_IdentityHeaders(t *testing.T) {
	req := newReq(t, map[string]string{
		"baggage":      "user.id=u_42",
		"TAS-Agent-Id": "agent-7",
		"traceparent":  "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
	})
	StripFromOutbound(req)
	if req.Header.Get("baggage") != "" {
		t.Errorf("baggage must be stripped before vendor")
	}
	if req.Header.Get("TAS-Agent-Id") != "" {
		t.Errorf("TAS-Agent-Id must be stripped before vendor")
	}
	if req.Header.Get("traceparent") == "" {
		t.Errorf("traceparent must NOT be stripped (standard header)")
	}
}
