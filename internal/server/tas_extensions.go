package server

import "github.com/tributary-ai/llm-router-waf/internal/types"

// tasExtensions are the TAS-native routing/"library" fields that extend the
// vendor request shapes. They are decoded directly into ChatRequest on
// /v1/chat/completions; on the vendor-compatible surfaces (/v1/messages,
// /v1/responses) they ride as extra top-level body fields (settable via the
// SDKs' extra_body) and are carried through the boundary translators so the
// same routing controls apply everywhere.
//
// Embedded anonymously in the vendor wire-request structs so the JSON fields
// are promoted inline.
type tasExtensions struct {
	OptimizeFor      types.OptimizationType `json:"optimize_for,omitempty"`
	RequiredFeatures []string               `json:"required_features,omitempty"`
	MaxCost          *float64               `json:"max_cost,omitempty"`
	RetryConfig      *types.RetryConfig     `json:"retry_config,omitempty"`
	FallbackConfig   *types.FallbackConfig  `json:"fallback_config,omitempty"`
}

// applyTASExtensions copies any set TAS extension onto the translated
// ChatRequest, leaving unset fields untouched.
func applyTASExtensions(req *types.ChatRequest, ext tasExtensions) {
	if ext.OptimizeFor != "" {
		req.OptimizeFor = ext.OptimizeFor
	}
	if len(ext.RequiredFeatures) > 0 {
		req.RequiredFeatures = ext.RequiredFeatures
	}
	if ext.MaxCost != nil {
		req.MaxCost = ext.MaxCost
	}
	if ext.RetryConfig != nil {
		req.RetryConfig = ext.RetryConfig
	}
	if ext.FallbackConfig != nil {
		req.FallbackConfig = ext.FallbackConfig
	}
}
