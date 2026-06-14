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
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/judge"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/tokens"
)

// judgeRunner wires the LLM-as-judge quality layer (§6.6) into the gateway:
// it samples completed AIQG responses, scores them off the hot path with a
// third model, and records the score to aiqg-dashboard-be. Nil when judging
// is disabled (no JudgeModel / pct<=0) — maybeJudge is then a no-op.
type judgeRunner struct {
	judge     *judge.Judge
	samplePct int
	recorder  *judgeRecorder
	log       *logrus.Logger
}

// newJudgeRunner builds the runner, or returns nil when judging is off / the
// dashboard isn't configured (nowhere to record).
func newJudgeRunner(router *routing.Router, model string, samplePct int, dashboardURL, internalAuth string, log *logrus.Logger) *judgeRunner {
	if model == "" || samplePct <= 0 || dashboardURL == "" || internalAuth == "" {
		return nil
	}
	return &judgeRunner{
		judge:     &judge.Judge{LLM: &routerCompletion{router: router}, Model: model},
		samplePct: samplePct,
		recorder:  &judgeRecorder{http: &http.Client{Timeout: 5 * time.Second}, baseURL: strings.TrimRight(dashboardURL, "/"), auth: internalAuth},
		log:       log,
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

// maybeJudge fires an async judge for a sampled, AIQG-attributed, non-streaming
// response. Strictly off the hot path: the client already has its response; a
// judge failure only means no score lands. Skips when judging is off, the
// request isn't AIQG-attributed (no tenant), or the response has no text.
func (jr *judgeRunner) maybeJudge(ctx context.Context, w http.ResponseWriter, req *types.ChatRequest, resp *types.ChatResponse) {
	if jr == nil || req == nil || resp == nil {
		return
	}
	eventID := w.Header().Get("TAS-Response-Event-Id")
	if eventID == "" || !jr.sampled(eventID) {
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

	// Detach from the request context (it may be canceled once the response
	// is flushed) — the judge call is independent work.
	go func() {
		jctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		score, err := jr.judge.Score(jctx, workflow, promptText, responseText)
		if err != nil {
			jr.log.WithError(err).Debug("aiqg judge: scoring failed")
			return
		}
		if score.Abstain {
			return // judge declined — don't record a non-signal
		}
		// The gateway already knows the experiment/variant (routing snapshot),
		// so the score carries its own attribution — the dashboard inserts it
		// directly, no event re-resolution.
		if err := jr.recorder.record(jctx, tenantID, eventID, expID, variant, score); err != nil {
			jr.log.WithError(err).Debug("aiqg judge: record failed")
		}
	}()
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

func (jr *judgeRecorder) record(ctx context.Context, tenantID, eventID, experimentID, variant string, s judge.Score) error {
	body, err := json.Marshal(map[string]any{
		"tenant_id":          tenantID,
		"response_event_id":  eventID,
		"experiment_id":      experimentID,
		"experiment_variant": variant,
		"workflow":           s.Workflow,
		"overall":            s.Overall,
		"dimensions":         s.Dimensions,
		"rubric_version":     s.RubricVersion,
	})
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
