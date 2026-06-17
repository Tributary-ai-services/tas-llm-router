package policy

import (
	"encoding/json"
	"strings"
)

// ReductionPolicy is the gateway-side typed view of a bundle's
// `reduction` config (the `PolicyBundle.Spec.reduction` block — see
// aether-shared/data-models/aiqg/extraction-policy.md). It governs the
// Gatekeeper payload-reduction pipeline.
//
// Plan #7 Phase 2: this slice parses + exposes the policy. A later slice
// reads it to shadow/apply extraction. Mode semantics:
//   - off / "" / projected : do not run the real extractor (projected
//     savings are still computed by CLEAR v0.2 regardless).
//   - shadow               : run on SampleRate of eligible traffic to
//     MEASURE reduction, but do NOT apply it to the vendor call.
//   - active               : apply the reduction (real token drop).
type ReductionPolicy struct {
	Mode       string         `json:"mode"`
	MinTokens  int            `json:"min_tokens,omitempty"`
	SampleRate float64        `json:"sample_rate,omitempty"`
	Steps      ReductionSteps `json:"steps"`
}

type ReductionSteps struct {
	Chunking  *ChunkingStep  `json:"chunking,omitempty"`
	Relevance *RelevanceStep `json:"relevance,omitempty"`
	SLM       *SLMStep       `json:"slm,omitempty"`
}

type ChunkingStep struct {
	Enabled bool `json:"enabled"`
	Size    int  `json:"size,omitempty"`
	Overlap int  `json:"overlap,omitempty"`
}

type RelevanceStep struct {
	Enabled    bool    `json:"enabled"`
	Threshold  float64 `json:"threshold,omitempty"`
	TopK       int     `json:"top_k,omitempty"`
	TopKRatio  float64 `json:"top_k_ratio,omitempty"`
	EmbedModel string  `json:"embed_model,omitempty"`
}

type SLMStep struct {
	Enabled   bool   `json:"enabled"`
	Model     string `json:"model,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
}

// Reduction mode constants.
const (
	ReductionOff       = "off"
	ReductionProjected = "projected"
	ReductionShadow    = "shadow"
	ReductionActive    = "active"
)

// ParseReduction decodes the raw `reduction` block. Returns nil (not an
// error) for empty input — the common "no extraction policy" case. A
// malformed block returns an error so the caller can log it and fall
// back to projected-only rather than silently mis-extracting.
func ParseReduction(raw json.RawMessage) (*ReductionPolicy, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rp ReductionPolicy
	if err := json.Unmarshal(raw, &rp); err != nil {
		return nil, err
	}
	rp.Mode = strings.ToLower(strings.TrimSpace(rp.Mode))
	if rp.Mode == "" {
		rp.Mode = ReductionProjected
	}
	return &rp, nil
}

// RunsExtractor reports whether the real extractor should run at all
// (shadow measures, active applies). off/projected never run it.
func (r *ReductionPolicy) RunsExtractor() bool {
	if r == nil {
		return false
	}
	return r.Mode == ReductionShadow || r.Mode == ReductionActive
}

// Applies reports whether the reduction is applied to the vendor call
// (only active). Shadow runs the extractor but keeps the original payload.
func (r *ReductionPolicy) Applies() bool {
	return r != nil && r.Mode == ReductionActive
}

// EligibleBySize reports whether a prompt of promptTokens clears the
// MinTokens floor (small prompts aren't worth extracting). True when no
// floor is set.
func (r *ReductionPolicy) EligibleBySize(promptTokens int) bool {
	if r == nil {
		return false
	}
	if r.MinTokens <= 0 {
		return true
	}
	return promptTokens >= r.MinTokens
}
