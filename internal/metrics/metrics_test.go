package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestCountersDoNotAdvanceWithoutTraffic is the regression test for the bug this
// package replaces. The old exporter computed counters as
// time.Now().Unix()/10, so the values climbed on their own and rate() reported
// steady traffic against a service serving none. Anything that reintroduces a
// clock-derived value fails here.
func TestCountersDoNotAdvanceWithoutTraffic(t *testing.T) {
	scrape := func() string {
		srv := httptest.NewServer(promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}))
		defer srv.Close()
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("scrape: %v", err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return string(b)
	}

	first := scrape()
	// Comfortably longer than the old handler's ten-second bucket, so a
	// clock-derived counter would have moved.
	time.Sleep(50 * time.Millisecond)
	second := scrape()

	if first != second {
		t.Errorf("metrics changed with no traffic between scrapes.\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestNoFabricatedSeries guards the deletions. Eight series in the old handler
// were constants with no data source; re-adding one without instrumentation
// behind it should fail rather than quietly resurface on a dashboard.
func TestNoFabricatedSeries(t *testing.T) {
	banned := []string{
		"llm_router_security_score",
		"llm_router_threat_level",
		"llm_router_active_api_keys",
		"llm_router_input_sanitized_total",
		"llm_router_validation_failures_total",
		"llm_router_security_events_total",
		"llm_router_audit_events_total",
		"llm_router_rate_limit_usage",
	}

	families, err := Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		for _, b := range banned {
			if f.GetName() == b {
				t.Errorf("series %q is exposed again; it has no data source and was removed deliberately", b)
			}
		}
	}
}

// TestRequestsTotalHasNoClientIPLabel guards the cardinality fix. The old
// handler carried client_ip, which is bounded only by the number of distinct
// callers.
func TestRequestsTotalHasNoClientIPLabel(t *testing.T) {
	RequestsTotal.WithLabelValues("anthropic", "POST", "200").Inc()

	families, err := Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "llm_router_requests_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "client_ip" {
					t.Error("client_ip is back on llm_router_requests_total: unbounded cardinality")
				}
			}
		}
	}
}

// TestMiddlewareRecordsRealOutcome checks that the labels come from what
// actually happened rather than from a constant: the provider the router
// reported, and the status the handler returned.
func TestMiddlewareRecordsRealOutcome(t *testing.T) {
	RequestsTotal.Reset()

	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(providerHeader, "anthropic")
		w.WriteHeader(http.StatusTooManyRequests)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	got := testutil.ToFloat64(RequestsTotal.WithLabelValues("anthropic", "POST", "429"))
	if got != 1 {
		t.Errorf("requests_total{provider=anthropic,status_code=429} = %v, want 1", got)
	}
}

// TestMiddlewareLabelsUnroutedRequestsAsNone covers the auth-rejection case.
// A request the gateway refuses never reaches a provider, and attributing it to
// one would make an auth outage look like a vendor problem.
func TestMiddlewareLabelsUnroutedRequestsAsNone(t *testing.T) {
	RequestsTotal.Reset()

	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // no provider header set
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	got := testutil.ToFloat64(RequestsTotal.WithLabelValues("none", "POST", "401"))
	if got != 1 {
		t.Errorf("requests_total{provider=none,status_code=401} = %v, want 1", got)
	}
}

// TestMiddlewarePreservesFlusher guards streaming. The wrapper replaces the
// ResponseWriter, so without an explicit Flush passthrough a handler's
// type assertion to http.Flusher fails and server-sent events stop streaming.
func TestMiddlewarePreservesFlusher(t *testing.T) {
	var flushable bool
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, flushable = w.(http.Flusher)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if !flushable {
		t.Error("wrapped ResponseWriter is not an http.Flusher: streaming responses would stop flushing")
	}
}

// TestImplicitOKIsRecorded covers a handler that writes a body without calling
// WriteHeader, which is what a streaming response does for its first chunk.
func TestImplicitOKIsRecorded(t *testing.T) {
	RequestsTotal.Reset()

	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if got := testutil.ToFloat64(RequestsTotal.WithLabelValues("none", "POST", "200")); got != 1 {
		t.Errorf("implicit 200 not recorded, got %v", got)
	}
}

// TestProviderHealthReadsAtScrapeTime checks the gauge follows a changing
// source. A mirrored gauge that some path forgets to update would keep
// asserting a stale value, which is the failure this collector avoids.
func TestProviderHealthReadsAtScrapeTime(t *testing.T) {
	reg := prometheus.NewRegistry()
	healthy := true
	c := &healthCollector{
		desc: prometheus.NewDesc("llm_router_provider_health", "test",
			[]string{"provider"}, nil),
		status: func() map[string]bool { return map[string]bool{"anthropic": healthy} },
	}
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}

	if got := testutil.CollectAndCount(c); got != 1 {
		t.Fatalf("expected 1 series, got %d", got)
	}
	if err := testutil.CollectAndCompare(c, strings.NewReader(`
# HELP llm_router_provider_health test
# TYPE llm_router_provider_health gauge
llm_router_provider_health{provider="anthropic"} 1
`)); err != nil {
		t.Errorf("healthy: %v", err)
	}

	healthy = false
	if err := testutil.CollectAndCompare(c, strings.NewReader(`
# HELP llm_router_provider_health test
# TYPE llm_router_provider_health gauge
llm_router_provider_health{provider="anthropic"} 0
`)); err != nil {
		t.Errorf("after going unhealthy: %v", err)
	}
}

// TestObserveCostSkipsUnpricedCalls checks that a model with no pricing does
// not record a zero. Adding 0 would assert the call was free rather than
// unpriced, and the series would exist as though it had been measured.
func TestObserveCostSkipsUnpricedCalls(t *testing.T) {
	CostTotal.Reset()

	ObserveCost("anthropic", "some-unpriced-model", 0)
	if got := testutil.CollectAndCount(CostTotal); got != 0 {
		t.Errorf("unpriced call created %d series, want 0", got)
	}

	ObserveCost("anthropic", "claude-sonnet-4-6", 0.0123)
	if got := testutil.ToFloat64(CostTotal.WithLabelValues("anthropic", "claude-sonnet-4-6")); got != 0.0123 {
		t.Errorf("cost = %v, want 0.0123", got)
	}
}

// TestObserveTokensSkipsZero mirrors the cost case for usage.
func TestObserveTokensSkipsZero(t *testing.T) {
	TokensTotal.Reset()

	ObserveTokens("openai", 0, 0)
	if got := testutil.CollectAndCount(TokensTotal); got != 0 {
		t.Errorf("zero usage created %d series, want 0", got)
	}

	ObserveTokens("openai", 12, 34)
	if got := testutil.ToFloat64(TokensTotal.WithLabelValues("openai", "input")); got != 12 {
		t.Errorf("input tokens = %v, want 12", got)
	}
	if got := testutil.ToFloat64(TokensTotal.WithLabelValues("openai", "output")); got != 34 {
		t.Errorf("output tokens = %v, want 34", got)
	}
}
