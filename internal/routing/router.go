package routing

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/providers"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/breaker"
)

// Router handles intelligent request routing to LLM providers
type Router struct {
	providers           map[string]providers.LLMProvider
	providerNames       []string // for round-robin
	roundRobinIndex     int
	healthStatus        map[string]*types.HealthStatus
	healthMu            sync.RWMutex // guards healthStatus (background prober writes while routing reads)
	logger              *logrus.Logger
	lastHealthCheck     time.Time
	healthCheckInterval time.Duration

	// breaker is passive outlier detection over real request outcomes. It is
	// a SECOND, INDEPENDENT input to health, not a replacement for the active
	// probe above: the probe can mark a provider down with no traffic at all,
	// and the breaker can mark it down while the probe still passes (see the
	// breaker package doc — OpenAI's probe is ListModels, which says nothing
	// about whether completions work). A target must satisfy both.
	breaker *breaker.Breaker
	// ejected caches breaker state so candidate filtering — which runs over
	// every provider on every request — does not make a Redis round trip per
	// candidate. Refreshed by the same ticker as the active probe. The
	// authoritative check, including the single-probe half-open gate, is the
	// one Admit call made against the FINAL selection.
	ejected   map[string]breaker.State
	ejectedMu sync.RWMutex
}

// RoutingStrategy defines how to route requests
type RoutingStrategy string

const (
	RoutingStrategyCostOptimized RoutingStrategy = "cost_optimized"
	RoutingStrategyPerformance   RoutingStrategy = "performance"
	RoutingStrategyRoundRobin    RoutingStrategy = "round_robin"
	RoutingStrategySpecific      RoutingStrategy = "specific"
)

// NewRouter creates a new router instance
func NewRouter(logger *logrus.Logger) *Router {
	return &Router{
		providers:           make(map[string]providers.LLMProvider),
		providerNames:       make([]string, 0),
		roundRobinIndex:     0,
		healthStatus:        make(map[string]*types.HealthStatus),
		logger:              logger,
		healthCheckInterval: 30 * time.Second,
		ejected:             make(map[string]breaker.State),
	}
}

// RegisterProvider adds a provider to the router
func (r *Router) RegisterProvider(name string, provider providers.LLMProvider) {
	r.providers[name] = provider
	r.providerNames = append(r.providerNames, name)

	// Initialize health status
	r.healthMu.Lock()
	r.healthStatus[name] = &types.HealthStatus{
		Status:      "unknown",
		LastChecked: 0,
	}
	r.healthMu.Unlock()

	r.logger.WithField("provider", name).Info("Provider registered")
}

// SetHealthCheckInterval overrides the provider health-check cadence. A
// non-positive value is ignored so the default remains in effect.
func (r *Router) SetHealthCheckInterval(d time.Duration) {
	if d > 0 {
		r.healthCheckInterval = d
	}
}

// StartHealthChecks launches a background goroutine that probes every registered
// provider once immediately (so replicas are warm from boot) and then every
// healthCheckInterval thereafter, independent of request traffic.
//
// Without this, provider health was only refreshed lazily inside Route(), so an
// idle replica never probed its providers and reported them as "unknown" ->
// overall "degraded" -> HTTP 503 on /health. With multiple replicas behind a
// Service, that surfaced as ~50% of /health checks returning 503. Call this once
// after all providers are registered.
func (r *Router) StartHealthChecks(ctx context.Context) {
	go func() {
		r.updateHealthStatus(ctx) // warm immediately at startup
		ticker := time.NewTicker(r.healthCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.updateHealthStatus(ctx)
			}
		}
	}()
}

// GetProvider returns a provider by name
func (r *Router) GetProvider(name string) (providers.LLMProvider, bool) {
	provider, exists := r.providers[name]
	return provider, exists
}

// ListProviders returns all registered provider names
func (r *Router) ListProviders() []string {
	names := make([]string, len(r.providerNames))
	copy(names, r.providerNames)
	return names
}

// Route selects the best provider for a request with retry and fallback support
func (r *Router) Route(ctx context.Context, req *types.ChatRequest) (*types.RouterMetadata, providers.LLMProvider, error) {
	start := time.Now()

	// Update health status if needed
	if time.Since(r.lastHealthCheck) > r.healthCheckInterval {
		// Use background context for health checks to avoid cancellation when request completes
		go r.updateHealthStatus(context.Background())
		r.lastHealthCheck = time.Now()
	}

	// Determine routing strategy
	strategy := r.determineStrategy(req)

	// A resolved route rule may PIN a provider (routing-decision.md §5.2). The
	// pin is preferred over the configured strategy, but is not yet absolute:
	// until ordered fallback chains exist (step 3), failing a request because
	// its pinned provider is momentarily unhealthy would turn a provider blip
	// into a tenant outage. So an unusable pin falls through to normal
	// selection and is recorded as not honoured, rather than erroring.
	decision, provider, err := r.routeWithPin(ctx, req, strategy)
	if err != nil {
		return nil, nil, err
	}

	// Authoritative breaker check on the FINAL selection. Candidate filtering
	// already used the cached state; this call is what owns the half-open
	// probe, so it happens exactly once per request against exactly one
	// target.
	decision, provider = r.admitOrReselect(ctx, req, decision, provider, strategy)

	// Initialize metadata tracking
	metadata := &types.RouterMetadata{
		Provider:       decision.SelectedProvider,
		Model:          req.Model,
		RoutingReason:  decision.Reasoning,
		EstimatedCost:  decision.EstimatedCost,
		ProcessingTime: time.Since(start),
		RequestID:      req.ID,
		AttemptCount:   1,
		FallbackUsed:   false,
	}

	// Check if retry is configured
	if req.RetryConfig != nil && req.RetryConfig.MaxAttempts > 1 {
		// Perform routing with retry
		metadata, provider, err = r.routeWithRetry(ctx, req, decision, metadata)
		if err != nil {
			return nil, nil, err
		}
	}

	// Check if fallback is configured and we have failures
	if req.FallbackConfig != nil && req.FallbackConfig.Enabled && len(metadata.FailedProviders) > 0 {
		// Attempt fallback if primary provider failed
		metadata, provider, err = r.routeWithFallback(ctx, req, decision, metadata)
		if err != nil {
			return nil, nil, err
		}
	}

	// Update final processing time
	metadata.ProcessingTime = time.Since(start)

	r.logger.WithFields(logrus.Fields{
		"provider":      metadata.Provider,
		"strategy":      strategy,
		"cost":          metadata.EstimatedCost,
		"attempts":      metadata.AttemptCount,
		"fallback_used": metadata.FallbackUsed,
		"duration_ms":   metadata.ProcessingTime.Milliseconds(),
	}).Info("Request routed")

	return metadata, provider, nil
}

// routeWithRetry attempts to route with retry logic
func (r *Router) routeWithRetry(ctx context.Context, req *types.ChatRequest, decision *RoutingDecision, metadata *types.RouterMetadata) (*types.RouterMetadata, providers.LLMProvider, error) {
	provider := r.providers[decision.SelectedProvider]
	maxAttempts := req.RetryConfig.MaxAttempts
	var lastError error

	// Track retry attempts
	var retryDelays []int64
	totalRetryStart := time.Now()

	// Attempt up to maxAttempts times
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		metadata.AttemptCount = attempt

		// For attempts beyond the first, apply backoff delay
		if attempt > 1 {
			delay := r.calculateBackoffDelay(req.RetryConfig, attempt-1)
			retryDelays = append(retryDelays, delay.Milliseconds())

			r.logger.WithFields(logrus.Fields{
				"provider": decision.SelectedProvider,
				"attempt":  attempt,
				"delay_ms": delay.Milliseconds(),
			}).Debug("Retrying request after backoff delay")

			select {
			case <-time.After(delay):
				// Continue with retry
			case <-ctx.Done():
				return nil, nil, fmt.Errorf("request cancelled during retry backoff: %w", ctx.Err())
			}
		}

		// A retry must fit the fleet-wide budget. A per-request attempt cap
		// cannot prevent a retry storm on its own: when a provider degrades,
		// every in-flight request retries at once and the retries become the
		// load that keeps it down.
		if attempt > 1 && r.breaker != nil && r.breaker.Enabled() {
			if !r.breakerFor(ctx).AllowRetry(ctx, breaker.Target(decision.SelectedProvider, req.Model)) {
				lastError = fmt.Errorf("retry budget exhausted for provider %s", decision.SelectedProvider)
				r.logger.WithField("provider", decision.SelectedProvider).
					Warn("Retry suppressed: budget exhausted")
				break
			}
		}

		// Check provider health before retry
		if !r.isProviderHealthy(decision.SelectedProvider) {
			lastError = fmt.Errorf("provider %s is not healthy", decision.SelectedProvider)
			r.logger.WithField("provider", decision.SelectedProvider).Warn("Provider unhealthy during retry")
			continue
		}

		// Attempt would succeed - return provider for actual request
		metadata.RetryDelays = retryDelays
		metadata.TotalRetryTime = time.Since(totalRetryStart).Milliseconds()

		r.logger.WithFields(logrus.Fields{
			"provider":     decision.SelectedProvider,
			"attempt":      attempt,
			"retry_delays": retryDelays,
		}).Info("Retry attempt ready")

		return metadata, provider, nil
	}

	// All retry attempts exhausted
	metadata.FailedProviders = append(metadata.FailedProviders, decision.SelectedProvider)
	return metadata, nil, fmt.Errorf("all retry attempts failed for provider %s: %w", decision.SelectedProvider, lastError)
}

// routeWithFallback attempts fallback to alternative providers
func (r *Router) routeWithFallback(ctx context.Context, req *types.ChatRequest, originalDecision *RoutingDecision, metadata *types.RouterMetadata) (*types.RouterMetadata, providers.LLMProvider, error) {
	// Build fallback chain based on configuration
	var fallbackChain []string

	if len(req.FallbackConfig.PreferredChain) > 0 {
		// Use client-specified fallback chain
		fallbackChain = req.FallbackConfig.PreferredChain
	} else {
		// Use automatically built fallback chain
		fallbackChain = originalDecision.FallbackChain
	}

	// Filter fallback chain based on configuration
	fallbackChain = r.filterFallbackChain(fallbackChain, req, originalDecision)

	if len(fallbackChain) == 0 {
		return metadata, nil, fmt.Errorf("no suitable fallback providers available")
	}

	r.logger.WithFields(logrus.Fields{
		"original_provider": originalDecision.SelectedProvider,
		"fallback_chain":    fallbackChain,
	}).Info("Attempting fallback routing")

	// Try each fallback provider
	for _, providerName := range fallbackChain {
		// Skip if provider already failed
		if contains(metadata.FailedProviders, providerName) {
			continue
		}

		// Check health
		if !r.isProviderHealthy(providerName) {
			r.logger.WithField("provider", providerName).Debug("Skipping unhealthy fallback provider")
			metadata.FailedProviders = append(metadata.FailedProviders, providerName)
			continue
		}

		provider := r.providers[providerName]

		// Check feature compatibility
		if req.FallbackConfig.RequireSameFeatures && !r.supportsRequiredFeatures(provider, req) {
			r.logger.WithField("provider", providerName).Debug("Fallback provider doesn't support required features")
			continue
		}

		// Check cost constraints
		if req.FallbackConfig.MaxCostIncrease != nil {
			costEst, err := provider.EstimateCost(req)
			if err == nil {
				costIncrease := (costEst.TotalCost - originalDecision.EstimatedCost) / originalDecision.EstimatedCost
				if costIncrease > *req.FallbackConfig.MaxCostIncrease {
					r.logger.WithFields(logrus.Fields{
						"provider":      providerName,
						"cost_increase": costIncrease,
						"max_allowed":   *req.FallbackConfig.MaxCostIncrease,
					}).Debug("Fallback provider exceeds cost threshold")
					continue
				}
			}
		}

		// Fallback provider is suitable
		metadata.Provider = providerName
		metadata.FallbackUsed = true
		metadata.RoutingReason = append(metadata.RoutingReason, fmt.Sprintf("Fallback to %s", providerName))

		r.logger.WithFields(logrus.Fields{
			"original_provider": originalDecision.SelectedProvider,
			"fallback_provider": providerName,
		}).Info("Fallback routing successful")

		return metadata, provider, nil
	}

	return metadata, nil, fmt.Errorf("all fallback providers failed or unavailable")
}

// calculateBackoffDelay calculates retry delay based on backoff strategy
func (r *Router) calculateBackoffDelay(config *types.RetryConfig, attempt int) time.Duration {
	var delay time.Duration

	switch config.BackoffType {
	case "exponential":
		// Exponential backoff: baseDelay * 2^attempt
		multiplier := math.Pow(2, float64(attempt))
		delay = time.Duration(float64(config.BaseDelay) * multiplier)
	case "linear":
		// Linear backoff: baseDelay * attempt
		delay = time.Duration(int64(config.BaseDelay) * int64(attempt))
	default:
		// Default to exponential
		multiplier := math.Pow(2, float64(attempt))
		delay = time.Duration(float64(config.BaseDelay) * multiplier)
	}

	// Cap delay at MaxDelay
	if config.MaxDelay > 0 && delay > config.MaxDelay {
		delay = config.MaxDelay
	}

	return delay
}

// filterFallbackChain filters fallback providers based on configuration
func (r *Router) filterFallbackChain(chain []string, req *types.ChatRequest, originalDecision *RoutingDecision) []string {
	var filtered []string

	for _, providerName := range chain {
		// Skip if provider doesn't exist
		if _, exists := r.providers[providerName]; !exists {
			continue
		}

		// Skip original provider
		if providerName == originalDecision.SelectedProvider {
			continue
		}

		filtered = append(filtered, providerName)
	}

	return filtered
}

// contains checks if a string slice contains a value
func contains(slice []string, value string) bool {
	for _, item := range slice {
		if item == value {
			return true
		}
	}
	return false
}

// determineStrategy decides which routing strategy to use
func (r *Router) determineStrategy(req *types.ChatRequest) RoutingStrategy {
	// Check for specific model request first
	if r.isSpecificProviderRequested(req.Model) {
		return RoutingStrategySpecific
	}

	// Use optimization preference if specified
	switch req.OptimizeFor {
	case types.OptimizeCost:
		return RoutingStrategyCostOptimized
	case types.OptimizePerformance:
		return RoutingStrategyPerformance
	default:
		return RoutingStrategyCostOptimized // Default to cost optimization
	}
}

// isSpecificProviderRequested checks if exactly one registered provider
// advertises the requested model in its capabilities.
func (r *Router) isSpecificProviderRequested(model string) bool {
	matchCount := 0
	for _, provider := range r.providers {
		for _, m := range provider.GetCapabilities().SupportedModels {
			if m.Name == model || m.ProviderModelID == model {
				matchCount++
				break
			}
		}
	}
	return matchCount == 1
}

// getProviderForModel returns the registered provider that advertises
// the requested model in its capabilities.
func (r *Router) getProviderForModel(model string) (string, bool) {
	for name, provider := range r.providers {
		for _, m := range provider.GetCapabilities().SupportedModels {
			if m.Name == model || m.ProviderModelID == model {
				return name, true
			}
		}
	}
	return "", false
}

// routeByStrategy routes the request using the specified strategy
func (r *Router) routeByStrategy(ctx context.Context, req *types.ChatRequest, strategy RoutingStrategy) (*RoutingDecision, providers.LLMProvider, error) {
	switch strategy {
	case RoutingStrategySpecific:
		return r.routeToSpecificProvider(ctx, req)
	case RoutingStrategyCostOptimized:
		return r.routeByCost(ctx, req)
	case RoutingStrategyPerformance:
		return r.routeByPerformance(ctx, req)
	case RoutingStrategyRoundRobin:
		return r.routeRoundRobin(ctx, req)
	default:
		return r.routeByCost(ctx, req)
	}
}

// routeToSpecificProvider routes to a provider based on model name
func (r *Router) routeToSpecificProvider(ctx context.Context, req *types.ChatRequest) (*RoutingDecision, providers.LLMProvider, error) {
	providerName, found := r.getProviderForModel(req.Model)
	if !found {
		return nil, nil, fmt.Errorf("no provider found for model %s", req.Model)
	}

	provider := r.providers[providerName]

	// Check if provider is healthy
	if !r.isProviderHealthy(providerName) {
		return nil, nil, fmt.Errorf("provider %s is not healthy", providerName)
	}

	// Get cost estimate
	costEst, err := provider.EstimateCost(req)
	if err != nil {
		r.logger.WithError(err).Warnf("Failed to estimate cost for %s", providerName)
		costEst = &types.CostEstimate{TotalCost: 0}
	}

	decision := &RoutingDecision{
		SelectedProvider:     providerName,
		Reasoning:            []string{fmt.Sprintf("Specific model requested: %s", req.Model)},
		EstimatedCost:        costEst.TotalCost,
		EstimatedLatency:     r.estimateLatency(providerName),
		FeatureCompatibility: r.checkFeatureCompatibility(provider, req),
		FallbackChain:        r.buildFallbackChain(providerName, req),
		RoutingContext:       r.buildRoutingContext("specific", req, []string{providerName}),
	}

	return decision, provider, nil
}

// routeByCost routes to the most cost-effective provider
func (r *Router) routeByCost(ctx context.Context, req *types.ChatRequest) (*RoutingDecision, providers.LLMProvider, error) {
	candidates := r.getHealthyProviders()
	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("no healthy providers available")
	}

	// Filter providers by feature requirements
	candidates = r.filterByFeatures(candidates, req)
	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("no providers support required features")
	}

	// Get cost estimates for all candidates
	type candidateWithCost struct {
		name     string
		provider providers.LLMProvider
		cost     float64
		estimate *types.CostEstimate
	}

	var costsAndProviders []candidateWithCost

	for _, name := range candidates {
		provider := r.providers[name]
		costEst, err := provider.EstimateCost(req)
		if err != nil {
			r.logger.WithError(err).Warnf("Failed to estimate cost for %s", name)
			continue
		}

		costsAndProviders = append(costsAndProviders, candidateWithCost{
			name:     name,
			provider: provider,
			cost:     costEst.TotalCost,
			estimate: costEst,
		})
	}

	if len(costsAndProviders) == 0 {
		return nil, nil, fmt.Errorf("could not estimate costs for any provider")
	}

	// Sort by cost (ascending)
	sort.Slice(costsAndProviders, func(i, j int) bool {
		return costsAndProviders[i].cost < costsAndProviders[j].cost
	})

	// Select the cheapest
	selected := costsAndProviders[0]

	// Build reasoning
	reasoning := []string{
		fmt.Sprintf("Cost-optimized routing selected %s", selected.name),
		fmt.Sprintf("Estimated cost: $%.6f", selected.cost),
	}

	if len(costsAndProviders) > 1 {
		next := costsAndProviders[1]
		savings := next.cost - selected.cost
		reasoning = append(reasoning, fmt.Sprintf("Saves $%.6f vs %s", savings, next.name))
	}

	// Build cost comparison data
	costComparison := make(map[string]float64)
	for _, candidate := range costsAndProviders {
		costComparison[candidate.name] = candidate.cost
	}

	decision := &RoutingDecision{
		SelectedProvider:     selected.name,
		Reasoning:            reasoning,
		EstimatedCost:        selected.cost,
		EstimatedLatency:     r.estimateLatency(selected.name),
		FeatureCompatibility: r.checkFeatureCompatibility(selected.provider, req),
		FallbackChain:        r.buildFallbackChain(selected.name, req),
		RoutingContext:       r.buildRoutingContextWithCosts("cost_optimized", req, candidates, costComparison),
	}

	return decision, selected.provider, nil
}

// routeByPerformance routes to the fastest provider
func (r *Router) routeByPerformance(ctx context.Context, req *types.ChatRequest) (*RoutingDecision, providers.LLMProvider, error) {
	candidates := r.getHealthyProviders()
	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("no healthy providers available")
	}

	// Filter providers by feature requirements
	candidates = r.filterByFeatures(candidates, req)
	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("no providers support required features")
	}

	// Select the candidate with the lowest estimated latency
	selected := candidates[0]
	bestLatency := r.estimateLatency(selected)
	for _, name := range candidates[1:] {
		if lat := r.estimateLatency(name); lat < bestLatency {
			bestLatency = lat
			selected = name
		}
	}

	provider := r.providers[selected]

	// Get cost estimate
	costEst, err := provider.EstimateCost(req)
	if err != nil {
		r.logger.WithError(err).Warnf("Failed to estimate cost for %s", selected)
		costEst = &types.CostEstimate{TotalCost: 0}
	}

	// Build performance comparison data
	performanceComparison := make(map[string]time.Duration)
	for _, name := range candidates {
		performanceComparison[name] = r.estimateLatency(name)
	}

	decision := &RoutingDecision{
		SelectedProvider:     selected,
		Reasoning:            []string{fmt.Sprintf("Performance-optimized routing selected %s", selected)},
		EstimatedCost:        costEst.TotalCost,
		EstimatedLatency:     r.estimateLatency(selected),
		FeatureCompatibility: r.checkFeatureCompatibility(provider, req),
		FallbackChain:        r.buildFallbackChain(selected, req),
		RoutingContext:       r.buildRoutingContextWithPerformance("performance", req, candidates, performanceComparison),
	}

	return decision, provider, nil
}

// routeRoundRobin routes using round-robin strategy
func (r *Router) routeRoundRobin(ctx context.Context, req *types.ChatRequest) (*RoutingDecision, providers.LLMProvider, error) {
	candidates := r.getHealthyProviders()
	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("no healthy providers available")
	}

	// Filter providers by feature requirements
	candidates = r.filterByFeatures(candidates, req)
	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("no providers support required features")
	}

	// Select next provider in round-robin fashion
	selected := candidates[r.roundRobinIndex%len(candidates)]
	r.roundRobinIndex++

	provider := r.providers[selected]

	// Get cost estimate
	costEst, err := provider.EstimateCost(req)
	if err != nil {
		r.logger.WithError(err).Warnf("Failed to estimate cost for %s", selected)
		costEst = &types.CostEstimate{TotalCost: 0}
	}

	decision := &RoutingDecision{
		SelectedProvider:     selected,
		Reasoning:            []string{fmt.Sprintf("Round-robin routing selected %s", selected)},
		EstimatedCost:        costEst.TotalCost,
		EstimatedLatency:     r.estimateLatency(selected),
		FeatureCompatibility: r.checkFeatureCompatibility(provider, req),
		FallbackChain:        r.buildFallbackChain(selected, req),
		RoutingContext:       r.buildRoutingContext("round_robin", req, candidates),
	}

	return decision, provider, nil
}

// getHealthyProviders returns a list of healthy provider names
func (r *Router) getHealthyProviders() []string {
	var healthy []string
	for name := range r.providers {
		if r.isProviderHealthy(name) {
			healthy = append(healthy, name)
		}
	}
	return healthy
}

// isProviderHealthy reports whether a provider may be considered as a
// candidate. Both inputs must agree: the active probe (is the vendor
// reachable) and the passive breaker (are real requests to it succeeding).
//
// A provider ejected by the breaker is excluded here so it is never SELECTED,
// which is different from being admitted — see admit(), which owns the
// half-open probe and is called once against the final choice.
func (r *Router) isProviderHealthy(name string) bool {
	if r.isEjected(name) {
		return false
	}
	r.healthMu.RLock()
	status, exists := r.healthStatus[name]
	r.healthMu.RUnlock()
	if !exists {
		return false
	}

	// Consider provider healthy if status is "healthy" or "unknown" (untested)
	return status.Status == "healthy" || status.Status == "unknown"
}

// isEjected reports the cached breaker verdict for a provider-level target.
// Reads the cache rather than the store: this runs per candidate per request.
func (r *Router) isEjected(name string) bool {
	r.ejectedMu.RLock()
	defer r.ejectedMu.RUnlock()
	return r.ejected[breaker.Target(name, "")] == breaker.Open
}

// SetBreaker attaches passive outlier detection. Safe to call with nil, which
// leaves the router on active health checks alone.
func (r *Router) SetBreaker(b *breaker.Breaker) { r.breaker = b }

// Breaker returns the attached breaker, or nil.
func (r *Router) Breaker() *breaker.Breaker { return r.breaker }

// refreshBreakerCache repopulates the ejection cache used by candidate
// filtering. Called on the health-check tick so the two health inputs refresh
// together.
func (r *Router) refreshBreakerCache(ctx context.Context) {
	if r.breaker == nil || !r.breaker.Enabled() {
		return
	}
	next := make(map[string]breaker.State, len(r.providerNames))
	for _, name := range r.providerNames {
		st, err := r.breaker.Status(ctx, breaker.Target(name, ""))
		if err != nil {
			continue
		}
		next[breaker.Target(name, "")] = st.State
	}
	r.ejectedMu.Lock()
	r.ejected = next
	r.ejectedMu.Unlock()
}

// RecordOutcome accounts a completed attempt against the breaker. Called by
// the server after the provider call returns, because only the caller knows
// whether the request actually succeeded — the router hands back a provider
// and never sees the result.
//
// Records at BOTH granularities: a vendor-wide outage and a single degraded
// model are different failures needing different responses, and recording only
// one of them makes the other undetectable.
func (r *Router) RecordOutcome(ctx context.Context, provider, model string, outcome breaker.Outcome) {
	if r.breaker == nil || !r.breaker.Enabled() {
		return
	}
	br := r.breakerFor(ctx)
	br.Record(ctx, breaker.Target(provider, ""), outcome)
	if model != "" {
		br.Record(ctx, breaker.Target(provider, model), outcome)
	}
}

// breakerFor returns the breaker to use for this request, applying any
// resilience overrides a matched route rule resolved. Falls back to the
// gateway-wide breaker when no rule set any.
func (r *Router) breakerFor(ctx context.Context) *breaker.Breaker {
	h, b := ResilienceFrom(ctx)
	return r.breaker.Override(h, b)
}

// filterByFeatures filters providers based on required features
func (r *Router) filterByFeatures(candidates []string, req *types.ChatRequest) []string {
	if len(req.RequiredFeatures) == 0 && len(req.Tools) == 0 && len(req.Functions) == 0 {
		return candidates // No special features required
	}

	var compatible []string

	for _, name := range candidates {
		provider := r.providers[name]
		if r.supportsRequiredFeatures(provider, req) {
			compatible = append(compatible, name)
		}
	}

	return compatible
}

// supportsRequiredFeatures checks if a provider supports the required features
func (r *Router) supportsRequiredFeatures(provider providers.LLMProvider, req *types.ChatRequest) bool {
	capabilities := provider.GetCapabilities()

	// Check explicit required features
	for _, feature := range req.RequiredFeatures {
		switch feature {
		case "functions", "function_calling":
			if !capabilities.SupportsFunctions {
				return false
			}
		case "vision":
			if !capabilities.SupportsVision {
				return false
			}
		case "structured_output":
			if !capabilities.SupportsStructuredOutput {
				return false
			}
		case "streaming":
			if !capabilities.SupportsStreaming {
				return false
			}
		case "assistants":
			if !capabilities.SupportsAssistants {
				return false
			}
		case "batch":
			if !capabilities.SupportsBatch {
				return false
			}
		}
	}

	// Check if tools/functions are requested
	if len(req.Tools) > 0 || len(req.Functions) > 0 {
		if !capabilities.SupportsFunctions {
			return false
		}
	}

	// Check multimodal content
	for _, msg := range req.Messages {
		if parts, ok := msg.Content.([]types.ContentPart); ok {
			for _, part := range parts {
				if part.Type == "image_url" {
					if !capabilities.SupportsVision {
						return false
					}
				}
			}
		}
	}

	return true
}

// checkFeatureCompatibility returns feature compatibility status
func (r *Router) checkFeatureCompatibility(provider providers.LLMProvider, req *types.ChatRequest) map[string]bool {
	capabilities := provider.GetCapabilities()

	compatibility := make(map[string]bool)
	compatibility["functions"] = capabilities.SupportsFunctions
	compatibility["vision"] = capabilities.SupportsVision
	compatibility["structured_output"] = capabilities.SupportsStructuredOutput
	compatibility["streaming"] = capabilities.SupportsStreaming
	compatibility["assistants"] = capabilities.SupportsAssistants
	compatibility["batch"] = capabilities.SupportsBatch

	return compatibility
}

// buildFallbackChain creates a fallback chain for the request
func (r *Router) buildFallbackChain(primary string, req *types.ChatRequest) []string {
	candidates := r.getHealthyProviders()
	var fallbacks []string

	for _, name := range candidates {
		if name != primary {
			provider := r.providers[name]
			if r.supportsRequiredFeatures(provider, req) {
				fallbacks = append(fallbacks, name)
			}
		}
	}

	return fallbacks
}

// estimateLatency returns a latency estimate from health-check measurements
// when available, falling back to a default.
func (r *Router) estimateLatency(providerName string) time.Duration {
	r.healthMu.RLock()
	status, exists := r.healthStatus[providerName]
	r.healthMu.RUnlock()
	if exists && status.ResponseTime > 0 {
		return time.Duration(status.ResponseTime) * time.Millisecond
	}
	return 1000 * time.Millisecond
}

// updateHealthStatus performs health checks on all providers
func (r *Router) updateHealthStatus(ctx context.Context) {
	for name, provider := range r.providers {
		start := time.Now()
		err := provider.HealthCheck(ctx)
		duration := time.Since(start)

		status := &types.HealthStatus{
			LastChecked:  time.Now().Unix(),
			ResponseTime: duration.Milliseconds(),
		}

		if err != nil {
			status.Status = "unhealthy"
			status.ErrorMessage = err.Error()
			r.logger.WithError(err).Warnf("Health check failed for %s", name)
		} else {
			status.Status = "healthy"
			r.logger.WithField("provider", name).Debug("Health check passed")
		}

		// Lock only for the map write — never while HealthCheck() is in flight.
		r.healthMu.Lock()
		r.healthStatus[name] = status
		r.healthMu.Unlock()
	}
	r.refreshBreakerCache(ctx)
}

// GetHealthStatus returns the health status of all providers
func (r *Router) GetHealthStatus() map[string]*types.HealthStatus {
	r.healthMu.RLock()
	defer r.healthMu.RUnlock()
	status := make(map[string]*types.HealthStatus)
	for name, health := range r.healthStatus {
		// Create a copy to avoid external modification
		status[name] = &types.HealthStatus{
			Status:       health.Status,
			ResponseTime: health.ResponseTime,
			LastChecked:  health.LastChecked,
			ErrorMessage: health.ErrorMessage,
		}
	}
	return status
}

// GetCapabilities returns capabilities of all providers
func (r *Router) GetCapabilities() map[string]types.ProviderCapabilities {
	capabilities := make(map[string]types.ProviderCapabilities)
	for name, provider := range r.providers {
		capabilities[name] = provider.GetCapabilities()
	}
	return capabilities
}

// buildRoutingContext creates a basic routing context
func (r *Router) buildRoutingContext(strategy string, req *types.ChatRequest, candidates []string) RoutingContext {
	return RoutingContext{
		Strategy:            strategy,
		RequestFeatures:     r.extractRequestFeatures(req),
		ProviderHealth:      r.getProviderHealthStatuses(),
		ConsideredProviders: candidates,
		Timestamp:           time.Now(),
	}
}

// buildRoutingContextWithCosts creates routing context with cost comparison data
func (r *Router) buildRoutingContextWithCosts(strategy string, req *types.ChatRequest, candidates []string, costs map[string]float64) RoutingContext {
	context := r.buildRoutingContext(strategy, req, candidates)
	context.CostComparison = costs
	return context
}

// buildRoutingContextWithPerformance creates routing context with performance comparison data
func (r *Router) buildRoutingContextWithPerformance(strategy string, req *types.ChatRequest, candidates []string, performance map[string]time.Duration) RoutingContext {
	context := r.buildRoutingContext(strategy, req, candidates)
	context.PerformanceComparison = performance
	return context
}

// extractRequestFeatures extracts features from the request that influence routing
func (r *Router) extractRequestFeatures(req *types.ChatRequest) []string {
	var features []string

	// Add explicit required features
	features = append(features, req.RequiredFeatures...)

	// Detect implicit features
	if len(req.Tools) > 0 || len(req.Functions) > 0 {
		features = append(features, "function_calling")
	}

	if req.Stream {
		features = append(features, "streaming")
	}

	if req.ResponseFormat != nil {
		features = append(features, "structured_output")
	}

	// Check for vision requirements in messages
	for _, msg := range req.Messages {
		if parts, ok := msg.Content.([]types.ContentPart); ok {
			for _, part := range parts {
				if part.Type == "image_url" {
					features = append(features, "vision")
					break
				}
			}
		}
	}

	return features
}

// getProviderHealthStatuses returns current health status of all providers
func (r *Router) getProviderHealthStatuses() map[string]string {
	r.healthMu.RLock()
	defer r.healthMu.RUnlock()
	healthStatuses := make(map[string]string)
	for name, status := range r.healthStatus {
		healthStatuses[name] = status.Status
	}
	return healthStatuses
}

// routeWithPin honours a context-pinned provider when one is set, usable and
// healthy; otherwise it falls back to the configured strategy.
//
// The fall-through is deliberate and interim — see the call site. When a pin is
// not honoured the reason is written into the decision's Reasoning so the event
// explains itself rather than silently routing elsewhere.
func (r *Router) routeWithPin(ctx context.Context, req *types.ChatRequest, strategy RoutingStrategy) (*RoutingDecision, providers.LLMProvider, error) {
	pin := PinnedProviderFrom(ctx)
	if pin == "" {
		return r.routeByStrategy(ctx, req, strategy)
	}

	prov, ok := r.providers[pin]
	if !ok {
		return r.routeByStrategyNoting(ctx, req, strategy, "pinned provider "+pin+" is not configured; used "+string(strategy))
	}
	if !r.isProviderHealthy(pin) {
		return r.routeByStrategyNoting(ctx, req, strategy, "pinned provider "+pin+" unhealthy; used "+string(strategy))
	}

	costEst, err := prov.EstimateCost(req)
	dec := &RoutingDecision{
		SelectedProvider: pin,
		Reasoning:        []string{"pinned by route rule: " + pin},
	}
	if err == nil && costEst != nil {
		dec.EstimatedCost = costEst.TotalCost
	}
	return dec, prov, nil
}

// admitOrReselect asks the breaker whether the chosen target may receive this
// request, and picks another provider when it may not.
//
// When the breaker denies and NO alternative exists, the request proceeds
// anyway and says so in the reasoning. That is deliberate: ejection exists to
// shift traffic away from a bad target, and with nowhere to shift it, refusing
// every request is strictly worse than trying a provider that may have
// recovered. Ejection protects a system that has somewhere else to go; it is
// not a kill switch. Once ordered fallback chains land (step 3) the "nowhere
// to go" case becomes rare and this can harden.
func (r *Router) admitOrReselect(ctx context.Context, req *types.ChatRequest, decision *RoutingDecision, provider providers.LLMProvider, strategy RoutingStrategy) (*RoutingDecision, providers.LLMProvider) {
	if r.breaker == nil || !r.breaker.Enabled() || decision == nil {
		return decision, provider
	}
	// A matched route rule may tighten or loosen the thresholds for THIS
	// request. The counters stay shared — see Breaker.Override.
	br := r.breakerFor(ctx)
	target := breaker.Target(decision.SelectedProvider, req.Model)
	ok, state := br.Admit(ctx, target)
	if ok {
		if state == breaker.HalfOpen {
			decision.Reasoning = append(decision.Reasoning,
				"breaker half-open: this request is the single recovery probe for "+target)
		}
		return decision, provider
	}

	// Denied. Look for any other healthy provider.
	for _, name := range r.providerNames {
		if name == decision.SelectedProvider || !r.isProviderHealthy(name) {
			continue
		}
		alt, exists := r.providers[name]
		if !exists {
			continue
		}
		if admitted, _ := br.Admit(ctx, breaker.Target(name, req.Model)); !admitted {
			continue
		}
		r.logger.WithFields(logrus.Fields{
			"ejected": decision.SelectedProvider,
			"chosen":  name,
		}).Warn("Breaker ejected the selected provider; routed to an alternative")
		return &RoutingDecision{
			SelectedProvider: name,
			EstimatedCost:    decision.EstimatedCost,
			Reasoning: append(decision.Reasoning,
				"breaker ejected "+decision.SelectedProvider+"; routed to "+name),
		}, alt
	}

	r.logger.WithField("provider", decision.SelectedProvider).
		Warn("Breaker ejected the selected provider but no alternative is available; proceeding")
	decision.Reasoning = append(decision.Reasoning,
		"breaker ejected "+decision.SelectedProvider+" but no alternative was available; proceeded anyway")
	return decision, provider
}

// routeByStrategyNoting routes normally but records why the pin was ignored.
func (r *Router) routeByStrategyNoting(ctx context.Context, req *types.ChatRequest, strategy RoutingStrategy, note string) (*RoutingDecision, providers.LLMProvider, error) {
	dec, prov, err := r.routeByStrategy(ctx, req, strategy)
	if err == nil && dec != nil {
		dec.Reasoning = append(dec.Reasoning, note)
	}
	return dec, prov, err
}
