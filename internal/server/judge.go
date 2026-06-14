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
		go jr.shadowEval(eventID, tenantID, expID, workflow, promptText, responseText, msgs, baseModel)
	}
}

// shadowEval replays the control prompt through each non-control variant and
// pairwise-judges control vs variant, recording a per-variant preference.
// Best-effort; off the hot path.
func (jr *judgeRunner) shadowEval(eventID, tenantID, expID, workflow, prompt, controlResp string, msgs []types.Message, baseModel string) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	variants := jr.experiments.NonControlOverrides(ctx, tenantID, expID)
	for _, v := range variants {
		variantResp, err := jr.replay(ctx, msgs, baseModel, v.Override)
		if err != nil || strings.TrimSpace(variantResp) == "" {
			jr.log.WithError(err).Debug("aiqg shadow-eval: replay failed")
			continue
		}
		// Randomize A/B order per the bias control (§6.6).
		variantFirst := crc32.ChecksumIEEE([]byte(eventID+":"+v.Key))%2 == 0
		pw, err := jr.judge.ScorePairwise(ctx, workflow, prompt, controlResp, variantResp, variantFirst)
		if err != nil || pw.Abstain {
			continue
		}
		if err := jr.recorder.recordPairwise(ctx, tenantID, eventID, expID, v.Key, workflow, pw); err != nil {
			jr.log.WithError(err).Debug("aiqg shadow-eval: record failed")
		}
	}
}

// replay runs the control prompt through a variant's override offline and
// returns the variant's response text. Uses the configured provider key (no
// customer key) and bypasses the AIQG middleware, same as the judge call.
func (jr *judgeRunner) replay(ctx context.Context, msgs []types.Message, baseModel string, override json.RawMessage) (string, error) {
	maxTokens := 256
	req := &types.ChatRequest{Model: baseModel, Messages: msgs, MaxTokens: &maxTokens}
	applyExperimentOverride(req, override) // swaps model / params to the variant
	_, provider, err := jr.router.Route(ctx, req)
	if err != nil {
		return "", err
	}
	resp, err := provider.ChatCompletion(ctx, req)
	if err != nil {
		return "", err
	}
	return extractResponseContent(resp), nil
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
// value=variant preference 0/0.5/1) attributed to the NON-control variant.
func (jr *judgeRecorder) recordPairwise(ctx context.Context, tenantID, eventID, experimentID, variant, workflow string, pw judge.PairwiseResult) error {
	return jr.post(ctx, map[string]any{
		"signal_type":        "judge_pairwise",
		"tenant_id":          tenantID,
		"response_event_id":  eventID,
		"experiment_id":      experimentID,
		"experiment_variant": variant,
		"workflow":           workflow,
		"overall":            pw.VariantPreference,
		"rubric_version":     pw.RubricVersion,
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
