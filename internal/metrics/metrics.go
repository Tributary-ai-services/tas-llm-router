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
	"time"

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

	// --- C4 semantic cache -------------------------------------------------
	//
	// These exist because nothing previously distinguished "the cache is
	// working" from "the cache never hits" from "the cache is serving wrong
	// answers". tas-llm-router#146 shipped a semantic cache with zero recall
	// and it was found a month later by reading a commit message.
	//
	// Deliberately NOT labelled by tenant: a tenant label makes the series
	// count track the customer count, and the calibration these feed is a
	// global property of the embedder, not a per-customer one.

	// SemCacheLookupsTotal counts every cascade decision by outcome.
	//
	// miss_rejected and miss_no_candidate are separated on purpose: the first
	// means L1 found something and L2 threw it out (the threshold is doing
	// work), the second means the store had nothing close (the cache is cold
	// or the traffic is not repetitive). They call for opposite responses and
	// a single "miss" hides which one you have.
	SemCacheLookupsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_router_semcache_lookups_total",
			Help: "Semantic cache cascade decisions by outcome.",
		},
		[]string{"outcome"},
	)

	// SemCacheTopSimilarity is the cosine of the closest candidate considered,
	// split by whether L2 accepted it.
	//
	// This is the one that turns the threshold from an argument into a
	// reading. The operating threshold should be the lowest value with no
	// rejected candidates above it, taken from this histogram on real traffic
	// — not a constant inherited from a model card. #146 inherited langcache's
	// published 0.93 and shipped a cache that never hit; the live config
	// inherited all-minilm's 0.87 and scores "can" against "can't" at 0.96.
	//
	// Buckets are dense between 0.80 and 0.98 because that is where every
	// threshold worth arguing about lives; coarse below, since a 0.3 candidate
	// tells you nothing you did not already know.
	SemCacheTopSimilarity = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llm_router_semcache_top_similarity",
			Help:    "Cosine similarity of the top L1 candidate, by L2 verdict.",
			Buckets: []float64{0.5, 0.6, 0.7, 0.75, 0.8, 0.825, 0.85, 0.875, 0.9, 0.925, 0.95, 0.96, 0.97, 0.98, 0.99, 1.0},
		},
		[]string{"verdict"},
	)

	// SemCacheRejectionsTotal counts L2 rejections by guard.
	//
	// A rising rate here is the earliest available sign that the embedder and
	// the threshold disagree with reality — the guards are catching what the
	// cosine let through. Falling to zero while lookups continue is equally
	// informative: either the traffic changed or a guard stopped running.
	SemCacheRejectionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_router_semcache_rejections_total",
			Help: "Semantic cache L2 rejections by guard reason.",
		},
		[]string{"reason"},
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
		SemCacheLookupsTotal,
		SemCacheTopSimilarity,
		SemCacheRejectionsTotal,
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
	// The semantic-cache outcomes matter most when they are zero: a hit rate
	// of nothing is the #146 failure, and an absent series renders as a blank
	// panel rather than as a zero anybody notices.
	for _, outcome := range []string{"semantic_hit", "shadow_hit", "miss_rejected", "miss_no_candidate"} {
		SemCacheLookupsTotal.WithLabelValues(outcome).Add(0)
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

// --- Model registry (epic #2, Phase 5 observability) ----------------------

var (
	// RegistrySyncDuration is how long one provider's discovery pass took.
	RegistrySyncDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llm_router_registry_sync_duration_seconds",
			Help:    "Duration of a model-registry discovery pass, by provider.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		},
		[]string{"provider"},
	)

	// RegistrySyncTotal counts discovery passes by provider and result
	// (success / error).
	RegistrySyncTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_router_registry_sync_total",
			Help: "Model-registry discovery passes by provider and result.",
		},
		[]string{"provider", "result"},
	)

	// ModelFallbackTotal counts registry fallbacks from an unavailable model to
	// its replacement.
	ModelFallbackTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_router_model_fallback_total",
			Help: "Model fallbacks performed by the registry, by from/to model.",
		},
		[]string{"from", "to"},
	)

	// ModelAliasResolutionTotal counts alias→model resolutions done at routing.
	ModelAliasResolutionTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_router_model_alias_resolution_total",
			Help: "Model alias resolutions, by alias and resolved model.",
		},
		[]string{"alias", "resolved"},
	)

	// ModelValidationTotal counts explicit model validations (the admin
	// validate endpoint / sync probes) by provider, model, and result.
	ModelValidationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_router_model_validation_total",
			Help: "Model availability validations by provider, model, and result.",
		},
		[]string{"provider", "model", "result"},
	)
)

func init() {
	Registry.MustRegister(
		RegistrySyncDuration,
		RegistrySyncTotal,
		ModelFallbackTotal,
		ModelAliasResolutionTotal,
		ModelValidationTotal,
	)
}

// ObserveRegistrySync records one provider's discovery pass: its duration and
// whether it succeeded.
func ObserveRegistrySync(provider string, d time.Duration, ok bool) {
	RegistrySyncDuration.WithLabelValues(provider).Observe(d.Seconds())
	result := "success"
	if !ok {
		result = "error"
	}
	RegistrySyncTotal.WithLabelValues(provider, result).Inc()
}

// ObserveModelFallback records a registry fallback from one model to another.
func ObserveModelFallback(from, to string) {
	ModelFallbackTotal.WithLabelValues(from, to).Inc()
}

// ObserveAliasResolution records an alias→model resolution.
func ObserveAliasResolution(alias, resolved string) {
	ModelAliasResolutionTotal.WithLabelValues(alias, resolved).Inc()
}

// ObserveModelValidation records a model validation outcome ("available",
// "unavailable", or "error").
func ObserveModelValidation(provider, model, result string) {
	ModelValidationTotal.WithLabelValues(provider, model, result).Inc()
}

// modelStatusCollector reports llm_router_model_status{provider,model,status}
// at scrape time from the registry, so it never goes stale the way a mirrored
// gauge would.
type modelStatusCollector struct {
	desc   *prometheus.Desc
	status func() []ModelStatusSample
}

// ModelStatusSample is one model's status for the scrape-time gauge.
type ModelStatusSample struct {
	Provider string
	Model    string
	Status   string
}

func (c *modelStatusCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *modelStatusCollector) Collect(ch chan<- prometheus.Metric) {
	if c.status == nil {
		return
	}
	for _, s := range c.status() {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, 1, s.Provider, s.Model, s.Status)
	}
}

// RegisterModelStatus wires llm_router_model_status to a live source read on
// every scrape. Call once at startup when the registry is enabled; the supplied
// function must be cheap and safe for concurrent use.
func RegisterModelStatus(status func() []ModelStatusSample) error {
	return Registry.Register(&modelStatusCollector{
		desc: prometheus.NewDesc(
			"llm_router_model_status",
			"Registered model status (1 per provider/model/status).",
			[]string{"provider", "model", "status"}, nil,
		),
		status: status,
	})
}
