package clear

import "math"

// PricingVersion is the identifier of the per-model pricing table baked
// into this binary. Bump when any rate changes so Spark re-score jobs
// can identify rows scored under stale pricing and recompute.
//
// Format: `pricing-vYYYY-MM-DD` reflecting the publication date of the
// rates encoded in modelPricing.
const PricingVersion = "pricing-v2026-10-05"

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
	// OpenAI — every chat model this account can serve that the vendor
	// publishes a per-token rate for. Rates from
	// developers.openai.com/api/docs/pricing, 2026-10-05.
	"openai:gpt-6-astra":   {InputCostPer1K: 0.01000, OutputCostPer1K: 0.05000}, // added 2026-10-05
	"openai:gpt-6.1-sol":   {InputCostPer1K: 0.00200, OutputCostPer1K: 0.01000}, // added 2026-10-05
	"openai:gpt-6-sol":     {InputCostPer1K: 0.00200, OutputCostPer1K: 0.01000}, // added 2026-10-05
	"openai:gpt-6-luna":    {InputCostPer1K: 0.00010, OutputCostPer1K: 0.00050}, // added 2026-10-05
	"openai:gpt-5.6-sol":   {InputCostPer1K: 0.00400, OutputCostPer1K: 0.02000}, // added 2026-10-05
	"openai:gpt-5.6-terra": {InputCostPer1K: 0.00200, OutputCostPer1K: 0.01200}, // added 2026-10-05
	"openai:gpt-5.6-luna":  {InputCostPer1K: 0.00020, OutputCostPer1K: 0.00120}, // added 2026-10-05
	"openai:gpt-5.5":       {InputCostPer1K: 0.00500, OutputCostPer1K: 0.03000}, // added 2026-10-05
	"openai:gpt-5.5-pro":   {InputCostPer1K: 0.03000, OutputCostPer1K: 0.18000}, // added 2026-10-05
	"openai:gpt-5.4":       {InputCostPer1K: 0.00250, OutputCostPer1K: 0.01500}, // added 2026-10-05
	"openai:gpt-5.4-mini":  {InputCostPer1K: 0.00075, OutputCostPer1K: 0.00450}, // added 2026-10-05
	"openai:gpt-5.4-nano":  {InputCostPer1K: 0.00020, OutputCostPer1K: 0.00125}, // added 2026-10-05
	"openai:gpt-5.4-pro":   {InputCostPer1K: 0.03000, OutputCostPer1K: 0.18000}, // added 2026-10-05
	"openai:gpt-5.3-codex": {InputCostPer1K: 0.00175, OutputCostPer1K: 0.01400}, // added 2026-10-05
	"openai:gpt-5.2":       {InputCostPer1K: 0.00175, OutputCostPer1K: 0.01400}, // added 2026-10-05
	"openai:gpt-5.2-pro":   {InputCostPer1K: 0.02100, OutputCostPer1K: 0.16800}, // added 2026-10-05
	"openai:gpt-5.1":       {InputCostPer1K: 0.00125, OutputCostPer1K: 0.01000}, // added 2026-10-05
	"openai:gpt-5":         {InputCostPer1K: 0.00125, OutputCostPer1K: 0.01000}, // added 2026-10-05
	"openai:gpt-5-mini":    {InputCostPer1K: 0.00025, OutputCostPer1K: 0.00200}, // added 2026-10-05
	"openai:gpt-5-nano":    {InputCostPer1K: 0.00005, OutputCostPer1K: 0.00040}, // added 2026-10-05
	"openai:gpt-5-pro":     {InputCostPer1K: 0.01500, OutputCostPer1K: 0.12000}, // added 2026-10-05
	"openai:gpt-4.1":       {InputCostPer1K: 0.00200, OutputCostPer1K: 0.00800}, // added 2026-10-05
	"openai:gpt-4.1-mini":  {InputCostPer1K: 0.00040, OutputCostPer1K: 0.00160}, // added 2026-10-05
	"openai:gpt-4.1-nano":  {InputCostPer1K: 0.00010, OutputCostPer1K: 0.00040}, // added 2026-10-05
	"openai:gpt-4o":        {InputCostPer1K: 0.00250, OutputCostPer1K: 0.01000},
	"openai:gpt-4o-mini":   {InputCostPer1K: 0.00015, OutputCostPer1K: 0.00060},
	"openai:gpt-3.5-turbo": {InputCostPer1K: 0.00050, OutputCostPer1K: 0.00150},
	"openai:o1":            {InputCostPer1K: 0.01500, OutputCostPer1K: 0.06000}, // added 2026-10-05
	"openai:o1-pro":        {InputCostPer1K: 0.15000, OutputCostPer1K: 0.60000}, // added 2026-10-05
	"openai:o3":            {InputCostPer1K: 0.00200, OutputCostPer1K: 0.00800}, // added 2026-10-05
	"openai:o3-mini":       {InputCostPer1K: 0.00110, OutputCostPer1K: 0.00440}, // added 2026-10-05
	"openai:o4-mini":       {InputCostPer1K: 0.00110, OutputCostPer1K: 0.00440}, // added 2026-10-05

	// Anthropic — every model /v1/models reports for this account, priced
	// from platform.claude.com/docs/en/about-claude/pricing, 2026-10-05.
	"anthropic:claude-fable-5-1":           {InputCostPer1K: 0.01000, OutputCostPer1K: 0.05000},
	"anthropic:claude-fable-5":             {InputCostPer1K: 0.01000, OutputCostPer1K: 0.05000}, // added 2026-10-05
	"anthropic:claude-opus-5-5":            {InputCostPer1K: 0.00400, OutputCostPer1K: 0.02000},
	"anthropic:claude-opus-5":              {InputCostPer1K: 0.00500, OutputCostPer1K: 0.02500},
	"anthropic:claude-opus-4-8":            {InputCostPer1K: 0.00500, OutputCostPer1K: 0.02500}, // added 2026-10-05
	"anthropic:claude-opus-4-7":            {InputCostPer1K: 0.00500, OutputCostPer1K: 0.02500}, // added 2026-10-05
	"anthropic:claude-opus-4-6":            {InputCostPer1K: 0.00500, OutputCostPer1K: 0.02500}, // rate CORRECTED 2026-10-05: was 0.015/0.075
	"anthropic:claude-opus-4-5-20251101":   {InputCostPer1K: 0.00500, OutputCostPer1K: 0.02500}, // added 2026-10-05
	"anthropic:claude-sonnet-5-5":          {InputCostPer1K: 0.00200, OutputCostPer1K: 0.01000},
	"anthropic:claude-sonnet-5":            {InputCostPer1K: 0.00200, OutputCostPer1K: 0.01000},
	"anthropic:claude-sonnet-4-6":          {InputCostPer1K: 0.00300, OutputCostPer1K: 0.01500},
	"anthropic:claude-sonnet-4-5-20250929": {InputCostPer1K: 0.00300, OutputCostPer1K: 0.01500}, // added 2026-10-05
	"anthropic:claude-haiku-4-5":           {InputCostPer1K: 0.00100, OutputCostPer1K: 0.00500}, // NOT in the account model list - see OPS-52
	"anthropic:claude-haiku-4-5-20251001":  {InputCostPer1K: 0.00100, OutputCostPer1K: 0.00500},

	// Legacy / replay-only rows, kept for historical re-scoring.
	"openai:gpt-4-turbo":                   {InputCostPer1K: 0.01000, OutputCostPer1K: 0.03000},
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

// bytesPerToken is the coarse char→token ratio used to convert byte
// sizes (from the Gatekeeper extractor) into token counts. ~4 bytes/token
// for English text. The single byte→token conversion point in the gateway
// (per Plan #7) so projected and measured reductions speak the same unit.
const bytesPerToken = 4.0

// TokensFromBytes converts a byte size to an estimated token count.
func TokensFromBytes(b int) int {
	if b <= 0 {
		return 0
	}
	return int(math.Ceil(float64(b) / bytesPerToken))
}

// Cost is the dollar breakdown of one request, returned by ActualCost.
// It's the *billed* cost in Contract v1 (= the invariant denominator the
// cost decomposition is bounded against). Priced=false when the
// vendor:model pricing isn't known — callers must not fabricate waste on
// unpriced traffic.
type Cost struct {
	InputUSD         float64 // uncached input tokens at the full input rate
	CacheCreationUSD float64 // cache-write (creation) tokens at CacheWriteMultiplier × input
	CacheReadUSD     float64 // cache-read (hit) tokens at CacheReadMultiplier × input
	OutputUSD        float64
	TotalUSD         float64
	Source           string // "vendor_usage" (counts from the vendor) | "computed"
	Priced           bool
	CacheAware       bool // true when cache-token counts were supplied (CacheCreation/CacheRead priced)
}

// Cache-rate multipliers, applied to a model's input rate. Every major
// provider prices cached tokens as a fraction/multiple of the input rate
// rather than as a separate per-model column, so we derive them here
// instead of widening modelPricingEntry:
//
//	cache-READ  (hit)      = 0.10× the input rate  (Anthropic; ~90% off)
//	cache-WRITE (creation) = 1.25× the input rate  (Anthropic, 5-minute TTL)
//
// The 1-hour-TTL write premium (2×) is not modeled — the gateway does not
// request 1-hour caching. Sources + cross-provider variation: AIQG_CACHING_PRIMER.md §4.
const (
	CacheReadMultiplier  = 0.10
	CacheWriteMultiplier = 1.25
)

// ActualCost computes the billed dollar cost of a request from token
// counts + the model's rates. In Contract v1 this equals the existing
// DollarCost total; it's surfaced separately as `actual_cost_usd` so the
// cost-decomposition fields have a stable denominator. usageFromVendor
// distinguishes vendor-reported usage from gateway-estimated counts
// (recorded as actual_cost_source).
func ActualCost(vendor, model string, promptTokens, completionTokens int, usageFromVendor bool) Cost {
	inputRate, outputRate, found := LookupPricing(vendor, model)
	if !found {
		return Cost{Priced: false}
	}
	in := (float64(promptTokens) / 1000.0) * inputRate
	out := (float64(completionTokens) / 1000.0) * outputRate
	src := "computed"
	if usageFromVendor {
		src = "vendor_usage"
	}
	return Cost{InputUSD: in, OutputUSD: out, TotalUSD: in + out, Source: src, Priced: true}
}

// CacheAwareCost prices a request using the vendor's cache-token breakdown.
// It is the cache-aware analog of ActualCost: instead of billing every input
// token at the full rate, it splits input into three classes at their real
// rates —
//
//	uncachedInput  × inputRate                          (full rate)
//	cacheCreation  × inputRate × CacheWriteMultiplier   (1.25×)
//	cacheRead      × inputRate × CacheReadMultiplier     (0.10×)
//
// This matches provider billing (e.g. Anthropic's usage reports input_tokens
// EXCLUSIVE of cache tokens, plus cache_creation_input_tokens and
// cache_read_input_tokens separately). Passing 0 for both cache counts yields
// exactly ActualCost's number, so callers with no cache data are unaffected.
// Returns Priced=false when the vendor:model pair isn't in the table.
func CacheAwareCost(vendor, model string, uncachedInput, cacheCreation, cacheRead, completion int, usageFromVendor bool) Cost {
	inputRate, outputRate, found := LookupPricing(vendor, model)
	if !found {
		return Cost{Priced: false}
	}
	in := (float64(uncachedInput) / 1000.0) * inputRate
	cw := (float64(cacheCreation) / 1000.0) * inputRate * CacheWriteMultiplier
	cr := (float64(cacheRead) / 1000.0) * inputRate * CacheReadMultiplier
	out := (float64(completion) / 1000.0) * outputRate
	src := "computed"
	if usageFromVendor {
		src = "vendor_usage"
	}
	return Cost{
		InputUSD:         in,
		CacheCreationUSD: cw,
		CacheReadUSD:     cr,
		OutputUSD:        out,
		TotalUSD:         in + cw + cr + out,
		Source:           src,
		Priced:           true,
		CacheAware:       true,
	}
}

// scoreCost converts the request's USD cost into a 0-100 score with a
// logarithmic curve. Tightened on 2026-06-06 by shifting the reference
// point one decade — modern small-context LLM calls (~$0.0005) were
// pegged at 100 under the old reference, so the chart never showed
// cost pressure for normal traffic. The new curve:
//
//	$0.0001 / request → 100 (Healthy)
//	$0.001  / request → 75  (typical small chat — now visible signal)
//	$0.01   / request → 50  (Marginal / Failing boundary)
//	$0.10   / request → 25
//	$1.00+  / request → 0
//
// Formula: score = 100 − 25 × log10(cost_usd × 10000), clamped to [0,100].
// Same 25-per-decade slope as before; just shifted left by one decade
// so the Healthy/Marginal/Failing bands cover the cost ranges actually
// observed in production rather than the spec's pre-LLM-era examples.
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
	score := 100.0 - 25.0*math.Log10(cost*10000.0)
	switch {
	case score > 100:
		score = 100
	case score < 0:
		score = 0
	}
	s := Score(score)
	return &s
}
