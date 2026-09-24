package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/routing"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/credentials"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/experiments"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/judge"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/tokens"
	"github.com/tributary-ai/llm-router-waf/pkg/clear"
)

// judgeRunner wires the LLM-as-judge quality layer (§6.6) into the gateway:
// it samples completed AIQG responses, scores them off the hot path with a
// third model, and records the score to aiqg-dashboard-be. Nil when judging
// is disabled (no JudgeModel / pct<=0) — maybeJudge is then a no-op.
type judgeRunner struct {
	judge       *judge.Judge
	samplePct   int
	shadowPct   int                   // % of control-arm experiment samples to shadow-eval (0 = off)
	experiments *experiments.Resolver // source of variant overrides for the replay
	router      *routing.Router       // for the offline variant replay
	recorder    *judgeRecorder
	// emitter gives evaluation spend the per-tenant attribution metrics
	// deliberately cannot carry (#184). Nil disables event emission without
	// disabling judging — the counters still fire.
	emitter events.Emitter
	region  string
	// creds resolves the tenant's own BYOK vendor key so an evaluation bills
	// the customer whose traffic prompted it, rather than the gateway's shared
	// key. Nil (BYOK unconfigured) means every eval uses the configured key,
	// which is the pre-#184 behaviour.
	creds *credentials.Resolver
	log   *logrus.Logger
}

// newJudgeRunner builds the runner, or returns nil when judging is off / the
// dashboard isn't configured (nowhere to record). shadowPct>0 + an experiments
// resolver enable pairwise shadow-eval (§6.3) on top of the pointwise judge.
func newJudgeRunner(router *routing.Router, model string, samplePct, shadowPct int, exp *experiments.Resolver, dashboardURL, internalAuth string, emitter events.Emitter, region string, creds *credentials.Resolver, log *logrus.Logger) *judgeRunner {
	if model == "" || samplePct <= 0 || dashboardURL == "" || internalAuth == "" {
		return nil
	}
	jr := &judgeRunner{
		samplePct:   samplePct,
		shadowPct:   shadowPct,
		experiments: exp,
		router:      router,
		recorder:    &judgeRecorder{http: &http.Client{Timeout: 5 * time.Second}, baseURL: strings.TrimRight(dashboardURL, "/"), auth: internalAuth},
		emitter:     emitter,
		region:      region,
		creds:       creds,
		log:         log,
	}
	// The adapter needs the runner back so a completed judge call can emit its
	// own attributed event; the runner owns the emitter and the region.
	jr.judge = &judge.Judge{LLM: &routerCompletion{router: router, runner: jr}, Model: model}
	return jr
}

// sampled returns true for the fraction of events to judge — deterministic on
// the response event id (uniform, stable, no RNG state).
func (jr *judgeRunner) sampled(eventID string) bool {
	if jr.samplePct >= 100 {
		return true
	}
	return int(crc32.ChecksumIEEE([]byte("judge:"+eventID))%100) < jr.samplePct
}

// shadowSampled is the (separate, usually smaller) shadow-eval sample — it
// replays + pairwise-judges, so it costs ~2× per sampled request.
//
// pct is the EFFECTIVE rate for one experiment: its declared
// guardrails.shadow_eval_pct when it set one, else the gateway-wide default.
// Taking it as an argument rather than reading jr.shadowPct is what makes the
// consent per-experiment (#184) — a shadow replay spends the tenant's money on
// an evaluation they never directly asked for, and the experiment that
// declares a rate is the thing that authorizes it.
func shadowSampled(eventID string, pct int) bool {
	if pct <= 0 {
		return false
	}
	if pct >= 100 {
		return true
	}
	return int(crc32.ChecksumIEEE([]byte("shadow:"+eventID))%100) < pct
}

// maybeJudge fires an async judge for a sampled, AIQG-attributed, non-streaming
// response. Strictly off the hot path: the client already has its response; a
// judge failure only means no score lands. Skips when judging is off, the
// request isn't AIQG-attributed (no tenant), or the response has no text.
func (jr *judgeRunner) maybeJudge(ctx context.Context, w http.ResponseWriter, req *types.ChatRequest, resp *types.ChatResponse) {
	if jr == nil || req == nil || resp == nil {
		return
	}
	// Each early return below removes a particular KIND of response from the
	// judged population, so each is counted (#184). Random sampling is
	// unbiased and stays uncounted; these are not.
	eventID := w.Header().Get("TAS-Response-Event-Id")
	if eventID == "" {
		metrics.JudgeExcludedTotal.WithLabelValues(metrics.JudgeExcludedNoEventID).Inc()
		return
	}
	tok := tokens.FromContext(ctx)
	if tok == nil {
		metrics.JudgeExcludedTotal.WithLabelValues(metrics.JudgeExcludedNotAttributed).Inc()
		return // not AIQG-attributed — no tenant to scope the score
	}
	responseText := extractResponseContent(resp)
	if strings.TrimSpace(responseText) == "" {
		metrics.JudgeExcludedTotal.WithLabelValues(metrics.JudgeExcludedEmpty).Inc()
		return // tool-call-only / empty — nothing semantic to judge
	}
	// vendor/model are the JOIN KEY, not decoration. Judge scores are stored by
	// aiqg-dashboard-be in the config Postgres; the (model, workflow) a response
	// belongs to lives in aiqg.event_metrics on TimescaleDB. Those are separate
	// servers, so no cross-database join exists and the judged row has to carry
	// the model itself or it can never be aggregated per candidate.
	//
	// Taken from the routing snapshot rather than resp.Model because
	// event_metrics.model is written from exactly this field
	// (pkg/aiqg/events/builder.go:711). A different source would join on a
	// value that usually matches, which is worse than one that never does.
	workflow, expID, variant, vendor, model := "", "", "", "", ""
	if r := middleware.RoutingFromContext(ctx); r != nil {
		snap := r.Snapshot()
		workflow, expID, variant = snap.Workflow, snap.ExperimentID, snap.ExperimentVariant
		vendor, model = snap.Vendor, snap.Model
	}
	// The judge grading its own output. pkg/aiqg/judge's package doc says the
	// caller enforces this ("the judge model MUST differ from the model that
	// produced the response") and no caller ever did — measured 2026-09-24,
	// 179 of 464 judged rows were self-graded, because AIQG_JUDGE_MODEL is set
	// to the model this tenant serves most.
	//
	// Recorded rather than skipped. The score is still worth having on a
	// dashboard, and the routing aggregate is where the conflict of interest
	// actually bites: a candidate that grades itself would be voting on its own
	// eligibility. dashboard-be excludes these from efficacy_judged.
	selfJudged := model != "" && model == jr.judge.Model
	promptText := promptFromMessages(req)
	tenantID := tok.TenantID

	// Attribution for whatever evaluation calls follow. Captured here because
	// this is the last point that still has the customer's token and the
	// response event id; the adapter that actually bills has neither.
	attr := &evalAttribution{
		TenantID:          tok.TenantID,
		AIQGAccountID:     tok.AIQGAccountID,
		TokenID:           tok.TokenID,
		SourceApp:         tok.SourceApp,
		ParentEventID:     eventID,
		ExperimentID:      expID,
		ExperimentVariant: variant,
		Workflow:          workflow,
		Path:              metrics.SpendPathJudge,
	}

	// Pointwise judge on the sampled fraction.
	if jr.sampled(eventID) {
		go func() {
			jctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			jctx = withEvalAttribution(jctx, attr)
			score, err := jr.judge.Score(jctx, workflow, promptText, responseText)
			if errors.Is(err, errEvalSkippedBYOK) {
				// A refusal, not a failure. The tenant is BYOK-only with no
				// stored key, so nothing was called and nothing billed;
				// logging it as a scoring failure would make a policy the
				// gateway correctly respected read as a bug. Already counted
				// as an exclusion at the decision point.
				return
			}
			if err != nil {
				jr.log.WithError(err).Debug("aiqg judge: scoring failed")
				return
			}
			if score.Abstain {
				return
			}
			// The gateway knows the experiment/variant (routing snapshot), so
			// the score carries its own attribution — no event re-resolution.
			if err := jr.recorder.record(jctx, tenantID, eventID, expID, variant,
				gradedSubject{Vendor: vendor, Model: model, JudgeModel: jr.judge.Model, SelfJudged: selfJudged}, score); err != nil {
				jr.log.WithError(err).Debug("aiqg judge: record failed")
			}
		}()
	}

	// Pairwise shadow-eval (§6.3): for a CONTROL-arm sample of an experiment,
	// replay the same prompt through each variant offline and judge head-to-
	// head. Zero user impact (the client already has control's response);
	// ~2× cost on the shadow sample. Pairs with dry_run (everyone is control).
	// The sampling gate moved INTO shadowEval: the effective rate is the
	// experiment's own declared guardrail, which is only known after the
	// resolver lookup. That lookup is a TTL-cached read on a goroutine off the
	// hot path, so doing it before the sample decision costs nothing the
	// customer can feel.
	if expID != "" && variant == "control" && jr.experiments != nil {
		msgs := append([]types.Message(nil), req.Messages...) // snapshot for the goroutine
		baseModel := req.Model
		// Carry control's own cap and usage: the cap so the variant is judged
		// under the same limit (#182), the usage so the recorded comparison is
		// a paired cost sample rather than a preference with no price (#183).
		go jr.shadowEval(attr, promptText, responseText, msgs, baseModel,
			cloneIntPtr(req.MaxTokens), resp.Usage)
	}
}

// replayResult is what one variant replay produced: the text the judge
// compares, and the economics of the call that produced it.
//
// The economics are not a byproduct. A shadow replay is a live, billed call on
// the customer's own prompt, so its token counts are the cleanest cost
// evidence available anywhere in the system — a paired sample against control
// on identical input, which no windowed aggregate can match. Before #183 they
// were read for the text and dropped on the floor.
type replayResult struct {
	Text         string
	Vendor       string // the provider that billed it — half of the pricing key
	Model        string // what the variant actually resolved to after the override
	Usage        *types.Usage
	FinishReason string
	Truncated    bool // the provider stopped at the cap — the comparison is suspect
}

// shadowEval replays the control prompt through each non-control variant and
// pairwise-judges control vs variant, recording a per-variant preference and
// the measured cost of both arms. Best-effort; off the hot path.
func (jr *judgeRunner) shadowEval(attr *evalAttribution, prompt, controlResp string, msgs []types.Message, baseModel string, controlMaxTokens *int, controlUsage *types.Usage) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	eventID, tenantID, expID, workflow := attr.ParentEventID, attr.TenantID, attr.ExperimentID, attr.Workflow

	// The pairwise comparison is itself a judge call, so it attributes under
	// the judge path. The replay below is a different kind of spend and gets
	// its own path — one ctx for both would file half this function's cost
	// under the wrong label.
	judgeCtx := withEvalAttribution(ctx, attr)

	variants, declaredPct := jr.experiments.ShadowEvalPlan(ctx, tenantID, expID)

	// An experiment that declares a rate authorizes its own shadow spend; one
	// that doesn't inherits the gateway-wide default, so an existing
	// deployment keeps behaving exactly as it did. Deliberately NOT
	// min(declared, global): the global defaults to 0, so that reading would
	// make every declared rate permanently inert and the guardrail
	// undeployable without changing two knobs at once.
	pct := jr.shadowPct
	if declaredPct != nil {
		pct = *declaredPct
	}
	if !shadowSampled(eventID, pct) {
		return
	}
	for _, v := range variants {
		// Built BEFORE the call, because it now decides whose key pays as well
		// as who gets billed. attr carries "control" — shadow-eval only fires
		// on the control arm — but the billed call is the variant's, and
		// filing it under control would credit the baseline with spend it
		// never incurred.
		replayAttr := *attr
		replayAttr.Path = metrics.SpendPathShadowReplay
		replayAttr.ExperimentVariant = v.Key

		rr, err := jr.replay(withEvalAttribution(ctx, &replayAttr), msgs, baseModel, controlMaxTokens, v.Override)
		// Count the spend before anything can discard the result: an abstaining
		// judge costs exactly what an agreeing one does, and a spend figure
		// that counts only successes understates the bill in the one direction
		// nobody notices.
		jr.countReplay(rr)
		if rr.Usage != nil {
			jr.emitEvalEvent(ctx, &replayAttr, rr.Vendor, rr.Model, rr.Usage, rr.FinishReason)
		}
		// A BYOK-only tenant with no stored key is a refusal, not a failure:
		// nothing was called and nothing billed, so counting it as a failed
		// replay would make a policy the gateway correctly respected look like
		// a bug it has.
		if errors.Is(err, errEvalSkippedBYOK) {
			continue
		}
		if err != nil || strings.TrimSpace(rr.Text) == "" {
			metrics.ShadowReplaysTotal.WithLabelValues(metrics.ShadowReplayFailed).Inc()
			jr.log.WithError(err).Debug("aiqg shadow-eval: replay failed")
			continue
		}
		// Randomize A/B order per the bias control (§6.6).
		variantFirst := crc32.ChecksumIEEE([]byte(eventID+":"+v.Key))%2 == 0
		pw, err := jr.judge.ScorePairwise(judgeCtx, workflow, prompt, controlResp, rr.Text, variantFirst)
		if err != nil {
			metrics.ShadowReplaysTotal.WithLabelValues(metrics.ShadowJudgeFailed).Inc()
			continue
		}
		if pw.Abstain {
			metrics.ShadowReplaysTotal.WithLabelValues(metrics.ShadowJudgeAbstain).Inc()
			continue
		}
		if err := jr.recorder.recordPairwise(ctx, tenantID, eventID, expID, v.Key, workflow, pw, rr, controlUsage); err != nil {
			metrics.ShadowReplaysTotal.WithLabelValues(metrics.ShadowRecordFailed).Inc()
			jr.log.WithError(err).Debug("aiqg shadow-eval: record failed")
			continue
		}
		metrics.ShadowReplaysTotal.WithLabelValues(metrics.ShadowRecorded).Inc()
	}
}

// countReplay records what a replay billed, whatever becomes of its result.
func (jr *judgeRunner) countReplay(rr replayResult) {
	if rr.Usage != nil {
		metrics.ShadowTokensTotal.WithLabelValues("input").Add(float64(rr.Usage.PromptTokens))
		metrics.ShadowTokensTotal.WithLabelValues("output").Add(float64(rr.Usage.CompletionTokens))
		countSpend(metrics.SpendPathShadowReplay, rr.Vendor, rr.Model, rr.Usage)
	}
	if rr.Truncated {
		metrics.ShadowTruncatedTotal.Inc()
	}
}

// countSpend prices one gateway-initiated evaluation call and adds it to the
// unbilled-spend total (#184).
//
// Tokens were already counted by the caller; this converts them to money,
// which a token count cannot stand in for — the judge and the replay run
// different models whose rates differ by more than an order of magnitude, so
// summing their tokens produces a number that is not a cost.
//
// When the vendor:model is absent from the pricing table the call is recorded
// as unpriced rather than skipped silently. That distinction matters: a
// missing pricing row otherwise makes evaluation look free instead of
// unmeasured, and the spend total would sit flat with nothing to indicate it
// is an undercount.
func countSpend(path, vendor, model string, u *types.Usage) {
	cost, ok := clear.DollarCost(vendor, model, u.PromptTokens, u.CompletionTokens)
	if !ok {
		metrics.UnpricedCallsTotal.WithLabelValues(path).Inc()
		return
	}
	metrics.UnbilledSpendUSDTotal.WithLabelValues(path).Add(cost)
}

// effectiveModel is the model that actually ran, which is what pricing must be
// keyed on. The registry may resolve an alias or fall back off a deprecated
// model (epic #2), in which case the requested name and the billed name are
// different strings — and pricing the requested one would silently mis-price
// every aliased call, or miss the table entirely and count it as unpriced.
func effectiveModel(meta *types.RouterMetadata, requested string) string {
	if meta != nil && meta.ResolvedModel != "" {
		return meta.ResolvedModel
	}
	return requested
}

// replay runs the control prompt through a variant's override offline and
// returns the variant's response together with its usage. Uses the configured
// provider key (no customer key) and bypasses the AIQG middleware, same as the
// judge call — see tas-llm-router#184 for the accounting and key-selection
// consequences of that bypass, which this function does not resolve.
func (jr *judgeRunner) replay(ctx context.Context, msgs []types.Message, baseModel string, controlMaxTokens *int, override json.RawMessage) (replayResult, error) {
	// Mirror the control request's cap rather than imposing one of our own
	// (#182). Control answered under the caller's limit; a variant capped at
	// some constant of ours is cut off mid-thought and then judged against a
	// COMPLETE control response — so the comparison measures our replay cap,
	// and measures it worse the more verbose the candidate model is, which is
	// backwards when the point is to find a cheaper model that still holds.
	// A nil cap stays nil: the provider default is what control got too.
	//
	// A variant's own override may still set max_tokens below, and should win
	// — a declared parameter is part of what the experiment is testing.
	req := &types.ChatRequest{Model: baseModel, Messages: msgs, MaxTokens: cloneIntPtr(controlMaxTokens)}
	applyExperimentOverride(req, override) // swaps model / params to the variant
	meta, provider, err := jr.router.Route(ctx, req)
	if err != nil {
		return replayResult{}, err
	}
	// Same key decision as the judge path: a replay is billed spend too, and a
	// BYOK-only tenant without a stored key must not have the gateway's shared
	// key spent on their behalf.
	dec := jr.resolveEvalKey(ctx, evalAttributionFrom(ctx), provider.GetProviderName())
	if dec.skip {
		return replayResult{}, errEvalSkippedBYOK
	}
	ctx = dec.ctx
	resp, err := provider.ChatCompletion(ctx, req)
	if err != nil {
		return replayResult{}, err
	}
	out := replayResult{
		Text:         extractResponseContent(resp),
		Vendor:       provider.GetProviderName(),
		Model:        effectiveModel(meta, req.Model),
		Usage:        resp.Usage,
		FinishReason: finishReasonOf(resp),
	}
	out.Truncated = out.FinishReason == "length"
	return out, nil
}

// cloneIntPtr copies a caller-owned pointer so the replay cannot alias — and
// therefore cannot be mutated through — a request the client still holds.
func cloneIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// finishReasonOf returns the first choice's finish reason, which is what
// distinguishes "the model stopped" from "we cut it off".
func finishReasonOf(resp *types.ChatResponse) string {
	if resp == nil || len(resp.Choices) == 0 {
		return ""
	}
	return resp.Choices[0].FinishReason
}

// promptFromMessages flattens the request's user/system turns to text for the
// judge (the question being answered). Tool/assistant turns are omitted —
// the judge scores the latest response against the ask.
func promptFromMessages(req *types.ChatRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role != "user" && m.Role != "system" {
			continue
		}
		if s, ok := m.Content.(string); ok && s != "" {
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// routerCompletion adapts the gateway's router to judge.Completion: it routes
// a one-shot request for the judge model and returns the text. Bypasses the
// AIQG HTTP middleware entirely (internal call), so judging never recurses and
// uses the gateway's configured provider key (not a customer key).
//
// It is also where judge spend is accounted (#184). This adapter, not the
// scoring call, is the accounting point: every judge call — pointwise Score and
// pairwise ScorePairwise alike — passes through here, and it runs before the
// judge can decide the result is unusable. An abstaining judge, an
// unparseable reply and a failed record all bill exactly what a clean score
// does, so accounting further up would silently omit them.
type routerCompletion struct {
	router *routing.Router
	// runner carries the emitter + region so a completed call can emit an
	// attributed event. The attribution itself rides the context, since this
	// adapter is constructed once at startup and shared by every tenant.
	runner *judgeRunner
}

func (rc *routerCompletion) Complete(ctx context.Context, model, system, user string) (string, error) {
	maxTokens := 400
	req := &types.ChatRequest{
		Model:     model,
		MaxTokens: &maxTokens,
		Messages: []types.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}
	meta, provider, err := rc.router.Route(ctx, req)
	if err != nil {
		metrics.JudgeCallsTotal.WithLabelValues(metrics.JudgeRouteFailed).Inc()
		return "", fmt.Errorf("judge route: %w", err)
	}
	// Whose key pays for this call. Resolved only now, because the key is
	// per-(tenant, vendor) and the vendor is not known until Route has picked
	// a provider. Returning before ChatCompletion means a skip never counts as
	// a provider failure — nothing was called.
	dec := rc.runner.resolveEvalKey(ctx, evalAttributionFrom(ctx), provider.GetProviderName())
	if dec.skip {
		return "", errEvalSkippedBYOK
	}
	ctx = dec.ctx
	resp, err := provider.ChatCompletion(ctx, req)
	if err != nil {
		// No usage is recoverable from a failed call, so this counts as an
		// attempt only. A provider that bills partial work on error would be
		// invisible here — an accepted floor, not a claim of completeness.
		metrics.JudgeCallsTotal.WithLabelValues(metrics.JudgeProviderFailed).Inc()
		return "", fmt.Errorf("judge completion: %w", err)
	}
	vendor, billedModel := provider.GetProviderName(), effectiveModel(meta, req.Model)
	countJudge(vendor, billedModel, resp.Usage)
	// Per-tenant attribution for the same spend the counters just recorded.
	rc.runner.emitEvalEvent(ctx, evalAttributionFrom(ctx), vendor, billedModel, resp.Usage, finishReasonOf(resp))
	return extractResponseContent(resp), nil
}

// countJudge records what one judge call billed. Usage comes from the
// provider's own report; its absence is counted as a distinct outcome rather
// than as zero tokens, so a provider that stops reporting usage surfaces as a
// gap instead of as a fall in spend.
func countJudge(vendor, model string, u *types.Usage) {
	if u == nil {
		metrics.JudgeCallsTotal.WithLabelValues(metrics.JudgeCompletedNoUsage).Inc()
		return
	}
	metrics.JudgeCallsTotal.WithLabelValues(metrics.JudgeCompleted).Inc()
	metrics.JudgeTokensTotal.WithLabelValues("input").Add(float64(u.PromptTokens))
	metrics.JudgeTokensTotal.WithLabelValues("output").Add(float64(u.CompletionTokens))
	countSpend(metrics.SpendPathJudge, vendor, model, u)
}

// judgeRecorder POSTs a judge score to aiqg-dashboard-be's internal ingest,
// which resolves the event → tenant/experiment/variant and stores it as a
// judge feedback row.
type judgeRecorder struct {
	http    *http.Client
	baseURL string
	auth    string
}

// gradedSubject is the provenance of one judged score: what was graded, and by
// whom.
//
// Vendor/Model are the join key (see maybeJudge). JudgeModel is what makes a
// grader swap measurable: judge.RubricVersion versions the RUBRIC, not the
// grader, so two models scoring under "v1" produce rows that look comparable
// and are not. Without the grader on the row there is no way to compare across
// a swap, re-score an old window, or show that a replacement judge beat the one
// it replaced — and that comparison cannot be reconstructed later, because
// nothing else records which model did the grading.
type gradedSubject struct {
	Vendor     string
	Model      string
	JudgeModel string
	SelfJudged bool
}

// record posts a pointwise judge score (signal_type=judge, value=overall).
func (jr *judgeRecorder) record(ctx context.Context, tenantID, eventID, experimentID, variant string, g gradedSubject, s judge.Score) error {
	return jr.post(ctx, map[string]any{
		"signal_type":        "judge",
		"tenant_id":          tenantID,
		"response_event_id":  eventID,
		"experiment_id":      experimentID,
		"experiment_variant": variant,
		"workflow":           s.Workflow,
		"overall":            s.Overall,
		"dimensions":         s.Dimensions,
		"rubric_version":     s.RubricVersion,
		"vendor":             g.Vendor,
		"model":              g.Model,
		"judge_model":        g.JudgeModel,
		"self_judged":        g.SelfJudged,
	})
}

// recordPairwise posts a shadow-eval result (signal_type=judge_pairwise,
// value=variant preference 0/0.5/1) attributed to the NON-control variant,
// together with the economics of both arms (#183).
//
// The preference alone answers "which answer is better?" and cannot answer
// "was it cheaper?" — even though the replay just measured exactly that. The
// two arms answered an identical prompt, so these token counts are a paired
// sample: the strongest cost comparison obtainable, and one that a windowed
// average over differently-shaped requests cannot reproduce.
//
// `variant_truncated` travels with it so a comparison distorted by a token cap
// can be excluded downstream rather than quietly averaged in.
func (jr *judgeRecorder) recordPairwise(ctx context.Context, tenantID, eventID, experimentID, variant, workflow string, pw judge.PairwiseResult, rr replayResult, controlUsage *types.Usage) error {
	shadow := map[string]any{
		"variant_model":         rr.Model,
		"variant_finish_reason": rr.FinishReason,
		"variant_truncated":     rr.Truncated,
	}
	if rr.Usage != nil {
		shadow["variant_input_tokens"] = rr.Usage.PromptTokens
		shadow["variant_output_tokens"] = rr.Usage.CompletionTokens
		if rr.Usage.CacheReadTokens > 0 {
			shadow["variant_cache_read_tokens"] = rr.Usage.CacheReadTokens
		}
		if rr.Usage.CacheCreationTokens > 0 {
			shadow["variant_cache_creation_tokens"] = rr.Usage.CacheCreationTokens
		}
	}
	if controlUsage != nil {
		shadow["control_input_tokens"] = controlUsage.PromptTokens
		shadow["control_output_tokens"] = controlUsage.CompletionTokens
		if controlUsage.CacheReadTokens > 0 {
			shadow["control_cache_read_tokens"] = controlUsage.CacheReadTokens
		}
	}
	return jr.post(ctx, map[string]any{
		"signal_type":        "judge_pairwise",
		"tenant_id":          tenantID,
		"response_event_id":  eventID,
		"experiment_id":      experimentID,
		"experiment_variant": variant,
		"workflow":           workflow,
		"overall":            pw.VariantPreference,
		"rubric_version":     pw.RubricVersion,
		"shadow":             shadow,
	})
}

func (jr *judgeRecorder) post(ctx context.Context, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, jr.baseURL+"/internal/judge", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Internal-Auth", jr.auth)
	resp, err := jr.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("judge record: status %d", resp.StatusCode)
	}
	return nil
}
