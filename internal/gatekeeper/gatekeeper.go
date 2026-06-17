package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/Tributary-ai-services/Gatekeeper/pkg/extract"
	"github.com/Tributary-ai-services/Gatekeeper/pkg/pipeline"
	"github.com/Tributary-ai-services/Gatekeeper/pkg/scan"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// ScanMeta carries per-request context for scanning.
type ScanMeta struct {
	TenantID  string
	RequestID string
	UserID    string
	Source    string // "llm_input" or "llm_output"
}

// Config holds Gatekeeper integration configuration.
type Config struct {
	Enabled           bool          `yaml:"enabled"`
	FailOpen          bool          `yaml:"fail_open"`
	HonorAttestations bool          `yaml:"honor_attestations"`
	ScanTimeout       time.Duration `yaml:"scan_timeout"`

	Inbound  ScanDirectionConfig `yaml:"inbound"`
	Outbound ScanDirectionConfig `yaml:"outbound"`

	// Extraction configures the shadow payload-reduction measurement
	// (Plan #7 Phase 2). When disabled (default), MeasureReduction is a
	// no-op and the gateway never calls the extractor — preserving the
	// pre-Phase-2 behavior. The actual run is further gated per-request
	// by the resolved bundle's reduction policy.
	Extraction ExtractionConfig `yaml:"extraction"`
}

// ExtractionConfig wires the Ollama-backed extractor used for shadow
// payload-reduction measurement.
type ExtractionConfig struct {
	Enabled        bool   `yaml:"enabled"`
	OllamaURL      string `yaml:"ollama_url"`       // e.g. http://ollama.tas-shared:11434
	EmbedModel     string `yaml:"embed_model"`      // e.g. all-minilm
	MinContentSize int    `yaml:"min_content_size"` // extractor floor (bytes)
}

// ScanPolicy controls which messages are scanned based on role and trust metadata.
type ScanPolicy struct {
	AlwaysScanRoles []string `yaml:"always_scan_roles"` // Roles always scanned (default: ["user"])
	NeverScanRoles  []string `yaml:"never_scan_roles"`  // Roles never scanned (default: ["assistant"])
	TrustMetaKey    string   `yaml:"trust_meta_key"`    // Metadata key to check (default: "trust")
	PreScannedValue string   `yaml:"pre_scanned_value"` // Value that marks content as pre-scanned (default: "pre_scanned")
}

// DefaultScanPolicy returns sensible defaults for selective scanning.
func DefaultScanPolicy() ScanPolicy {
	return ScanPolicy{
		AlwaysScanRoles: []string{"user"},
		NeverScanRoles:  []string{"assistant"},
		TrustMetaKey:    "trust",
		PreScannedValue: "pre_scanned",
	}
}

// ScanDirectionConfig configures scanning for a direction (inbound/outbound).
type ScanDirectionConfig struct {
	Enabled         bool       `yaml:"enabled"`
	ScanProfile     string     `yaml:"scan_profile"`
	TrustTier       string     `yaml:"trust_tier"`
	BlockOnCritical bool       `yaml:"block_on_critical"`
	ScanPolicy      ScanPolicy `yaml:"scan_policy"`
}

// DefaultConfig returns the default Gatekeeper configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:           true,
		FailOpen:          true,
		HonorAttestations: true,
		ScanTimeout:       30 * time.Second,
		Inbound: ScanDirectionConfig{
			Enabled:         true,
			ScanProfile:     "full",
			TrustTier:       "external",
			BlockOnCritical: true,
			ScanPolicy:      DefaultScanPolicy(),
		},
		Outbound: ScanDirectionConfig{
			Enabled:         true,
			ScanProfile:     "full",
			TrustTier:       "partner",
			BlockOnCritical: true,
			ScanPolicy:      DefaultScanPolicy(),
		},
	}
}

// Client wraps the Gatekeeper pipeline processor for LLM Router integration.
type Client struct {
	processor pipeline.Processor
	extractor extract.Extractor // nil unless Extraction.Enabled — shadow measurement only
	config    Config
	logger    *logrus.Logger
}

// New creates a new Gatekeeper client.
func New(cfg Config, logger *logrus.Logger) (*Client, error) {
	if logger == nil {
		logger = logrus.New()
	}

	// Create scanner with default registry
	scanner := scan.NewScanner()

	// Create processor with minimal options for LLM Router use
	procConfig := pipeline.DefaultProcessorConfig()
	procConfig.ServiceID = "tas-llm-router"
	procConfig.HonorAttestations = cfg.HonorAttestations
	procConfig.EnableExtraction = false  // LLM messages are already relevant
	procConfig.EnableStreaming = false   // LLM Router manages its own audit
	procConfig.EnableActions = false     // LLM Router handles blocking
	procConfig.EnableAttestation = false // Simplified for now
	if cfg.ScanTimeout > 0 {
		procConfig.ScanTimeout = cfg.ScanTimeout
	}

	processor := pipeline.NewProcessor(scanner, pipeline.WithConfig(procConfig))

	// Optional extractor for shadow payload-reduction measurement. Built
	// only when configured; embeddings-only (relevance step) — the SLM
	// step needs a GPU. The embedder talks to Ollama via SLM.URL (see
	// extract.NewExtractor). Failure to init is non-fatal: shadow
	// measurement just stays off.
	var extractor extract.Extractor
	if cfg.Extraction.Enabled {
		ec := extract.DefaultExtractorConfig()
		ec.EnableEmbedding = true
		ec.EnableSLM = false
		if cfg.Extraction.EmbedModel != "" {
			ec.Embedding.Model = cfg.Extraction.EmbedModel
		}
		ec.SLM.URL = cfg.Extraction.OllamaURL // extract.NewEmbedder reads the Ollama URL from SLM.URL
		if cfg.Extraction.MinContentSize > 0 {
			ec.MinContentSize = cfg.Extraction.MinContentSize
		}
		if ex, err := extract.NewExtractor(ec); err != nil {
			logger.WithError(err).Warn("gatekeeper: extractor init failed; shadow reduction disabled")
		} else {
			extractor = ex
		}
	}

	return &Client{
		processor: processor,
		extractor: extractor,
		config:    cfg,
		logger:    logger,
	}, nil
}

// ReductionMeasurement is the byte-level result of a real (shadow)
// extractor run, for the gateway to convert to measured token/USD savings.
type ReductionMeasurement struct {
	OriginalBytes           int
	ExtractedBytes          int
	SizeAfterRelevanceBytes int
	ChunksProcessed         int
	ChunksRetained          int
}

// RelevanceOverride carries per-bundle relevance tuning (from an AIQG
// policy bundle's reduction.steps.relevance) into a measurement run. Zero
// fields fall back to the extractor's configured defaults.
type RelevanceOverride struct {
	Threshold float64
	TopK      int
	Ratio     float64
}

// ReduceText runs the extractor on a single content blob and returns the
// REDUCED content alongside the measurement. This is the apply primitive
// (Plan #7 Phase 3): callers that want to actually shrink the payload use
// the returned content; measure-only callers (MeasureReduction) discard it.
// query is the relevance anchor; rel applies per-policy relevance tuning.
func (c *Client) ReduceText(ctx context.Context, content []byte, query string, rel RelevanceOverride) (reduced []byte, m *ReductionMeasurement, err error) {
	if c.extractor == nil {
		return nil, nil, fmt.Errorf("gatekeeper: extractor not configured")
	}
	if len(content) == 0 {
		return nil, nil, fmt.Errorf("gatekeeper: no content to reduce")
	}
	er, err := c.extractor.Extract(ctx, extract.ExtractRequest{
		Content:            content,
		Query:              query,
		ContentType:        "chat",
		RelevanceThreshold: rel.Threshold,
		TopKChunks:         rel.TopK,
		TopKRatio:          rel.Ratio,
	})
	if err != nil {
		return nil, nil, err
	}
	return er.Content, &ReductionMeasurement{
		OriginalBytes:           er.OriginalSize,
		ExtractedBytes:          er.ExtractedSize,
		SizeAfterRelevanceBytes: er.SizeAfterRelevance,
		ChunksProcessed:         er.ChunksProcessed,
		ChunksRetained:          er.ChunksRetained,
	}, nil
}

// MeasureReduction runs the extractor on the given messages purely to
// MEASURE how much context the relevance step would drop — it never
// mutates anything. Returns an error when the extractor isn't configured
// or the run fails (callers treat that as "no measurement"). query is the
// relevance anchor (typically the latest user turn). rel applies the
// resolved bundle's relevance tuning (zero fields = extractor defaults).
func (c *Client) MeasureReduction(ctx context.Context, messages []types.Message, query string, rel RelevanceOverride) (*ReductionMeasurement, error) {
	if c.extractor == nil {
		return nil, fmt.Errorf("gatekeeper: extractor not configured")
	}
	content := extractMessagesText(messages)
	if len(content) == 0 {
		return nil, fmt.Errorf("gatekeeper: no content to measure")
	}
	er, err := c.extractor.Extract(ctx, extract.ExtractRequest{
		Content:            content,
		Query:              query,
		ContentType:        "chat",
		RelevanceThreshold: rel.Threshold,
		TopKChunks:         rel.TopK,
		TopKRatio:          rel.Ratio,
	})
	if err != nil {
		return nil, err
	}
	return &ReductionMeasurement{
		OriginalBytes:           er.OriginalSize,
		ExtractedBytes:          er.ExtractedSize,
		SizeAfterRelevanceBytes: er.SizeAfterRelevance,
		ChunksProcessed:         er.ChunksProcessed,
		ChunksRetained:          er.ChunksRetained,
	}, nil
}

// ScanMessages extracts text from scannable messages and scans for violations.
// Messages are filtered based on the inbound ScanPolicy:
//   - Roles in AlwaysScanRoles (default: "user") are always scanned regardless of metadata.
//   - Roles in NeverScanRoles (default: "assistant") are always skipped.
//   - Other messages are skipped if metadata[TrustMetaKey] == PreScannedValue.
func (c *Client) ScanMessages(ctx context.Context, messages []types.Message, meta ScanMeta) (*pipeline.ProcessResult, error) {
	policy := c.config.Inbound.ScanPolicy
	if len(policy.AlwaysScanRoles) == 0 && len(policy.NeverScanRoles) == 0 {
		policy = DefaultScanPolicy()
	}

	// Filter to only scannable messages
	var scannable []types.Message
	for _, msg := range messages {
		if shouldScanMessage(msg, policy) {
			scannable = append(scannable, msg)
		}
	}

	content := extractMessagesText(scannable)
	if len(content) == 0 {
		return &pipeline.ProcessResult{Skipped: true, SkipReason: "all_messages_pre_scanned_or_empty"}, nil
	}

	profile := parseScanProfile(c.config.Inbound.ScanProfile)
	tier := parseTrustTier(c.config.Inbound.TrustTier)

	req := pipeline.ProcessRequest{
		Content:        content,
		ContentType:    "chat",
		TrustTier:      tier,
		ScanProfile:    profile,
		TenantID:       meta.TenantID,
		RequestID:      meta.RequestID,
		UserID:         meta.UserID,
		Source:         meta.Source,
		SkipExtraction: true,
		SkipStreaming:  true,
	}

	return c.processor.Process(ctx, req)
}

// shouldScanMessage determines whether a message should be included in content scanning.
func shouldScanMessage(msg types.Message, policy ScanPolicy) bool {
	role := strings.ToLower(msg.Role)

	// Always scan roles in the always-scan list (e.g. "user")
	for _, r := range policy.AlwaysScanRoles {
		if role == strings.ToLower(r) {
			return true
		}
	}

	// Never scan roles in the never-scan list (e.g. "assistant")
	for _, r := range policy.NeverScanRoles {
		if role == strings.ToLower(r) {
			return false
		}
	}

	// For other roles (system, tool), check trust metadata
	if msg.Metadata != nil {
		trustKey := policy.TrustMetaKey
		if trustKey == "" {
			trustKey = "trust"
		}
		preScannedVal := policy.PreScannedValue
		if preScannedVal == "" {
			preScannedVal = "pre_scanned"
		}
		if val, ok := msg.Metadata[trustKey]; ok {
			if strVal, ok := val.(string); ok && strVal == preScannedVal {
				return false
			}
		}
	}

	// Default: scan the message
	return true
}

// ScanResponse scans LLM output content for violations.
func (c *Client) ScanResponse(ctx context.Context, content string, meta ScanMeta) (*pipeline.ProcessResult, error) {
	if content == "" {
		return &pipeline.ProcessResult{Skipped: true, SkipReason: "empty_content"}, nil
	}

	profile := parseScanProfile(c.config.Outbound.ScanProfile)
	tier := parseTrustTier(c.config.Outbound.TrustTier)

	req := pipeline.ProcessRequest{
		Content:        []byte(content),
		ContentType:    "chat",
		TrustTier:      tier,
		ScanProfile:    profile,
		TenantID:       meta.TenantID,
		RequestID:      meta.RequestID,
		UserID:         meta.UserID,
		Source:         "llm_output",
		SkipExtraction: true,
		SkipStreaming:  true,
	}

	return c.processor.Process(ctx, req)
}

// ShouldBlock checks whether the scan result warrants blocking.
func (c *Client) ShouldBlock(result *pipeline.ProcessResult, direction string) bool {
	if result == nil || result.ScanResult == nil || result.Skipped {
		return false
	}

	var dirConfig ScanDirectionConfig
	if direction == "inbound" {
		dirConfig = c.config.Inbound
	} else {
		dirConfig = c.config.Outbound
	}

	if !dirConfig.BlockOnCritical {
		return false
	}

	// Block if any critical severity findings exist
	for _, f := range result.ScanResult.Findings {
		if f.Severity.Value() >= scan.SeverityCritical.Value() {
			return true
		}
	}

	// Also block if the action result says to block
	if result.ActionResult != nil && result.ActionResult.Blocked {
		return true
	}

	return false
}

// Close releases resources.
func (c *Client) Close() error {
	if c.extractor != nil {
		_ = c.extractor.Close()
	}
	if c.processor != nil {
		return c.processor.Close()
	}
	return nil
}

// extractMessagesText concatenates text content from all messages.
func extractMessagesText(messages []types.Message) []byte {
	var parts []string

	for _, msg := range messages {
		switch v := msg.Content.(type) {
		case string:
			if v != "" {
				parts = append(parts, v)
			}
		case []interface{}:
			for _, item := range v {
				if m, ok := item.(map[string]interface{}); ok {
					if t, ok := m["type"].(string); ok && t == "text" {
						if text, ok := m["text"].(string); ok && text != "" {
							parts = append(parts, text)
						}
					}
				}
			}
		default:
			// Try JSON marshal fallback
			if b, err := json.Marshal(v); err == nil {
				s := string(b)
				if s != "" && s != "null" {
					parts = append(parts, s)
				}
			}
		}
	}

	if len(parts) == 0 {
		return nil
	}

	return []byte(strings.Join(parts, "\n"))
}

// parseScanProfile converts a string to a ScanProfile.
func parseScanProfile(s string) scan.ScanProfile {
	switch strings.ToLower(s) {
	case "compliance":
		return scan.ProfileCompliance
	case "pii_only":
		return scan.ProfilePIIOnly
	case "injection_only":
		return scan.ProfileInjectionOnly
	default:
		return scan.ProfileFull
	}
}

// parseTrustTier converts a string to a TrustTier.
func parseTrustTier(s string) scan.TrustTier {
	switch strings.ToLower(s) {
	case "internal":
		return scan.TierInternal
	case "partner":
		return scan.TierPartner
	default:
		return scan.TierExternal
	}
}

// FormatBlockMessage returns a user-facing message for a blocked request.
func FormatBlockMessage(result *pipeline.ProcessResult, direction string) string {
	if result == nil || result.ScanResult == nil {
		return fmt.Sprintf("%s request blocked by content policy", direction)
	}

	critCount := 0
	for _, f := range result.ScanResult.Findings {
		if f.Severity.Value() >= scan.SeverityCritical.Value() {
			critCount++
		}
	}

	if critCount > 0 {
		return fmt.Sprintf("%s request blocked: %d critical content policy violation(s) detected", direction, critCount)
	}
	return fmt.Sprintf("%s request blocked by content policy", direction)
}
