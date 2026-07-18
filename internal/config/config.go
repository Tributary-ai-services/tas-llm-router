package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/providers/anthropic"
	"github.com/tributary-ai/llm-router-waf/internal/providers/openai"
	"github.com/tributary-ai/llm-router-waf/internal/security"
	"github.com/tributary-ai/llm-router-waf/internal/server"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/tokens"
)

// Config represents the complete application configuration
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Router     RouterConfig     `yaml:"router"`
	Providers  ProvidersConfig  `yaml:"providers"`
	Logging    LoggingConfig    `yaml:"logging"`
	Security   SecurityConfig   `yaml:"security"`
	Gatekeeper GatekeeperConfig `yaml:"gatekeeper"`
	AIQG       AIQGConfig       `yaml:"aiqg"`
}

// AIQGConfig configures the AIQG ingress feature. Empty/zero means
// AIQG is disabled — the middleware doesn't mount and existing traffic
// is completely unaffected. Flipping Enabled=true wires the timing
// collector + header parser + Path A entry middleware onto the three
// completion routes; the LogEmitter ships paired CloudEvents to logrus
// (→ Loki via the existing Alloy pipeline).
//
// MVP Tokens is a small in-process list; production deployments swap
// to a backend-client Resolver against aiqg-dashboard-be once that
// service ships.
type AIQGConfig struct {
	Enabled bool   `yaml:"enabled"`
	Strict  bool   `yaml:"strict"`
	Region  string `yaml:"region"`
	// IPCaptureMode gates how the client IP is recorded on AIQG events:
	// "off" (none), "minimized" (truncated /24·/48 prefix; the default
	// when empty), "full" (raw). Set "full" on internal/self-hosted
	// deployments; leave default for the privacy-minimized product.
	// Env: AIQG_IP_CAPTURE_MODE.
	IPCaptureMode string            `yaml:"ip_capture_mode"`
	Tokens        []AIQGTokenConfig `yaml:"tokens"`

	// EmitterType selects the AIQG event sink: "log" (default,
	// Loki via logrus), "kafka", or "both". Env override:
	// AIQG_EMITTER_TYPE.
	EmitterType string `yaml:"emitter_type"`

	// Kafka broker + topic when EmitterType is "kafka" or "both".
	// Env overrides: AIQG_KAFKA_BROKERS (comma-separated),
	// AIQG_KAFKA_TOPIC.
	Kafka AIQGKafkaConfig `yaml:"kafka"`

	// Dashboard backend integration. When both DashboardURL +
	// DashboardInternalAuthToken are set, the AIQG middleware uses
	// an HTTP DashboardResolver against aiqg-dashboard-be's
	// /internal/auth/validate endpoint instead of the in-memory
	// Tokens list. The Tokens list becomes a bootstrap fallback.
	//
	// Env overrides:
	//   AIQG_DASHBOARD_URL
	//   AIQG_DASHBOARD_INTERNAL_AUTH_TOKEN
	DashboardURL               string `yaml:"dashboard_url"`
	DashboardInternalAuthToken string `yaml:"dashboard_internal_auth_token"`

	// LinkageRedisURL enables the deterministic `linked`-tier attribution
	// (docs/AIQG-AGENT-FLOW-ATTRIBUTION.md §A): a Redis (redis://…) URL for
	// the tool_call_id echo index. Empty = linkage disabled (events emit
	// without step/parent topology). Env: AIQG_LINKAGE_REDIS_URL.
	LinkageRedisURL string `yaml:"linkage_redis_url"`

	// LLM-as-judge quality layer (docs/AIQG-EXPERIMENTS-RUNNER.md §6.6).
	// JudgeModel is the third model that rubric-scores a sample of responses
	// (MUST differ from production traffic's models to avoid self-preference).
	// JudgeSamplePct (0–100) is the fraction of AIQG responses judged async,
	// off the hot path. Judging is off when JudgeModel is empty or pct<=0.
	// Env: AIQG_JUDGE_MODEL, AIQG_JUDGE_SAMPLE_PCT.
	JudgeModel     string `yaml:"judge_model"`
	JudgeSamplePct int    `yaml:"judge_sample_pct"`
	// ShadowEvalPct (0–100) is the fraction of CONTROL-arm experiment samples
	// to pairwise shadow-eval — replay through each variant offline + judge
	// head-to-head. Costs ~2× per shadow sample, so default 0 (off; opt-in).
	// Env: AIQG_SHADOW_EVAL_PCT.
	ShadowEvalPct int `yaml:"shadow_eval_pct"`

	// ResponseCache is the C1 exact-match response cache (docs/AIQG-CACHING.md).
	// Default off. Reuses the linkage Redis client; falls back to a per-pod
	// in-memory cache when Redis isn't configured.
	ResponseCache AIQGResponseCacheConfig `yaml:"response_cache"`

	// SemCache is the C4 semantic response cache (docs/AIQG-SEMANTIC-CACHING.md).
	// Default off. Uses the dedicated redis-semcache (FT.*) + Ollama embeddings.
	SemCache AIQGSemCacheConfig `yaml:"semantic_cache"`
}

// AIQGSemCacheConfig configures the C4 semantic cache. Env:
// AIQG_SEMCACHE_ENABLED, AIQG_SEMCACHE_SHADOW (default true), AIQG_SEMCACHE_MIN_SIMILARITY,
// AIQG_SEMCACHE_TTL (Go duration), AIQG_SEMCACHE_REDIS_URL, AIQG_SEMCACHE_OLLAMA_URL,
// AIQG_SEMCACHE_EMBED_MODEL, AIQG_SEMCACHE_DIM, AIQG_SEMCACHE_JUDGE_DAILY_USD.
type AIQGSemCacheConfig struct {
	Enabled       bool          `yaml:"enabled"`
	Shadow        bool          `yaml:"shadow"`
	MinSimilarity float64       `yaml:"min_similarity"`
	TTL           time.Duration `yaml:"ttl"`
	RedisURL      string        `yaml:"redis_url"`
	OllamaURL     string        `yaml:"ollama_url"`
	EmbedModel    string        `yaml:"embed_model"`
	Dim           int           `yaml:"dim"`
	// JudgeDailyUSD caps the L3 async judge's spend per UTC day (§14.1 — judge
	// tokens are recurring opex). Default 0.50; 0 (or negative) means unlimited.
	// When the day's cap is reached the judge stops grading until UTC midnight.
	JudgeDailyUSD float64 `yaml:"judge_daily_usd"`
	// JudgeEnabled turns on the L3 async judge (grades sampled near-misses/would-
	// hits off the hot path → labeled pairs + sampled FPR). Default OFF: it is
	// recurring LLM opex, opt-in per §14.1. Env AIQG_SEMCACHE_JUDGE_ENABLED.
	JudgeEnabled bool `yaml:"judge_enabled"`
	// JudgeModel is the grader model. Empty falls back to the AIQG JudgeModel.
	// Env AIQG_SEMCACHE_JUDGE_MODEL.
	JudgeModel string `yaml:"judge_model"`
	// JudgeSampleRate is the fraction of eligible lookups graded (0..1). Default
	// 1.0 — at current traffic every data point is wanted. Env
	// AIQG_SEMCACHE_JUDGE_SAMPLE_RATE.
	JudgeSampleRate float64 `yaml:"judge_sample_rate"`
}

// AIQGResponseCacheConfig configures the C1 response cache. Env:
// AIQG_RESPONSE_CACHE_ENABLED, AIQG_RESPONSE_CACHE_TTL (Go duration),
// AIQG_RESPONSE_CACHE_MAX_BODY_BYTES, AIQG_RESPONSE_CACHE_ALLOW_NONDETERMINISTIC.
type AIQGResponseCacheConfig struct {
	Enabled bool `yaml:"enabled"`
	// TTL is the entry lifetime. Empty/zero defaults to 5m at wire-up.
	TTL time.Duration `yaml:"ttl"`
	// MaxBodyBytes skips storing responses larger than this (0 = no limit).
	MaxBodyBytes int `yaml:"max_body_bytes"`
	// AllowNondeterministic caches temperature>0 requests too. Default false
	// (require_deterministic=true) — the safe posture.
	AllowNondeterministic bool `yaml:"allow_nondeterministic"`
	// InExperiments caches WITHIN experiments (C3, variant folded into the key)
	// instead of bypassing experiment-claimed requests. Default false.
	InExperiments bool `yaml:"in_experiments"`
}

// AIQGKafkaConfig configures the Kafka emitter. Defined here (not in
// internal/server) so the YAML loader doesn't import server packages.
type AIQGKafkaConfig struct {
	Brokers []string `yaml:"brokers"`
	Topic   string   `yaml:"topic"`
}

// AIQGTokenConfig is the in-memory MVP token entry. Mirrors
// pkg/aiqg/tokens.ConfigToken so the conversion in ToServerConfig is
// a straight field copy; defined here to keep the config package's
// YAML schema self-describing without pulling pkg/aiqg/tokens into
// callers that only need to parse config.
type AIQGTokenConfig struct {
	TokenID       string `yaml:"token_id"`
	Token         string `yaml:"token"`
	TenantID      string `yaml:"tenant_id"`
	AIQGAccountID string `yaml:"aiqg_account_id"`
	SourceApp     string `yaml:"source_app"`
	Suspended     bool   `yaml:"suspended"`
}

// GatekeeperConfig holds content scanning configuration
type GatekeeperConfig struct {
	Enabled           bool                `yaml:"enabled"`
	FailOpen          bool                `yaml:"fail_open"`
	HonorAttestations bool                `yaml:"honor_attestations"`
	ScanTimeout       time.Duration       `yaml:"scan_timeout"`
	Inbound           ScanDirectionConfig `yaml:"inbound"`
	Outbound          ScanDirectionConfig `yaml:"outbound"`
	Extraction        ExtractionConfig    `yaml:"extraction"`
	Redaction         RedactionConfig     `yaml:"redaction"`
}

// RedactionConfig wires inbound PII redaction (G1). Default OFF (redaction is
// lossy). Enable via GATEKEEPER_REDACT_ENABLED=true; strategy via
// GATEKEEPER_REDACT_STRATEGY (mask|replace|hash, default mask).
type RedactionConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Strategy string `yaml:"strategy"`
}

// ExtractionConfig wires the Ollama-backed extractor used for AIQG shadow
// payload-reduction measurement (Plan #7 Phase 2). Disabled by default;
// enabled only on the AIQG gateway via env (AIQG_EXTRACTION_*).
type ExtractionConfig struct {
	Enabled        bool   `yaml:"enabled"`
	OllamaURL      string `yaml:"ollama_url"`       // e.g. http://ollama.tas-shared:11434
	EmbedModel     string `yaml:"embed_model"`      // e.g. all-minilm
	MinContentSize int    `yaml:"min_content_size"` // extractor floor (bytes)
	ApplyDisabled  bool   `yaml:"apply_disabled"`   // Phase 4 kill-switch: never APPLY active reduction
	// SLM rewrite step — optional cloud-backed compression (Plan #7 SLM unblock).
	SLMEnabled   bool   `yaml:"slm_enabled"`
	SLMProvider  string `yaml:"slm_provider"` // ollama | openai | anthropic
	SLMModel     string `yaml:"slm_model"`
	SLMBaseURL   string `yaml:"slm_base_url"`
	SLMAPIKey    string `yaml:"slm_api_key"`
	SLMMaxTokens int    `yaml:"slm_max_tokens"`
}

// ScanPolicyConfig controls which messages are scanned based on role and trust metadata.
type ScanPolicyConfig struct {
	AlwaysScanRoles []string `yaml:"always_scan_roles"` // Roles always scanned (default: ["user"])
	NeverScanRoles  []string `yaml:"never_scan_roles"`  // Roles never scanned (default: ["assistant"])
}

// ScanDirectionConfig configures scanning for a direction (inbound/outbound)
type ScanDirectionConfig struct {
	Enabled         bool             `yaml:"enabled"`
	ScanProfile     string           `yaml:"scan_profile"`
	TrustTier       string           `yaml:"trust_tier"`
	BlockOnCritical bool             `yaml:"block_on_critical"`
	ScanPolicy      ScanPolicyConfig `yaml:"scan_policy"`
}

// ServerConfig holds HTTP server configuration
type ServerConfig struct {
	Port           string        `yaml:"port"`
	ReadTimeout    time.Duration `yaml:"read_timeout"`
	WriteTimeout   time.Duration `yaml:"write_timeout"`
	MaxHeaderBytes int           `yaml:"max_header_bytes"`
}

// RouterConfig holds routing engine configuration
type RouterConfig struct {
	DefaultStrategy        string        `yaml:"default_strategy"`
	HealthCheckInterval    time.Duration `yaml:"health_check_interval"`
	MaxCostThreshold       float64       `yaml:"max_cost_threshold"`
	EnableFallbackChaining bool          `yaml:"enable_fallback_chaining"`
	RequestTimeout         time.Duration `yaml:"request_timeout"`
}

// ProvidersConfig holds configuration for all providers
type ProvidersConfig struct {
	OpenAI    *openai.OpenAIConfig       `yaml:"openai"`
	Anthropic *anthropic.AnthropicConfig `yaml:"anthropic"`
}

// LoggingConfig holds logging configuration
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"` // "json" or "text"
	Output string `yaml:"output"` // "stdout", "stderr", or file path
}

// SecurityConfig holds security-related configuration
type SecurityConfig struct {
	APIKeys           []string         `yaml:"api_keys"`
	RateLimiting      RateLimitConfig  `yaml:"rate_limiting"`
	CORS              CORSConfig       `yaml:"cors"`
	RequestValidation ValidationConfig `yaml:"request_validation"`
}

// RateLimitConfig holds rate limiting configuration
type RateLimitConfig struct {
	Enabled        bool          `yaml:"enabled"`
	RequestsPerMin int           `yaml:"requests_per_minute"`
	BurstSize      int           `yaml:"burst_size"`
	WindowDuration time.Duration `yaml:"window_duration"`
}

// CORSConfig holds CORS configuration
type CORSConfig struct {
	AllowedOrigins []string `yaml:"allowed_origins"`
	AllowedMethods []string `yaml:"allowed_methods"`
	AllowedHeaders []string `yaml:"allowed_headers"`
}

// ValidationConfig holds request validation configuration
type ValidationConfig struct {
	MaxRequestSize   int64 `yaml:"max_request_size"`
	MaxMessageLength int   `yaml:"max_message_length"`
	MaxMessages      int   `yaml:"max_messages"`
}

// LoadConfig loads configuration from file and environment variables
func LoadConfig(configPath string) (*Config, error) {
	config := &Config{}

	// Set defaults
	config.setDefaults()

	// Load from file if provided
	if configPath != "" {
		if err := config.loadFromFile(configPath); err != nil {
			return nil, fmt.Errorf("failed to load config from file: %w", err)
		}
	}

	// Override with environment variables
	config.loadFromEnv()

	// Validate configuration
	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return config, nil
}

// setDefaults sets default configuration values
func (c *Config) setDefaults() {
	// Server defaults
	c.Server = ServerConfig{
		Port:           "8080",
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   30 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1MB
	}

	// Router defaults
	c.Router = RouterConfig{
		DefaultStrategy:        "cost_optimized",
		HealthCheckInterval:    30 * time.Second,
		MaxCostThreshold:       1.0,
		EnableFallbackChaining: true,
		RequestTimeout:         120 * time.Second,
	}

	// Logging defaults
	c.Logging = LoggingConfig{
		Level:  "info",
		Format: "json",
		Output: "stdout",
	}

	// Security defaults
	c.Security = SecurityConfig{
		APIKeys: []string{},
		RateLimiting: RateLimitConfig{
			Enabled:        false,
			RequestsPerMin: 60,
			BurstSize:      10,
			WindowDuration: time.Minute,
		},
		CORS: CORSConfig{
			AllowedOrigins: []string{"*"},
			AllowedMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
			AllowedHeaders: []string{"Content-Type", "Authorization", "X-API-Key"},
		},
		RequestValidation: ValidationConfig{
			MaxRequestSize:   10 << 20, // 10MB
			MaxMessageLength: 100000,   // 100k characters
			MaxMessages:      50,
		},
	}

	// Gatekeeper defaults
	c.Gatekeeper = GatekeeperConfig{
		Enabled:           true,
		FailOpen:          true,
		HonorAttestations: true,
		ScanTimeout:       30 * time.Second,
		Inbound: ScanDirectionConfig{
			Enabled:         true,
			ScanProfile:     "full",
			TrustTier:       "external",
			BlockOnCritical: true,
			ScanPolicy: ScanPolicyConfig{
				AlwaysScanRoles: []string{"user"},
				NeverScanRoles:  []string{"assistant"},
			},
		},
		Outbound: ScanDirectionConfig{
			Enabled:         true,
			ScanProfile:     "full",
			TrustTier:       "partner",
			BlockOnCritical: true,
			ScanPolicy: ScanPolicyConfig{
				AlwaysScanRoles: []string{"user"},
				NeverScanRoles:  []string{"assistant"},
			},
		},
	}

	// Provider defaults
	c.Providers = ProvidersConfig{
		OpenAI: &openai.OpenAIConfig{
			Models: []types.ModelInfo{
				{
					Name:             "gpt-4o",
					ProviderModelID:  "gpt-4o",
					InputCostPer1K:   0.005,
					OutputCostPer1K:  0.015,
					MaxContextWindow: 128000,
					MaxOutputTokens:  4096,
				},
				{
					Name:             "gpt-4o-mini",
					ProviderModelID:  "gpt-4o-mini",
					InputCostPer1K:   0.00015,
					OutputCostPer1K:  0.0006,
					MaxContextWindow: 128000,
					MaxOutputTokens:  16384,
				},
				{
					Name:             "gpt-3.5-turbo",
					ProviderModelID:  "gpt-3.5-turbo",
					InputCostPer1K:   0.0015,
					OutputCostPer1K:  0.002,
					MaxContextWindow: 16385,
					MaxOutputTokens:  4096,
				},
			},
			Timeout: 120 * time.Second,
		},
		Anthropic: &anthropic.AnthropicConfig{
			Models: []types.ModelInfo{
				{
					Name:             "claude-opus-4-6",
					ProviderModelID:  "claude-opus-4-6",
					InputCostPer1K:   0.015,
					OutputCostPer1K:  0.075,
					MaxContextWindow: 1000000,
					MaxOutputTokens:  128000,
				},
				{
					Name:             "claude-sonnet-4-6",
					ProviderModelID:  "claude-sonnet-4-6",
					InputCostPer1K:   0.003,
					OutputCostPer1K:  0.015,
					MaxContextWindow: 1000000,
					MaxOutputTokens:  64000,
				},
				{
					Name:             "claude-haiku-4-5-20251001",
					ProviderModelID:  "claude-haiku-4-5-20251001",
					InputCostPer1K:   0.0008,
					OutputCostPer1K:  0.004,
					MaxContextWindow: 200000,
					MaxOutputTokens:  64000,
				},
			},
			Timeout: 120 * time.Second,
		},
	}
}

// loadFromFile loads configuration from YAML file
func (c *Config) loadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, c); err != nil {
		return fmt.Errorf("failed to parse YAML config: %w", err)
	}

	return nil
}

// loadFromEnv loads configuration from environment variables
func (c *Config) loadFromEnv() {
	// Server configuration
	if port := os.Getenv("LLM_ROUTER_PORT"); port != "" {
		c.Server.Port = port
	}

	// Provider API keys
	if openaiKey := os.Getenv("OPENAI_API_KEY"); openaiKey != "" {
		if c.Providers.OpenAI != nil {
			c.Providers.OpenAI.APIKey = openaiKey
		}
	} else if c.Providers.OpenAI != nil && c.Providers.OpenAI.APIKey == "" {
		// Disable OpenAI provider if no API key is provided
		c.Providers.OpenAI = nil
	}

	if anthropicKey := os.Getenv("ANTHROPIC_API_KEY"); anthropicKey != "" {
		if c.Providers.Anthropic != nil {
			c.Providers.Anthropic.APIKey = anthropicKey
		}
	} else if c.Providers.Anthropic != nil && c.Providers.Anthropic.APIKey == "" {
		// Disable Anthropic provider if no API key is provided
		c.Providers.Anthropic = nil
	}

	// Logging configuration
	if level := os.Getenv("LLM_ROUTER_LOG_LEVEL"); level != "" {
		c.Logging.Level = level
	}

	if format := os.Getenv("LLM_ROUTER_LOG_FORMAT"); format != "" {
		c.Logging.Format = format
	}

	// Server timeout configuration
	if rt := os.Getenv("SERVER_READ_TIMEOUT"); rt != "" {
		if d, err := time.ParseDuration(rt); err == nil {
			c.Server.ReadTimeout = d
		}
	}
	if wt := os.Getenv("SERVER_WRITE_TIMEOUT"); wt != "" {
		if d, err := time.ParseDuration(wt); err == nil {
			c.Server.WriteTimeout = d
		}
	}

	// Router configuration
	if strategy := os.Getenv("LLM_ROUTER_DEFAULT_STRATEGY"); strategy != "" {
		c.Router.DefaultStrategy = strategy
	}

	if rt := os.Getenv("ROUTER_REQUEST_TIMEOUT"); rt != "" {
		if d, err := time.ParseDuration(rt); err == nil {
			c.Router.RequestTimeout = d
		}
	}

	// Gatekeeper configuration
	if enabled := os.Getenv("GATEKEEPER_ENABLED"); enabled == "false" {
		c.Gatekeeper.Enabled = false
	}
	if failOpen := os.Getenv("GATEKEEPER_FAIL_OPEN"); failOpen == "false" {
		c.Gatekeeper.FailOpen = false
	}
	if st := os.Getenv("GATEKEEPER_SCAN_TIMEOUT"); st != "" {
		if d, err := time.ParseDuration(st); err == nil {
			c.Gatekeeper.ScanTimeout = d
		}
	}

	// Inbound PII redaction (G1). Off unless GATEKEEPER_REDACT_ENABLED=true —
	// redaction is lossy, so it's opt-in.
	if os.Getenv("GATEKEEPER_REDACT_ENABLED") == "true" {
		c.Gatekeeper.Redaction.Enabled = true
	}
	if s := os.Getenv("GATEKEEPER_REDACT_STRATEGY"); s != "" {
		c.Gatekeeper.Redaction.Strategy = s
	}

	// AIQG shadow payload-reduction extractor (Plan #7 Phase 2). Off unless
	// AIQG_EXTRACTION_ENABLED=true — only the AIQG gateway sets these.
	if enabled := os.Getenv("AIQG_EXTRACTION_ENABLED"); enabled == "true" {
		c.Gatekeeper.Extraction.Enabled = true
	}
	if u := os.Getenv("AIQG_EXTRACTION_OLLAMA_URL"); u != "" {
		c.Gatekeeper.Extraction.OllamaURL = u
	}
	if m := os.Getenv("AIQG_EXTRACTION_EMBED_MODEL"); m != "" {
		c.Gatekeeper.Extraction.EmbedModel = m
	}
	if n := os.Getenv("AIQG_EXTRACTION_MIN_CONTENT_SIZE"); n != "" {
		if v, err := strconv.Atoi(n); err == nil {
			c.Gatekeeper.Extraction.MinContentSize = v
		}
	}
	// Phase 4 break-glass kill-switch — when "true", active reduction never
	// mutates payloads (downgrades to shadow). Default off.
	if d := os.Getenv("AIQG_EXTRACTION_APPLY_DISABLED"); d == "true" {
		c.Gatekeeper.Extraction.ApplyDisabled = true
	}
	// SLM rewrite step (Plan #7 SLM unblock) — cloud-backed compression.
	if e := os.Getenv("AIQG_EXTRACTION_SLM_ENABLED"); e == "true" {
		c.Gatekeeper.Extraction.SLMEnabled = true
	}
	if p := os.Getenv("AIQG_EXTRACTION_SLM_PROVIDER"); p != "" {
		c.Gatekeeper.Extraction.SLMProvider = p
	}
	if m := os.Getenv("AIQG_EXTRACTION_SLM_MODEL"); m != "" {
		c.Gatekeeper.Extraction.SLMModel = m
	}
	if u := os.Getenv("AIQG_EXTRACTION_SLM_BASE_URL"); u != "" {
		c.Gatekeeper.Extraction.SLMBaseURL = u
	}
	if k := os.Getenv("AIQG_EXTRACTION_SLM_API_KEY"); k != "" {
		c.Gatekeeper.Extraction.SLMAPIKey = k
	}
	if n := os.Getenv("AIQG_EXTRACTION_SLM_MAX_TOKENS"); n != "" {
		if v, err := strconv.Atoi(n); err == nil {
			c.Gatekeeper.Extraction.SLMMaxTokens = v
		}
	}

	// AIQG ingress — env overrides for the simple scalars. Tokens are
	// only loadable via YAML (env vars are a poor shape for a list of
	// structs).
	if enabled := os.Getenv("AIQG_ENABLED"); enabled != "" {
		c.AIQG.Enabled = enabled == "true" || enabled == "1"
	}
	if strict := os.Getenv("AIQG_STRICT"); strict != "" {
		c.AIQG.Strict = strict == "true" || strict == "1"
	}
	if region := os.Getenv("AIQG_REGION"); region != "" {
		c.AIQG.Region = region
	}
	if mode := os.Getenv("AIQG_IP_CAPTURE_MODE"); mode != "" {
		c.AIQG.IPCaptureMode = mode
	}
	if et := os.Getenv("AIQG_EMITTER_TYPE"); et != "" {
		c.AIQG.EmitterType = et
	}
	if brokers := os.Getenv("AIQG_KAFKA_BROKERS"); brokers != "" {
		// Comma-separated list (matches the Kafka client convention).
		c.AIQG.Kafka.Brokers = splitCommaTrim(brokers)
	}
	if topic := os.Getenv("AIQG_KAFKA_TOPIC"); topic != "" {
		c.AIQG.Kafka.Topic = topic
	}
	if u := os.Getenv("AIQG_LINKAGE_REDIS_URL"); u != "" {
		c.AIQG.LinkageRedisURL = u
	}
	if m := os.Getenv("AIQG_JUDGE_MODEL"); m != "" {
		c.AIQG.JudgeModel = m
	}
	if p := os.Getenv("AIQG_JUDGE_SAMPLE_PCT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			c.AIQG.JudgeSamplePct = n
		}
	}
	if p := os.Getenv("AIQG_SHADOW_EVAL_PCT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			c.AIQG.ShadowEvalPct = n
		}
	}
	if v := os.Getenv("AIQG_RESPONSE_CACHE_ENABLED"); v != "" {
		c.AIQG.ResponseCache.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("AIQG_RESPONSE_CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.AIQG.ResponseCache.TTL = d
		}
	}
	if v := os.Getenv("AIQG_RESPONSE_CACHE_MAX_BODY_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.AIQG.ResponseCache.MaxBodyBytes = n
		}
	}
	if v := os.Getenv("AIQG_RESPONSE_CACHE_ALLOW_NONDETERMINISTIC"); v != "" {
		c.AIQG.ResponseCache.AllowNondeterministic = v == "true" || v == "1"
	}
	if v := os.Getenv("AIQG_RESPONSE_CACHE_IN_EXPERIMENTS"); v != "" {
		c.AIQG.ResponseCache.InExperiments = v == "true" || v == "1"
	}
	// C4 semantic cache. Shadow defaults ON when enabled (safe first mode).
	if v := os.Getenv("AIQG_SEMCACHE_ENABLED"); v != "" {
		c.AIQG.SemCache.Enabled = v == "true" || v == "1"
		c.AIQG.SemCache.Shadow = true
	}
	if v := os.Getenv("AIQG_SEMCACHE_SHADOW"); v != "" {
		c.AIQG.SemCache.Shadow = v == "true" || v == "1"
	}
	if v := os.Getenv("AIQG_SEMCACHE_MIN_SIMILARITY"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.AIQG.SemCache.MinSimilarity = f
		}
	}
	if v := os.Getenv("AIQG_SEMCACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.AIQG.SemCache.TTL = d
		}
	}
	if v := os.Getenv("AIQG_SEMCACHE_REDIS_URL"); v != "" {
		c.AIQG.SemCache.RedisURL = v
	}
	if v := os.Getenv("AIQG_SEMCACHE_OLLAMA_URL"); v != "" {
		c.AIQG.SemCache.OllamaURL = v
	}
	if v := os.Getenv("AIQG_SEMCACHE_EMBED_MODEL"); v != "" {
		c.AIQG.SemCache.EmbedModel = v
	}
	if v := os.Getenv("AIQG_SEMCACHE_DIM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.AIQG.SemCache.Dim = n
		}
	}
	// L3 judge daily spend cap defaults to $0.50/UTC-day when unset (§14.1). An
	// explicit env value overrides, and env "0" means unlimited (YAML cannot
	// express unlimited — a zero there reads as unset and takes the default).
	if c.AIQG.SemCache.JudgeDailyUSD == 0 {
		c.AIQG.SemCache.JudgeDailyUSD = 0.50
	}
	if v := os.Getenv("AIQG_SEMCACHE_JUDGE_DAILY_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.AIQG.SemCache.JudgeDailyUSD = f
		}
	}
	if v := os.Getenv("AIQG_SEMCACHE_JUDGE_ENABLED"); v != "" {
		c.AIQG.SemCache.JudgeEnabled = v == "true" || v == "1"
	}
	if v := os.Getenv("AIQG_SEMCACHE_JUDGE_MODEL"); v != "" {
		c.AIQG.SemCache.JudgeModel = v
	}
	// Sample rate defaults to 1.0 (grade everything) unless set; env overrides.
	if c.AIQG.SemCache.JudgeSampleRate == 0 {
		c.AIQG.SemCache.JudgeSampleRate = 1.0
	}
	if v := os.Getenv("AIQG_SEMCACHE_JUDGE_SAMPLE_RATE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.AIQG.SemCache.JudgeSampleRate = f
		}
	}
	if u := os.Getenv("AIQG_DASHBOARD_URL"); u != "" {
		c.AIQG.DashboardURL = u
	}
	if t := os.Getenv("AIQG_DASHBOARD_INTERNAL_AUTH_TOKEN"); t != "" {
		c.AIQG.DashboardInternalAuthToken = t
	}
	// AIQG_TOKENS_FILE points to a separate YAML file containing the
	// token list (typically mounted from a Kubernetes Secret so the
	// list never sits in a ConfigMap). When set + readable, the file's
	// tokens REPLACE any tokens already loaded from config.yaml —
	// this matches the operator's mental model of "the Secret is the
	// source of truth for live tokens." A missing or unreadable file
	// is logged but does not fail startup; the AIQG middleware
	// gracefully degrades to the no-resolver permissive path
	// documented in pkg/aiqg/middleware.
	if path := os.Getenv("AIQG_TOKENS_FILE"); path != "" {
		if err := c.loadAIQGTokensFromFile(path); err != nil {
			// Use stdlib log here; structured logger isn't initialized
			// at this point in startup.
			fmt.Fprintf(os.Stderr, "warning: AIQG_TOKENS_FILE=%q could not be loaded: %v\n", path, err)
		}
	}
}

// splitCommaTrim parses a comma-separated list, trimming whitespace
// around each entry and dropping empties. Used for env-var lists like
// AIQG_KAFKA_BROKERS where the convention is `host1:9092,host2:9092`.
func splitCommaTrim(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range splitOnComma(s) {
		if t := trimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Small stdlib indirections kept tight to avoid pulling strings into
// the config import surface unnecessarily.
func splitOnComma(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// loadAIQGTokensFromFile reads a tokens file and replaces c.AIQG.Tokens
// with its contents. The file shape is:
//
//	tokens:
//	  - token_id: "uuid"
//	    token: "tas_qg_live_..."
//	    tenant_id: "..."
//	    aiqg_account_id: "..."
//	    source_app: "..."
//	    suspended: false
//
// This is intentionally a sub-schema of the main Config so the same
// YAML structure works whether tokens come from config.yaml or a
// separately-mounted Secret.
func (c *Config) loadAIQGTokensFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read tokens file: %w", err)
	}
	var wrapper struct {
		Tokens []AIQGTokenConfig `yaml:"tokens"`
	}
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return fmt.Errorf("parse tokens file: %w", err)
	}
	c.AIQG.Tokens = wrapper.Tokens
	return nil
}

// validate validates the configuration
func (c *Config) validate() error {
	// Validate server port
	if c.Server.Port == "" {
		return fmt.Errorf("server port cannot be empty")
	}

	// Validate router strategy
	validStrategies := map[string]bool{
		"cost_optimized": true,
		"performance":    true,
		"round_robin":    true,
		"specific":       true,
	}

	if !validStrategies[c.Router.DefaultStrategy] {
		return fmt.Errorf("invalid default strategy: %s", c.Router.DefaultStrategy)
	}

	// Validate logging level
	validLogLevels := map[string]bool{
		"debug": true,
		"info":  true,
		"warn":  true,
		"error": true,
		"fatal": true,
	}

	if !validLogLevels[c.Logging.Level] {
		return fmt.Errorf("invalid log level: %s", c.Logging.Level)
	}

	// Validate provider configurations
	providerCount := 0

	if c.Providers.OpenAI != nil {
		if c.Providers.OpenAI.APIKey == "" {
			return fmt.Errorf("OpenAI API key is required when OpenAI provider is enabled")
		}
		if len(c.Providers.OpenAI.Models) == 0 {
			return fmt.Errorf("OpenAI provider must have at least one model configured")
		}
		providerCount++
	}

	if c.Providers.Anthropic != nil {
		if c.Providers.Anthropic.APIKey == "" {
			return fmt.Errorf("Anthropic API key is required when Anthropic provider is enabled")
		}
		if len(c.Providers.Anthropic.Models) == 0 {
			return fmt.Errorf("Anthropic provider must have at least one model configured")
		}
		providerCount++
	}

	if providerCount == 0 {
		return fmt.Errorf("at least one provider must be configured")
	}

	return nil
}

// ToServerConfig converts to server.ServerConfig
func (c *Config) ToServerConfig() *server.ServerConfig {
	return &server.ServerConfig{
		Port:           c.Server.Port,
		ReadTimeout:    c.Server.ReadTimeout,
		WriteTimeout:   c.Server.WriteTimeout,
		MaxHeaderBytes: c.Server.MaxHeaderBytes,
		Security:       c.ToSecurityMiddlewareConfig(),
		AIQG:           c.ToAIQGServerConfig(),
	}
}

// ToAIQGServerConfig converts to server.AIQGServerConfig. Returns nil
// when AIQG isn't enabled — server.NewServer treats that as "skip the
// middleware entirely". Token list is copied through verbatim.
func (c *Config) ToAIQGServerConfig() *server.AIQGServerConfig {
	if !c.AIQG.Enabled {
		return nil
	}
	out := &server.AIQGServerConfig{
		Enabled:                    c.AIQG.Enabled,
		Strict:                     c.AIQG.Strict,
		Region:                     c.AIQG.Region,
		IPCaptureMode:              c.AIQG.IPCaptureMode,
		EmitterType:                c.AIQG.EmitterType,
		DashboardURL:               c.AIQG.DashboardURL,
		DashboardInternalAuthToken: c.AIQG.DashboardInternalAuthToken,
		LinkageRedisURL:            c.AIQG.LinkageRedisURL,
		JudgeModel:                 c.AIQG.JudgeModel,
		JudgeSamplePct:             c.AIQG.JudgeSamplePct,
		ShadowEvalPct:              c.AIQG.ShadowEvalPct,
		Kafka: server.AIQGKafkaConfig{
			Brokers: c.AIQG.Kafka.Brokers,
			Topic:   c.AIQG.Kafka.Topic,
		},
		ResponseCache: server.AIQGResponseCacheConfig{
			Enabled:               c.AIQG.ResponseCache.Enabled,
			TTL:                   c.AIQG.ResponseCache.TTL,
			MaxBodyBytes:          c.AIQG.ResponseCache.MaxBodyBytes,
			AllowNondeterministic: c.AIQG.ResponseCache.AllowNondeterministic,
			InExperiments:         c.AIQG.ResponseCache.InExperiments,
		},
		SemCache: server.AIQGSemCacheConfig{
			Enabled:       c.AIQG.SemCache.Enabled,
			Shadow:        c.AIQG.SemCache.Shadow,
			MinSimilarity: c.AIQG.SemCache.MinSimilarity,
			TTL:           c.AIQG.SemCache.TTL,
			RedisURL:      c.AIQG.SemCache.RedisURL,
			OllamaURL:     c.AIQG.SemCache.OllamaURL,
			EmbedModel:      c.AIQG.SemCache.EmbedModel,
			Dim:             c.AIQG.SemCache.Dim,
			JudgeDailyUSD:   c.AIQG.SemCache.JudgeDailyUSD,
			JudgeEnabled:    c.AIQG.SemCache.JudgeEnabled,
			JudgeModel:      c.AIQG.SemCache.JudgeModel,
			JudgeSampleRate: c.AIQG.SemCache.JudgeSampleRate,
		},
	}
	if len(c.AIQG.Tokens) > 0 {
		out.Tokens = make([]tokens.ConfigToken, len(c.AIQG.Tokens))
		for i, t := range c.AIQG.Tokens {
			out.Tokens[i] = tokens.ConfigToken{
				TokenID:       t.TokenID,
				Token:         t.Token,
				TenantID:      t.TenantID,
				AIQGAccountID: t.AIQGAccountID,
				SourceApp:     t.SourceApp,
				Suspended:     t.Suspended,
			}
		}
	}
	return out
}

// ToSecurityMiddlewareConfig converts to middleware.SecurityMiddlewareConfig
func (c *Config) ToSecurityMiddlewareConfig() *middleware.SecurityMiddlewareConfig {
	return &middleware.SecurityMiddlewareConfig{
		Auth: &security.Config{
			APIKeys:        c.Security.APIKeys,
			RequireAuth:    len(c.Security.APIKeys) > 0,
			AllowedOrigins: c.Security.CORS.AllowedOrigins,
		},
		RateLimit: &security.RateLimitConfig{
			Enabled:           c.Security.RateLimiting.Enabled,
			RequestsPerMinute: c.Security.RateLimiting.RequestsPerMin,
			BurstSize:         c.Security.RateLimiting.BurstSize,
			WindowDuration:    c.Security.RateLimiting.WindowDuration,
			CleanupInterval:   5 * time.Minute,
		},
		Validation: &security.ValidationConfig{
			MaxRequestSize: 10 * 1024 * 1024, // 10MB
			AllowedMethods: c.Security.CORS.AllowedMethods,
			ContentTypes:   []string{"application/json", "text/plain"},
			MaxJSONDepth:   20,
			MaxFieldLength: 1024,
		},
		Audit: &security.AuditConfig{
			Enabled:       true,
			BufferSize:    1000,
			FlushInterval: 10 * time.Second,
		},
	}
}

// SaveToFile saves the current configuration to a YAML file
func (c *Config) SaveToFile(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal config to YAML: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// GetEnabledProviders returns a list of enabled provider names
func (c *Config) GetEnabledProviders() []string {
	var providers []string

	if c.Providers.OpenAI != nil && c.Providers.OpenAI.APIKey != "" {
		providers = append(providers, "openai")
	}

	if c.Providers.Anthropic != nil && c.Providers.Anthropic.APIKey != "" {
		providers = append(providers, "anthropic")
	}

	return providers
}
