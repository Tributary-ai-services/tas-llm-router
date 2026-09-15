package server

import (
	"context"
	"net/http"
	"net/url"

	"github.com/tributary-ai/llm-router-waf/internal/instrumentation"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
)

// evalAttribution identifies the request an internal evaluation call is ABOUT.
//
// The gateway's judge and shadow-replay calls are made on its own behalf, with
// no inbound HTTP request and no tenant on the call path — routerCompletion is
// a single shared adapter, constructed once at startup. Metrics solved half of
// this in #184 by counting spend globally, but deliberately carry no tenant
// label: per-tenant cardinality belongs on the event path, not in a metric
// whose series count would track customer count.
//
// So attribution travels on the context, which the judge package passes
// through untouched. That keeps judge.Completion's interface unchanged — the
// alternative was widening a seam with two implementers for the benefit of
// one.
type evalAttribution struct {
	TenantID      string
	AIQGAccountID string
	TokenID       string
	SourceApp     string

	// ParentEventID is the response event this evaluation scored. It becomes
	// the emitted event's parent_step_id, so eval spend can be joined back to
	// the request that caused it.
	ParentEventID string

	ExperimentID      string
	ExperimentVariant string
	Workflow          string

	// Path is metrics.SpendPathJudge or metrics.SpendPathShadowReplay — the
	// same label the spend counters use, so metric and event agree.
	Path string
}

type evalAttributionKey struct{}

// withEvalAttribution returns a context carrying a for the evaluation calls
// made under it.
func withEvalAttribution(ctx context.Context, a *evalAttribution) context.Context {
	if a == nil {
		return ctx
	}
	return context.WithValue(ctx, evalAttributionKey{}, a)
}

// evalAttributionFrom recovers the attribution, or nil when the call was not
// made under one (in which case the event is skipped rather than emitted with
// an empty tenant — an unattributed event is worse than no event, because it
// silently pollutes per-tenant totals with a blank bucket).
func evalAttributionFrom(ctx context.Context) *evalAttribution {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(evalAttributionKey{}).(*evalAttribution)
	return a
}

// evalRequestPath is the synthetic endpoint recorded on evaluation events. It
// is not a route the server serves; it exists so the events are filterable as
// a group and never collide with a real customer endpoint.
const evalRequestPath = "/internal/aiqg/eval"

// syntheticEvalRequest builds the minimal *http.Request events.Build needs.
//
// Build reads only Method, URL.Path, RemoteAddr, and three headers from the
// request — everything else it needs comes from the view structs. Synthesizing
// it is far less invasive than widening Build's signature, which every real
// request also flows through.
func syntheticEvalRequest() *http.Request {
	return &http.Request{
		Method: http.MethodPost,
		URL:    &url.URL{Path: evalRequestPath},
		Header: http.Header{},
	}
}

// emitEvalEvent emits one AIQG event for a completed evaluation call, giving
// the gateway's own spend the same per-tenant attribution customer traffic has.
//
// Marked synthetic so it never inflates customer usage or quality aggregates:
// exclusion downstream gates on Data.Synthetic, and the reason records WHICH
// kind of non-customer traffic this is. Cost is computed by events.Build from
// the same pkg/clear pricing table the billing figures use, so the event and
// the aiqg_unbilled_spend_usd_total counter cannot disagree about rates.
//
// Best-effort and off the hot path: a failed emit costs telemetry, never the
// evaluation itself, and never the customer request that triggered it.
func (jr *judgeRunner) emitEvalEvent(ctx context.Context, a *evalAttribution, vendor, model string, u *types.Usage, finishReason string) {
	if jr == nil || jr.emitter == nil || a == nil || a.TenantID == "" {
		return
	}

	routingView := events.RoutingView{
		Vendor:       vendor,
		Model:        model,
		Streaming:    false,
		StreamingSet: true,
		FinishReason: finishReason,
	}
	if u != nil {
		routingView.PromptTokens = u.PromptTokens
		routingView.CompletionTokens = u.CompletionTokens
		routingView.UsageSet = true
		routingView.CacheCreationTokens = u.CacheCreationTokens
		routingView.CacheReadTokens = u.CacheReadTokens
	}

	reqEnv, respEnv := events.Build(
		syntheticEvalRequest(),
		events.AIQGHeadersView{
			SourceApp: a.SourceApp,
			Synthetic: true,
			Workflow:  a.Workflow,
		},
		routingView,
		events.TokenView{
			TenantID:       a.TenantID,
			AIQGAccountID:  a.AIQGAccountID,
			TASAuthTokenID: a.TokenID,
			SourceApp:      a.SourceApp,
		},
		instrumentation.Snapshot{},
		events.BuildOptions{
			HTTPStatus:        http.StatusOK,
			Region:            jr.region,
			IPCaptureMode:     "off", // no client to attribute — nothing to capture
			FinishReason:      finishReason,
			ExperimentID:      a.ExperimentID,
			ExperimentVariant: a.ExperimentVariant,
			// ParentStepID only. Linked stays false: it means evidence matched
			// (an echoed tool_call_id), and asserting it here would promote
			// identity_source to "linked" on a call that proved no such thing.
			// Build assigns the step fields unconditionally, so the parent
			// reference lands without that claim.
			Linkage: events.Linkage{ParentStepID: a.ParentEventID},
		},
	)

	// Build hardcodes the reason to "declared" whenever Synthetic is set,
	// which is right for customer traffic that declared itself via header and
	// wrong here: nobody declared this, the gateway originated it. Overriding
	// after the fact keeps that rule intact for the path every real request
	// takes.
	respEnv.Data.SyntheticReason = events.SyntheticGatewayEval

	if err := jr.emitter.Emit(ctx, reqEnv, respEnv); err != nil {
		metrics.EvalEventsFailedTotal.WithLabelValues(a.Path).Inc()
		jr.log.WithError(err).Debug("aiqg eval: event emission failed")
		return
	}
	metrics.EvalEventsTotal.WithLabelValues(a.Path).Inc()
}
