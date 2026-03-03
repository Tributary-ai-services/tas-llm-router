package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

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
}

// ScanDirectionConfig configures scanning for a direction (inbound/outbound).
type ScanDirectionConfig struct {
	Enabled         bool   `yaml:"enabled"`
	ScanProfile     string `yaml:"scan_profile"`
	TrustTier       string `yaml:"trust_tier"`
	BlockOnCritical bool   `yaml:"block_on_critical"`
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
		},
		Outbound: ScanDirectionConfig{
			Enabled:         true,
			ScanProfile:     "full",
			TrustTier:       "partner",
			BlockOnCritical: true,
		},
	}
}

// Client wraps the Gatekeeper pipeline processor for LLM Router integration.
type Client struct {
	processor pipeline.Processor
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
	procConfig.EnableStreaming = false    // LLM Router manages its own audit
	procConfig.EnableActions = false     // LLM Router handles blocking
	procConfig.EnableAttestation = false // Simplified for now
	if cfg.ScanTimeout > 0 {
		procConfig.ScanTimeout = cfg.ScanTimeout
	}

	processor := pipeline.NewProcessor(scanner, pipeline.WithConfig(procConfig))

	return &Client{
		processor: processor,
		config:    cfg,
		logger:    logger,
	}, nil
}

// ScanMessages extracts text from messages and scans for violations.
func (c *Client) ScanMessages(ctx context.Context, messages []types.Message, meta ScanMeta) (*pipeline.ProcessResult, error) {
	content := extractMessagesText(messages)
	if len(content) == 0 {
		return &pipeline.ProcessResult{Skipped: true, SkipReason: "empty_content"}, nil
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
		SkipStreaming:   true,
	}

	return c.processor.Process(ctx, req)
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
		SkipStreaming:   true,
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
