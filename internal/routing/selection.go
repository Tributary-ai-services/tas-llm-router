package routing

import (
	"context"
	"strings"
	"time"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"

	"github.com/tributary-ai/llm-router-waf/internal/providers"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// Rule-driven selection — routing-decision.md §5.7, step 5.
//
// A matched route rule may replace the gateway's configured strategy for one
// request. This is the entry point for expected_cost and weighted; the existing
// cost / performance / round-robin paths are untouched, because a rule that does
// not ask for a new strategy must behave exactly as it did before.

// selectionKey carries the rule's selection and switching blocks, and the
// measured verbosity table, on the request context — alongside the pin, the
// chain and affinity, for the same reason: routing metadata must never reach a
// vendor payload.
type selectionKey struct{}

type selectionCtx struct {
	Selection resilience.Selection
	Switching resilience.Switching
	Verbosity []resilience.Verbosity
	// StabilityKey makes weighted selection deterministic per conversation and
	// gives dwell something to key on.
	StabilityKey string
}

// WithSelection returns a context carrying a rule's selection settings.
func WithSelection(ctx context.Context, sel resilience.Selection, sw resilience.Switching, verbosity []resilience.Verbosity, stabilityKey string) context.Context {
	if sel.IsZero() && sw.IsZero() && len(verbosity) == 0 {
		return ctx
	}
	return context.WithValue(ctx, selectionKey{}, selectionCtx{
		Selection: sel, Switching: sw, Verbosity: verbosity, StabilityKey: stabilityKey,
	})
}

// SelectionFrom returns the request's selection settings.
func SelectionFrom(ctx context.Context) (selectionCtx, bool) {
	s, ok := ctx.Value(selectionKey{}).(selectionCtx)
	return s, ok
}

// SetDwellStore attaches fleet-wide dwell state for switching hysteresis.
func (r *Router) SetDwellStore(d DwellStore) { r.dwell = d }

// modelPrices resolves per-1k prices from the provider capability matrix, which
// is where the gateway already holds them.
//
// This is the concrete half of open question 6: dashboard-be measures the
// TOKENS and the gateway applies the PRICES. Only one service knows prices, and
// it is the one computing cost — so the two cannot disagree about a bill.
func (r *Router) modelPrices(provider, model string) (float64, float64, bool) {
	p, ok := r.providers[provider]
	if !ok {
		return 0, 0, false
	}
	for _, m := range p.GetCapabilities().SupportedModels {
		if strings.EqualFold(m.Name, model) || strings.EqualFold(m.ProviderModelID, model) {
			if m.InputCostPer1K <= 0 && m.OutputCostPer1K <= 0 {
				return 0, 0, false
			}
			return m.InputCostPer1K, m.OutputCostPer1K, true
		}
	}
	return 0, 0, false
}

// routeBySelection applies a rule's strategy, or reports that it did not.
//
// Returns handled=false when no rule asked for one of the new strategies, so
// the caller falls through to the existing behaviour untouched.
func (r *Router) routeBySelection(ctx context.Context, req *types.ChatRequest, workflow string) (*RoutingDecision, providers.LLMProvider, bool) {
	sc, ok := SelectionFrom(ctx)
	if !ok {
		return nil, nil, false
	}
	chain := ChainFrom(ctx)
	// Quality gates filter the candidate set BEFORE selection prices anything.
	// Lexicographic: a candidate either clears the floor or it does not, and
	// cost only chooses between those that already have.
	gated := map[string]bool{}
	for _, name := range r.gatedCandidates(ctx, workflow) {
		gated[name] = true
	}
	eligible := func(name string) bool {
		return chain.AllowedTarget(name) && r.isProviderHealthy(ctx, name) && gated[name]
	}

	switch sc.Selection.Strategy {
	case resilience.StrategyWeighted:
		name, picked := weightedPick(sc.Selection.Weights, sc.StabilityKey, eligible)
		if !picked {
			// Every weighted arm ineligible: fall through rather than fail, so
			// a canary whose arms are both unhealthy still serves traffic.
			return nil, nil, false
		}
		prov := r.providers[name]
		return &RoutingDecision{
			SelectedProvider: name,
			Reasoning:        []string{"weighted selection chose " + name},
		}, prov, true

	case resilience.StrategyExpectedCost:
		return r.routeByExpectedCost(ctx, req, workflow, sc, eligible)
	}
	return nil, nil, false
}

// routeByExpectedCost prices every eligible candidate from measured verbosity
// and picks the cheapest, subject to switching hysteresis.
func (r *Router) routeByExpectedCost(ctx context.Context, req *types.ChatRequest, workflow string, sc selectionCtx, eligible func(string) bool) (*RoutingDecision, providers.LLMProvider, bool) {
	idx := newVerbosityIndex(sc.Verbosity, resilience.DefaultVerbositySampleFloor)
	inTokens := estimateInputTokens(req)

	var estimates []costEstimate
	for _, name := range r.providerNames {
		if !eligible(name) {
			continue
		}
		model := req.Model
		if model == "" {
			continue
		}
		estimates = append(estimates, idx.estimate(name, model, workflow, req, inTokens, r.modelPrices))
	}
	if len(estimates) == 0 {
		return nil, nil, false
	}

	best, anyMeasured := cheapest(estimates)
	if best.Provider == "" {
		return nil, nil, false
	}

	reasoning := []string{}
	if anyMeasured {
		reasoning = append(reasoning, "expected_cost: priced from measured verbosity")
	} else {
		// The honest report. Claiming expected_cost while every candidate fell
		// back to max_tokens would describe a strategy that did not run — and
		// an operator who believes it is running has no reason to look.
		reasoning = append(reasoning, "expected_cost abstained: no usable measurements; priced at max_tokens as before")
	}
	if best.Reason != "" {
		reasoning = append(reasoning, best.Reason)
	}

	// Hysteresis applies only when we are proposing a CHANGE from the model's
	// own provider — a first request has nothing to switch away from.
	if current, found := r.getProviderForModel(req.Model); found && current != best.Provider {
		cur := idx.estimate(current, req.Model, workflow, req, inTokens, r.modelPrices)
		warm := AffinityFrom(ctx).Held
		d := ShouldSwitch(sc.Switching, sc.StabilityKey, cur, best, warm, r.dwell, nowFunc())
		if !d.Allow {
			prov := r.providers[current]
			return &RoutingDecision{
				SelectedProvider: current,
				EstimatedCost:    cur.Total,
				Reasoning:        append(reasoning, "stayed on "+current+": "+d.Reason),
			}, prov, true
		}
		if r.dwell != nil && sc.Switching.DwellSeconds > 0 {
			r.dwell.RecordSwitch(sc.StabilityKey, nowFunc(), dwellTTL(sc.Switching.DwellSeconds))
		}
	}

	return &RoutingDecision{
		SelectedProvider: best.Provider,
		EstimatedCost:    best.Total,
		Reasoning:        reasoning,
	}, r.providers[best.Provider], true
}

// estimateInputTokens approximates prompt size. Deliberately crude: input is
// 1-10% of the bill, and a better estimate here would refine the term that
// barely matters while the output term — the one this whole feature exists to
// fix — carries the decision.
func estimateInputTokens(req *types.ChatRequest) int {
	if req == nil {
		return 0
	}
	chars := 0
	for _, m := range req.Messages {
		if s, ok := m.Content.(string); ok {
			chars += len(s)
		}
	}
	return chars / 4
}

// workflowKey carries the request's workflow for verbosity lookup, which is
// keyed on (model, workflow).
type workflowKey struct{}

// WithWorkflow returns a context carrying the request's workflow.
func WithWorkflow(ctx context.Context, w string) context.Context {
	if w == "" {
		return ctx
	}
	return context.WithValue(ctx, workflowKey{}, w)
}

// WorkflowFrom returns the request's workflow, or "".
func WorkflowFrom(ctx context.Context) string {
	w, _ := ctx.Value(workflowKey{}).(string)
	return w
}

// nowFunc is the clock, injectable for tests.
var nowFunc = time.Now

// dwellTTL bounds the stored switch record to the window it governs: once the
// window passes the record has no meaning.
func dwellTTL(seconds int) time.Duration { return time.Duration(seconds) * time.Second }

// gatedCandidates returns the providers surviving this rule's quality floors,
// and stamps what was excluded onto the decision context.
func (r *Router) gatedCandidates(ctx context.Context, workflow string) []string {
	modelFor := func(provider string) string {
		// Quality is measured per model, and a provider serves whichever model
		// the request named — so the candidate's model is the request's model.
		if req, ok := ctx.Value(gateModelKey{}).(string); ok {
			return req
		}
		return ""
	}
	kept, excluded, note := gateCandidates(ctx, r.providerNames, modelFor, workflow)
	if h, ok := ctx.Value(gateResultKey{}).(*gateResultHolder); ok {
		h.excluded, h.note = excluded, note
	}
	return kept
}

type gateModelKey struct{}
type gateResultKey struct{}

type gateResultHolder struct {
	excluded []ExcludedCandidate
	note     string
}

// WithGateContext prepares a request for quality gating, carrying the model
// being judged and a slot for the result.
func WithGateContext(ctx context.Context, model string) context.Context {
	ctx = context.WithValue(ctx, gateModelKey{}, model)
	return context.WithValue(ctx, gateResultKey{}, &gateResultHolder{})
}

// GateExclusions returns candidates removed by quality floors, and any note
// about abstention, for stamping onto the event.
func GateExclusions(ctx context.Context) ([]ExcludedCandidate, string) {
	h, ok := ctx.Value(gateResultKey{}).(*gateResultHolder)
	if !ok {
		return nil, ""
	}
	return h.excluded, h.note
}
