package instrumentation

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
)

// ClientTrace returns a *httptrace.ClientTrace bound to the collector in
// ctx. Each hook stamps its timestamp on the collector exactly once
// (the underlying stamp helpers are idempotent). Use Attach to wire
// this onto an outbound HTTP request:
//
//	ctx = instrumentation.Attach(ctx)
//	req = req.WithContext(ctx)
//	resp, _ := http.DefaultClient.Do(req)
//
// When ctx carries no collector, Attach is a no-op and the request
// proceeds unmodified — non-AIQG callers see zero behavior change.
func ClientTrace(ctx context.Context) *httptrace.ClientTrace {
	c := FromContext(ctx)
	if c == nil {
		return nil
	}
	return &httptrace.ClientTrace{
		DNSStart: func(_ httptrace.DNSStartInfo) {
			c.stampDNSStart()
		},
		DNSDone: func(_ httptrace.DNSDoneInfo) {
			c.stampDNSDone()
		},
		ConnectStart: func(_, _ string) {
			c.stampConnectStart()
		},
		ConnectDone: func(_, _ string, _ error) {
			c.stampConnectDone()
		},
		TLSHandshakeStart: func() {
			c.stampTLSHandshakeStart()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			c.stampTLSHandshakeDone()
		},
		GotConn: func(_ httptrace.GotConnInfo) {
			c.stampGotConn()
		},
		GotFirstResponseByte: func() {
			// TTFB only — see StampTTFB doc. The first response BYTE is
			// the response headers landing. TTFT is the first non-empty
			// content delta, which the stream consumer stamps separately.
			StampTTFB(contextWithCollector(c))
		},
	}
}

// Attach binds the collector's ClientTrace to ctx for outbound HTTP
// requests. Returns the new context; callers do `req.WithContext(Attach(ctx))`.
// Safe to call when no collector is attached — Attach returns ctx unchanged.
func Attach(ctx context.Context) context.Context {
	trace := ClientTrace(ctx)
	if trace == nil {
		return ctx
	}
	return httptrace.WithClientTrace(ctx, trace)
}

// AttachToRequest is a convenience wrapper that returns a copy of req
// with the trace context attached. Equivalent to:
//
//	req = req.WithContext(Attach(req.Context()))
func AttachToRequest(req *http.Request) *http.Request {
	return req.WithContext(Attach(req.Context()))
}

// contextWithCollector returns a minimal context carrying c — used by
// GotFirstResponseByte where we don't have access to the original
// request context inside the closure (the closure captures c directly
// to avoid that lookup, but StampTTFB is the public API that gates on
// the context lookup, so we wrap it back). This indirection keeps the
// public API consistent: every stamp goes through ctx.
func contextWithCollector(c *TimingCollector) context.Context {
	return WithCollector(context.Background(), c)
}
