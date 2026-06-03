// Package clear implements the CLEAR (Cost, Latency, Efficacy,
// Assurance, Reliability) scoring framework as defined in the AIQG
// source-spec §2 and placed gateway-side per build-vs-reuse §7.2.
//
// Scoring runs at request close inside tas-llm-router. The pkg/aiqg/events
// package calls Score() with the inputs available at that moment and
// embeds the resulting *Scores into the response event payload. Spark
// aggregates pre-computed scores; it does not re-derive them.
//
// MVP scope (this slice):
//   - Latency — fully computed from the TimingCollector snapshot using
//     workflow-specific SLA thresholds (3s customer-support, 30s code-gen,
//     etc.) and a linear decay around the target.
//   - Cost, Efficacy, Assurance, Reliability — signatures defined,
//     return nil pointers (distinct from "scored as zero"). Filled by
//     future slices:
//       * Cost: needs token-accounting from response body
//       * Efficacy: needs response-body capture + structural-validity check
//       * Assurance: needs Gatekeeper finding integration
//       * Reliability: needs conversation-threading + retry detection
//   - Composite — equal-weight average of every dimension that produced
//     a non-nil score (per build-vs-reuse §7.5). Nil when zero dimensions
//     were scored.
//
// Every emitted event carries Version so re-scoring jobs can identify
// candidate rows (the §7.2 escape hatch). When a scoring formula changes,
// bump Version and the Spark re-score job filters on the old value.
package clear

// Version is the scoring code path identifier stamped on every emitted
// event. Bumped when any dimension's formula changes. Format:
// `clear-v<major>.<minor>-<phase>`.
//
// MVP (v0.1): Latency only. Cost/Efficacy/Assurance/Reliability stubbed.
// v0.2 will add Cost once token-accounting plumbing lands.
const Version = "clear-v0.1-mvp"

// Score is a 0-100 dimension or composite score. The spec
// (response-event.md §"Why smallint not numeric") uses int16 for storage
// compactness; we match.
type Score = int16

// Scores carries the five CLEAR dimensions plus the composite. Every
// field is a pointer so callers can distinguish "not scored" from
// "scored as zero" — both are legitimate states. A nil dimension is
// excluded from the composite calculation.
//
// JSON tag layout mirrors response-event.md §"CLEAR Scores".
type Scores struct {
	Cost           *Score `json:"cost,omitempty"`
	Latency        *Score `json:"latency,omitempty"`
	Efficacy       *Score `json:"efficacy,omitempty"`
	Assurance      *Score `json:"assurance,omitempty"`
	Reliability    *Score `json:"reliability,omitempty"`
	Composite      *Score `json:"composite,omitempty"`
	WeightsApplied string `json:"weights_applied,omitempty"`
}

// Input bundles every signal the scorer reads. Designed so MVP fields
// are concrete and future-slice additions can be appended without
// breaking existing callers (struct-literal callers fail loudly on
// unknown fields; named-field callers are tolerant).
type Input struct {
	// From TimingCollector.Snapshot (pointers because the snapshot
	// uses them as "captured / not captured" tri-state).
	EndToEndMs        *int64
	GatewayOverheadMs *int64
	VendorTTFTMs      *int64

	// From request context
	Workflow   string // CLEAR workflow type (rag, agentic, code_generation, ...)
	HTTPStatus int    // 0 means gateway never wrote a response

	// Future-slice inputs (left for forward compatibility):
	//   PromptTokens, CompletionTokens *int       // Cost
	//   ResponseBodyValid              *bool      // Efficacy structural
	//   ComplianceFindingCount         *int       // Assurance
	//   RetryObserved                  *bool      // Reliability
}

// Compute runs every dimension scorer against in and returns the
// populated Scores struct. Dimensions whose inputs aren't yet plumbed
// return nil pointers — they're absent from the JSON, not zero.
//
// Composite is the equal-weight average of every non-nil dimension.
// When no dimension produced a score (e.g. HTTPStatus=0 from a
// pre-validation gateway block), Composite is also nil.
//
// Named Compute rather than Score to avoid collision with the Score
// type alias.
func Compute(in Input) *Scores {
	s := &Scores{
		Latency: scoreLatency(in),
		// Cost/Efficacy/Assurance/Reliability remain nil for MVP.
	}
	s.Composite = composite(s)
	if s.Composite != nil {
		s.WeightsApplied = "equal-weight-non-nil"
	}
	return s
}

// scoreLatency converts EndToEndMs into a 0-100 score against the
// workflow-specific SLA.
//
// Formula: `score = 100 − 50 × (actual / target − 1)`, clamped to [0,100].
// This yields:
//
//	actual = target  → 100 (Healthy)
//	actual = 1.5×target → 75 (boundary Healthy↔Marginal per §2.2.1)
//	actual = 2×target   → 50 (boundary Marginal↔Failing)
//	actual ≥ 3×target   → 0
//
// Returns nil when EndToEndMs wasn't stamped (request never completed)
// or when HTTPStatus is 0 (gateway-blocked, latency is meaningless).
func scoreLatency(in Input) *Score {
	if in.EndToEndMs == nil {
		return nil
	}
	if in.HTTPStatus == 0 {
		return nil
	}
	target := float64(slaMsForWorkflow(in.Workflow))
	actual := float64(*in.EndToEndMs)
	if target <= 0 {
		return nil
	}
	score := 100.0 - 50.0*(actual/target-1.0)
	switch {
	case score > 100:
		score = 100
	case score < 0:
		score = 0
	}
	s := Score(score)
	return &s
}

// slaMsForWorkflow returns the end-to-end SLA threshold for a workflow
// type, in milliseconds. Thresholds are drawn from CLEAR's published
// domain-specific guidance (§2.2.1: 3s customer-support, 30s code-gen)
// and extended for the workflow enum in workflow-classification.md §1.1.
//
// The unknown / unmapped case returns 10s — the median of the explicit
// thresholds, chosen so a misclassified workflow scores conservatively
// in the middle rather than passing or failing trivially.
func slaMsForWorkflow(workflow string) int64 {
	switch workflow {
	case "single_turn_qa":
		return 3000 // CLEAR §2.2.1 customer-support threshold
	case "classification_extraction":
		return 5000
	case "rag":
		return 5000
	case "summarization":
		return 15000
	case "code_generation":
		return 30000 // CLEAR §2.2.1 code-generation threshold
	case "agentic":
		return 30000
	default:
		return 10000
	}
}

// composite returns the equal-weight mean of every non-nil dimension
// score (per build-vs-reuse §7.5 "Equal-weight default 0.2 each; future
// per-account weights from account.scoring_weights").
//
// Returns nil when no dimension produced a score.
func composite(s *Scores) *Score {
	dims := []*Score{s.Cost, s.Latency, s.Efficacy, s.Assurance, s.Reliability}
	var sum int
	var count int
	for _, p := range dims {
		if p != nil {
			sum += int(*p)
			count++
		}
	}
	if count == 0 {
		return nil
	}
	avg := Score(sum / count)
	return &avg
}
