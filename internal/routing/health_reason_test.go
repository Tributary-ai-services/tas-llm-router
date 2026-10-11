package routing

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// OPS-55: the 2026-10-05 outage. Every Claude path in TAS went down because our
// own prepaid account ran out of credit, and the only thing anyone was told was
// `provider anthropic is not healthy` -- which reads as the vendor being down.
// This is the error text the vendor actually returned that day.
func TestClassifyHealthFailure_TheRealOutageMessage(t *testing.T) {
	err := errors.New(`POST "https://api.anthropic.com/v1/messages": 400 Bad Request ` +
		`{"type":"error","error":{"type":"invalid_request_error",` +
		`"message":"Your credit balance is too low to access the Anthropic API. ` +
		`Please go to Plans & Billing to upgrade or purchase credits."}}`)
	if got := classifyHealthFailure(err); got != ReasonCreditExhausted {
		t.Errorf("the credit-exhaustion message classified as %q, want %q", got, ReasonCreditExhausted)
	}
}

// A spend limit is the OTHER way this account stops serving, and it arrives as
// a 400 rather than a 401 -- so it must be checked before the auth cases or it
// would land there.
func TestClassifyHealthFailure_SpendLimitIsNotAuth(t *testing.T) {
	err := errors.New("400 Bad Request: this request would exceed your organization's spend limit")
	if got := classifyHealthFailure(err); got != ReasonCreditExhausted {
		t.Errorf("spend limit classified as %q, want %q", got, ReasonCreditExhausted)
	}
}

func TestClassifyHealthFailure_Taxonomy(t *testing.T) {
	cases := []struct {
		err  string
		want string
	}{
		{"401 Unauthorized: invalid x-api-key", ReasonAuth},
		{"403 Forbidden", ReasonAuth},
		{"429 Too Many Requests: rate limit exceeded", ReasonRateLimited},
		{"529 overloaded_error", ReasonRateLimited},
		{"500 Internal Server Error", ReasonVendorUnavailable},
		{"503 Service Unavailable", ReasonVendorUnavailable},
		{`dial tcp 1.2.3.4:443: connection refused`, ReasonNetwork},
		{"context deadline exceeded", ReasonNetwork},
		{"no such host", ReasonNetwork},
		{"something nobody has seen before", ReasonOther},
	}
	for _, c := range cases {
		if got := classifyHealthFailure(errors.New(c.err)); got != c.want {
			t.Errorf("%q classified as %q, want %q", c.err, got, c.want)
		}
	}
}

func TestClassifyHealthFailure_NilIsHealthy(t *testing.T) {
	if got := classifyHealthFailure(nil); got != ReasonHealthy {
		t.Errorf("nil classified as %q, want empty", got)
	}
}

// The caller-facing text must NOT carry the vendor's raw message: it holds
// request ids, workspace ids and our account's billing state, and this message
// can reach the public edge.
func TestHumanHealthReason_LeaksNoVendorDetail(t *testing.T) {
	for _, reason := range []string{
		ReasonCreditExhausted, ReasonAuth, ReasonRateLimited,
		ReasonVendorUnavailable, ReasonNetwork, ReasonOther,
	} {
		msg := humanHealthReason(reason)
		if msg == "" {
			t.Errorf("reason %q has no caller-facing text", reason)
		}
		for _, leak := range []string{"api.anthropic.com", "req_", "wrkspc_", "x-api-key", "sk-"} {
			if strings.Contains(msg, leak) {
				t.Errorf("reason %q leaks %q: %s", reason, leak, msg)
			}
		}
	}
}

// Credit exhaustion is the one an operator must be able to act on immediately,
// so its wording has to say whose fault it is -- ours, not the vendor's. That
// is the entire point of OPS-55.
func TestHumanHealthReason_CreditSaysItIsOurAccount(t *testing.T) {
	msg := strings.ToLower(humanHealthReason(ReasonCreditExhausted))
	if !strings.Contains(msg, "credit") && !strings.Contains(msg, "spend limit") {
		t.Errorf("the credit reason does not name the cause: %s", msg)
	}
	if !strings.Contains(msg, "upstream account") {
		t.Errorf("the credit reason does not make clear it is OUR account: %s", msg)
	}
}

// The 503 a caller receives must name the reason. Before OPS-55 it was the bare
// "provider X is not healthy" whatever the cause, which is what sent the first
// ten minutes of that incident to the wrong system.
func TestUnhealthyError_NamesTheClassifiedReason(t *testing.T) {
	r := NewRouter(logrus.New())
	r.healthMu.Lock()
	r.healthStatus["anthropic"] = &types.HealthStatus{
		Status:       "unhealthy",
		Reason:       ReasonCreditExhausted,
		ErrorMessage: `400 {"message":"Your credit balance is too low"} req_011Cfq`,
	}
	r.healthMu.Unlock()

	msg := r.unhealthyError("anthropic").Error()
	if !strings.Contains(msg, ReasonCreditExhausted) {
		t.Errorf("the 503 does not name the reason: %s", msg)
	}
	if !strings.Contains(msg, "credit") {
		t.Errorf("the 503 does not explain the reason: %s", msg)
	}
	// And it must not relay the vendor's raw text to the caller.
	for _, leak := range []string{"req_011Cfq", "Your credit balance is too low"} {
		if strings.Contains(msg, leak) {
			t.Errorf("the 503 leaks vendor detail %q: %s", leak, msg)
		}
	}
}

// An unknown provider, or one with no recorded reason, must still produce the
// old message rather than something malformed.
func TestUnhealthyError_FallsBackCleanly(t *testing.T) {
	r := NewRouter(logrus.New())
	if msg := r.unhealthyError("nosuch").Error(); msg != "provider nosuch is not healthy" {
		t.Errorf("unexpected fallback message: %s", msg)
	}
}

// GetHealthStatus copies HealthStatus field by field, so a field added to the
// type and not to the copy reads as its zero value to every caller. That is
// exactly how OPS-55 shipped broken: the prober logged reason="auth" while
// llm_router_provider_unhealthy_reason published reason="other", because the
// metric reads this copy.
//
// Reflection rather than a hand-written assertion per field: the point is to
// fail when SOMEONE ELSE adds a field, which a hand-written list cannot do.
func TestGetHealthStatus_CopiesEveryField(t *testing.T) {
	r := NewRouter(logrus.New())

	// Fill every field with a distinctive non-zero value.
	src := &types.HealthStatus{}
	v := reflect.ValueOf(src).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("probe-" + v.Type().Field(i).Name)
		case reflect.Int64, reflect.Int:
			f.SetInt(int64(100 + i))
		default:
			t.Fatalf("HealthStatus.%s has unhandled kind %s; extend this test",
				v.Type().Field(i).Name, f.Kind())
		}
	}
	r.healthMu.Lock()
	r.healthStatus["probe"] = src
	r.healthMu.Unlock()

	got := r.GetHealthStatus()["probe"]
	if got == nil {
		t.Fatal("GetHealthStatus dropped the entry entirely")
	}
	gv := reflect.ValueOf(got).Elem()
	for i := 0; i < gv.NumField(); i++ {
		name := gv.Type().Field(i).Name
		if !reflect.DeepEqual(gv.Field(i).Interface(), v.Field(i).Interface()) {
			t.Errorf("HealthStatus.%s was not copied: got %v, want %v — add it to "+
				"GetHealthStatus, or every caller reads its zero value",
				name, gv.Field(i).Interface(), v.Field(i).Interface())
		}
	}
}

// The verbatim message Anthropic returned on 2026-10-11, as a 400
// invalid_request_error, when the account's configured API usage limit was
// reached. It was classified `other` -- so the caller got "the last upstream
// health check failed" and the credit alert could not fire -- which is the
// whole failure this classifier exists to prevent.
//
// Kept verbatim rather than paraphrased: the bug was that we matched the words
// we expected instead of the words the vendor sends.
func TestClassifyHealthFailure_AnthropicSpecifiedUsageLimit(t *testing.T) {
	err := errors.New(`anthropic health check failed: POST "https://api.anthropic.com/v1/messages": ` +
		`400 Bad Request (Request-ID: req_011CfvjQR4hpWpXvNYVwDMcA) ` +
		`{"type":"error","error":{"type":"invalid_request_error","message":` +
		`"You have reached your specified API usage limits. You will regain access on 2026-11-01 at 00:00 UTC."},` +
		`"request_id":"req_011CfvjQR4hpWpXvNYVwDMcA"}`)
	if got := classifyHealthFailure(err); got != ReasonCreditExhausted {
		t.Errorf("classifyHealthFailure = %q, want %q\n"+
			"A spend cap is the one failure mode that needs a human in a billing console, so "+
			"classifying it %q sends whoever is paged looking for an outage that is not there.",
			got, ReasonCreditExhausted, got)
	}
	// And the caller must be told something that points at billing.
	if msg := humanHealthReason(classifyHealthFailure(err)); !strings.Contains(msg, "spend limit") {
		t.Errorf("humanHealthReason = %q, want it to mention a spend limit", msg)
	}
}
