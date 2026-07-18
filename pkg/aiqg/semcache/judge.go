package semcache

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
)

// L3 — the async judge (docs/AIQG-SEMANTIC-CACHING.md §5, §14.1). It runs OFF the
// request's critical path. Near-misses (L2-rejected candidates in the danger
// band) and would-hits are SAMPLED to a queue; an LLM judge grades, post-hoc,
// whether the cached answer would have been correct for the new query. Its output
// is three things at once:
//
//   - the LABELED PAIR SET that §9.2 calibration needs — a false hit lives in
//     exactly this population, so this is the only way to build the second class
//     of pairs ("looks similar, must NOT share an answer") from our own traffic;
//   - the sampled FALSE-HIT RATE (§14) — our only ground truth for an enabled route;
//   - the per-entry threshold-adaptation signal (§9.3, see SimCalibrator).
//
// It costs LLM tokens, sampled but real (§14.1) — so it is a deliberately
// separate, opt-in stage, never on by default.

// Sample is one lookup worth grading: the incoming query and the candidate the
// cascade surfaced for it. The caller (the shadow/serve path) fills CachedAnswer
// with the assistant text of the stored response so this package never parses
// vendor JSON — it stays store- and schema-agnostic.
type Sample struct {
	Scope        Scope
	Query        string  // post-redaction incoming prompt (the semantic key)
	CachedPrompt string  // the candidate entry's stored prompt (Entry.Prompt)
	CachedAnswer string  // assistant text of the candidate's stored response
	Similarity   float64 // L1 cosine similarity of the candidate
	// Observed is the cascade state that produced this sample: StateShadowHit /
	// StateSemanticHit (L2 passed — a served/would-serve hit, the FPR numerator) or
	// StateMiss (L1 candidate that L2 rejected — grades whether L2 was right).
	Observed string
	// RejectReason is the L2 guard that fired when Observed == StateMiss.
	RejectReason string
}

// wouldServe reports whether this sample is one the cache would (or did) serve —
// i.e. L2 passed. Only these count toward the sampled false-hit rate; a graded
// L2-rejected near-miss measures L2's precision, not the cache's FPR.
func (s Sample) wouldServe() bool {
	return s.Observed == StateShadowHit || s.Observed == StateSemanticHit
}

// Verdict is the judge's post-hoc grade of one sample.
type Verdict struct {
	// Correct is true when the cached answer would correctly and completely answer
	// the incoming query. A partially-correct or off-by-an-entity answer is false.
	Correct bool
	// Confidence is the judge's self-reported certainty [0,1] (0 when unknown).
	Confidence float64
	Reason     string
}

// Grader grades a sample. Implementations wrap an LLM call; the interface keeps
// the loop testable and the transport (the router's own client, a direct vendor
// call, a human queue) swappable.
type Grader interface {
	Grade(ctx context.Context, s Sample) (Verdict, error)
}

// GraderFunc adapts a function to Grader (for tests and simple wirings).
type GraderFunc func(ctx context.Context, s Sample) (Verdict, error)

// Grade implements Grader.
func (f GraderFunc) Grade(ctx context.Context, s Sample) (Verdict, error) { return f(ctx, s) }

// ChatFunc is a single-shot LLM call: a system + user prompt in, the model's text
// out. The caller binds it to the router's own client at wiring time, so this
// package depends on no server types.
type ChatFunc func(ctx context.Context, system, user string) (string, error)

// PromptGrader is the default Grader: it asks an LLM, BLIND (§14.1 step 2 — the
// judge is not told which answer is cached vs fresh, only "does this answer this
// question"), whether the cached answer is correct for the incoming query.
type PromptGrader struct{ chat ChatFunc }

// NewPromptGrader binds a PromptGrader to a chat transport.
func NewPromptGrader(chat ChatFunc) *PromptGrader { return &PromptGrader{chat: chat} }

const judgeSystemPrompt = `You are a strict evaluator for a response cache. You are given a USER QUESTION and a CANDIDATE ANSWER. Decide whether the candidate answer is fully correct and complete for that exact question.

Rules:
- Judge only whether the answer is correct for THIS question. Do not reward fluent but off-topic answers.
- Any mismatch of a specific entity, product tier, number, amount, version, or date makes it INCORRECT (e.g. an answer about the base card does not answer a question about the "Reserve" tier).
- A partial or hedged answer that omits the asked-for specifics is INCORRECT.
- If you cannot tell, answer false.

Reply with ONLY a JSON object: {"correct": <true|false>, "confidence": <0..1>, "reason": "<short>"}`

// Grade implements Grader via the bound chat transport.
func (g *PromptGrader) Grade(ctx context.Context, s Sample) (Verdict, error) {
	if g == nil || g.chat == nil {
		return Verdict{}, fmt.Errorf("semcache: PromptGrader has no chat transport")
	}
	user := fmt.Sprintf("USER QUESTION:\n%s\n\nCANDIDATE ANSWER:\n%s", s.Query, s.CachedAnswer)
	out, err := g.chat(ctx, judgeSystemPrompt, user)
	if err != nil {
		return Verdict{}, err
	}
	return parseVerdict(out)
}

// parseVerdict reads the judge's reply. It prefers the strict JSON contract and
// falls back to a lexical yes/no scan so a chatty model still yields a usable
// grade rather than an error (fail toward "incorrect" — never a false hit).
func parseVerdict(out string) (Verdict, error) {
	if i := strings.IndexByte(out, '{'); i >= 0 {
		if j := strings.LastIndexByte(out, '}'); j > i {
			var raw struct {
				Correct    bool    `json:"correct"`
				Confidence float64 `json:"confidence"`
				Reason     string  `json:"reason"`
			}
			if err := json.Unmarshal([]byte(out[i:j+1]), &raw); err == nil {
				return Verdict{Correct: raw.Correct, Confidence: clamp01(raw.Confidence), Reason: raw.Reason}, nil
			}
		}
	}
	// Fallback: no parseable JSON. Only an explicit affirmative reads as correct.
	l := strings.ToLower(out)
	correct := strings.Contains(l, "\"correct\": true") ||
		strings.HasPrefix(strings.TrimSpace(l), "true") ||
		strings.Contains(l, "yes, ") || strings.Contains(l, "answer is correct")
	return Verdict{Correct: correct, Reason: "parsed from non-JSON reply"}, nil
}

// SampleConfig governs which lookups are enqueued for grading (§9.2 step 2, §10.1
// async_judge). The band is the danger zone where genuine near-dupes and true
// near-misses overlap and only ground truth separates them.
type SampleConfig struct {
	// BandLo, BandHi bound the sampled similarity band (the danger zone, ~0.88–0.97).
	BandLo, BandHi float64
	// Rate is the fraction of eligible lookups sampled (0..1). Deterministic per
	// (scope, query) so the same near-miss samples consistently and tests are stable.
	Rate float64
	// IncludeHits also samples would-hits ABOVE the band (§14.1 step 1: sample a
	// small fraction of served semantic hits — that is the served-FPR ground truth).
	IncludeHits bool
}

// ShouldSample decides whether a lookup with the given similarity and cascade
// state is enqueued. It is band membership AND a deterministic rate gate.
func (sc SampleConfig) ShouldSample(sim float64, state, dedupeKey string) bool {
	if sc.Rate <= 0 {
		return false
	}
	inBand := sim >= sc.BandLo && sim <= sc.BandHi
	aboveBand := sim > sc.BandHi
	served := state == StateShadowHit || state == StateSemanticHit
	eligible := inBand || (sc.IncludeHits && aboveBand && served)
	if !eligible {
		return false
	}
	return rateGate(dedupeKey, sc.Rate)
}

// rateGate is a stable, RNG-free sampler: hash the key into [0,10000) and keep it
// when below rate*10000. Same key ⇒ same decision (idempotent under retries, and
// deterministic in tests).
func rateGate(key string, rate float64) bool {
	if rate >= 1 {
		return true
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32()%10000) < int(rate*10000)
}
