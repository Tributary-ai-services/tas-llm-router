package routing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"

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
	// The breaker is now constructed unconditionally; the gateway default (or a
	// per-tenant control) decides whether it ACTS. These tests exercise the
	// enabled path, so set the default on — the equivalent of the env flag
	// AIQG_BREAKER_ENABLED=true with no per-tenant override.
	r.SetBreakerDefaultEnabled(true)
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

	if r.isProviderHealthy(context.Background(), "openai") {
		t.Fatal("ejected provider still reported healthy — the breaker verdict never reached selection")
	}
	if !r.isProviderHealthy(context.Background(), "anthropic") {
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
	if r.isProviderHealthy(context.Background(), "openai") {
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
	r.SetBreakerDefaultEnabled(true)

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

	if !r.isProviderHealthy(context.Background(), "anthropic") {
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
	if !r.isProviderHealthy(context.Background(), "openai") {
		t.Fatal("nil breaker excluded a healthy provider")
	}
	dec := &RoutingDecision{SelectedProvider: "openai"}
	got, provider := r.admitOrReselect(ctx, &types.ChatRequest{}, dec, r.providers["openai"], RoutingStrategySpecific)
	if got != dec || provider == nil {
		t.Fatal("nil breaker altered the decision")
	}
}

// Per-tenant control folds over the gateway default. The breaker is always
// constructed now, so what decides whether it acts for a request is
// breakerEnabled(ctx): the tenant control (if any) over the env default.
func TestBreakerPerTenantControlGate(t *testing.T) {
	tru, fls := true, false
	cases := []struct {
		name       string
		defaultOn  bool
		control    *resilience.Controls
		wantActive bool
	}{
		// Zero behaviour change: default off + no tenant control = off, exactly
		// as before per-tenant controls existed.
		{"default-off, no control", false, nil, false},
		{"default-on, no control", true, nil, true},
		// A tenant opts in even though the gateway default is off.
		{"default-off, control on", false, &resilience.Controls{BreakerEnabled: &tru}, true},
		// A tenant opts out even though the gateway default is on.
		{"default-on, control off", true, &resilience.Controls{BreakerEnabled: &fls}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := breakerRouter(t, breaker.Config{ConsecutiveErrors: 2})
			r.SetBreakerDefaultEnabled(tc.defaultOn)
			ctx := WithControls(context.Background(), tc.control)
			if got := r.breakerEnabled(ctx); got != tc.wantActive {
				t.Fatalf("breakerEnabled = %v, want %v", got, tc.wantActive)
			}
		})
	}
}

// Breaker isolation gives a tenant its OWN ejection keyspace: a provider
// tripping under tenant A never affects an isolated tenant B, nor the shared
// fleet view. (#161)
func TestBreakerIsolationSeparatesTenants(t *testing.T) {
	tru := true
	r, _ := breakerRouter(t, breaker.Config{ConsecutiveErrors: 2})

	isoCtl := &resilience.Controls{BreakerEnabled: &tru, BreakerIsolated: &tru}
	ctxA := WithBreakerTenant(WithControls(context.Background(), isoCtl), "tenant-A")
	ctxB := WithBreakerTenant(WithControls(context.Background(), isoCtl), "tenant-B")
	ctxShared := WithControls(context.Background(), &resilience.Controls{BreakerEnabled: &tru})

	// Tenant A trips openai — recorded into A's keyspace only.
	for i := 0; i < 2; i++ {
		r.RecordOutcome(ctxA, "openai", "", breaker.ServerError)
	}

	// Each isolated request reads its own verdict, computed once (as Route does).
	ctxA = WithIsolatedEjected(ctxA, r.computeIsolatedEjected(ctxA))
	ctxB = WithIsolatedEjected(ctxB, r.computeIsolatedEjected(ctxB))

	if !r.isEjected(ctxA, "openai") {
		t.Fatal("tenant A's own failures should eject openai for A")
	}
	if r.isEjected(ctxB, "openai") {
		t.Fatal("A's failures must NOT eject openai for isolated tenant B")
	}
	// The shared fleet view never saw A's isolated failures (they keyed to
	// tenant-A/openai, not openai).
	r.refreshBreakerCache(ctxShared)
	if r.isEjected(ctxShared, "openai") {
		t.Fatal("isolated failures must not leak into the shared fleet view")
	}
}

// The selection-time ejection check is gated on the request's effective
// breaker: a force-off tenant is never filtered by another tenant's failures,
// while a cooldown-on shared tenant still is. (#161)
func TestEjectionOnlyFiltersBreakerOnRequests(t *testing.T) {
	tru, fls := true, false
	r, b := breakerRouter(t, breaker.Config{ConsecutiveErrors: 2})

	// A shared provider genuinely trips.
	for i := 0; i < 2; i++ {
		b.Record(context.Background(), breaker.Target("openai", ""), breaker.ServerError)
	}
	r.refreshBreakerCache(context.Background())

	if r.isEjected(WithControls(context.Background(), &resilience.Controls{BreakerEnabled: &fls}), "openai") {
		t.Fatal("a force-off tenant must not see the shared ejection")
	}
	if !r.isEjected(WithControls(context.Background(), &resilience.Controls{BreakerEnabled: &tru}), "openai") {
		t.Fatal("a cooldown-on shared tenant should see the shared ejection")
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

// ---------------------------------------------------------------------------
// Per-request overrides (the seam between a route rule and the breaker).
// ---------------------------------------------------------------------------

// A rule that sets one threshold must keep the rest. Resetting unset fields to
// zero would silently disable ejection for every rule carrying one setting.
func TestOverrideMergesRatherThanReplaces(t *testing.T) {
	base := breaker.New(breaker.NewMemoryStore(), breaker.Config{ConsecutiveErrors: 5, EjectFor: 30 * time.Second})
	got := base.Override(&resilience.Health{EjectForSeconds: 90}, nil).Config()

	if got.EjectFor != 90*time.Second {
		t.Fatalf("eject_for = %v, want the override", got.EjectFor)
	}
	if got.ConsecutiveErrors != 5 {
		t.Fatalf("consecutive_errors = %d, want the base value preserved", got.ConsecutiveErrors)
	}
}

// Overrides change the thresholds a decision is judged against, never which
// counters it reads: two rules disagreeing about eject_for must still observe
// the same provider, or each would see only its own slice of the traffic and
// neither would detect an outage.
//
// Note which direction this works in. The trip rule is evaluated by whichever
// config RECORDS an outcome, so a stricter rule ejects on its own traffic and
// the ejection is then visible to everyone — rather than a lenient rule's
// records being retroactively re-judged against a stricter threshold.
func TestOverrideSharesTheSameCounters(t *testing.T) {
	base := breaker.New(breaker.NewMemoryStore(), breaker.Config{ConsecutiveErrors: 5})
	strict := base.Override(&resilience.Health{ConsecutiveErrors: 2}, nil)
	ctx := context.Background()

	// The strict rule trips on its own threshold...
	strict.Record(ctx, "openai", breaker.ServerError)
	strict.Record(ctx, "openai", breaker.ServerError)

	// ...and the ejection is fleet-wide, not private to that rule. This is the
	// property that matters: an ejected provider is ejected for everyone.
	if ok, _ := base.Admit(ctx, "openai"); ok {
		t.Fatal("an ejection made through an override was not visible to the base breaker")
	}

	// Counters are shared too, so the window reflects all traffic rather than
	// one rule's slice.
	st, _ := base.Status(ctx, "openai")
	if st.Total == 0 {
		t.Fatal("outcomes recorded through the override are missing from the shared window")
	}
}

func TestNilOverrideReturnsTheSameBreaker(t *testing.T) {
	base := breaker.New(breaker.NewMemoryStore(), breaker.Config{})
	if base.Override(nil, nil) != base {
		t.Fatal("a no-op override should not allocate a new breaker")
	}
}

// The context carrier must survive derivation — resolution and routing are
// several context wraps apart — and must store nothing when nothing is set.
func TestResilienceContextRoundTrip(t *testing.T) {
	ctx := WithResilience(context.Background(), &resilience.Health{EjectForSeconds: 90}, nil)
	ctx = context.WithValue(ctx, struct{ k string }{"unrelated"}, 1)
	h, b := ResilienceFrom(ctx)
	if h == nil || h.EjectForSeconds != 90 {
		t.Fatalf("health did not survive context derivation: %+v", h)
	}
	if b != nil {
		t.Fatal("budgets should be nil when unset")
	}

	h2, b2 := ResilienceFrom(WithResilience(context.Background(), nil, nil))
	if h2 != nil || b2 != nil {
		t.Fatal("an empty override must store nothing")
	}
}
