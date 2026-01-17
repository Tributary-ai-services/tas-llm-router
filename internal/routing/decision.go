package routing

import (
	"time"
)

// RoutingDecision contains information about a routing decision
type RoutingDecision struct {
	// The selected provider name
	SelectedProvider string `json:"selected_provider"`
	
	// Human-readable reasoning for the decision
	Reasoning []string `json:"reasoning"`
	
	// Cost and performance estimates
	EstimatedCost    float64       `json:"estimated_cost"`
	EstimatedLatency time.Duration `json:"estimated_latency"`
	
	// Feature compatibility matrix
	FeatureCompatibility map[string]bool `json:"feature_compatibility"`
	
	// Fallback providers that could handle this request
	FallbackChain []string `json:"fallback_chain"`
	
	// Additional routing context
	RoutingContext RoutingContext `json:"routing_context"`
}

// RoutingContext contains additional context about the routing decision
type RoutingContext struct {
	// Strategy used for routing
	Strategy string `json:"strategy"`
	
	// Request features that influenced routing
	RequestFeatures []string `json:"request_features"`
	
	// Provider health scores at time of routing
	ProviderHealth map[string]string `json:"provider_health"`
	
	// Alternative providers that were considered
	ConsideredProviders []string `json:"considered_providers"`
	
	// Routing decision timestamp
	Timestamp time.Time `json:"timestamp"`
	
	// Cost comparison data
	CostComparison map[string]float64 `json:"cost_comparison,omitempty"`
	
	// Performance comparison data  
	PerformanceComparison map[string]time.Duration `json:"performance_comparison,omitempty"`
}