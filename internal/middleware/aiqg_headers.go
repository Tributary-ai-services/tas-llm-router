// Package middleware: AIQG header taxonomy parser.
//
// Implements the TAS-* request header set defined in
// aether-shared/data-models/aiqg/source-spec-v0.2.md §3.5 and named as the
// implementation site in event-timestamps.md §225
// (`internal/middleware/aiqg_headers.go`). Headers parsed:
//
//	TAS-Auth                       Gateway auth token (tas_qg_live_*)
//	TAS-Policy                     Comma-separated policy names
//	TAS-Policy-Bundle              Named policy bundle (mutually exclusive with TAS-Policy)
//	TAS-Workflow                   Customer-supplied workflow override (enum)
//	TAS-Upstream-Authorization     Per-request vendor key override
//	TAS-Trace                      "1" → return captured event as response header
//	TAS-Dry-Run                    "1" → compute decisions but do not enforce
//	TAS-Source-App                 Free-form source-app override (max 128 chars)
//
// All headers are optional from the parser's perspective — the Path A auth
// slice will enforce TAS-Auth presence on the customer ingress. ParseHeaders
// only fails on malformed values (TAS-Policy + TAS-Policy-Bundle both set,
// TAS-Workflow not in the enum). Unknown TAS-* headers are ignored, not
// rejected, so a future header addition doesn't break old gateways.
//
// The parsed AIQGHeaders are attached to request context via WithHeaders so
// downstream consumers (workflow classifier, policy resolver, event emitter)
// can read them without re-parsing. StripFromOutbound removes the TAS-*
// headers from the request before it leaves the gateway — vendors must never
// see TAS-* on the wire.
package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// AIQGHeaders is the parsed view of TAS-* request headers. Zero-value is a
// valid "no AIQG headers present" state — every field is optional at parse
// time. Use IsZero to test whether the request carried any AIQG control
// headers at all.
type AIQGHeaders struct {
	Auth                  string   // TAS-Auth (raw token; format-validated, not authenticated)
	Policy                []string // TAS-Policy (comma-split, trimmed)
	PolicyBundle          string   // TAS-Policy-Bundle
	Workflow              string   // TAS-Workflow (validated against enum)
	UpstreamAuthorization string   // TAS-Upstream-Authorization (raw, never logged)
	Trace                 bool     // TAS-Trace=1
	DryRun                bool     // TAS-Dry-Run=1
	SourceApp             string   // TAS-Source-App (max 128 chars)
}

// IsZero reports whether any AIQG header was set. Used by the Path A
// gating logic to decide whether to enter AIQG mode at all.
func (h AIQGHeaders) IsZero() bool {
	return h.Auth == "" && len(h.Policy) == 0 && h.PolicyBundle == "" &&
		h.Workflow == "" && h.UpstreamAuthorization == "" &&
		!h.Trace && !h.DryRun && h.SourceApp == ""
}

// canonicalHeaderNames is the closed set of inbound TAS-* headers we
// recognize. StripFromOutbound iterates this list, so adding a new header
// requires adding it here. Kept private; tests reference it via exported
// helpers.
var canonicalHeaderNames = []string{
	"TAS-Auth",
	"TAS-Policy",
	"TAS-Policy-Bundle",
	"TAS-Workflow",
	"TAS-Upstream-Authorization",
	"TAS-Trace",
	"TAS-Dry-Run",
	"TAS-Source-App",
}

// validWorkflows is the closed enum from
// aether-shared/data-models/aiqg/workflow-classification.md §1.1.
// `unknown` is intentionally excluded — a customer who genuinely doesn't
// know their workflow should omit the header, not assert ignorance.
var validWorkflows = map[string]struct{}{
	"single_turn_qa":            {},
	"rag":                       {},
	"agentic":                   {},
	"summarization":             {},
	"code_generation":           {},
	"classification_extraction": {},
}

// Maximum length for TAS-Source-App per request-event.md §80.
const maxSourceAppLen = 128

// authTokenPrefix is the prefix every customer-facing TAS-Auth token
// carries (source-spec §3.5.1). Format check only; the auth-mode slice
// will resolve the token to an account.
const authTokenPrefix = "tas_qg_live_"

// Errors returned by ParseHeaders. Callers map these to the appropriate
// HTTP status (400 for malformed, 401 for missing auth — but missing auth
// is the Path A slice's concern, not this one).
var (
	ErrPolicyConflict        = errors.New("aiqg: TAS-Policy and TAS-Policy-Bundle are mutually exclusive")
	ErrAuthMalformed         = errors.New("aiqg: TAS-Auth token is malformed")
	ErrSourceAppTooLong      = fmt.Errorf("aiqg: TAS-Source-App exceeds %d characters", maxSourceAppLen)
	ErrUnknownWorkflowSilent = errors.New("aiqg: TAS-Workflow value is not in the enum (will fall through to heuristic)")
)

// ParseHeaders extracts the TAS-* header taxonomy from req. Returns a
// populated AIQGHeaders on success.
//
// Validation rules (per source-spec §3.5):
//   - TAS-Policy and TAS-Policy-Bundle are mutually exclusive (ErrPolicyConflict)
//   - TAS-Auth, if present, must start with "tas_qg_live_" (ErrAuthMalformed)
//   - TAS-Source-App, if present, must be ≤ 128 chars (ErrSourceAppTooLong)
//   - TAS-Workflow: if the value isn't in the enum, the field is cleared
//     and ErrUnknownWorkflowSilent is returned alongside the parsed
//     headers — callers can choose to log and proceed (matches
//     workflow-classification.md §3 "invalid override → log + fall through")
//
// All other malformations cause the function to return a partially
// populated struct and the relevant error. A nil error guarantees the
// returned headers are safe to attach via WithHeaders.
func ParseHeaders(req *http.Request) (AIQGHeaders, error) {
	h := AIQGHeaders{
		Auth:                  strings.TrimSpace(req.Header.Get("TAS-Auth")),
		PolicyBundle:          strings.TrimSpace(req.Header.Get("TAS-Policy-Bundle")),
		Workflow:              strings.TrimSpace(req.Header.Get("TAS-Workflow")),
		UpstreamAuthorization: req.Header.Get("TAS-Upstream-Authorization"),
		Trace:                 req.Header.Get("TAS-Trace") == "1",
		DryRun:                req.Header.Get("TAS-Dry-Run") == "1",
		SourceApp:             strings.TrimSpace(req.Header.Get("TAS-Source-App")),
	}

	if raw := req.Header.Get("TAS-Policy"); raw != "" {
		parts := strings.Split(raw, ",")
		policies := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				policies = append(policies, p)
			}
		}
		h.Policy = policies
	}

	if len(h.Policy) > 0 && h.PolicyBundle != "" {
		return h, ErrPolicyConflict
	}

	if h.Auth != "" && !strings.HasPrefix(h.Auth, authTokenPrefix) {
		return h, ErrAuthMalformed
	}

	if len(h.SourceApp) > maxSourceAppLen {
		return h, ErrSourceAppTooLong
	}

	if h.Workflow != "" {
		if _, ok := validWorkflows[h.Workflow]; !ok {
			// Per spec: log + clear + fall through. The caller decides
			// whether to log; we just return the soft error.
			h.Workflow = ""
			return h, ErrUnknownWorkflowSilent
		}
	}

	return h, nil
}

// StripFromOutbound removes every TAS-* header from req.Header. Call this
// on the outbound request before forwarding to the vendor — TAS-* headers
// must never reach OpenAI/Anthropic. Idempotent; safe to call on a
// request with no TAS-* headers.
//
// Also strips TAS-Upstream-Authorization specifically — the Path A slice
// will copy its value into the outbound Authorization header BEFORE
// calling this, so by the time we strip, the value is already where it
// needs to go.
func StripFromOutbound(req *http.Request) {
	for _, name := range canonicalHeaderNames {
		req.Header.Del(name)
	}
}

// Context plumbing — same shape as instrumentation.WithCollector, so
// downstream consumers learn one pattern.

type ctxKey struct{}

var headersKey = ctxKey{}

// WithHeaders attaches parsed AIQG headers to ctx. Downstream consumers
// (policy resolver, workflow classifier, event emitter) call FromContext
// to read them back.
func WithHeaders(ctx context.Context, h AIQGHeaders) context.Context {
	return context.WithValue(ctx, headersKey, h)
}

// FromContext returns the AIQG headers attached to ctx. The second return
// is false when no headers were attached — callers should treat this as
// "non-AIQG request" and skip AIQG-specific work entirely.
func FromContext(ctx context.Context) (AIQGHeaders, bool) {
	if ctx == nil {
		return AIQGHeaders{}, false
	}
	h, ok := ctx.Value(headersKey).(AIQGHeaders)
	return h, ok
}
