package middleware

import (
	"context"
	"testing"
)

// AIQG-50's first two defects: the response event carried no error detail, and
// the one log line that named the cause had no request_event_id, flow_id or
// model, so it could not be joined to the event it explained. Putting the
// vendor's verdict on the sidecar makes it attributable by construction.
func TestStampVendorError_ReachesTheSnapshot(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	if s := r.Snapshot(); s.VendorErrorStatus != 0 || s.VendorErrorMessage != "" {
		t.Fatalf("snapshot carried a vendor error before stamping: %#v", s)
	}
	StampVendorError(ctx, 400, "invalid_request_error",
		"streaming is required for operations that may take longer than 10 minutes")

	s := r.Snapshot()
	if s.VendorErrorStatus != 400 {
		t.Errorf("VendorErrorStatus = %d, want 400", s.VendorErrorStatus)
	}
	if s.VendorErrorType != "invalid_request_error" {
		t.Errorf("VendorErrorType = %q", s.VendorErrorType)
	}
	if s.VendorErrorMessage == "" {
		t.Errorf("the vendor's reason was dropped -- the whole point of AIQG-50")
	}
}

// First-write-wins: the first refusal is the one that ended the request, and a
// later generic failure must not overwrite the specific reason.
func TestStampVendorError_FirstRefusalWins(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)
	StampVendorError(ctx, 400, "invalid_request_error", "streaming is required")
	StampVendorError(ctx, 503, "overloaded_error", "overloaded")

	s := r.Snapshot()
	if s.VendorErrorStatus != 400 || s.VendorErrorType != "invalid_request_error" {
		t.Errorf("a later refusal overwrote the first: %#v", s)
	}
}

func TestStampVendorError_NilContextIsSafe(t *testing.T) {
	StampVendorError(context.Background(), 500, "x", "y") // no routing sidecar
}
