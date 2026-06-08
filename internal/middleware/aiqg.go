// Package middleware: AIQG mode entry — Path A auth wire-up.
//
// This middleware is the front door for AIQG mode. It runs before any
// other handler on the customer-facing ingress and decides whether a
// request enters AIQG processing.
//
// Decision logic (per build-vs-reuse §2.2 + §7.3 strict resolution):
//
//   1. Both TAS-Auth and Authorization present → enter Path A:
//      - parse the TAS-* header taxonomy and attach to ctx
//      - create a TimingCollector and attach to ctx
//      - stamp StampReceived on entry, StampComplete on response end
//      - hand off to the downstream chain
//   2. TAS-Auth present but Authorization missing → 401 (cfg.Strict only)
//   3. TAS-Auth missing → depends on cfg.Strict:
//      - Strict (customer-facing ingress): 401, diagnostic body
//      - Permissive (internal ingress): pass through unchanged, no
//        AIQG state attached — preserves existing internal-routing behavior
//
// The middleware never persists Authorization. The header value is
// already in r.Header for the rest of the chain; Path A's promise
// ("we never hold your keys") is enforced by the *absence* of storage
// code, not by anything this middleware does. The strip-from-outbound
// step happens at the proxy/SDK boundary, not here.
//
// Validation errors on the TAS-* taxonomy (e.g. TAS-Policy and
// TAS-Policy-Bundle both set) return 400 with a diagnostic body.
// ErrUnknownWorkflowSilent is logged but does not fail the request —
// the workflow field is already cleared by the parser and the heuristic
// classifier will fill it in.
package middleware

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/sirupsen/logrus"
	"github.com/tributary-ai/llm-router-waf/internal/instrumentation"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/tokens"
)

// AIQGConfig configures the AIQG entry middleware.
type AIQGConfig struct {
	// Strict makes missing TAS-Auth (or missing Authorization when
	// TAS-Auth is present) return 401. Use on the customer-facing
	// ingress (gateway.aiqg.tas.io). Leave false on the internal
	// ingress so internal callers continue to use stored vendor keys.
	Strict bool

	// Logger is used for warnings (invalid TAS-Workflow values) and the
	// path_a_auth_rejected audit event. Required.
	Logger *logrus.Logger

	// Emitter publishes the paired (request, response) AIQG events on
	// response completion. Nil is treated as events.NoopEmitter — the
	// middleware never panics on a nil emitter, but production should
	// always wire a real one (LogEmitter for MVP, KafkaEmitter later).
	Emitter events.Emitter

	// Region is stamped on every emitted RequestEvent (e.g. "us-east",
	// "us-west", "eu"). Set from deployment config. Empty is allowed
	// during local dev and is omitted from the event JSON.
	Region string

	// Resolver turns the TAS-Auth bearer into a tenant_id / account_id
	// for event attribution. Nil is allowed during incremental rollout:
	// the middleware skips resolution, every request is treated as
	// opaque-token-known, and emitted events carry empty tenant fields.
	// Once a Resolver is configured, unknown tokens become 401 and
	// suspended accounts become 403.
	Resolver tokens.Resolver
}

// NewAIQG returns the middleware constructor. The returned handler is a
// standard http.Handler decorator compatible with gorilla/mux's
// Router.Use.
//
// Panics on a nil logger — middleware misconfiguration should fail at
// boot, not in production.
func NewAIQG(cfg AIQGConfig) func(http.Handler) http.Handler {
	if cfg.Logger == nil {
		panic("middleware.NewAIQG: Logger is required")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleAIQG(cfg, next, w, r)
		})
	}
}

func handleAIQG(cfg AIQGConfig, next http.Handler, w http.ResponseWriter, r *http.Request) {
	tasAuth := r.Header.Get("TAS-Auth")
	customerAuth := r.Header.Get("Authorization")

	// Case 1: no TAS-Auth. Either reject (strict ingress) or pass
	// through untouched (internal ingress; preserves existing behavior).
	if tasAuth == "" {
		if cfg.Strict {
			rejectPathA(cfg.Logger, w, r, "TAS-Auth")
			return
		}
		next.ServeHTTP(w, r)
		return
	}

	// Case 2: TAS-Auth set, Authorization missing. Path A explicitly
	// requires both — there's no fallback to stored keys on AIQG
	// ingress (build-vs-reuse §7.3 strict resolution). This is
	// rejected in BOTH strict and permissive modes because once a
	// caller asserts TAS-Auth they've opted into Path A semantics.
	if customerAuth == "" {
		rejectPathA(cfg.Logger, w, r, "Authorization")
		return
	}

	// Case 3: both headers present. Parse the TAS-* taxonomy.
	parsed, err := ParseHeaders(r)
	switch {
	case err == nil:
		// happy path
	case errors.Is(err, ErrUnknownWorkflowSilent):
		// Per spec: log and fall through. parsed.Workflow has been cleared.
		cfg.Logger.WithFields(logrus.Fields{
			"event":          "aiqg.workflow_override_invalid",
			"workflow_value": r.Header.Get("TAS-Workflow"),
			"path":           r.URL.Path,
		}).Warn("TAS-Workflow value not in enum; falling through to heuristic")
	default:
		writeValidationError(cfg.Logger, w, r, err)
		return
	}

	// Resolve TAS-Auth to a tenant/account when a resolver is wired.
	// Unknown tokens become 401 (path_a_auth_rejected with reason=
	// token_unknown); suspended accounts become 403. When no resolver
	// is configured, tokens stay opaque and emitted events carry empty
	// tenant fields — this is the incremental-rollout path.
	var resolvedToken *tokens.Token
	if cfg.Resolver != nil {
		tok, err := cfg.Resolver.Resolve(r.Context(), parsed.Auth)
		switch {
		case errors.Is(err, tokens.ErrTokenNotFound):
			rejectTokenUnknown(cfg.Logger, w, r)
			return
		case errors.Is(err, tokens.ErrTokenSuspended):
			rejectAccountSuspended(cfg.Logger, w, r, tok)
			return
		case err != nil:
			cfg.Logger.WithError(err).WithField("event", "aiqg.token_resolve_error").
				Error("AIQG token resolver returned unexpected error")
			writeResolverError(w)
			return
		}
		resolvedToken = tok
	}

	// Enter AIQG mode: attach collector + headers + routing sidecar +
	// resolved token, stamp Received, arrange for StampComplete + event
	// emission on response end. The routing sidecar is empty here; the
	// completion handlers populate it via StampVendor / StampModel /
	// StampStreaming as those decisions are made.
	collector := instrumentation.NewCollector()
	routing := NewRouting()
	ctx := instrumentation.WithCollector(r.Context(), collector)
	ctx = WithHeaders(ctx, parsed)
	ctx = WithRouting(ctx, routing)
	if resolvedToken != nil {
		ctx = tokens.WithToken(ctx, resolvedToken)
	}
	instrumentation.StampReceived(ctx)

	sw := &statusCapturingResponseWriter{ResponseWriter: w}

	emitter := cfg.Emitter
	if emitter == nil {
		emitter = events.NoopEmitter{}
	}

	// Defers fire LIFO: emit runs AFTER StampComplete so the snapshot
	// it builds includes the response_complete_at timestamp. The
	// routing sidecar is read at the same moment, so any vendor/model
	// stamps made by the handler reach the event.
	defer func() {
		reqEnv, respEnv := events.Build(r, headersView(parsed), routingView(routing), tokenView(resolvedToken), collector.Snapshot(), events.BuildOptions{
			HTTPStatus: sw.status(),
			Region:     cfg.Region,
		})
		if err := emitter.Emit(ctx, reqEnv, respEnv); err != nil {
			cfg.Logger.WithError(err).Warn("AIQG event emission failed")
		}
	}()
	defer instrumentation.StampComplete(ctx)

	next.ServeHTTP(sw, r.WithContext(ctx))
}

// headersView projects an AIQGHeaders into the read-only view the
// events package consumes. Defined here (in internal/middleware) to
// avoid pkg/aiqg/events importing internal/middleware (which would
// create an import cycle).
func headersView(h AIQGHeaders) events.AIQGHeadersView {
	return events.AIQGHeadersView{
		SourceApp:    h.SourceApp,
		Workflow:     h.Workflow,
		Policy:       h.Policy,
		PolicyBundle: h.PolicyBundle,
		DryRun:       h.DryRun,
		Trace:        h.Trace,
	}
}

// routingView snapshots the mutable Routing sidecar into the read-only
// shape events.Build expects. Same anti-cycle motivation as headersView.
// Tolerates a nil collector (returns the zero view) so the middleware
// doesn't have to nil-check before passing it to Build.
func routingView(r *Routing) events.RoutingView {
	if r == nil {
		return events.RoutingView{}
	}
	s := r.Snapshot()
	return events.RoutingView{
		Vendor:           s.Vendor,
		Model:            s.Model,
		Streaming:        s.Streaming,
		StreamingSet:     s.StreamingSet,
		PromptTokens:     s.PromptTokens,
		CompletionTokens: s.CompletionTokens,
		UsageSet:         s.UsageSet,
		InboundFindings:  s.InboundFindings,
		OutboundFindings: s.OutboundFindings,
		ScanRan:          s.ScanRan,
		FinishReason:     s.FinishReason,
		AttemptCount:     s.AttemptCount,
		FallbackUsed:     s.FallbackUsed,
		RetrySet:         s.RetrySet,
		Workflow:         s.Workflow,
		NISTFindings:     s.NISTFindings,
		TagFindings:      s.TagFindings,
	}
}

// tokenView projects a resolved tokens.Token into the read-only shape
// events.Build expects. Nil token (no resolver configured) returns the
// zero view; events then carry empty tenant_id / aiqg_account_id /
// tas_auth_token_id fields, which downstream consumers can detect and
// filter out as non-attributed traffic.
func tokenView(t *tokens.Token) events.TokenView {
	if t == nil {
		return events.TokenView{}
	}
	return events.TokenView{
		TenantID:       t.TenantID,
		AIQGAccountID:  t.AIQGAccountID,
		TASAuthTokenID: t.TokenID,
		SourceApp:      t.SourceApp,
	}
}

// rejectTokenUnknown emits 401 for a TAS-Auth bearer that the resolver
// didn't recognize. Distinct response code (`token_unknown`) so
// dashboards can break out genuine misconfigurations from "auth header
// missing" failures.
func rejectTokenUnknown(log *logrus.Logger, w http.ResponseWriter, r *http.Request) {
	log.WithFields(logrus.Fields{
		"event":  "aiqg.path_a_auth_rejected",
		"reason": "token_unknown",
		"path":   r.URL.Path,
		"method": r.Method,
	}).Warn("Path A auth rejected — token unknown")

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", `TAS realm="aiqg"`)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"code":"path_a_auth_required","message":"AIQG ingress requires a recognized TAS-Auth token","reason":"token_unknown","docs":"https://docs.tas.scharber.com/aiqg/auth"}}`))
}

// rejectAccountSuspended emits 403 for a TAS-Auth bearer that resolves
// to a suspended account. Per spec request-event.md §564 we still
// recognize the token; we reject the request before forwarding. The
// resolved token info is logged for support to triage.
func rejectAccountSuspended(log *logrus.Logger, w http.ResponseWriter, r *http.Request, tok *tokens.Token) {
	fields := logrus.Fields{
		"event":  "aiqg.account_suspended",
		"path":   r.URL.Path,
		"method": r.Method,
	}
	if tok != nil {
		fields["aiqg_account_id"] = tok.AIQGAccountID
		fields["tenant_id"] = tok.TenantID
	}
	log.WithFields(fields).Warn("AIQG request rejected — account suspended")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":{"code":"account_suspended","message":"AIQG account is currently suspended; contact support"}}`))
}

// writeResolverError emits 503 for resolver-side failures (e.g. the
// production backend resolver can't reach the token store). 503 instead
// of 500 because the failure is upstream-dependency unavailability, and
// callers should retry rather than treat the request as permanently
// broken.
func writeResolverError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"error":{"code":"token_resolver_unavailable","message":"AIQG token resolver is temporarily unavailable; retry"}}`))
}

// statusCapturingResponseWriter records the HTTP status code written
// by downstream handlers so the AIQG event emitter can include it in
// the response event. Wraps the underlying writer transparently.
type statusCapturingResponseWriter struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

func (w *statusCapturingResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.code = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write captures the implicit 200 that net/http writes when a handler
// calls Write without first calling WriteHeader.
func (w *statusCapturingResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.code = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// status returns the captured HTTP status, defaulting to 200 if the
// handler never wrote anything (matches net/http's implicit-200 behavior).
func (w *statusCapturingResponseWriter) status() int {
	if !w.wroteHeader {
		return http.StatusOK
	}
	return w.code
}

// rejectPathA emits the strict-mode 401 response and the
// path_a_auth_rejected audit event (per audit-log-entry.md §97). The
// response body names the missing header so customers can self-diagnose
// without contacting support (per §7.3 mitigation).
func rejectPathA(log *logrus.Logger, w http.ResponseWriter, r *http.Request, missing string) {
	log.WithFields(logrus.Fields{
		"event":          "aiqg.path_a_auth_rejected",
		"missing_header": missing,
		"path":           r.URL.Path,
		"method":         r.Method,
		"remote_addr":    r.RemoteAddr,
	}).Warn("Path A auth rejected")

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", `TAS realm="aiqg"`)
	w.WriteHeader(http.StatusUnauthorized)

	body := fmt.Sprintf(`{"error":{"code":"path_a_auth_required","message":"AIQG ingress requires both TAS-Auth and Authorization headers; %s is missing","missing_header":%q,"docs":"https://docs.tas.scharber.com/aiqg/auth"}}`, missing, missing)
	_, _ = w.Write([]byte(body))
}

// writeValidationError emits 400 when the TAS-* header taxonomy fails
// schema validation (ErrPolicyConflict, ErrAuthMalformed,
// ErrSourceAppTooLong). The error.Error() text is safe to return —
// none of these errors contain customer-controlled values.
func writeValidationError(log *logrus.Logger, w http.ResponseWriter, r *http.Request, err error) {
	log.WithFields(logrus.Fields{
		"event": "aiqg.header_validation_failed",
		"path":  r.URL.Path,
		"err":   err.Error(),
	}).Warn("AIQG header validation failed")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)

	body := fmt.Sprintf(`{"error":{"code":"aiqg_header_invalid","message":%q}}`, err.Error())
	_, _ = w.Write([]byte(body))
}
