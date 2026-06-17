package clear

import "math"

// Cost decomposition (CLEAR v0.2, Contract v1 — projected basis only).
//
// Splits a request's billed cost into the three source-spec §1.4/§2.1
// categories and projects per-method payload-reduction savings, using a
// cheap inline heuristic (no network, no embeddings, <1ms). This is the
// "projected" basis — what the gateway *could* save if extraction ran.
// Measured/actual reductions (Contract v2) come only from the real
// extractor under an experiment/policy and are NOT computed here.
//
// Invariant (#6): direct + induced + genuine ≤ actual.TotalUSD. The
// bound clamp below enforces it at the source so the eventual SQL CHECK
// can never be violated; TestDecompose_BoundInvariant is the canary.
const (
	// cerK shapes the context-efficiency proxy CER = r/(r+K) where
	// r = completion/prompt. K=0.5 means a request whose output is half
	// its input scores CER≈0.5 (half the context was "worth it").
	cerK = 0.5
	// bloatCERMultiplier discounts CER when the inbound scan flagged
	// bloat/instruction-stuffing — strong signal that context is padded.
	bloatCERMultiplier = 0.7
	// slmCompress is the assumed standalone compression an SLM rewrite
	// achieves on the prompt (~25%). Low confidence — a coarse prior
	// until the real SLM step measures it (Contract v2).
	slmCompress = 0.25

	confMedium = "medium"
	confLow    = "low"

	reductionModeProjected = "projected"
)

// Decomposition is the projected cost breakdown for one priced request.
// All USD fields are bounded by actual.TotalUSD (see invariant). The
// bare DirectPayloadWaste* are the documented alias of the projected
// basis (Contract v1 D1-B) — kept so existing dashboards/CHECK keep
// working; relabel as "projected" in the UI.
type Decomposition struct {
	ReductionMode          string
	ContextEfficiencyRatio *float64 // pointer: 0.0 (all context wasted) vs absent

	DirectPayloadWasteTokens int
	DirectPayloadWasteUSD    float64

	ProjectedReductionRelevanceUSD        float64
	ProjectedReductionRelevanceConfidence string
	ProjectedReductionSLMUSD              float64
	ProjectedReductionSLMConfidence       string
	ProjectedReductionCombinedUSD         float64

	InducedOutputWasteEstimatedUSD float64
	GenuinePostModelWasteUSD       float64
	GatewayAddressablePct          float64
}

// DecomposeCost runs the heuristic. Call only when actual.Priced — never
// fabricate waste on unpriced traffic.
func DecomposeCost(in Input, actual Cost) Decomposition {
	prompt, completion := 0, 0
	if in.PromptTokens != nil {
		prompt = *in.PromptTokens
	}
	if in.CompletionTokens != nil {
		completion = *in.CompletionTokens
	}

	// Context-efficiency ratio: how much of the input "earned its keep".
	r := float64(completion) / math.Max(float64(prompt), 1)
	cer := r / (r + cerK)
	if in.InboundBloatFindings > 0 {
		cer *= bloatCERMultiplier
	}
	cer = clamp01(cer)
	relFrac := 1 - cer // fraction of input context projected droppable

	inputCost := actual.InputUSD

	// Direct payload waste = input dollars spent on droppable context.
	directUSD := inputCost * relFrac
	directTokens := int(math.Round(float64(prompt) * relFrac))

	// Per-method standalone projections.
	relevanceUSD := directUSD                                    // relevance/top-K drops the droppable context
	slmUSD := inputCost * slmCompress                            // SLM rewrite compresses the whole prompt ~25%
	combinedUSD := inputCost * (1 - (1-relFrac)*(1-slmCompress)) // compound the two

	// Induced output waste (retries/abandonment) — 0 in v1 (retry
	// linkage is Contract v2).
	inducedUSD := 0.0

	// Genuine post-model waste: spend that no payload reduction could
	// have saved. A hard failure (HTTP ≥400 / content_filter) wastes the
	// whole spend; high/critical OUTBOUND findings (the model produced
	// something unsafe despite clean input) waste the output spend.
	genuineUSD := 0.0
	switch {
	case in.HTTPStatus >= 400 || in.FinishReason == "content_filter":
		genuineUSD = actual.TotalUSD
	case outboundHighCritical(in.OutboundFindingsBySeverity) > 0:
		genuineUSD = actual.OutputUSD
	}

	// Bound clamp — enforce the invariant at the source.
	budget := actual.TotalUSD
	genuineUSD = clampUSD(genuineUSD, 0, budget)
	origDirect := directUSD
	directUSD = clampUSD(directUSD, 0, budget-genuineUSD)
	if origDirect > 0 && directUSD < origDirect {
		// Scale tokens to the clamped dollar figure so the two agree.
		directTokens = int(math.Round(float64(directTokens) * (directUSD / origDirect)))
	}
	// Per-method reductions can't exceed the input spend.
	relevanceUSD = clampUSD(relevanceUSD, 0, inputCost)
	slmUSD = clampUSD(slmUSD, 0, inputCost)
	combinedUSD = clampUSD(combinedUSD, 0, inputCost)

	// Gateway-addressable share of the spend (direct + induced).
	addressablePct := 0.0
	if budget > 0 {
		addressablePct = (directUSD + inducedUSD) / budget * 100
	}

	cerOut := cer
	return Decomposition{
		ReductionMode:                         reductionModeProjected,
		ContextEfficiencyRatio:                &cerOut,
		DirectPayloadWasteTokens:              directTokens,
		DirectPayloadWasteUSD:                 directUSD,
		ProjectedReductionRelevanceUSD:        relevanceUSD,
		ProjectedReductionRelevanceConfidence: confMedium,
		ProjectedReductionSLMUSD:              slmUSD,
		ProjectedReductionSLMConfidence:       confLow,
		ProjectedReductionCombinedUSD:         combinedUSD,
		InducedOutputWasteEstimatedUSD:        inducedUSD,
		GenuinePostModelWasteUSD:              genuineUSD,
		GatewayAddressablePct:                 addressablePct,
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func clampUSD(v, lo, hi float64) float64 {
	if hi < lo {
		hi = lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// outboundHighCritical sums high+critical findings in an outbound
// severity map (nil-safe).
func outboundHighCritical(bySeverity map[string]int) int {
	if bySeverity == nil {
		return 0
	}
	return bySeverity["high"] + bySeverity["critical"]
}
