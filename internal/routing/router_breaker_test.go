package routing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/breaker"
)

// These tests cover the join between the router and the breaker: the breaker's
// own semantics are proven in pkg/aiqg/breaker. What matters here is that an
// ejection actually changes WHICH PROVIDER SERVES A REQUEST — a breaker whose
// verdict never reaches selection is an elaborate no-op.

func breakerRouter(t *testing.T, cfg breaker.Config) (*Router, *breaker.Breaker) {
	t.Helper()
	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)
	r := NewRouter(logger)
	r.RegisterProvider("openai", &stubProvider{name: "openai"})
	r.RegisterProvider("anthropic", &stubProvider{name: "anthropic"})
	// Mark both healthy so the ACTIVE probe cannot be what excludes a
	// provider — otherwise this would pass for the wrong reason.
	r.updateHealthStatus(context.Background())

	b := breaker.New(breaker.NewMemoryStore(), cfg)
	r.SetBreaker(b)
	return r, b
}

func TestEjectedProviderIsNotSelected(t *testing.T) {
	r, b := breakerRouter(t, breaker.Config{ConsecutiveErrors: 2})
	ctx := context.Background()

	// Eject openai at the provider level and refresh the cache candidate
	// filtering reads.
	for i := 0; i < 2; i++ {
		b.Record(ctx, breaker.Target("openai", ""), breaker.ServerError)
	}
	r.refreshBreakerCache(ctx)

	if r.isProviderHealthy("openai") {
		t.Fatal("ejected provider still reported healthy — the breaker verdict never reached selection")
	}
	if !r.isProviderHealthy("anthropic") {
		t.Fatal("ejecting one provider must not affect another")
	}
}

// The active probe and the breaker are independent inputs, and either one
// alone is enough to exclude a provider.
func TestActiveProbeAndBreakerAreIndependent(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)
	r := NewRouter(logger)
	r.RegisterProvider("openai", &stubProvider{name: "openai", healthErr: errors.New("probe down")})
	r.updateHealthStatus(context.Background())
	r.SetBreaker(breaker.New(breaker.NewMemoryStore(), breaker.Config{}))

	// Breaker is perfectly happy; the probe is not. Still excluded.
	if r.isProviderHealthy("openai") {
		t.Fatal("probe-unhealthy provider was selected despite a closed breaker")
	}
}

// A pin does not exempt a target from the breaker — but with no alternative
// the request proceeds rather than failing, which is the documented posture
// (ejection shifts traffic; it is not a kill switch).
func TestAdmitReselectsWhenSelectedProviderIsEjected(t *testing.T) {
	r, b := breakerRouter(t, breaker.Config{ConsecutiveErrors: 2})
	ctx := context.Background()
	req := &types.ChatRequest{Model: "gpt-4o-mini"}

	for i := 0; i < 2; i++ {
		b.Record(ctx, breaker.Target("openai", req.Model), breaker.ServerError)
	}
	r.refreshBreakerCache(ctx)

	dec := &RoutingDecision{SelectedProvider: "openai", Reasoning: []string{"initial"}}
	got, provider := r.admitOrReselect(ctx, req, dec, r.providers["openai"], RoutingStrategySpecific)

	if got.SelectedProvider != "anthropic" {
		t.Fatalf("selected %q after openai was ejected, want anthropic", got.SelectedProvider)
	}
	if provider == nil {
		t.Fatal("reselection returned a nil provider")
	}
	// The event must explain itself; "why did this go elsewhere" cannot
	// require reading gateway logs.
	if !reasoningMentions(got.Reasoning, "breaker ejected openai") {
		t.Fatalf("reasoning %v does not record the ejection", got.Reasoning)
	}
}

func TestProceedsWhenEjectedAndNoAlternativeExists(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)
	r := NewRouter(logger)
	r.RegisterProvider("openai", &stubProvider{name: "openai"})
	r.updateHealthStatus(context.Background())
	b := breaker.New(breaker.NewMemoryStore(), breaker.Config{ConsecutiveErrors: 2})
	r.SetBreaker(b)

	ctx := context.Background()
	req := &types.ChatRequest{Model: "gpt-4o-mini"}
	for i := 0; i < 2; i++ {
		b.Record(ctx, breaker.Target("openai", req.Model), breaker.ServerError)
	}
	r.refreshBreakerCache(ctx)

	dec := &RoutingDecision{SelectedProvider: "openai"}
	got, provider := r.admitOrReselect(ctx, req, dec, r.providers["openai"], RoutingStrategySpecific)
	if got.SelectedProvider != "openai" || provider == nil {
		t.Fatal("with nowhere else to go the request must proceed, not fail")
	}
	if !reasoningMentions(got.Reasoning, "no alternative") {
		t.Fatalf("reasoning %v does not record that it proceeded anyway", got.Reasoning)
	}
}

// RecordOutcome must write BOTH granularities: a vendor-wide outage and a
// single degraded model need different responses, and recording only one makes
// the other undetectable.
func TestRecordOutcomeWritesBothGranularities(t *testing.T) {
	r, b := breakerRouter(t, breaker.Config{ConsecutiveErrors: 2})
	ctx := context.Background()

	r.RecordOutcome(ctx, "openai", "gpt-4o-mini", breaker.ServerError)
	r.RecordOutcome(ctx, "openai", "gpt-4o-mini", breaker.ServerError)

	provState, _ := b.Status(ctx, breaker.Target("openai", ""))
	modelState, _ := b.Status(ctx, breaker.Target("openai", "gpt-4o-mini"))
	if provState.State != breaker.Open {
		t.Fatalf("provider-level target state = %v, want open", provState.State)
	}
	if modelState.State != breaker.Open {
		t.Fatalf("model-level target state = %v, want open", modelState.State)
	}
}

// The tas-llm-router#151 shape, end to end through the router: a pinned
// provider that does not serve the requested model produces a 404 stream. It
// must never eject the provider, or one bad rule takes a healthy vendor out
// for every tenant on the gateway.
func TestModelNotFoundDoesNotEjectTheProvider(t *testing.T) {
	r, _ := breakerRouter(t, breaker.Config{ConsecutiveErrors: 2})
	ctx := context.Background()
	notFound := errors.New(`anthropic api call failed: POST "https://api.anthropic.com/v1/messages": 404 Not Found {"type":"not_found_error","message":"model: gpt-4o-mini"}`)

	for i := 0; i < 50; i++ {
		r.RecordOutcome(ctx, "anthropic", "gpt-4o-mini", breaker.ClassifyError(notFound))
	}
	r.refreshBreakerCache(ctx)

	if !r.isProviderHealthy("anthropic") {
		t.Fatal("a misconfigured pin ejected a healthy provider — client errors must never count")
	}
}

// A nil breaker must leave every path working, so the feature can be switched
// off without touching call sites.
func TestNilBreakerLeavesRoutingUnchanged(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)
	r := NewRouter(logger)
	r.RegisterProvider("openai", &stubProvider{name: "openai"})
	r.updateHealthStatus(context.Background())

	ctx := context.Background()
	r.RecordOutcome(ctx, "openai", "gpt-4o-mini", breaker.ServerError) // must not panic
	if !r.isProviderHealthy("openai") {
		t.Fatal("nil breaker excluded a healthy provider")
	}
	dec := &RoutingDecision{SelectedProvider: "openai"}
	got, provider := r.admitOrReselect(ctx, &types.ChatRequest{}, dec, r.providers["openai"], RoutingStrategySpecific)
	if got != dec || provider == nil {
		t.Fatal("nil breaker altered the decision")
	}
}

func reasoningMentions(reasons []string, want string) bool {
	for _, s := range reasons {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
