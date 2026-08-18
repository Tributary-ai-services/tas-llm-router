package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/routing"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/cacheconfig"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/semcache"
	"github.com/tributary-ai/llm-router-waf/pkg/clear"
)

// L3 judge wiring (docs/AIQG-SEMANTIC-CACHING.md §5, §14.1). This is the seam
// between the store-agnostic semcache.Loop and the gateway: the grader is bound
// to the router's own client (priced from real token usage), the labeled pairs
// land in Redis, and the loop's counters are exported to Prometheus so the daily
// budget and the sampled FPR are observable. The judge is opt-in (opex) and never
// touches the request path — it only reads what the shadow cascade already
// computed and enqueues it, off-latency.

// The danger band sampled for grading (§9.2): near-misses here and would-hits at
// or above it. Fixed for now; promote to config if a route needs its own band.
const (
	semJudgeBandLo = 0.88
	semJudgeBandHi = 0.97
	semJudgeQueue  = 512
)

// buildSemJudge constructs the L3 judge loop for the semantic cache, or returns
// (nil, nil) when judging is disabled or misconfigured (no grader model). The
// caller starts the loop's Run goroutine and enqueues samples from the shadow
// path. scRedis is the dedicated semcache Redis (the pair sink shares it).
func buildSemJudge(cfg AIQGSemCacheConfig, fallbackModel string, router *routing.Router, scRedis *redis.Client, log *logrus.Logger) (*semcache.Loop, semcache.SampleConfig) {
	sampleCfg := semcache.SampleConfig{
		BandLo:      semJudgeBandLo,
		BandHi:      semJudgeBandHi,
		Rate:        cfg.JudgeSampleRate,
		IncludeHits: true, // would-hits above the band are the served-FPR ground truth
	}
	if !cfg.JudgeEnabled {
		return nil, sampleCfg
	}
	model := cfg.JudgeModel
	if model == "" {
		model = fallbackModel
	}
	if model == "" {
		log.Warn("AIQG semcache judge enabled but no judge model (AIQG_SEMCACHE_JUDGE_MODEL / AIQG_JUDGE_MODEL); judge disabled")
		return nil, sampleCfg
	}

	grader := semcache.NewPromptGrader((&semJudgeGrader{router: router, model: model}).chat)
	// NOT under aiqg:scache: — that prefix holds the cached vectors, and an
	// embedder cutover REQUIRES flushing them (all-minilm and langcache are both
	// 384-dim, so the store cannot tell their vectors apart). The judged pairs are
	// the training corpus for SimCalibrator and must SURVIVE that flush; sharing
	// the prefix meant a documented, necessary `flush aiqg:scache:*` silently
	// destroyed weeks of labels. Observed for real on 2026-08-17.
	sink := &redisPairSink{rdb: scRedis, key: semcache.LabeledPairsKey, max: 50000}
	budget := semcache.NewDailyBudget(cfg.JudgeDailyUSD)
	loop := semcache.NewLoop(grader, sink, semcache.NewSimCalibrator(0.5, 20), budget, semJudgeQueue)

	log.WithFields(logrus.Fields{
		"model":       model,
		"sample_rate": cfg.JudgeSampleRate,
		"band":        fmt.Sprintf("[%.2f,%.2f]", semJudgeBandLo, semJudgeBandHi),
		"daily_usd":   cfg.JudgeDailyUSD,
	}).Info("AIQG semantic-cache L3 judge enabled (off-path; grades sampled near-misses → labeled pairs + sampled FPR)")
	return loop, sampleCfg
}

// enqueueForJudge samples one shadow lookup and, if selected, hands it to the L3
// judge. The candidate graded is the winning entry on a would-hit, or the
// L2-rejected near-miss on a miss. No-op when judging is disabled. Runs in the
// shadow goroutine (already off the request path); Enqueue itself never blocks.
func (s *Server) enqueueForJudge(scope semcache.Scope, prompt string, out semcache.Outcome, cc *cacheconfig.Config) {
	if s.semJudge == nil {
		return
	}
	// Per-tenant judge control: a tenant may opt OUT of grading (JudgeEnabled) or
	// dial its own sample rate, over the global defaults. (The Loop's daily $ cap
	// remains a global opex ceiling — see AIQG_SEMCACHE_JUDGE_DAILY_USD.)
	if !cc.JudgeEnabled(true) {
		return
	}
	var cand *semcache.Entry
	switch out.State {
	case semcache.StateShadowHit, semcache.StateSemanticHit:
		cand = out.Entry
	case semcache.StateMiss:
		cand = out.TopCandidate
	}
	if cand == nil {
		return
	}
	sampleCfg := s.semJudgeCfg
	sampleCfg.Rate = cc.JudgeSampleRate(s.semJudgeCfg.Rate)
	if !sampleCfg.ShouldSample(out.Similarity, out.State, scope.TenantID+"|"+prompt) {
		return
	}
	s.semJudge.Enqueue(semcache.Sample{
		Scope:        scope,
		Query:        prompt,
		CachedPrompt: cand.Prompt,
		CachedAnswer: answerFromResponseBytes(cand.Response),
		Similarity:   out.Similarity,
		Observed:     out.State,
		RejectReason: out.RejectReason,
		// Per-tenant daily cap (0 = none; the global ceiling still applies).
		DailyUSD: cc.JudgeDailyUSD(0),
	})
}

// answerFromResponseBytes pulls the assistant text out of a stored response blob
// (a marshaled types.ChatResponse) so the judge grades the answer that would have
// been served — keeping semcache free of the vendor response schema.
func answerFromResponseBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var resp types.ChatResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		return ""
	}
	return extractResponseContent(&resp)
}

// semJudgeGrader routes a grade prompt through the gateway's own router (the
// configured provider key, never a customer key), matching the judge-quality
// layer's routerCompletion, and prices the call from the vendor's reported token
// usage — so semcache never imports a pricing table.
type semJudgeGrader struct {
	router *routing.Router
	model  string
}

// chat is a semcache.ChatFunc: system+user (+ optional assistant prefill) in,
// (text, costUSD, err) out. A non-empty prefill is sent as a trailing assistant
// turn so the model must continue from it — Anthropic honors this and returns
// only the continuation (no opening brace), which the grader stitches back.
func (g *semJudgeGrader) chat(ctx context.Context, system, user, prefill string) (string, float64, error) {
	maxTokens := 200
	var temp float32 // 0 → deterministic grade
	msgs := []types.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	if prefill != "" {
		msgs = append(msgs, types.Message{Role: "assistant", Content: prefill})
	}
	req := &types.ChatRequest{
		Model:       g.model,
		MaxTokens:   &maxTokens,
		Temperature: &temp, // deterministic grade
		Messages:    msgs,
	}
	_, provider, err := g.router.Route(ctx, req)
	if err != nil {
		return "", 0, fmt.Errorf("semjudge route: %w", err)
	}
	resp, err := provider.ChatCompletion(ctx, req)
	if err != nil {
		return "", 0, fmt.Errorf("semjudge completion: %w", err)
	}
	var costUSD float64
	if resp.Usage != nil {
		if c, ok := clear.DollarCost(provider.GetProviderName(), g.model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens); ok {
			costUSD = c
		}
	}
	return extractResponseContent(resp), costUSD, nil
}

// redisPairSink appends judged pairs to a capped Redis list — the labeled set
// §9.2 needs, mined from live traffic. LPUSH + periodic LTRIM bounds it; a sink
// error is returned (the loop counts it) but never reaches a request.
type redisPairSink struct {
	rdb *redis.Client
	key string
	max int64
}

// Record implements semcache.PairSink.
func (s *redisPairSink) Record(ctx context.Context, p semcache.JudgedPair) error {
	if s == nil || s.rdb == nil {
		return nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	pipe := s.rdb.Pipeline()
	pipe.LPush(cctx, s.key, b)
	if s.max > 0 {
		pipe.LTrim(cctx, s.key, 0, s.max-1) // keep the newest `max`
	}
	_, err = pipe.Exec(cctx)
	return err
}

// semJudgeCollector exports the loop's in-memory counters to Prometheus on each
// scrape (the loop owns the source of truth; this is a live view, not a mirror).
// Registered on the AIQG registry so /aiqg/metrics carries it.
type semJudgeCollector struct {
	loop *semcache.Loop

	graded        *prometheus.Desc
	budgetSkipped *prometheus.Desc
	dropped       *prometheus.Desc
	errors        *prometheus.Desc
	wouldServe    *prometheus.Desc
	falseHits     *prometheus.Desc
	l2Rejected    *prometheus.Desc
	l2Correct     *prometheus.Desc
	spentUSD      *prometheus.Desc
	budgetCap     *prometheus.Desc
	budgetSpent   *prometheus.Desc
	budgetRemain  *prometheus.Desc
}

func newSemJudgeCollector(loop *semcache.Loop) *semJudgeCollector {
	d := func(name, help string) *prometheus.Desc { return prometheus.NewDesc(name, help, nil, nil) }
	return &semJudgeCollector{
		loop:          loop,
		graded:        d("aiqg_semcache_judge_graded_total", "L3 judge grades completed."),
		budgetSkipped: d("aiqg_semcache_judge_budget_skipped_total", "Samples skipped because the daily judge $ cap was reached."),
		dropped:       d("aiqg_semcache_judge_dropped_total", "Samples dropped because the judge queue was full."),
		errors:        d("aiqg_semcache_judge_errors_total", "Judge or pair-sink errors."),
		wouldServe:    d("aiqg_semcache_judge_would_serve_total", "Graded samples where L2 passed (the sampled-FPR denominator)."),
		falseHits:     d("aiqg_semcache_judge_false_hits_total", "Of would-serve grades, those the judge ruled incorrect (sampled false hits)."),
		l2Rejected:    d("aiqg_semcache_judge_l2_rejected_total", "Graded samples where L2 rejected the candidate."),
		l2Correct:     d("aiqg_semcache_judge_l2_correct_total", "Of L2-rejected grades, those the judge agreed were wrong (L2 precision)."),
		spentUSD:      d("aiqg_semcache_judge_spent_usd_total", "Lifetime judge spend this process (USD)."),
		budgetCap:     d("aiqg_semcache_judge_budget_cap_usd", "Configured daily judge spend cap (USD); 0 = unlimited."),
		budgetSpent:   d("aiqg_semcache_judge_budget_spent_usd", "Judge spend so far today (USD, resets at UTC midnight)."),
		budgetRemain:  d("aiqg_semcache_judge_budget_remaining_usd", "Remaining judge budget today (USD)."),
	}
}

// Describe implements prometheus.Collector.
func (c *semJudgeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.graded
	ch <- c.budgetSkipped
	ch <- c.dropped
	ch <- c.errors
	ch <- c.wouldServe
	ch <- c.falseHits
	ch <- c.l2Rejected
	ch <- c.l2Correct
	ch <- c.spentUSD
	ch <- c.budgetCap
	ch <- c.budgetSpent
	ch <- c.budgetRemain
}

// Collect implements prometheus.Collector — reads the loop live on each scrape.
func (c *semJudgeCollector) Collect(ch chan<- prometheus.Metric) {
	st := c.loop.Stats()
	capUSD, spent, remaining, _ := c.loop.Budget()
	counter := func(desc *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, v)
	}
	gauge := func(desc *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v)
	}
	counter(c.graded, float64(st.Graded))
	counter(c.budgetSkipped, float64(st.BudgetSkipped))
	counter(c.dropped, float64(st.Dropped))
	counter(c.errors, float64(st.Errors))
	counter(c.wouldServe, float64(st.WouldServe))
	counter(c.falseHits, float64(st.FalseHits))
	counter(c.l2Rejected, float64(st.L2Rejected))
	counter(c.l2Correct, float64(st.L2Correct))
	counter(c.spentUSD, st.SpentUSD)
	gauge(c.budgetCap, capUSD)
	gauge(c.budgetSpent, spent)
	gauge(c.budgetRemain, remaining)
}

// registerSemJudgeMetrics wires the collector onto the AIQG registry. Idempotent
// enough for one call at startup; a duplicate registration is logged, not fatal.
func registerSemJudgeMetrics(loop *semcache.Loop, log *logrus.Logger) {
	if loop == nil {
		return
	}
	if err := metrics.Registry.Register(newSemJudgeCollector(loop)); err != nil {
		log.WithError(err).Warn("AIQG semcache judge: metric registration failed")
	}
}
