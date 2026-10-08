package events

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

// The response event's JSON keys are a contract: dashboard-be's Loki queries
// and the Spark aggregator read them by name. These three are new in AIQG-50.
func TestResponseEvent_VendorErrorJSONContract(t *testing.T) {
	d := ResponseEvent{
		Status:         StatusVendorError,
		HTTPStatus:     400,
		UpstreamStatus: 400,
		ErrorType:      "invalid_request_error",
		ErrorMessage:   "streaming is required for operations that may take longer than 10 minutes",
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"upstream_status":400`, `"error_type":"invalid_request_error"`, `"error_message":"streaming is required`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("missing %s in %s", key, b)
		}
	}
}

// All three are omitempty, so a successful response is byte-identical to before
// this change -- the additive guarantee every consumer of this event relies on.
func TestResponseEvent_VendorErrorFieldsAbsentOnSuccess(t *testing.T) {
	b, err := json.Marshal(ResponseEvent{Status: StatusSuccess, HTTPStatus: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"upstream_status", "error_type", "error_message"} {
		if strings.Contains(string(b), key) {
			t.Errorf("%s present on a success event: %s", key, b)
		}
	}
}

// UpstreamStatus is deliberately SEPARATE from HTTPStatus. The AIQG-50 case is a
// vendor 400 that the gateway used to return as 500; keeping both lets a reader
// see the translation rather than guess at it. A test, because collapsing them
// into one field is the obvious "simplification".
func TestResponseEvent_UpstreamStatusIsNotTheReturnedStatus(t *testing.T) {
	b, _ := json.Marshal(ResponseEvent{
		Status: StatusVendorError, HTTPStatus: 502, UpstreamStatus: 529,
	})
	s := string(b)
	if !strings.Contains(s, `"http_status":502`) || !strings.Contains(s, `"upstream_status":529`) {
		t.Errorf("the two statuses did not both survive: %s", s)
	}
}

// A FAILURE must be logged at error level. This event was emitted at Info
// regardless of outcome, so `| level="ERROR"` -- the first query anyone runs
// during an incident -- missed every vendor failure the gateway had ever
// recorded. That is the half of AIQG-50 with the most operational cost, and the
// one testable as a true before/after because the API did not change.
func TestEmit_FailureStatusLogsAtErrorLevel(t *testing.T) {
	cases := map[string]logrus.Level{
		StatusVendorError:      logrus.ErrorLevel,
		StatusGatewayError:     logrus.ErrorLevel,
		StatusTimeout:          logrus.ErrorLevel,
		StatusSuccess:          logrus.InfoLevel,
		StatusPolicyBlocked:    logrus.InfoLevel, // an outcome we produced on purpose
		StatusClientDisconnect: logrus.InfoLevel,
	}
	for status, want := range cases {
		logger, hook := test.NewNullLogger()
		logger.SetLevel(logrus.DebugLevel)
		em := &LogEmitter{Logger: logger}

		resp := ResponseEnvelope{Data: ResponseEvent{Status: status, HTTPStatus: 200}}
		if err := em.Emit(context.Background(), RequestEnvelope{}, resp); err != nil {
			t.Fatalf("%s: Emit: %v", status, err)
		}
		var got *logrus.Entry
		for _, e := range hook.AllEntries() {
			if e.Message == "aiqg response event" {
				got = e
			}
		}
		if got == nil {
			t.Fatalf("%s: no response event logged", status)
		}
		if got.Level != want {
			t.Errorf("status %q logged at %v, want %v", status, got.Level, want)
		}
	}
}
