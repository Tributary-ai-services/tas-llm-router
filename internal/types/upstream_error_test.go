package types

import (
	"errors"
	"fmt"
	"testing"
)

// AIQG-50's third defect: a deterministic vendor 400 was returned as
// "HTTP 500 ... usually temporary -- try again in a moment". Retrying an
// identical invalid_request fails identically, and that advice invites the
// retry loop that was observed burning $1.68 over 11 attempts. Retryable() is
// what stops the gateway giving that advice.
func TestUpstreamError_Retryable(t *testing.T) {
	cases := map[int]bool{
		400: false, // invalid_request -- deterministic, the AIQG-50 case
		401: false,
		403: false,
		404: false,
		413: false,
		422: false,
		408: true, // the two 4xx that mean "later", not "never"
		429: true,
		500: true,
		502: true,
		503: true,
		529: true,
	}
	for status, want := range cases {
		ue := &UpstreamError{Status: status, Message: "x"}
		if got := ue.Retryable(); got != want {
			t.Errorf("status %d: Retryable() = %v, want %v", status, got, want)
		}
	}
}

// The error must survive errors.As through a wrap, because the server classifies
// it several frames above the provider that built it.
func TestUpstreamError_UnwrapsThroughAWrap(t *testing.T) {
	inner := errors.New("dial tcp: refused")
	ue := &UpstreamError{Status: 400, Type: "invalid_request_error", Message: "streaming is required", Err: inner}
	wrapped := fmt.Errorf("completion failed: %w", ue)

	var got *UpstreamError
	if !errors.As(wrapped, &got) {
		t.Fatalf("errors.As failed through a wrap")
	}
	if got.Status != 400 || got.Type != "invalid_request_error" {
		t.Errorf("detail lost: %#v", got)
	}
	if !errors.Is(wrapped, inner) {
		t.Errorf("the underlying error was not reachable")
	}
}

// A nil receiver must not panic: the classification path reaches this on any
// error, including ones that are not upstream failures at all.
func TestUpstreamError_NilIsSafe(t *testing.T) {
	var ue *UpstreamError
	if ue.Retryable() {
		t.Errorf("nil UpstreamError reported retryable")
	}
	if ue.Error() != "" {
		t.Errorf("nil UpstreamError produced a message")
	}
}

func TestUpstreamError_MessageNamesTheVendorVerdict(t *testing.T) {
	withType := (&UpstreamError{Status: 400, Type: "invalid_request_error", Message: "streaming is required"}).Error()
	if withType != "upstream 400 invalid_request_error: streaming is required" {
		t.Errorf("unexpected message: %q", withType)
	}
	noType := (&UpstreamError{Status: 503, Message: "overloaded"}).Error()
	if noType != "upstream 503: overloaded" {
		t.Errorf("unexpected message: %q", noType)
	}
}
