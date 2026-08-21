package routing

import (
	"strings"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// expected_cost — pricing a candidate by what it will probably emit, not by the
// ceiling it is allowed to emit (routing-decision.md §5.7, step 5).
//
// # What this replaces
//
// EstimateCost in both providers estimates output as max_tokens — the CEILING —
// defaulting to a literal 100 when unset. Output is 90-99% of the bill. So cost
// routing optimises the ~8% it can see and guesses the ~92% that decides the
// price. Across our own events on one workflow with near-identical input, models
// differ in output length by 7.3x, which is far larger than the price gaps
// routing is trying to exploit.
//
// # The estimate, and why max_tokens stays in it
//
//	expected = in_tokens x p_in + min(E[out], max_tokens) x p_out
//
// The expectation is what you expect to pay. max_tokens is the cap you cannot
// exceed, and it stays in as a CAP rather than being replaced, because a
// truncated answer is still an answer you paid for. Dropping it would let a
// model with a long measured tail look expensive on a request that could never
// reach that tail.
//
// # Abstention is a first-class outcome
//
// Below the sample floor, or with no measurement at all, this does NOT quietly
// fall back to the max_tokens guess and present the result as measured. It
// reports that it abstained and why, so the event can say so. A router that
// silently reverts to the behaviour it was meant to replace — while an operator
// believes expected_cost is running — is worse than one that never shipped: the
// operator has no reason to look.
//
// # Verbosity is not quality
//
// Unguarded, this optimises for terseness: it will happily prefer a model that
// answers badly in fewer tokens. It is meant to pair with the efficacy floor in
// §5.8 (step 6). Until that exists, expected_cost is opt-in per rule, and the
// UI should say what it optimises for.

// priceLookup resolves per-1k token prices for a provider's model.
type priceLookup func(provider, model string) (inPer1K, outPer1K float64, ok bool)

// costEstimate is one candidate's price, with the evidence behind it.
type costEstimate struct {
	Provider string
	Model    string
	// Total is the estimated dollar cost for this request.
	Total float64
	// Measured is true when E[out] came from the verbosity table rather than
	// from max_tokens.
	Measured bool
	// Reason explains an abstention, for the event.
	Reason string
	// ExpectedOut is the output-token expectation actually used.
	ExpectedOut float64
}

// verbosityIndex is a lookup over the measured table.
type verbosityIndex struct {
	rows  map[string]resilience.Verbosity
	floor int
}

func newVerbosityIndex(rows []resilience.Verbosity, floor int) *verbosityIndex {
	idx := &verbosityIndex{rows: make(map[string]resilience.Verbosity, len(rows)), floor: floor}
	if idx.floor <= 0 {
		idx.floor = resilience.DefaultVerbositySampleFloor
	}
	for _, r := range rows {
		idx.rows[verbosityKey(r.Model, r.Workflow)] = r
	}
	return idx
}

func verbosityKey(model, workflow string) string {
	return strings.ToLower(strings.TrimSpace(model)) + "\x00" + strings.ToLower(strings.TrimSpace(workflow))
}

// lookup returns the usable measurement for a (model, workflow), falling back
// to the workflow-agnostic row.
//
// The fallback matters because workflow is optional on a request: a tenant that
// never sets it would otherwise have no measurements at all, even with
// thousands of events. A workflow-specific row is preferred when present since
// verbosity varies more by workload than by anything else — the same model
// measured 131 tokens on single_turn_qa and 285 on code_generation.
func (v *verbosityIndex) lookup(model, workflow string) (resilience.Verbosity, bool) {
	if v == nil {
		return resilience.Verbosity{}, false
	}
	if workflow != "" {
		if r, ok := v.rows[verbosityKey(model, workflow)]; ok && r.Usable(v.floor) {
			return r, true
		}
	}
	if r, ok := v.rows[verbosityKey(model, "")]; ok && r.Usable(v.floor) {
		return r, true
	}
	return resilience.Verbosity{}, false
}

// estimate prices one candidate.
func (v *verbosityIndex) estimate(provider, model, workflow string, req *types.ChatRequest, inTokens int, prices priceLookup) costEstimate {
	est := costEstimate{Provider: provider, Model: model}
	pIn, pOut, ok := prices(provider, model)
	if !ok {
		est.Reason = "no price configured for " + provider + "/" + model
		return est
	}

	row, measured := v.lookup(model, workflow)
	expectedOut := 0.0
	switch {
	case measured:
		expectedOut, est.Measured = row.MeanOutputTokens, true
	default:
		// The honest fallback: today's behaviour, reported as such.
		expectedOut = float64(maxTokensOrDefault(req))
		est.Reason = "no usable verbosity measurement for " + model + "/" + workflowOrNone(workflow) +
			"; priced at max_tokens as before"
	}

	// max_tokens as a CAP, always — including on the measured path. A model
	// whose measured mean exceeds this request's cap cannot actually emit that
	// many tokens here.
	if mt := maxTokensOrDefault(req); mt > 0 && expectedOut > float64(mt) {
		expectedOut = float64(mt)
	}

	est.ExpectedOut = expectedOut
	est.Total = (float64(inTokens)/1000.0)*pIn + (expectedOut/1000.0)*pOut
	return est
}

func workflowOrNone(w string) string {
	if w == "" {
		return "(no workflow)"
	}
	return w
}

// maxTokensOrDefault mirrors the providers' own default so the fallback path
// prices identically to today rather than subtly differently.
func maxTokensOrDefault(req *types.ChatRequest) int {
	if req != nil && req.MaxTokens != nil && *req.MaxTokens > 0 {
		return *req.MaxTokens
	}
	return 100
}

// cheapest returns the lowest-cost estimate, and whether ANY candidate was
// priced from a measurement.
//
// The second return exists so the caller can stamp a single honest answer on
// the event. Reporting "expected_cost" when every candidate fell back to
// max_tokens would describe a strategy that did not run.
func cheapest(estimates []costEstimate) (costEstimate, bool) {
	var best costEstimate
	anyMeasured := false
	for i, e := range estimates {
		if e.Total <= 0 {
			continue // unpriced candidate; skip rather than treat as free
		}
		if e.Measured {
			anyMeasured = true
		}
		if i == 0 || best.Total <= 0 || e.Total < best.Total {
			best = e
		}
	}
	return best, anyMeasured
}
