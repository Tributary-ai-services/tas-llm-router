// Package metrics defines the real Prometheus surface for the router,
// replacing the hand-rolled string-concatenation handler that previously
// served /metrics.
//
// # What was wrong with the old handler
//
// It built the exposition format with fmt.Sprintf and derived nearly every
// value from wall-clock time:
//
//	baseRequests := time.Now().Unix() / 10
//	llm_router_requests_total{...} = 150 + baseRequests*3
//
// That is worse than a stuck exporter. A stuck counter yields rate() == 0 and
// looks obviously broken; a clock-derived counter yields a plausible constant,
// so dashboards showed steady traffic and a "traffic stopped" alert could never
// fire — on a service receiving no traffic at all. llm_router_cost_total
// climbed about $0.05 every ten seconds regardless of whether a single request
// was served, on a gateway whose purpose is cost attribution.
//
// # Why a dedicated registry
//
// Same reasoning as pkg/aiqg/metrics: an explicit registry keeps the surface
// enumerable and lets tests call Registry.Gather() to assert on samples.
// Nothing here registers into prometheus.DefaultRegisterer.
//
// # Series that were deliberately NOT carried over
//
// Eight series in the old handler had no data source anywhere in the codebase —
// they existed only as constants inside the mock handler: security_score,
// threat_level, active_api_keys, input_sanitized_total, validation_failures_total,
// security_events_total, audit_events_total, and rate_limit_usage. They are not
// reimplemented here. Emitting a
// hardcoded security score is worse than emitting nothing, because a dashboard
// renders it as a measurement. Making them real means building the underlying
// instrumentation, which is feature work rather than metrics plumbing.
//
// The client_ip label on requests_total is also gone. With five hardcoded
// addresses it was harmless; against real traffic every distinct caller address
// becomes a new time series, which is the classic cardinality explosion.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Registry is the dedicated registerer for router metrics, served at /metrics
// via promhttp.HandlerFor.
var Registry = prometheus.NewRegistry()

var (
	// RequestsTotal counts completion requests by the provider that actually
	// served them, the HTTP method, and the status returned to the caller.
	// Provider is "none" when routing never selected one (an auth or
	// validation failure), which keeps auth noise from looking like provider
	// traffic.
	RequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_router_requests_total",
			Help: "Completion requests by serving provider, method, and response status.",
		},
		[]string{"provider", "method", "status_code"},
	)

	// RequestDurationSeconds is end-to-end latency as the caller experiences
	// it, including scanning, routing, and any fallback hops.
	//
	// Buckets are stretched well past a typical web SLO: a language-model call
	// routinely takes seconds, and a chain that walks two providers takes
	// longer still. Default buckets top out at 10s and would collapse the
	// interesting tail into +Inf.
	RequestDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llm_router_request_duration_seconds",
			Help:    "End-to-end request latency including scanning, routing, and fallback.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120},
		},
		[]string{"provider", "method"},
	)

	// InFlightRequests is the number of completion requests currently being
	// served. Replaces the old active_connections constant of 5.
	InFlightRequests = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llm_router_active_connections",
			Help: "Completion requests currently in flight.",
		},
	)

	// TokensTotal counts tokens by provider and direction. Type is "input" or
	// "output", matching the label the dashboards already query.
	TokensTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_router_tokens_total",
			Help: "Tokens processed by provider and direction.",
		},
		[]string{"provider", "type"},
	)

	// CostTotal accumulates dollar cost per provider and model, from the same
	// clear.DollarCost call that feeds spend attribution — so the metric and
	// the billing record cannot disagree.
	CostTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_router_cost_total",
			Help: "Cumulative cost in USD by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// ErrorsTotal counts failures by provider and classified error type.
	ErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_router_errors_total",
			Help: "Errors by provider and error type.",
		},
		[]string{"provider", "error_type"},
	)

	// AuthAttemptsTotal counts gateway (Path A) authentication outcomes. Result:
	//   success   — token resolved (or opaque-proceed on the permissive path)
	//   malformed — token failed the tas_qg_live_ shape check before any lookup
	//   unknown   — well-formed token, not recognized by the resolver
	//   suspended — token resolved to a suspended account (403)
	//   missing   — strict ingress, no credential presented (401)
	AuthAttemptsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_router_auth_attempts_total",
			Help: "Gateway authentication attempts by outcome.",
		},
		[]string{"result"},
	)

	// BlockedRequestsTotal counts requests refused by policy enforcement.
	// Direction is "inbound" (the prompt) or "outbound" (the completion), so a
	// dashboard can distinguish leaking secrets from ingesting them.
	BlockedRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_router_blocked_requests_total",
			Help: "Requests blocked by policy enforcement, by direction.",
		},
		[]string{"direction"},
	)

	// RateLimitHitsTotal counts requests rejected by the rate limiter.
	RateLimitHitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_router_rate_limit_hits_total",
			Help: "Requests rejected by the rate limiter.",
		},
		[]string{"tier"},
	)
)

func init() {
	Registry.MustRegister(
		RequestsTotal,
		RequestDurationSeconds,
		InFlightRequests,
		TokensTotal,
		CostTotal,
		ErrorsTotal,
		AuthAttemptsTotal,
		RateLimitHitsTotal,
		BlockedRequestsTotal,
	)
	seed()
}

// seed pre-initializes zero-valued series for counters whose label sets are
// known and bounded. A CounterVec child is exported only once it is touched, so
// without this a freshly restarted pod serves scrapes with these series ABSENT
// — and a blank panel reads as "no data", dangerously close to "zero events"
// (#170). Seeding makes the honest zero observable from pod start. Counters with
// unbounded label values (error_type) are seeded only for the known combinations.
func seed() {
	for _, result := range []string{"success", "malformed", "unknown", "suspended", "missing"} {
		AuthAttemptsTotal.WithLabelValues(result).Add(0)
	}
	RateLimitHitsTotal.WithLabelValues("default").Add(0)
	for _, provider := range []string{"openai", "anthropic"} {
		ErrorsTotal.WithLabelValues(provider, "completion_failed").Add(0)
	}
}

// healthCollector reports provider health by asking the router at scrape time
// rather than maintaining a mirrored gauge.
//
// A mirrored gauge has to be updated from wherever health changes, and any path
// that forgets leaves the metric asserting a stale value indefinitely — the
// same class of failure as the mock handler, arrived at honestly. Reading at
// scrape time cannot drift.
type healthCollector struct {
	desc   *prometheus.Desc
	status func() map[string]bool
}

func (c *healthCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *healthCollector) Collect(ch chan<- prometheus.Metric) {
	if c.status == nil {
		return
	}
	for provider, healthy := range c.status() {
		v := 0.0
		if healthy {
			v = 1.0
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, v, provider)
	}
}

// RegisterProviderHealth wires llm_router_provider_health to a live source.
// Call once during server construction. The supplied function is invoked on
// every scrape, so it must be cheap and safe for concurrent use.
func RegisterProviderHealth(status func() map[string]bool) error {
	return Registry.Register(&healthCollector{
		desc: prometheus.NewDesc(
			"llm_router_provider_health",
			"Provider health status (1=healthy, 0=unhealthy).",
			[]string{"provider"}, nil,
		),
		status: status,
	})
}

// ObserveTokens records usage for one completed call. Zero values are skipped
// so a provider that reports no usage does not create an all-zero series.
func ObserveTokens(provider string, input, output int) {
	if input > 0 {
		TokensTotal.WithLabelValues(provider, "input").Add(float64(input))
	}
	if output > 0 {
		TokensTotal.WithLabelValues(provider, "output").Add(float64(output))
	}
}

// ObserveCost records dollar cost for one completed call. Non-positive costs
// are skipped: DollarCost returns 0 for models it has no pricing for, and
// adding zero would imply the call was free rather than unpriced.
func ObserveCost(provider, model string, usd float64) {
	if usd > 0 {
		CostTotal.WithLabelValues(provider, model).Add(usd)
	}
}
