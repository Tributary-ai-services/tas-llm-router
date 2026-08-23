// Package middleware: AIQG mode entry — Path A auth wire-up.
//
// This middleware is the front door for AIQG mode. It runs before any
// other handler on the customer-facing ingress and decides whether a
// request enters AIQG processing.
//
// Decision logic (per build-vs-reuse §2.2 + §7.3 strict resolution):
//
//  1. Both TAS-Auth and Authorization present → enter Path A:
//     - parse the TAS-* header taxonomy and attach to ctx
//     - create a TimingCollector and attach to ctx
//     - stamp StampReceived on entry, StampComplete on response end
//     - hand off to the downstream chain
//  2. TAS-Auth present but Authorization missing → 401 (cfg.Strict only)
//  3. TAS-Auth missing → depends on cfg.Strict:
//     - Strict (customer-facing ingress): 401, diagnostic body
//     - Permissive (internal ingress): pass through unchanged, no
//     AIQG state attached — preserves existing internal-routing behavior
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
	"context"
	"errors"
	"fmt"
	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/tributary-ai/llm-router-waf/internal/instrumentation"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/experiments"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/linkage"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/policy"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/promptcache"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/tokens"

	"github.com/tributary-ai/llm-router-waf/internal/workflow"
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

	// IPCaptureMode gates client-IP recording on events: "off",
	// "minimized" (truncated prefix; default when empty), "full" (raw).
	IPCaptureMode string

	// Resolver turns the TAS-Auth bearer into a tenant_id / account_id
	// for event attribution. Nil is allowed during incremental rollout:
	// the middleware skips resolution, every request is treated as
	// opaque-token-known, and emitted events carry empty tenant fields.
	// Once a Resolver is configured, unknown tokens become 401 and
	// suspended accounts become 403.
	Resolver tokens.Resolver

	// PolicyResolver picks the AIQG policy_bundle that applies to each
	// request (Phase 4.0). Nil is allowed — the middleware emits
	// events without a resolved_policy_bundle field, matching pre-4.0
	// behavior. When configured but the resolver returns an error,
	// the middleware falls back to policy.Default() so the event
	// always has a resolution (the "every request resolves" gate).
	PolicyResolver policy.Resolver

	// Linkage is the deterministic `linked`-tier store (tool_call_id echo
	// index, docs/AIQG-AGENT-FLOW-ATTRIBUTION.md §A). Nil disables linkage —
	// events emit without step/parent topology, same as pre-C2a behavior.
	Linkage linkage.Store

	// PromptCache measures vendor prompt-cache opportunity
	// (docs/AIQG-PROMPT-CACHE-CONTROL.md §9, P0): it reports whether a
	// request's cacheable tools+system prefix recurred inside one cache
	// lifetime. Measure-only — it never touches the outbound request. Nil
	// disables the probe.
	PromptCache promptcache.Probe

	// Experiments resolves the experiment + variant that claims a request
	// (Phase D, docs/AIQG-EXPERIMENTS-RUNNER.md). Nil disables experiments —
	// no assignment, no stamping. dry_run/running experiments get stamped on
	// the event; the routing override (running) is applied separately.
	Experiments *experiments.Resolver
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

	// Native-SDK auth (Design 1): a stock OpenAI/Anthropic SDK can only
	// populate its provider's native credential slot — Authorization: Bearer
	// (OpenAI SDK) or x-api-key (Anthropic SDK) — and has no way to set the
	// custom TAS-Auth header. When TAS-Auth is absent but one of those slots
	// carries OUR gateway token (the distinctive `tas_qg_live_` prefix), lift
	// it into TAS-Auth so the rest of the Path A chain resolves it exactly as
	// if the customer had sent TAS-Auth. The prefix gate is what keeps this
	// unambiguous: a real BYOK vendor key (sk-…, sk-ant-…) never matches, so
	// it falls through untouched. This is what makes
	//   OpenAI(api_key="tas_qg_live_…", base_url="<gw>/v1")
	//   Anthropic(api_key="tas_qg_live_…", base_url="<gw>")
	// work with no custom headers. Power users who instead put a real vendor
	// key in the native slot set TAS-Auth explicitly (Design 2) and never hit
	// this path.
	if tasAuth == "" {
		if tok, from := recoverGatewayToken(r); tok != "" {
			r.Header.Set("TAS-Auth", tok)
			tasAuth = tok
			// The native slot held only our gateway token (Design 1), not an
			// upstream vendor key — clear it so the tas_qg_live_ secret is
			// never forwarded to the vendor. The effective upstream key is
			// selected later by applyBYOKKey (stored credential → shared key).
			switch from {
			case tokenFromAuthorization:
				r.Header.Del("Authorization")
				customerAuth = ""
			case tokenFromXAPIKey:
				r.Header.Del("X-Api-Key")
			}
		}
	}

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

	// Case 2: TAS-Auth set, Authorization missing. Historically rejected, but
	// with BYOK (Plan #14) the vendor key can come from a stored credential or
	// the TAS shared key, so Authorization is now OPTIONAL. The effective-key
	// resolution after routing (server.applyBYOKKey) decides: stored key →
	// inject; no key + shared-fallback allowed → TAS key; BYOK-only + no key →
	// 402. Fall through to parse the TAS-* taxonomy. (An unused customerAuth
	// here is fine — the raw Authorization is handled downstream, not injected.)
	_ = customerAuth

	// Case 3: parse the TAS-* taxonomy (works with or without Authorization).
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
			writeResolverError(w, r)
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

	// Phase 4.0 — resolve which policy bundle applies. Observation-only:
	// stash on the request context so events.Build can stamp it on
	// the response event. Errors degrade to Default() so "every event
	// has a resolution" is upheld even when the resolver is down.
	// Initial resolution at receipt (header / source_app / path only).
	// The completion handler refines it at routing time via
	// ResolveBundleForRouting once model + workflow are known. A holder on
	// ctx lets the deferred emit read whichever resolution is current.
	bundleResolution := &bundleResolutionHolder{res: policy.Default()}
	ctx = withBundleResolution(ctx, bundleResolution)

	// Holder for an optional shadow payload-reduction measurement, filled
	// by the completion handler's goroutine and read by the deferred emit.
	reductionMeasurement := &reductionMeasurementHolder{}
	ctx = withReductionMeasurement(ctx, reductionMeasurement)

	// Stash the resolved tenant so downstream handlers (the response caches,
	// which tenant-namespace their keys) get the AIQG-resolved tenant — it lives
	// in the Path-A token, not security.GetAuthInfo, so extractTenantID can't
	// find it otherwise. Empty tenant would collapse every tenant's cache into
	// one namespace (a cross-tenant leak).
	if resolvedToken != nil && resolvedToken.TenantID != "" {
		ctx = withResolvedTenant(ctx, resolvedToken.TenantID)
	}
	if cfg.PolicyResolver != nil && resolvedToken != nil {
		res, err := cfg.PolicyResolver.Resolve(ctx, policy.ResolveRequest{
			TenantID:           resolvedToken.TenantID,
			AIQGAccountID:      resolvedToken.AIQGAccountID,
			PolicyBundleHeader: parsed.PolicyBundle,
			SourceApp:          parsed.SourceApp,
			Path:               r.URL.Path,
		})
		if err != nil {
			// Don't fail the request — operators see the resolver
			// degradation via the warning, and the request still
			// flows with a Default() resolution stamped on its event.
			cfg.Logger.WithError(err).WithFields(logrus.Fields{
				"event":     "aiqg.policy_resolve_error",
				"tenant_id": resolvedToken.TenantID,
				"bundle":    parsed.PolicyBundle,
			}).Warn("AIQG policy resolver returned error; defaulting to observe-all")
		}
		bundleResolution.set(res)
		// Stash for routing-time refinement. Skipped when an explicit
		// TAS-Policy-Bundle header already pinned the bundle — model /
		// workflow matching can't override an operator's explicit choice.
		if parsed.PolicyBundle == "" {
			ctx = withPolicyResolve(ctx, &policyResolveCtx{
				resolver:  cfg.PolicyResolver,
				tenantID:  resolvedToken.TenantID,
				accountID: resolvedToken.AIQGAccountID,
				sourceApp: parsed.SourceApp,
			})
		}
	}

	sw := &statusCapturingResponseWriter{ResponseWriter: w}

	// Pre-generate the response event id so we can advertise it as a
	// response header BEFORE the downstream handler flushes the body
	// (works for streaming too). Clients and the /feedback endpoint
	// correlate feedback against this exact id. Build reuses it.
	respEventID := events.NewEventID()
	w.Header().Set("TAS-Response-Event-Id", respEventID)

	// Experiments (Phase D): stash the resolver + this request's sticky
	// identity on ctx so the completion handler can resolve the variant at
	// ROUTING time (when model + workflow are known) — applying a running
	// experiment's override before Router.Route, and stamping
	// experiment_id/variant on the routing sidecar for the event.
	if cfg.Experiments != nil && resolvedToken != nil {
		ctx = withExperimentResolveCtx(ctx, &experimentResolveCtx{
			resolver:  cfg.Experiments,
			tenantID:  resolvedToken.TenantID,
			id:        experimentIdentity(parsed, resolvedToken, r, respEventID),
			sourceApp: experimentSourceApp(parsed, resolvedToken),
		})
	}

	emitter := cfg.Emitter
	if emitter == nil {
		emitter = events.NoopEmitter{}
	}

	// Defers fire LIFO: emit runs AFTER StampComplete so the snapshot
	// it builds includes the response_complete_at timestamp. The
	// routing sidecar is read at the same moment, so any vendor/model
	// stamps made by the handler reach the event.
	defer func() {
		// Linked tier: resolve the flow/step this request continues (it
		// echoed a tool_call_id we served) and index the ids this response
		// served (so the next step links back here). Uses a fresh context —
		// the request context may already be cancelled in this deferred path.
		lk := resolveLinkage(cfg, parsed, routing, resolvedToken, respEventID)

		// P0 prompt-cache probe: would this request's cacheable prefix have
		// been a vendor cache read? Measure-only, and deliberately here in the
		// deferred path — it runs after the response is served, so it cannot
		// add latency to, or fail, the request it measures.
		probePromptCache(cfg, routing, resolvedToken)

		// Experiment attribution (Phase D): read the variant the completion
		// handler resolved + stamped at routing time. dry_run stamps only;
		// running additionally applied the override before Router.Route.
		expSnap := routing.Snapshot()

		reqEnv, respEnv := events.Build(r, headersView(parsed), routingView(routing), tokenView(resolvedToken), collector.Snapshot(), events.BuildOptions{
			HTTPStatus:      sw.status(),
			Region:          cfg.Region,
			IPCaptureMode:   cfg.IPCaptureMode,
			ResponseEventID: respEventID,
			Linkage:         lk,
			ResolvedPolicyBundle: func() *events.ResolvedPolicyBundle {
				res := bundleResolution.get()
				return &events.ResolvedPolicyBundle{
					BundleID:   res.BundleID,
					BundleName: res.BundleName,
					Source:     res.Source,
				}
			}(),
			ExperimentID:         expSnap.ExperimentID,
			ExperimentVariant:    expSnap.ExperimentVariant,
			ReductionMeasurement: reductionMeasurement.getWait(reductionEmitWait),
		})
		if err := emitter.Emit(ctx, reqEnv, respEnv); err != nil {
			cfg.Logger.WithError(err).Warn("AIQG event emission failed")
		}
	}()
	defer instrumentation.StampComplete(ctx)

	next.ServeHTTP(sw, r.WithContext(ctx))
}

// tokenSource names which native SDK credential slot a recovered gateway
// token came from, so handleAIQG can clear exactly that slot.
type tokenSource int

const (
	tokenFromNone          tokenSource = iota
	tokenFromAuthorization             // OpenAI SDK: Authorization: Bearer <token>
	tokenFromXAPIKey                   // Anthropic SDK: x-api-key: <token>
)

// recoverGatewayToken looks for the gateway token (`tas_qg_live_*`) in a stock
// SDK's native credential slot — `Authorization: Bearer <token>` (OpenAI SDK)
// or `x-api-key: <token>` (Anthropic SDK) — and reports which slot it came
// from. Returns ("", tokenFromNone) when neither slot carries our prefix.
//
// The prefix gate is load-bearing: a real vendor key in these slots (sk-…,
// sk-ant-…) does NOT start with `tas_qg_live_`, so it is not mistaken for our
// token and is left in place for downstream (BYOK) handling. Authorization is
// checked before x-api-key; a request that (unusually) carries our token in
// both resolves from Authorization.
func recoverGatewayToken(r *http.Request) (string, tokenSource) {
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "" {
		tok := auth
		// Strip an optional "Bearer " scheme (what the OpenAI SDK sends);
		// also accept a bare token for hand-rolled clients.
		if len(auth) >= 7 && strings.EqualFold(auth[:7], "Bearer ") {
			tok = strings.TrimSpace(auth[7:])
		}
		if strings.HasPrefix(tok, authTokenPrefix) {
			return tok, tokenFromAuthorization
		}
	}
	if xk := strings.TrimSpace(r.Header.Get("X-Api-Key")); xk != "" {
		if strings.HasPrefix(xk, authTokenPrefix) {
			return xk, tokenFromXAPIKey
		}
	}
	return "", tokenFromNone
}

// probePromptCache measures vendor prompt-cache opportunity for one request
// (docs/AIQG-PROMPT-CACHE-CONTROL.md §9, P0) and logs the observation.
//
// Why this exists: the router cannot currently set cache_control at all (§1),
// so vendor prompt caching is unreachable and 30 days of production traffic
// show zero cached tokens (§9.1). Before adding a control surface that costs a
// 1.25× write premium on prefixes that never recur, we measure whether they
// recur at all — from live traffic, changing nothing. A `prefix_seen=true` here
// is a cache read we are currently forgoing.
//
// Emits one structured line per cacheable request, so the reuse rate is a Loki
// query rather than a new pipeline:
//
//	sum(count_over_time({...} |= "prompt-cache probe" | json | prefix_seen="true" [7d]))
//	  / sum(count_over_time({...} |= "prompt-cache probe" [7d]))
//
// Best-effort throughout: this runs from a deferred path on an already-served
// response, so every failure degrades to "no measurement", never to an error
// the client can see.
func probePromptCache(cfg AIQGConfig, routing *Routing, tok *tokens.Token) {
	if cfg.PromptCache == nil || routing == nil || tok == nil {
		return
	}
	rs := routing.Snapshot()
	if rs.CachePrefixHash == "" {
		return // nothing cacheable in this request — not a miss, not a datapoint
	}

	// Fresh context: the request context may already be cancelled here.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	seen, err := cfg.PromptCache.Observe(ctx, tok.TenantID, rs.CachePrefixHash)
	if err != nil {
		cfg.Logger.WithError(err).Warn("aiqg prompt-cache probe failed")
		return
	}

	// prompt_tokens sizes the prize: on a hit, roughly this many tokens would
	// have billed at the 0.1× read rate instead of 1.0×. Logged per-request so
	// the rollout decision can be made on tokens, not just hit count — a 2%
	// hit rate on 70k-token prefixes is a different call than 2% on 500-token
	// ones.
	f := logrus.Fields{
		"cache_prefix_hash": rs.CachePrefixHash,
		"prefix_seen":       seen,
		"tenant_id":         tok.TenantID,
		"model":             rs.Model,
		"vendor":            rs.Vendor,
	}
	if rs.UsageSet {
		f["prompt_tokens"] = rs.PromptTokens
	}
	cfg.Logger.WithFields(f).Info("aiqg prompt-cache probe")
}

// resolveLinkage runs the deterministic `linked`-tier resolution + indexing
// (docs/AIQG-AGENT-FLOW-ATTRIBUTION.md §A) for one request/response:
//   - resolve the tool_call_ids this request echoed → the (flow, step) we
//     served them from = the parent step + the flow this request continues;
//   - index the tool_call_ids this response served under the effective flow
//     so the NEXT request echoing them links back to this step.
//
// Uses a fresh context — the request context may already be cancelled in the
// deferred path this runs from. Best-effort: any store error is logged and
// degrades to no linkage rather than failing the (already-served) request.
func resolveLinkage(cfg AIQGConfig, parsed AIQGHeaders, routing *Routing, tok *tokens.Token, respEventID string) events.Linkage {
	lk := events.Linkage{StepID: respEventID}
	if cfg.Linkage == nil || tok == nil || routing == nil {
		return lk
	}
	hv := headersView(parsed)
	rs := routing.Snapshot()
	tenantID := tok.TenantID

	// Fingerprinted tier (§B): pass the request's structural signature to the
	// builder, which scopes it per (tenant, principal) into an AgentSurrogateID.
	// Independent of the deterministic store lookups below.
	lk.Fingerprint = rs.Fingerprint

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Resolve: did this request echo a tool_call we served? (evidence)
	if link, ok, err := cfg.Linkage.ResolveToolCalls(ctx, tenantID, rs.EchoedToolCallIDs); err != nil {
		cfg.Logger.WithError(err).Warn("aiqg linkage resolve failed")
	} else if ok {
		lk.ParentStepID = link.StepID
		lk.FlowID = link.FlowID
		lk.Linked = true
	}

	// Prefix chaining (§A.2): a tool-less multi-turn thread re-sends a
	// conversation state we served as the prefix of its next request. When the
	// prefix hash matches an indexed state, this request is provably the next
	// turn of that conversation. Independent of the flow/step tool-echo tier —
	// it threads conversation_id, not flow topology.
	if rs.PrefixHash != "" {
		if convID, ok, err := cfg.Linkage.ResolvePrefix(ctx, tenantID, rs.PrefixHash); err != nil {
			cfg.Logger.WithError(err).Warn("aiqg prefix-chain resolve failed")
		} else if ok {
			lk.ConversationID = convID
			lk.Linked = true
		}
	}

	// Root anchor: an untagged request that SERVES tool_calls starts a flow;
	// anchor it on this step id so children group under it. Not "linked" —
	// there's no echo evidence yet, only an outgoing tool call.
	if lk.FlowID == "" && hv.FlowID == "" && hv.TraceID == "" && len(rs.ServedToolCallIDs) > 0 {
		lk.FlowID = respEventID
	}

	// Effective flow the event will carry (mirrors the builder precedence:
	// asserted header > traceparent > linked/anchor).
	effFlow := hv.FlowID
	if effFlow == "" {
		effFlow = hv.TraceID
	}
	if effFlow == "" {
		effFlow = lk.FlowID
	}

	// Index served ids under (effFlow, respEventID) so a later echo links here.
	if effFlow != "" && len(rs.ServedToolCallIDs) > 0 {
		if err := cfg.Linkage.IndexToolCalls(ctx, tenantID, rs.ServedToolCallIDs, linkage.Link{FlowID: effFlow, StepID: respEventID}); err != nil {
			cfg.Logger.WithError(err).Warn("aiqg linkage index failed")
		}
	}

	// Index the conversation state this response leaves, so the next turn's
	// prefix links back. The conversation id is the one we resolved (this turn
	// continues an existing thread) or, for a thread head, this step's id —
	// minted as a *candidate* in the index only. A head turn's own event is NOT
	// stamped with a conversation_id (no evidence it'll be continued), so
	// one-shot chats never flood the Conversations view; the id surfaces only
	// once a real continuation resolves it. Mirrors the tool-echo root anchor.
	if rs.StateHash != "" {
		convID := lk.ConversationID
		if convID == "" {
			convID = respEventID
		}
		if err := cfg.Linkage.IndexPrefix(ctx, tenantID, rs.StateHash, convID); err != nil {
			cfg.Logger.WithError(err).Warn("aiqg prefix-chain index failed")
		}
	}
	return lk
}

// experimentResolveCtx carries everything the completion handler needs to
// resolve the experiment variant at routing time. Stashed on the request
// context by handleAIQG.
type experimentResolveCtx struct {
	resolver  *experiments.Resolver
	tenantID  string
	id        experiments.Identity
	sourceApp string
}

type experimentCtxKey struct{}

func withExperimentResolveCtx(ctx context.Context, ec *experimentResolveCtx) context.Context {
	return context.WithValue(ctx, experimentCtxKey{}, ec)
}

// ResolveExperimentForRouting is called by the chat-completion handler once the
// model + workflow are known. It resolves the variant claiming this request,
// stamps experiment_id/variant on the routing sidecar (so the event carries
// them), and returns the Decision — Apply=true means the caller should apply
// the variant's override before Router.Route. Returns nil when no experiment
// is configured/claims the request. Best-effort: never fails the request.
func ResolveExperimentForRouting(ctx context.Context, model, workflowType, path string) *experiments.Decision {
	ec, _ := ctx.Value(experimentCtxKey{}).(*experimentResolveCtx)
	if ec == nil || ec.resolver == nil {
		return nil
	}
	attrs := experiments.ReqAttrs{
		SourceApp:    ec.sourceApp,
		Model:        model,
		WorkflowType: workflowType,
		Path:         path,
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	d := ec.resolver.Resolve(rctx, ec.tenantID, ec.id, attrs)
	if d != nil {
		StampExperiment(ctx, d.ExperimentID, d.Variant)
	}
	return d
}

// bundleResolutionHolder is a mutable, concurrency-safe cell for the
// request's policy resolution. The middleware seeds it at receipt; the
// completion handler refines it at routing time (ResolveBundleForRouting);
// the deferred emit reads whichever value is current.
type bundleResolutionHolder struct {
	mu  sync.Mutex
	res policy.Resolution
}

func (h *bundleResolutionHolder) set(r policy.Resolution) {
	h.mu.Lock()
	h.res = r
	h.mu.Unlock()
}

func (h *bundleResolutionHolder) get() policy.Resolution {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.res
}

type bundleResolutionCtxKey struct{}

func withBundleResolution(ctx context.Context, h *bundleResolutionHolder) context.Context {
	return context.WithValue(ctx, bundleResolutionCtxKey{}, h)
}

// reductionMeasurementHolder is a concurrency-safe cell for the shadow
// payload-reduction measurement (Plan #7 Phase 2). The completion handler
// fills it from a goroutine running concurrently with the vendor call; the
// deferred emit reads whatever is present (best-effort — absent if the
// measurement didn't finish before emit).
// reductionEmitWait bounds how long the deferred emit blocks for an
// in-flight shadow measurement. The embed-only extract (all-minilm) is
// sub-second for typical chat payloads; this is generous headroom.
const reductionEmitWait = 5 * time.Second

type reductionMeasurementHolder struct {
	mu      sync.Mutex
	m       *events.ReductionMeasurement
	pending bool
	done    chan struct{}
}

// expect marks that a shadow measurement is in flight so getWait blocks
// (bounded) for the result instead of returning nil immediately.
func (h *reductionMeasurementHolder) expect() {
	h.mu.Lock()
	if h.done == nil {
		h.done = make(chan struct{})
	}
	h.pending = true
	h.mu.Unlock()
}

func (h *reductionMeasurementHolder) set(m *events.ReductionMeasurement) {
	h.mu.Lock()
	h.m = m
	h.pending = false
	if h.done != nil {
		close(h.done)
		h.done = nil
	}
	h.mu.Unlock()
}

// getWait returns the measurement, waiting up to timeout if a shadow run
// was started (expect) but hasn't landed yet. Called from the deferred
// emit, which runs after the client response is flushed — so the wait adds
// no client-facing latency, only a small delay to event emission.
func (h *reductionMeasurementHolder) getWait(timeout time.Duration) *events.ReductionMeasurement {
	h.mu.Lock()
	if !h.pending {
		m := h.m
		h.mu.Unlock()
		return m
	}
	done := h.done
	h.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(timeout):
		}
	}
	h.mu.Lock()
	m := h.m
	h.mu.Unlock()
	return m
}

type reductionMeasurementCtxKey struct{}

func withReductionMeasurement(ctx context.Context, h *reductionMeasurementHolder) context.Context {
	return context.WithValue(ctx, reductionMeasurementCtxKey{}, h)
}

type resolvedTenantCtxKey struct{}

func withResolvedTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, resolvedTenantCtxKey{}, tenantID)
}

// ResolvedTenantFromContext returns the AIQG Path-A resolved tenant id, or ""
// when not in AIQG mode / unresolved. Handlers that tenant-namespace state (the
// response caches) read this because the tenant lives in the AIQG token, not
// security.GetAuthInfo.
func ResolvedTenantFromContext(ctx context.Context) string {
	t, _ := ctx.Value(resolvedTenantCtxKey{}).(string)
	return t
}

// ExpectReductionMeasurement signals that a shadow measurement goroutine has
// been launched for this request, so the deferred emit waits (bounded) for
// the result rather than emitting before it lands. Call BEFORE starting the
// goroutine. No-op if no holder is on the context.
func ExpectReductionMeasurement(ctx context.Context) {
	if h, ok := ctx.Value(reductionMeasurementCtxKey{}).(*reductionMeasurementHolder); ok {
		h.expect()
	}
}

// StampReductionMeasurement records a completed shadow measurement on the
// request so the emitted event carries the Contract v2 measured fields.
// Called from the gateway's shadow goroutine; safe after the request
// context is cancelled (it writes to a holder, not the ctx).
func StampReductionMeasurement(ctx context.Context, m *events.ReductionMeasurement) {
	if h, ok := ctx.Value(reductionMeasurementCtxKey{}).(*reductionMeasurementHolder); ok {
		h.set(m)
	}
}

// policyResolveCtx carries what ResolveBundleForRouting needs to re-resolve
// the bundle at routing time. Only stashed when no explicit bundle header
// was set (an explicit header is final).
type policyResolveCtx struct {
	resolver  policy.Resolver
	tenantID  string
	accountID string
	sourceApp string
}

type policyResolveCtxKey struct{}

func withPolicyResolve(ctx context.Context, pc *policyResolveCtx) context.Context {
	return context.WithValue(ctx, policyResolveCtxKey{}, pc)
}

// ResolvedReduction returns the parsed payload-reduction policy for the
// current request (from the resolved bundle's Spec.reduction), or nil
// when there's none / outside AIQG mode. Plan #7 Phase 2: the extraction
// decision point will call this once it's wired to run/shadow Gatekeeper
// extract. Best-effort: a malformed policy returns (nil, err) so the
// caller logs and falls back to projected-only.
func ResolvedReduction(ctx context.Context) (*policy.ReductionPolicy, error) {
	holder, _ := ctx.Value(bundleResolutionCtxKey{}).(*bundleResolutionHolder)
	if holder == nil {
		return nil, nil
	}
	return policy.ParseReduction(holder.get().Reduction)
}

// ResolveBundleForRouting re-resolves the policy bundle once the requested
// model + workflow are known (routing time), so route rules that target by
// model/workflow take effect. Best-effort: on any error it keeps the
// receipt-time resolution. No-op when an explicit header already pinned the
// bundle (the policy-resolve ctx isn't stashed in that case) or outside
// AIQG mode.
func ResolveBundleForRouting(ctx context.Context, model, workflowType, path string) {
	pc, _ := ctx.Value(policyResolveCtxKey{}).(*policyResolveCtx)
	holder, _ := ctx.Value(bundleResolutionCtxKey{}).(*bundleResolutionHolder)
	if pc == nil || holder == nil || pc.resolver == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	res, err := pc.resolver.Resolve(rctx, policy.ResolveRequest{
		TenantID:      pc.tenantID,
		AIQGAccountID: pc.accountID,
		SourceApp:     pc.sourceApp,
		Path:          path,
		Model:         model,
		WorkflowType:  workflowType,
	})
	if err != nil {
		return
	}
	holder.set(res)
}

// experimentIdentity builds the sticky-key ladder from the request's parsed
// headers + token, ending at respEventID (the always-present random fallback).
func experimentIdentity(parsed AIQGHeaders, tok *tokens.Token, r *http.Request, respEventID string) experiments.Identity {
	principal := tok.SourceApp
	if principal == "" {
		principal = tok.TokenID
	}
	flowID := parsed.FlowID
	if flowID == "" {
		flowID = parsed.TraceID
	}
	convID := parsed.ConversationID
	if convID == "" {
		convID = parsed.Baggage["session.id"]
	}
	return experiments.Identity{
		UserID:         parsed.Baggage["user.id"],
		ConversationID: convID,
		FlowID:         flowID,
		PrincipalID:    principal,
		ClientIP:       experimentClientIP(r),
		RequestID:      respEventID,
	}
}

func experimentSourceApp(parsed AIQGHeaders, tok *tokens.Token) string {
	if parsed.SourceApp != "" {
		return parsed.SourceApp
	}
	return tok.SourceApp
}

// experimentClientIP extracts a stable client IP for the assignment key —
// first X-Forwarded-For hop, else RemoteAddr host. Hashing input only; not
// stored.
func experimentClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
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

		AgentID:          h.AgentID,
		AgentName:        h.AgentName,
		AgentVersion:     h.AgentVersion,
		FlowID:           h.FlowID,
		ConversationID:   h.ConversationID,
		TraceID:          h.TraceID,
		Synthetic:        h.Synthetic,
		BaggageUserID:    h.Baggage["user.id"],
		BaggageSessionID: h.Baggage["session.id"],

		// OTel gen_ai.* declared signals. Map the raw op-name to a
		// workflow_type here (mapped "" = fall through to heuristic); carry
		// the raw op-name + agent/conversation for the identity ladder and
		// classification drift. See otel-genai-ingestion.md §3/§4.
		OTelWorkflow:       otelWorkflow(h.OTelOperation),
		OTelOperation:      h.OTelOperation,
		OTelAgentID:        h.OTelAgentID,
		OTelAgentName:      h.OTelAgentName,
		OTelConversationID: h.OTelConversationID,
		OTelSystem:         h.OTelSystem,
		OTelMapVersion:     otelMapVersion(h.OTelOperation),
	}
}

// otelMapVersion returns the op-name→workflow map version when an OTel
// operation was present (so historical classification drift stays
// interpretable across map changes), else "".
func otelMapVersion(op string) string {
	if op == "" {
		return ""
	}
	return workflow.OTelOperationMapVersion
}

// otelWorkflow maps an OTel gen_ai.operation.name to a workflow_type,
// returning "" when the op-name should fall through to the heuristic
// classifier (generic/excluded/unknown ops). Thin wrapper over
// workflow.OperationToWorkflow so headersView stays declarative.
func otelWorkflow(op string) string {
	if wf, ok := workflow.OperationToWorkflow(op); ok {
		return wf
	}
	return ""
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
		PromptCacheMode:            s.PromptCacheMode,
		PromptCacheBreakpoints:     s.PromptCacheBreakpoints,
		EnforcementMode:            s.EnforcementMode,
		EnforcementOutcome:         s.EnforcementOutcome,
		EnforcementPatterns:        s.EnforcementPatterns,
		Findings:                   toEventFindings(s.ScanFindings),
		FindingsTruncated:          s.FindingsTruncated,
		SignalsExcluded:            toEventExclusions(s.SignalsExcluded),
		SignalsNote:                s.SignalsNote,
		AffinityHeld:               s.AffinityHeld,
		AffinityEpoch:              s.AffinityEpoch,
		AffinityReason:             s.AffinityReason,
		Vendor:                     s.Vendor,
		Model:                      s.Model,
		Streaming:                  s.Streaming,
		StreamingSet:               s.StreamingSet,
		PromptTokens:               s.PromptTokens,
		CompletionTokens:           s.CompletionTokens,
		CacheCreationTokens:        s.CacheCreationTokens,
		CacheReadTokens:            s.CacheReadTokens,
		UsageSet:                   s.UsageSet,
		InboundFindings:            s.InboundFindings,
		OutboundFindings:           s.OutboundFindings,
		ScanRan:                    s.ScanRan,
		RedactionCount:             s.RedactionCount,
		CacheState:                 s.CacheState,
		CacheKeyHash:               s.CacheKeyHash,
		CacheSavedPromptTokens:     s.CacheSavedPromptTokens,
		CacheSavedCompletionTokens: s.CacheSavedCompletionTokens,
		CacheSavedCostUSD:          s.CacheSavedCostUSD,
		CacheSimilarity:            s.CacheSimilarity,
		CacheThreshold:             s.CacheThreshold,
		FinishReason:               s.FinishReason,
		CredentialSource:           s.CredentialSource,
		CredentialID:               s.CredentialID,
		AttemptCount:               s.AttemptCount,
		FallbackUsed:               s.FallbackUsed,
		RetrySet:                   s.RetrySet,
		Workflow:                   s.Workflow,
		NISTFindings:               s.NISTFindings,
		TagFindings:                s.TagFindings,
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

// isAnthropicWirePath reports whether the request targets the Anthropic
// Messages API (POST /v1/messages). The AIQG middleware runs before the handler
// sets the response-format flag, so auth/validation rejections detect the
// Anthropic wire from the path to render an error a stock Anthropic SDK parses.
func isAnthropicWirePath(r *http.Request) bool {
	return strings.HasSuffix(r.URL.Path, "/messages")
}

// writeAnthropicWireError writes Anthropic's {type:"error",error:{type,message}}
// envelope, so an Anthropic SDK surfaces a well-formed error on an auth/validation
// rejection instead of the gateway's OpenAI-ish body.
func writeAnthropicWireError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q}}`, errType, message)))
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

	w.Header().Set("WWW-Authenticate", `TAS realm="aiqg"`)
	if isAnthropicWirePath(r) {
		writeAnthropicWireError(w, http.StatusUnauthorized, "authentication_error", "AIQG ingress requires a recognized TAS-Auth token")
		return
	}
	w.Header().Set("Content-Type", "application/json")
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

	if isAnthropicWirePath(r) {
		writeAnthropicWireError(w, http.StatusForbidden, "permission_error", "AIQG account is currently suspended; contact support")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":{"code":"account_suspended","message":"AIQG account is currently suspended; contact support"}}`))
}

// writeResolverError emits 503 for resolver-side failures (e.g. the
// production backend resolver can't reach the token store). 503 instead
// of 500 because the failure is upstream-dependency unavailability, and
// callers should retry rather than treat the request as permanently
// broken.
func writeResolverError(w http.ResponseWriter, r *http.Request) {
	if isAnthropicWirePath(r) {
		writeAnthropicWireError(w, http.StatusServiceUnavailable, "api_error", "AIQG token resolver is temporarily unavailable; retry")
		return
	}
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

// Flush delegates to the wrapped ResponseWriter when it implements
// http.Flusher. Streaming response handlers (chat completions with
// stream=true) type-assert the ResponseWriter to http.Flusher and
// panic when the assertion fails. Without this delegation the AIQG
// middleware silently broke all streaming traffic — confirmed via
// a "interface conversion: *statusCapturingResponseWriter is not
// http.Flusher" panic surfaced in Loki on 2026-06-09.
//
// Net/http's chunked-encoding path requires Flush to push bytes onto
// the wire; in streaming mode every chunk handler calls Flush after
// each Write. Without this, chunks buffer until the response ends —
// turning a streaming response back into a non-streaming one (or
// panicking with an unchecked type assertion).
func (w *statusCapturingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
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

	w.Header().Set("WWW-Authenticate", `TAS realm="aiqg"`)
	if isAnthropicWirePath(r) {
		writeAnthropicWireError(w, http.StatusUnauthorized, "authentication_error", fmt.Sprintf("AIQG ingress requires authentication; %s is missing", missing))
		return
	}
	w.Header().Set("Content-Type", "application/json")
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

	if isAnthropicWirePath(r) {
		writeAnthropicWireError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)

	body := fmt.Sprintf(`{"error":{"code":"aiqg_header_invalid","message":%q}}`, err.Error())
	_, _ = w.Write([]byte(body))
}

// ResolvedTarget returns the provider/model a route rule steers this request
// to, or nil when no rule set one. Read at routing time, after
// ResolveBundleForRouting has refined the resolution with model + workflow.
//
// Exported so the completion handler can pin the router without importing the
// middleware's internal holder type.
func ResolvedTarget(ctx context.Context) *policy.Target {
	holder, _ := ctx.Value(bundleResolutionCtxKey{}).(*bundleResolutionHolder)
	if holder == nil {
		return nil
	}
	return holder.get().Target
}

// ResolvedResilience returns the matched rule's health/budgets overrides, or
// nils when no rule set any.
func ResolvedResilience(ctx context.Context) (*resilience.Health, *resilience.Budgets) {
	holder, _ := ctx.Value(bundleResolutionCtxKey{}).(*bundleResolutionHolder)
	if holder == nil {
		return nil, nil
	}
	res := holder.get()
	return res.Health, res.Budgets
}

// ResolvedEnforcement returns the bundle's enforcement mode and its rule
// actions keyed by pattern_id.
//
// Defaults to observe with no rules, so an unresolved or unconfigured bundle
// enforces nothing — the safe direction, since an operator who has not chosen
// enforcement has not consented to it.
func ResolvedEnforcement(ctx context.Context) (resilience.Mode, map[string]string) {
	holder, _ := ctx.Value(bundleResolutionCtxKey{}).(*bundleResolutionHolder)
	if holder == nil {
		return resilience.ModeObserve, nil
	}
	res := holder.get()
	mode := resilience.ModeObserve
	if res.Enforcement != nil && res.Enforcement.Mode != "" {
		mode = res.Enforcement.Mode
	}
	return mode, res.RuleActions
}

// ResolvedLimits returns the matched rule's context/output caps.
func ResolvedLimits(ctx context.Context) resilience.Limits {
	holder, _ := ctx.Value(bundleResolutionCtxKey{}).(*bundleResolutionHolder)
	if holder == nil {
		return resilience.Limits{}
	}
	if l := holder.get().Limits; l != nil {
		return *l
	}
	return resilience.Limits{}
}

// ResolvedSignals returns the matched rule's quality floors and the measured
// evidence they gate against.
func ResolvedSignals(ctx context.Context) (resilience.Signals, []resilience.QualitySignal) {
	holder, _ := ctx.Value(bundleResolutionCtxKey{}).(*bundleResolutionHolder)
	if holder == nil {
		return resilience.Signals{}, nil
	}
	res := holder.get()
	var sig resilience.Signals
	if res.Signals != nil {
		sig = *res.Signals
	}
	return sig, res.Quality
}

// ResolvedSelection returns the matched rule's selection and switching blocks
// plus the measured verbosity table, or zero values when no rule set them.
func ResolvedSelection(ctx context.Context) (resilience.Selection, resilience.Switching, []resilience.Verbosity) {
	holder, _ := ctx.Value(bundleResolutionCtxKey{}).(*bundleResolutionHolder)
	if holder == nil {
		return resilience.Selection{}, resilience.Switching{}, nil
	}
	res := holder.get()
	var sel resilience.Selection
	var sw resilience.Switching
	if res.Selection != nil {
		sel = *res.Selection
	}
	if res.Switching != nil {
		sw = *res.Switching
	}
	return sel, sw, res.Verbosity
}

// ResolvedAffinity returns the matched rule's affinity block, or nil when no
// rule configured one.
func ResolvedAffinity(ctx context.Context) *resilience.Affinity {
	holder, _ := ctx.Value(bundleResolutionCtxKey{}).(*bundleResolutionHolder)
	if holder == nil {
		return nil
	}
	return holder.get().Affinity
}

// ResolvedControls returns the tenant's per-tenant feature switches, or nil
// when the resolution carried none (the tenant expressed no preference and the
// gateway env defaults stand).
func ResolvedControls(ctx context.Context) *resilience.Controls {
	holder, _ := ctx.Value(bundleResolutionCtxKey{}).(*bundleResolutionHolder)
	if holder == nil {
		return nil
	}
	return holder.get().Controls
}

// ResolvedFallback returns the matched rule's failover chain and the tenant's
// constraints. Constraints may be present with no chain — they bound routing
// whether or not a rule configured failover.
func ResolvedFallback(ctx context.Context) (*resilience.Fallback, *resilience.Constraints) {
	holder, _ := ctx.Value(bundleResolutionCtxKey{}).(*bundleResolutionHolder)
	if holder == nil {
		return nil, nil
	}
	res := holder.get()
	return res.Fallback, res.Constraints
}

// toEventExclusions converts the middleware's local gate-exclusion shape to the
// event schema's. The duplication is deliberate — see GateExclusion.
func toEventExclusions(in []GateExclusion) []events.ExcludedCandidate {
	if len(in) == 0 {
		return nil
	}
	out := make([]events.ExcludedCandidate, len(in))
	for i, e := range in {
		out[i] = events.ExcludedCandidate{
			Provider: e.Provider, Model: e.Model,
			Dimension: e.Dimension, Reason: e.Reason,
		}
	}
	return out
}

// toEventFindings converts the sidecar's local finding shape to the event
// schema's. Duplication is deliberate — see ScanFinding.
func toEventFindings(in []ScanFinding) []events.Finding {
	if len(in) == 0 {
		return nil
	}
	out := make([]events.Finding, len(in))
	for i, f := range in {
		out[i] = events.Finding{
			PatternID: f.PatternID, Severity: f.Severity, Direction: f.Direction,
			FieldPath: f.FieldPath, Offset: f.Offset, Length: f.Length,
			ValuePreview: f.ValuePreview, ValueHash: f.ValueHash,
			Confidence: f.Confidence, Redacted: f.Redacted, Frameworks: f.Frameworks,
		}
	}
	return out
}

// WithResolvedEnforcementForTest injects an enforcement decision without a full
// policy resolution. Test-only: the production path always comes from resolve.
func WithResolvedEnforcementForTest(ctx context.Context, mode resilience.Mode, actions map[string]string) context.Context {
	h := &bundleResolutionHolder{}
	h.set(policy.Resolution{
		Enforcement: &resilience.Enforcement{Mode: mode},
		RuleActions: actions,
	})
	return context.WithValue(ctx, bundleResolutionCtxKey{}, h)
}
