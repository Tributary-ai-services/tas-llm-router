package instrumentation

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ClientTrace returns nil when no collector is in context — Attach must
// then leave ctx unchanged. This is how non-AIQG requests stay zero-cost.
func TestClientTraceNoCollectorReturnsNil(t *testing.T) {
	if got := ClientTrace(context.Background()); got != nil {
		t.Fatalf("ClientTrace returned non-nil with no collector: %#v", got)
	}
	ctx := context.Background()
	if got := Attach(ctx); got != ctx {
		t.Fatalf("Attach should return ctx unchanged when no collector attached")
	}
}

// Attaching the trace to an actual outbound request must populate the
// collector's HTTP-trace timestamps via the httptrace hooks. We can't
// reliably observe DNS/TLS on a localhost test server, but GotConn and
// GotFirstResponseByte (-> StampTTFB) will fire.
func TestAttachStampsHTTPTrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	c := NewCollector()
	ctx := WithCollector(context.Background(), c)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req = AttachToRequest(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}

	if c.gotConnAt.IsZero() {
		t.Errorf("gotConnAt not stamped — GotConn hook didn't fire")
	}
	if c.firstByteAt.IsZero() {
		t.Errorf("firstByteAt not stamped — GotFirstResponseByte hook didn't fire")
	}
	// ConnectStart/Done may or may not fire if the client reuses a
	// pooled connection from an earlier test, but on a fresh DefaultClient
	// hitting a freshly bound test server they should.
	if c.connectStartAt.IsZero() {
		t.Logf("connectStartAt not stamped (may be a pooled connection)")
	}
}

// AttachToRequest with no collector in ctx returns an equivalent request
// (no trace hooks attached) — no panic, no side effects on the request.
func TestAttachToRequestNoCollector(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	got := AttachToRequest(req)
	if got == nil {
		t.Fatalf("AttachToRequest returned nil")
	}
	// We can't directly assert "no client trace attached," but we can
	// confirm calling FromContext on the returned request's context
	// still returns nil collector.
	if c := FromContext(got.Context()); c != nil {
		t.Fatalf("FromContext non-nil on attached-with-no-collector request: %#v", c)
	}
}
