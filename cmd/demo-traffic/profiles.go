// Agent traffic profiles for the AIQG demo-traffic generator.
//
// Eight profiles cover the real traffic shapes that flow through the
// gateway. Each maps to one of the canonical CLEAR workflow types
// (workflow-classification.md §1.1) and carries distributions for
// tokens, latency, finish_reason, and retry behaviour tuned so that —
// once fed through the real clear.Compute scorer — the CLEAR grid shows
// a believable spread rather than a flat line.
//
// Compliance / vague / hedging are NOT separate profiles; per the
// product direction they're sprinkled across every profile as
// cross-cutting attributes (see synth.go's attribute injectors), with
// each profile carrying a signature matcher that's guaranteed to fire
// at least once per pass so every dashboard panel lights up.
package main

import "math/rand"

// rng wraps the seeded source so sampling helpers read cleanly.
type rng struct{ r *rand.Rand }

func (g rng) intIn(lo, hi int) int {
	if hi <= lo {
		return lo
	}
	return lo + g.r.Intn(hi-lo+1)
}

func (g rng) chance(p float64) bool { return g.r.Float64() < p }

func (g rng) pickMatcher(opts []matcher) matcher { return opts[g.r.Intn(len(opts))] }

// modelChoice is a vendor:model pair the profile routes to. Only pairs
// present in pkg/clear's modelPricing table produce a Cost score, so
// every entry here must exist there.
type modelChoice struct {
	Vendor string
	Model  string
}

// profile defines one agent traffic shape.
type profile struct {
	Key      string // stable id, used in labels/logs
	Workflow string // canonical CLEAR workflow_type
	Models   []modelChoice

	PromptTokensLo, PromptTokensHi         int
	CompletionTokensLo, CompletionTokensHi int

	// EndToEndMs is sampled in [Lo,Hi]; tune relative to the workflow's
	// SLA in clear/clear.go slaMsForWorkflow so the Latency score moves.
	E2ELoMs, E2EHiMs int64

	// finishReason distribution. Weighted pick over the entries.
	FinishReasons []weightedStr
	// attemptCount distribution (1 = clean first try).
	Attempts []weightedInt
	// FallbackProb is the chance a retried request also used a fallback
	// provider (extra Reliability penalty).
	FallbackProb float64

	// SignatureMatcher fires on the first event of every pass for this
	// profile, guaranteeing the profile's characteristic story shows up
	// even in a tiny single pass. Zero value = no signature.
	SignatureMatcher matcher

	// Extra per-profile tendency to carry its signature matcher on
	// subsequent events (on top of the global cross-cutting rates).
	SignatureProb float64
}

type weightedStr struct {
	V string
	W int
}
type weightedInt struct {
	V int
	W int
}

func (g rng) weightedStr(opts []weightedStr) string {
	total := 0
	for _, o := range opts {
		total += o.W
	}
	n := g.r.Intn(total)
	for _, o := range opts {
		if n < o.W {
			return o.V
		}
		n -= o.W
	}
	return opts[len(opts)-1].V
}

func (g rng) weightedInt(opts []weightedInt) int {
	total := 0
	for _, o := range opts {
		total += o.W
	}
	n := g.r.Intn(total)
	for _, o := range opts {
		if n < o.W {
			return o.V
		}
		n -= o.W
	}
	return opts[len(opts)-1].V
}

// Common model menus.
var (
	cheapModels = []modelChoice{
		{"openai", "gpt-4o-mini"},
		{"anthropic", "claude-haiku-4-5-20251001"},
	}
	midModels = []modelChoice{
		{"openai", "gpt-4o"},
		{"anthropic", "claude-sonnet-4-6"},
	}
	frontierModels = []modelChoice{
		{"openai", "gpt-4o"},
		{"anthropic", "claude-sonnet-4-6"},
		{"anthropic", "claude-opus-4-6"},
	}
)

// profiles is the canonical eight-agent catalog.
var profiles = []profile{
	{
		Key:                "basic_qa",
		Workflow:           "single_turn_qa",
		Models:             cheapModels,
		PromptTokensLo:     50, PromptTokensHi: 400,
		CompletionTokensLo: 30, CompletionTokensHi: 300,
		E2ELoMs:            400, E2EHiMs: 2400, // SLA 3000 → mostly Healthy
		FinishReasons:      []weightedStr{{"stop", 97}, {"length", 3}},
		Attempts:           []weightedInt{{1, 98}, {2, 2}},
		FallbackProb:       0.0,
		// No signature — basic Q&A is the clean baseline.
	},
	{
		Key:                "conversational",
		Workflow:           "single_turn_qa", // multi-turn variant; history inflates prompt tokens
		Models:             cheapModels,
		PromptTokensLo:     300, PromptTokensHi: 3000,
		CompletionTokensLo: 80, CompletionTokensHi: 600,
		E2ELoMs:            800, E2EHiMs: 3800, // some breaches of the 3000 SLA → Latency dips
		FinishReasons:      []weightedStr{{"stop", 95}, {"length", 5}},
		Attempts:           []weightedInt{{1, 88}, {2, 10}, {3, 2}}, // reliability dips over a session
		FallbackProb:       0.05,
		SignatureMatcher:   mHallucinationHedge,
		SignatureProb:      0.10,
	},
	{
		Key:                "rag",
		Workflow:           "rag",
		Models:             midModels,
		PromptTokensLo:     1500, PromptTokensHi: 12000, // retrieved context
		CompletionTokensLo: 100, CompletionTokensHi: 700,
		E2ELoMs:            1500, E2EHiMs: 6500, // SLA 5000
		FinishReasons:      []weightedStr{{"stop", 96}, {"length", 4}},
		Attempts:           []weightedInt{{1, 96}, {2, 4}},
		FallbackProb:       0.02,
		SignatureMatcher:   mBloatedContext, // the RAG bloat story
		SignatureProb:      0.30,
	},
	{
		Key:                "mcp_agentic",
		Workflow:           "agentic",
		Models:             midModels,
		PromptTokensLo:     800, PromptTokensHi: 5000,
		CompletionTokensLo: 100, CompletionTokensHi: 1200,
		E2ELoMs:            4000, E2EHiMs: 42000, // SLA 30000 → long-tail latency
		FinishReasons:      []weightedStr{{"tool_calls", 70}, {"stop", 25}, {"content_filter", 5}},
		Attempts:           []weightedInt{{1, 75}, {2, 18}, {3, 7}}, // tool loops retry
		FallbackProb:       0.12,
		SignatureMatcher:   mRefusal, // agentic refusal/abort story
		SignatureProb:      0.18,
	},
	{
		Key:                "summarization",
		Workflow:           "summarization",
		Models:             midModels,
		PromptTokensLo:     4000, PromptTokensHi: 20000, // large input
		CompletionTokensLo: 150, CompletionTokensHi: 900, // small output
		E2ELoMs:            3000, E2EHiMs: 18000, // SLA 15000
		FinishReasons:      []weightedStr{{"stop", 85}, {"length", 15}}, // truncation common
		Attempts:           []weightedInt{{1, 97}, {2, 3}},
		FallbackProb:       0.02,
		SignatureMatcher:   mInstructionStuffing,
		SignatureProb:      0.15,
	},
	{
		Key:                "code_generation",
		Workflow:           "code_generation",
		Models:             frontierModels,
		PromptTokensLo:     400, PromptTokensHi: 4000,
		CompletionTokensLo: 200, CompletionTokensHi: 2500,
		E2ELoMs:            3000, E2EHiMs: 30000, // SLA 30000
		FinishReasons:      []weightedStr{{"stop", 88}, {"length", 12}},
		Attempts:           []weightedInt{{1, 85}, {2, 12}, {3, 3}}, // regenerate on bad code
		FallbackProb:       0.06,
		SignatureMatcher:   mMalformedOutput,
		SignatureProb:      0.20,
	},
	{
		Key:                "classification_extraction",
		Workflow:           "classification_extraction",
		Models:             cheapModels,
		PromptTokensLo:     200, PromptTokensHi: 1500,
		CompletionTokensLo: 20, CompletionTokensHi: 200, // structured, tiny
		E2ELoMs:            300, E2EHiMs: 4200, // SLA 5000
		FinishReasons:      []weightedStr{{"stop", 92}, {"length", 8}},
		Attempts:           []weightedInt{{1, 95}, {2, 5}},
		FallbackProb:       0.01,
		SignatureMatcher:   mMalformedOutput, // JSON conformance failures
		SignatureProb:      0.12,
	},
	{
		Key:                "multi_agent_orchestration",
		Workflow:           "agentic",
		Models:             frontierModels,
		PromptTokensLo:     2000, PromptTokensHi: 15000, // fan-out context amplification
		CompletionTokensLo: 300, CompletionTokensHi: 2500,
		E2ELoMs:            8000, E2EHiMs: 50000, // SLA 30000 → frequent breaches
		FinishReasons:      []weightedStr{{"tool_calls", 78}, {"stop", 18}, {"content_filter", 4}},
		Attempts:           []weightedInt{{1, 70}, {2, 20}, {3, 10}},
		FallbackProb:       0.15,
		SignatureMatcher:   mUnboundedLoop, // the headline savings story
		SignatureProb:      0.25,
	},
}
