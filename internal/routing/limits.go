package routing

import (
	"context"
	"strconv"
	"strings"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// Context and output limits — routing-decision.md §5, step 7.
//
// # What this turns into a decision
//
// Without limits, a request exceeding a model's window is discovered by the
// VENDOR: a 400 after the full round trip, worded by whoever wrote that API,
// with the latency already spent. With them the gateway knows the window before
// it dials.
//
// # The interaction that decides whether this feature helps or hurts
//
// A pre-flight rejection must feed the SAME path as a vendor context-overflow,
// not bypass it.
//
// Context overflow is fallback-eligible precisely because a larger-window model
// can serve the identical request unchanged. So if a limit breach returned an
// error to the caller, limits would be strictly WORSE than having none: the
// vendor's 400 would at least have advanced the chain, while our tidy local
// rejection would not. Detecting the problem earlier must not mean recovering
// from it less.
//
// So a breach is translated into the same failure class the chain already knows
// how to act on, and the chain decides. Only when no tier can fit does the
// caller see an error — and then it is a better error than the vendor's,
// because it names the window and the overage.
//
// # Estimation is deliberately crude
//
// A four-characters-per-token approximation. Precision here would buy little:
// the decision is whether a prompt is roughly within a window that is usually
// orders of magnitude larger or smaller than the request, and a request close
// enough to the boundary for estimator error to matter will be caught by the
// vendor anyway — which is the path that already works.

// limitsKey carries the rule's limits on the request context.
type limitsKey struct{}

// WithLimits returns a context carrying context/output caps.
func WithLimits(ctx context.Context, l resilience.Limits) context.Context {
	if l.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, limitsKey{}, l)
}

// LimitsFrom returns the request's caps.
func LimitsFrom(ctx context.Context) resilience.Limits {
	l, _ := ctx.Value(limitsKey{}).(resilience.Limits)
	return l
}

// LimitBreach describes a request that cannot fit a target.
type LimitBreach struct {
	Provider string
	Model    string
	// Window is the effective limit that was exceeded, and Overage how far
	// past it the request is. Both travel because "3,000 tokens over a 200,000
	// window" tells an operator whether to trim a prompt or change models,
	// while "too large" does not.
	Window  int
	Overage int
	// FromLimit is true when the tenant's configured cap bound the request
	// rather than the model's own window — the two call for different fixes,
	// and only one of them is the vendor's doing.
	FromLimit bool
}

// Error renders a breach for a caller.
func (b LimitBreach) Error() string {
	src := "the model's context window"
	if b.FromLimit {
		src = "this route's configured context limit"
	}
	return "request is " + strconv.Itoa(b.Overage) + " tokens over " + src +
		" of " + strconv.Itoa(b.Window) + " for " + b.Provider + "/" + b.Model
}

// CheckLimits reports whether a request fits a target, using the model's
// advertised capability and the rule's caps.
//
// Returns nil when it fits, or when nothing is known about the model — an
// unknown window must not become an implicit zero, which would reject
// everything.
func (r *Router) CheckLimits(ctx context.Context, provider, model string, req *types.ChatRequest) *LimitBreach {
	advertisedCtx, _, ok := r.modelCaps(provider, model)
	if !ok {
		return nil
	}
	l := LimitsFrom(ctx)
	estimated := estimateInputTokens(req)
	over, by := l.ExceedsContext(estimated, advertisedCtx)
	if !over {
		return nil
	}
	window := l.EffectiveContextWindow(advertisedCtx)
	return &LimitBreach{
		Provider: provider, Model: model,
		Window: window, Overage: by,
		FromLimit: l.MaxContextWindow > 0 && l.MaxContextWindow <= advertisedCtx,
	}
}

// ApplyOutputCap lowers a request's max_tokens to the effective ceiling.
//
// Mutates the request rather than rejecting it: a caller asking for more output
// than the tenant permits has still made a serviceable request, and bounding it
// serves them better than refusing. Raising is never possible — the effective
// cap is the smallest of advertised, configured and requested.
func (r *Router) ApplyOutputCap(ctx context.Context, provider, model string, req *types.ChatRequest) (applied int, changed bool) {
	if req == nil {
		return 0, false
	}
	_, advertisedOut, ok := r.modelCaps(provider, model)
	if !ok {
		return 0, false
	}
	l := LimitsFrom(ctx)
	requested := 0
	if req.MaxTokens != nil {
		requested = *req.MaxTokens
	}
	eff := l.EffectiveOutputTokens(advertisedOut, requested)
	if eff <= 0 || eff == requested {
		return requested, false
	}
	req.MaxTokens = &eff
	return eff, true
}

// modelCaps reads a model's advertised capability from the provider matrix.
//
// This is the seam the model registry (tas-llm-router #2-#6) will replace: it
// changes where the numbers come from, not what limits mean. Everything above
// this function is written against the numbers, not their source.
func (r *Router) modelCaps(provider, model string) (contextWindow, outputTokens int, ok bool) {
	p, exists := r.providers[provider]
	if !exists {
		return 0, 0, false
	}
	for _, m := range p.GetCapabilities().SupportedModels {
		if strings.EqualFold(m.Name, model) || strings.EqualFold(m.ProviderModelID, model) {
			return m.MaxContextWindow, m.MaxOutputTokens, true
		}
	}
	return 0, 0, false
}

// ModelCapsFor exposes advertised capability for write-time validation.
func (r *Router) ModelCapsFor(provider, model string) (resilience.ModelCaps, bool) {
	c, o, ok := r.modelCaps(provider, model)
	if !ok {
		return resilience.ModelCaps{}, false
	}
	return resilience.ModelCaps{Provider: provider, Model: model, MaxContextWindow: c, MaxOutputTokens: o}, true
}
