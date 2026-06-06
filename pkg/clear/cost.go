package clear

import "math"

// PricingVersion is the identifier of the per-model pricing table baked
// into this binary. Bump when any rate changes so Spark re-score jobs
// can identify rows scored under stale pricing and recompute.
//
// Format: `pricing-vYYYY-MM-DD` reflecting the publication date of the
// rates encoded in modelPricing.
const PricingVersion = "pricing-v2026-06-05"

// modelPricingEntry is the input/output rate pair for one vendor:model.
// Rates are USD per 1,000 tokens (matches source-spec §2.1.3's CNA/CPS
// arithmetic). Vendor APIs typically publish rates per million; we
// divide by 1,000 here so the per-1k math is straight multiplication.
type modelPricingEntry struct {
	InputCostPer1K  float64
	OutputCostPer1K float64
}

// modelPricing is a sparse table of vendor:model → rate. Unknown
// combinations skip Cost scoring (the dimension stays nil rather than
// scoring a guess). The table is intentionally short — adding a model
// requires a deliberate edit + PricingVersion bump.
//
// Rates are mid-2026 public list prices. Customer-negotiated rates are
// out of scope for the in-binary table; the dashboard-backed Resolver
// slice will eventually pull per-account overrides.
var modelPricing = map[string]modelPricingEntry{
	// OpenAI
	"openai:gpt-4o-mini":           {InputCostPer1K: 0.00015, OutputCostPer1K: 0.00060},
	"openai:gpt-4o":                {InputCostPer1K: 0.00250, OutputCostPer1K: 0.01000},
	"openai:gpt-4-turbo":           {InputCostPer1K: 0.01000, OutputCostPer1K: 0.03000},
	"openai:gpt-3.5-turbo":         {InputCostPer1K: 0.00050, OutputCostPer1K: 0.00150},

	// Anthropic — Claude 4.x family (current TAS deployment, see
	// tas-llm-router/configs/config.yaml). Keep rates in sync with that
	// file; the config and this table can drift independently because
	// the config doesn't feed the scorer (the table is the
	// authoritative source for CLEAR.Cost).
	"anthropic:claude-opus-4-6":            {InputCostPer1K: 0.01500, OutputCostPer1K: 0.07500},
	"anthropic:claude-sonnet-4-6":          {InputCostPer1K: 0.00300, OutputCostPer1K: 0.01500},
	"anthropic:claude-haiku-4-5-20251001":  {InputCostPer1K: 0.00080, OutputCostPer1K: 0.00400},

	// Anthropic — Claude 3.x family (legacy, kept for replay /
	// historical re-scoring; remove once Spark re-score has caught up
	// to the 4.x cutover).
	"anthropic:claude-3-7-sonnet-20250219": {InputCostPer1K: 0.00300, OutputCostPer1K: 0.01500},
	"anthropic:claude-3-5-sonnet-20241022": {InputCostPer1K: 0.00300, OutputCostPer1K: 0.01500},
	"anthropic:claude-3-5-haiku-20241022":  {InputCostPer1K: 0.00080, OutputCostPer1K: 0.00400},
	"anthropic:claude-3-opus-20240229":     {InputCostPer1K: 0.01500, OutputCostPer1K: 0.07500},
	"anthropic:claude-3-haiku-20240307":    {InputCostPer1K: 0.00025, OutputCostPer1K: 0.00125},
}

// LookupPricing returns the rates for a vendor:model pair, or false if
// the combination isn't in the table. Exported so events.Build can
// populate the per-event token-accounting block (which carries the
// dollar costs separately from the Cost score).
func LookupPricing(vendor, model string) (inputPer1K, outputPer1K float64, ok bool) {
	if vendor == "" || model == "" {
		return 0, 0, false
	}
	p, ok := modelPricing[vendor+":"+model]
	if !ok {
		return 0, 0, false
	}
	return p.InputCostPer1K, p.OutputCostPer1K, true
}

// DollarCost computes the total USD cost of a request given token
// counts and the model's rates. Returns 0 (and ok=false) when pricing
// isn't known for the vendor:model pair — callers should treat that
// as "cost unknown, do not score".
func DollarCost(vendor, model string, promptTokens, completionTokens int) (cost float64, ok bool) {
	inputRate, outputRate, found := LookupPricing(vendor, model)
	if !found {
		return 0, false
	}
	cost = (float64(promptTokens)/1000.0)*inputRate + (float64(completionTokens)/1000.0)*outputRate
	return cost, true
}

// scoreCost converts the request's USD cost into a 0-100 score with a
// logarithmic curve. The boundary points are chosen so the score maps
// to source-spec §2.1.3's Healthy / Marginal / Failing tiers at round
// cost values:
//
//	$0.001 / request → 100 (Healthy)
//	$0.01  / request → 75  (Healthy / Marginal boundary)
//	$0.10  / request → 50  (Marginal / Failing boundary)
//	$1.00  / request → 25
//	$10+   / request → 0
//
// Formula: score = 100 − 25 × log10(cost_usd × 1000), clamped to [0,100].
// The 25-per-decade slope spans 4 orders of magnitude — wide enough to
// distinguish a $0.001 classifier call from a $1 long-context summary
// without collapsing every score into the same band.
//
// Returns nil when:
//   - HTTPStatus is 0 (gateway-blocked; cost is meaningless)
//   - Prompt+completion token counts are both zero (no usage to score)
//   - Pricing for the vendor:model isn't in the table
func scoreCost(in Input) *Score {
	if in.HTTPStatus == 0 {
		return nil
	}
	if in.PromptTokens == nil && in.CompletionTokens == nil {
		return nil
	}
	prompt, completion := 0, 0
	if in.PromptTokens != nil {
		prompt = *in.PromptTokens
	}
	if in.CompletionTokens != nil {
		completion = *in.CompletionTokens
	}
	if prompt == 0 && completion == 0 {
		return nil
	}
	cost, ok := DollarCost(in.Vendor, in.Model, prompt, completion)
	if !ok {
		return nil
	}
	// log10(0) is -Inf; guard against pricing-table entries with both
	// rates at zero (which shouldn't exist but defensive code is cheap).
	if cost <= 0 {
		s := Score(100)
		return &s
	}
	score := 100.0 - 25.0*math.Log10(cost*1000.0)
	switch {
	case score > 100:
		score = 100
	case score < 0:
		score = 0
	}
	s := Score(score)
	return &s
}
