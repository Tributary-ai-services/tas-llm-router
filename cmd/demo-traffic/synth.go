// Event synthesis for the AIQG demo-traffic generator.
//
// For each simulated request we sample inputs from a profile, apply the
// cross-cutting attribute injectors, then run those inputs through the
// REAL pkg/clear scorer (clear.Compute) and the REAL pricing table
// (clear.DollarCost). The generator never invents a CLEAR score or a
// dollar figure — production code derives both, so demo data and live
// data are scored by an identical code path.
//
// The output is a flat field map whose keys match the top-level fields
// promoted by pkg/aiqg/events/emitter.go, so the dashboard's LogQL
// (`{namespace="tas-llm-router"} |= "aiqg response event" | json |
// unwrap <field>`) reads it byte-for-byte the same as a real gateway
// line.
package main

import (
	"fmt"
	"time"

	"github.com/tributary-ai/llm-router-waf/pkg/clear"
)

// rates carries the global cross-cutting attribute probabilities,
// sprinkled across every profile per the product direction.
type rates struct {
	Compliance float64
	Vague      float64
	Hedging    float64
	Error      float64 // chance the request failed upstream (vendor_error)
}

// synthResult is the per-event rollup the CLI summarizes at the end —
// enough to report the "potential savings" headline.
type synthResult struct {
	Profile     string
	CostUSD     float64
	Composite   int
	HasComposite bool
	Avoidable   bool    // carried at least one avoidable-cost matcher
	AvoidableUSD float64 // cost attributed to avoidable matchers
}

// avoidablePatternIDs mirrors avoidable_cost.go's four categories — used
// to tag the run summary's recoverable spend.
var avoidablePatternIDs = map[string]bool{
	mBloatedContext.PatternID:      true,
	mInstructionStuffing.PatternID: true,
	mHallucinationHedge.PatternID:  true,
	mRepetition.PatternID:          true,
	mVaguePrompt.PatternID:         true,
	mUnboundedLoop.PatternID:       true,
	mRefusal.PatternID:             true,
}

// synthEvent builds one response-event field map and its rollup.
//
// signature=true forces the profile's signature matcher to fire,
// guaranteeing the profile's characteristic story appears at least once
// per pass even when --per-profile is tiny.
func synthEvent(g rng, p profile, signature bool, r rates, tenantID, accountID string, ts time.Time) (map[string]any, synthResult) {
	mc := p.Models[g.r.Intn(len(p.Models))]
	prompt := g.intIn(p.PromptTokensLo, p.PromptTokensHi)
	completion := g.intIn(p.CompletionTokensLo, p.CompletionTokensHi)
	finish := g.weightedStr(p.FinishReasons)
	attempts := g.weightedInt(p.Attempts)
	fallback := attempts > 1 && g.chance(p.FallbackProb)
	e2e := int64(g.intIn(int(p.E2ELoMs), int(p.E2EHiMs)))

	// --- gather matchers (cross-cutting attributes) ---
	var attached []matcher
	add := func(m matcher) {
		if m.PatternID == "" {
			return
		}
		for _, a := range attached {
			if a.PatternID == m.PatternID {
				return
			}
		}
		attached = append(attached, m)
	}
	if signature {
		add(p.SignatureMatcher)
	} else if p.SignatureProb > 0 && g.chance(p.SignatureProb) {
		add(p.SignatureMatcher)
	}
	if g.chance(r.Compliance) {
		add(g.pickMatcher(complianceMatchers))
	}
	if g.chance(r.Vague) {
		add(g.pickMatcher(vagueMatchers))
	}
	if g.chance(r.Hedging) {
		add(g.pickMatcher(hedgingMatchers))
	}

	// A refusal / safety / injection finding means the model produced no
	// useful answer — reflect that in finish_reason so Efficacy (0 on
	// content_filter) and the Refusal avoidable-cost story stay coherent.
	for _, m := range attached {
		switch m.PatternID {
		case mRefusal.PatternID, mHarmRequest.PatternID, mExplicitJail.PatternID,
			mInjectionPrompt.PatternID, mInjectionSQL.PatternID, mCredSolicit.PatternID:
			finish = "content_filter"
		}
	}

	// --- build the Assurance finding maps from attached matchers ---
	inbound := map[string]int{}
	tagFindings := map[string]int{}
	nistFindings := map[string]int{}
	worst := ""
	for _, m := range attached {
		inbound[m.Severity]++
		tagFindings[m.PatternID]++
		nistFindings[m.NIST]++
		if severityRank(m.Severity) > severityRank(worst) {
			worst = m.Severity
		}
	}

	// --- error path: upstream failure, no completion ---
	status := "success"
	httpStatus := 200
	if g.chance(r.Error) {
		status = "vendor_error"
		httpStatus = 502
		completion = 0
		finish = "" // no finish_reason on a failed call → Efficacy nil
	}

	// --- run the REAL scorer + pricing ---
	in := clear.Input{
		EndToEndMs:                &e2e,
		Workflow:                  p.Workflow,
		HTTPStatus:                httpStatus,
		Vendor:                    mc.Vendor,
		Model:                     mc.Model,
		PromptTokens:              &prompt,
		CompletionTokens:          &completion,
		AssuranceScanRan:          true, // the gateway always scans in AIQG mode
		InboundFindingsBySeverity: inbound,
		FinishReason:              finish,
		AttemptCount:              attempts,
		FallbackUsed:              fallback,
	}
	gwOverhead := int64(g.intIn(6, 45))
	in.GatewayOverheadMs = &gwOverhead
	ttft := int64(g.intIn(40, 1200))
	if ttft > e2e {
		ttft = e2e / 2
	}
	in.VendorTTFTMs = &ttft

	scores := clear.Compute(in)
	costUSD, _ := clear.DollarCost(mc.Vendor, mc.Model, prompt, completion)

	// --- latency decomposition (for /metrics/latency + the waterfall) ---
	ingress := int64(g.intIn(2, 15))
	egress := int64(g.intIn(2, 20))
	ttfb := ttft - int64(g.intIn(10, 60))
	if ttfb < 1 {
		ttfb = 1
	}
	generation := e2e - ttft - egress
	if generation < 1 {
		generation = 1
	}
	medInterToken := int64(1)
	if completion > 0 {
		medInterToken = generation / int64(completion)
		if medInterToken < 1 {
			medInterToken = 1
		}
	}

	// --- assemble the emitter field map ---
	f := map[string]any{
		// logrus envelope — `|= "aiqg response event"` matches msg.
		"msg":   "aiqg response event",
		"level": "info",
		"time":  ts.UTC().Format(time.RFC3339Nano),

		"ce_type":           "com.tas.aiqg.response.v1",
		"ce_id":             g.uuid(),
		"response_event_id": g.uuid(),
		"request_event_id":  g.uuid(),
		"http_status":       httpStatus,
		"status":            status,
		"chunk_count":       maxInt(1, completion/8),
		"streamed":          true,
		"finish_reason":     finish,
		"tenant_id":         tenantID,
		"aiqg_account_id":   accountID,
		"gateway_version":   "demo-traffic-generator",
		"scoring_version":   "clear-mvp",
		"workflow":          p.Workflow,
		"vendor":            mc.Vendor,
		"model":             mc.Model,

		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
		"total_cost_usd":    costUSD,

		"end_to_end_ms":         e2e,
		"gateway_ingress_ms":    ingress,
		"gateway_egress_ms":     egress,
		"gateway_overhead_ms":   gwOverhead,
		"vendor_ttfb_ms":        ttfb,
		"vendor_ttft_ms":        ttft,
		"vendor_generation_ms":  generation,
		"median_inter_token_ms": medInterToken,

		// Assurance counts always emit when a scan ran.
		"assurance_inbound_count":  len(attached),
		"assurance_outbound_count": 0,
	}
	if finish == "" {
		delete(f, "finish_reason") // omit empty, matching emitter behaviour
	}
	if worst != "" {
		f["assurance_worst_severity"] = worst
	}
	putScores(f, scores)
	for k, v := range nistFindings {
		f["nist_"+k] = v
	}
	for k, v := range tagFindings {
		f["tag_"+sanitizeTagKey(k)] = v
	}

	// --- rollup ---
	res := synthResult{Profile: p.Key, CostUSD: costUSD}
	if scores != nil && scores.Composite != nil {
		res.Composite = int(*scores.Composite)
		res.HasComposite = true
	}
	for _, m := range attached {
		if avoidablePatternIDs[m.PatternID] {
			res.Avoidable = true
			res.AvoidableUSD = costUSD
			break
		}
	}
	return f, res
}

// putScores promotes each non-nil CLEAR dimension as the dashboard
// expects (clear_cost, clear_latency, ... clear_composite), int16 → int.
func putScores(f map[string]any, s *clear.Scores) {
	if s == nil {
		return
	}
	if s.Cost != nil {
		f["clear_cost"] = int(*s.Cost)
	}
	if s.Latency != nil {
		f["clear_latency"] = int(*s.Latency)
	}
	if s.Efficacy != nil {
		f["clear_efficacy"] = int(*s.Efficacy)
	}
	if s.Assurance != nil {
		f["clear_assurance"] = int(*s.Assurance)
	}
	if s.Reliability != nil {
		f["clear_reliability"] = int(*s.Reliability)
	}
	if s.Composite != nil {
		f["clear_composite"] = int(*s.Composite)
	}
}

// sanitizeTagKey mirrors pkg/aiqg/events/emitter.go and
// aiqg-dashboard-be's sanitizeTag: Loki labels can't carry hyphens.
func sanitizeTagKey(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			b[i] = '_'
		} else {
			b[i] = s[i]
		}
	}
	return string(b)
}

// uuid renders a v4-shaped identifier from the seeded source so runs
// are reproducible under --seed.
func (g rng) uuid() string {
	var b [16]byte
	for i := range b {
		b[i] = byte(g.r.Intn(256))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
