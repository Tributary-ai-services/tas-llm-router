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

	// Redaction configures inbound message redaction (G1,
	// docs/AIQG-GATEKEEPER-INTEGRATION.md). Default OFF: redaction is LOSSY —
	// masking customer@acme.com to [EMAIL] degrades what the model can answer —
	// so it is opt-in. (Per-route targeting via the resolved policy bundle is a
	// follow-up; this global flag is the first slice.) It is also the precondition
	// for response caching: a cache built before this stores raw PII at rest.
	Redaction RedactionConfig `yaml:"redaction"`

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
	// ApplyDisabled is the global break-glass kill-switch for ACTIVE payload
	// reduction (Plan #7 Phase 4). When true, active bundles still resolve but
	// the gateway never mutates the payload — they downgrade to shadow
	// measurement. Shadow/projected are unaffected. Toggle via env without an
	// image rebuild.
	ApplyDisabled bool `yaml:"apply_disabled"`

	// SLM rewrite step (Plan #7). The relevance/embedding step always runs on
	// Ollama; the optional SLM compression step can run on a cloud provider so
	// it works without a local GPU. Disabled by default.
	SLMEnabled   bool   `yaml:"slm_enabled"`
	SLMProvider  string `yaml:"slm_provider"` // ollama | openai | anthropic
	SLMModel     string `yaml:"slm_model"`    // e.g. gpt-4o-mini, claude-haiku-4-5-20251001, phi3.5
	SLMBaseURL   string `yaml:"slm_base_url"` // optional override; provider default otherwise
	SLMAPIKey    string `yaml:"slm_api_key"`  // bearer / x-api-key for cloud providers
	SLMMaxTokens int    `yaml:"slm_max_tokens"`
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

// RedactionConfig configures inbound message redaction (G1). Strategy is a
// deterministic, infra-free redaction — "mask" (j***@***.com, default),
// "replace" ([EMAIL]), or "hash" ([HASH:…]). Tokenize is NOT offered: it needs
// Databunker (not deployed) and would mint per-call tokens that break caching.
type RedactionConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Strategy string `yaml:"strategy"`
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
	// scanner + redactor back the inbound redaction path (G1,
	// docs/AIQG-GATEKEEPER-INTEGRATION.md). The pipeline's only content-mutating
	// path is the Databunker tokenizer (not deployed), so mask/replace/hash
	// redaction goes through the scanner's ScanString + the RedactionEngine
	// directly — deterministic and infra-free.
	scanner  scan.Scanner
	redactor scan.RedactionEngine
	config   Config
	logger   *logrus.Logger
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
		if cfg.Extraction.EmbedModel != "" {
			ec.Embedding.Model = cfg.Extraction.EmbedModel
		}
		// Embeddings always run on Ollama; its own URL is decoupled from the
		// SLM step so the SLM can target a cloud provider.
		ec.Embedding.URL = cfg.Extraction.OllamaURL
		if cfg.Extraction.MinContentSize > 0 {
			ec.MinContentSize = cfg.Extraction.MinContentSize
		}
		// Optional SLM compression step. Provider "ollama" needs a local GPU;
		// "openai"/"anthropic" route the rewrite to a cloud model (Plan #7
		// SLM unblock). Default off → relevance-only reduction.
		ec.EnableSLM = cfg.Extraction.SLMEnabled
		if cfg.Extraction.SLMEnabled {
			ec.SLM = extract.SLMConfig{
				Provider:  cfg.Extraction.SLMProvider,
				URL:       cfg.Extraction.SLMBaseURL,
				Model:     cfg.Extraction.SLMModel,
				APIKey:    cfg.Extraction.SLMAPIKey,
				MaxTokens: cfg.Extraction.SLMMaxTokens,
			}
			if cfg.Extraction.SLMProvider == extract.SLMProviderOllama && cfg.Extraction.SLMBaseURL == "" {
				ec.SLM.URL = cfg.Extraction.OllamaURL // local SLM shares the Ollama endpoint
			}
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
		scanner:   scanner,
		redactor:  scan.NewRedactionEngine(),
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

// RedactMessages returns a NEW message slice with PII redacted in each scannable
// message, plus the number of findings redacted (G1). The original slice is not
// mutated; the caller assigns the result to req.Messages when redaction is
// enabled for the route.
//
// It re-scans per message rather than reusing the block-path scan: ScanMessages
// concatenates message text to decide blocking, and those offsets don't map back
// to individual messages. Per-message scan is the correct granularity for
// write-back, at the cost of a second scan (acceptable for G1; attestation
// removes the double-scan later).
//
// Redaction is deterministic and content-derived (mask/replace/hash), so the
// redacted form is byte-stable — identical input yields identical output, which
// keeps prompt caching intact and lets a later cache key on the redacted prompt.
// Only string content is redacted; multimodal ([]interface{}) content is left
// unchanged in v1. Never call this on a request that was blocked.
//
// On a scan error it returns the ORIGINAL messages, 0, and the error — the caller
// fails open (send the original, same as pre-G1 behavior) rather than dropping
// the request.
func (c *Client) RedactMessages(ctx context.Context, messages []types.Message, meta ScanMeta, strategy string) ([]types.Message, int, error) {
	if c == nil || c.scanner == nil || c.redactor == nil {
		return messages, 0, nil
	}

	policy := c.config.Inbound.ScanPolicy
	if len(policy.AlwaysScanRoles) == 0 && len(policy.NeverScanRoles) == 0 {
		policy = DefaultScanPolicy()
	}
	strat := parseRedactionStrategy(strategy)
	profile := parseScanProfile(c.config.Inbound.ScanProfile)
	tier := parseTrustTier(c.config.Inbound.TrustTier)

	out := make([]types.Message, len(messages))
	copy(out, messages)

	total := 0
	for i := range out {
		if !shouldScanMessage(out[i], policy) {
			continue
		}
		s, ok := out[i].Content.(string)
		if !ok || s == "" {
			continue // multimodal / empty — not redacted in v1
		}

		cfg := scan.DefaultScanConfig()
		cfg.Profile = profile
		cfg.TrustTier = tier
		cfg.RedactionMode = strat // force our strategy; never the tokenize default

		res, err := c.scanner.ScanString(ctx, s, cfg)
		if err != nil {
			return messages, 0, err
		}
		if res == nil || len(res.Findings) == 0 {
			continue
		}
		redacted, rerr := c.redactor.Redact(s, res.Findings, strat)
		if rerr != nil {
			// Detection succeeded, redaction of this message failed: leave the
			// message unredacted rather than dropping content, and don't count it.
			continue
		}
		out[i].Content = redacted
		total += len(res.Findings)
	}
	return out, total, nil
}

// parseRedactionStrategy maps a config string to a deterministic, infra-free
// strategy. Tokenize (Databunker) and remove (silent data loss) are excluded;
// anything unrecognized → mask.
func parseRedactionStrategy(s string) scan.RedactionStrategy {
	switch s {
	case "replace":
		return scan.RedactionReplace
	case "hash":
		return scan.RedactionHash
	default:
		return scan.RedactionMask
	}
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
