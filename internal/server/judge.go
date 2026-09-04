package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/routing"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/experiments"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/judge"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/tokens"
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
	log         *logrus.Logger
}

// newJudgeRunner builds the runner, or returns nil when judging is off / the
// dashboard isn't configured (nowhere to record). shadowPct>0 + an experiments
// resolver enable pairwise shadow-eval (§6.3) on top of the pointwise judge.
func newJudgeRunner(router *routing.Router, model string, samplePct, shadowPct int, exp *experiments.Resolver, dashboardURL, internalAuth string, log *logrus.Logger) *judgeRunner {
	if model == "" || samplePct <= 0 || dashboardURL == "" || internalAuth == "" {
		return nil
	}
	return &judgeRunner{
		judge:       &judge.Judge{LLM: &routerCompletion{router: router}, Model: model},
		samplePct:   samplePct,
		shadowPct:   shadowPct,
		experiments: exp,
		router:      router,
		recorder:    &judgeRecorder{http: &http.Client{Timeout: 5 * time.Second}, baseURL: strings.TrimRight(dashboardURL, "/"), auth: internalAuth},
		log:         log,
	}
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
func (jr *judgeRunner) shadowSampled(eventID string) bool {
	if jr.shadowPct <= 0 {
		return false
	}
	if jr.shadowPct >= 100 {
		return true
	}
	return int(crc32.ChecksumIEEE([]byte("shadow:"+eventID))%100) < jr.shadowPct
}

// maybeJudge fires an async judge for a sampled, AIQG-attributed, non-streaming
// response. Strictly off the hot path: the client already has its response; a
// judge failure only means no score lands. Skips when judging is off, the
// request isn't AIQG-attributed (no tenant), or the response has no text.
func (jr *judgeRunner) maybeJudge(ctx context.Context, w http.ResponseWriter, req *types.ChatRequest, resp *types.ChatResponse) {
	if jr == nil || req == nil || resp == nil {
		return
	}
	eventID := w.Header().Get("TAS-Response-Event-Id")
	if eventID == "" {
		return
	}
	tok := tokens.FromContext(ctx)
	if tok == nil {
		return // not AIQG-attributed — no tenant to scope the score
	}
	responseText := extractResponseContent(resp)
	if strings.TrimSpace(responseText) == "" {
		return // tool-call-only / empty — nothing semantic to judge
	}
	workflow, expID, variant := "", "", ""
	if r := middleware.RoutingFromContext(ctx); r != nil {
		snap := r.Snapshot()
		workflow, expID, variant = snap.Workflow, snap.ExperimentID, snap.ExperimentVariant
	}
	promptText := promptFromMessages(req)
	tenantID := tok.TenantID

	// Pointwise judge on the sampled fraction.
	if jr.sampled(eventID) {
		go func() {
			jctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			score, err := jr.judge.Score(jctx, workflow, promptText, responseText)
			if err != nil {
				jr.log.WithError(err).Debug("aiqg judge: scoring failed")
				return
			}
			if score.Abstain {
				return
			}
			// The gateway knows the experiment/variant (routing snapshot), so
			// the score carries its own attribution — no event re-resolution.
			if err := jr.recorder.record(jctx, tenantID, eventID, expID, variant, score); err != nil {
				jr.log.WithError(err).Debug("aiqg judge: record failed")
			}
		}()
	}

	// Pairwise shadow-eval (§6.3): for a CONTROL-arm sample of an experiment,
	// replay the same prompt through each variant offline and judge head-to-
	// head. Zero user impact (the client already has control's response);
	// ~2× cost on the shadow sample. Pairs with dry_run (everyone is control).
	if expID != "" && variant == "control" && jr.experiments != nil && jr.shadowSampled(eventID) {
		msgs := append([]types.Message(nil), req.Messages...) // snapshot for the goroutine
		baseModel := req.Model
		// Carry control's own cap and usage: the cap so the variant is judged
		// under the same limit (#182), the usage so the recorded comparison is
		// a paired cost sample rather than a preference with no price (#183).
		go jr.shadowEval(eventID, tenantID, expID, workflow, promptText, responseText, msgs, baseModel,
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
	Model        string // what the variant actually resolved to after the override
	Usage        *types.Usage
	FinishReason string
	Truncated    bool // the provider stopped at the cap — the comparison is suspect
}

// shadowEval replays the control prompt through each non-control variant and
// pairwise-judges control vs variant, recording a per-variant preference and
// the measured cost of both arms. Best-effort; off the hot path.
func (jr *judgeRunner) shadowEval(eventID, tenantID, expID, workflow, prompt, controlResp string, msgs []types.Message, baseModel string, controlMaxTokens *int, controlUsage *types.Usage) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	variants := jr.experiments.NonControlOverrides(ctx, tenantID, expID)
	for _, v := range variants {
		rr, err := jr.replay(ctx, msgs, baseModel, controlMaxTokens, v.Override)
		// Count the spend before anything can discard the result: an abstaining
		// judge costs exactly what an agreeing one does, and a spend figure
		// that counts only successes understates the bill in the one direction
		// nobody notices.
		jr.countReplay(rr)
		if err != nil || strings.TrimSpace(rr.Text) == "" {
			metrics.ShadowReplaysTotal.WithLabelValues(metrics.ShadowReplayFailed).Inc()
			jr.log.WithError(err).Debug("aiqg shadow-eval: replay failed")
			continue
		}
		// Randomize A/B order per the bias control (§6.6).
		variantFirst := crc32.ChecksumIEEE([]byte(eventID+":"+v.Key))%2 == 0
		pw, err := jr.judge.ScorePairwise(ctx, workflow, prompt, controlResp, rr.Text, variantFirst)
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
	}
	if rr.Truncated {
		metrics.ShadowTruncatedTotal.Inc()
	}
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
	_, provider, err := jr.router.Route(ctx, req)
	if err != nil {
		return replayResult{}, err
	}
	resp, err := provider.ChatCompletion(ctx, req)
	if err != nil {
		return replayResult{}, err
	}
	out := replayResult{
		Text:         extractResponseContent(resp),
		Model:        req.Model,
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
type routerCompletion struct{ router *routing.Router }

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
	_, provider, err := rc.router.Route(ctx, req)
	if err != nil {
		return "", fmt.Errorf("judge route: %w", err)
	}
	resp, err := provider.ChatCompletion(ctx, req)
	if err != nil {
		return "", fmt.Errorf("judge completion: %w", err)
	}
	return extractResponseContent(resp), nil
}

// judgeRecorder POSTs a judge score to aiqg-dashboard-be's internal ingest,
// which resolves the event → tenant/experiment/variant and stores it as a
// judge feedback row.
type judgeRecorder struct {
	http    *http.Client
	baseURL string
	auth    string
}

// record posts a pointwise judge score (signal_type=judge, value=overall).
func (jr *judgeRecorder) record(ctx context.Context, tenantID, eventID, experimentID, variant string, s judge.Score) error {
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
