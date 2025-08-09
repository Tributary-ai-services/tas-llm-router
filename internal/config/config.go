package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/providers/anthropic"
	"github.com/tributary-ai/llm-router-waf/internal/providers/openai"
	"github.com/tributary-ai/llm-router-waf/internal/security"
	"github.com/tributary-ai/llm-router-waf/internal/server"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// Config represents the complete application configuration
type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Router    RouterConfig     `yaml:"router"`
	Providers ProvidersConfig  `yaml:"providers"`
	Logging   LoggingConfig    `yaml:"logging"`
	Security  SecurityConfig   `yaml:"security"`
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
	DefaultStrategy         string        `yaml:"default_strategy"`
	HealthCheckInterval     time.Duration `yaml:"health_check_interval"`
	MaxCostThreshold        float64       `yaml:"max_cost_threshold"`
	EnableFallbackChaining  bool          `yaml:"enable_fallback_chaining"`
	RequestTimeout          time.Duration `yaml:"request_timeout"`
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
	APIKeys          []string          `yaml:"api_keys"`
	RateLimiting     RateLimitConfig   `yaml:"rate_limiting"`
	CORS             CORSConfig        `yaml:"cors"`
	RequestValidation ValidationConfig `yaml:"request_validation"`
}

// RateLimitConfig holds rate limiting configuration
type RateLimitConfig struct {
	Enabled         bool          `yaml:"enabled"`
	RequestsPerMin  int           `yaml:"requests_per_minute"`
	BurstSize       int           `yaml:"burst_size"`
	WindowDuration  time.Duration `yaml:"window_duration"`
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
		DefaultStrategy:         "cost_optimized",
		HealthCheckInterval:     30 * time.Second,
		MaxCostThreshold:        1.0,
		EnableFallbackChaining:  true,
		RequestTimeout:          120 * time.Second,
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
	
	// Provider defaults
	c.Providers = ProvidersConfig{
		OpenAI: &openai.OpenAIConfig{
			Models: []types.ModelInfo{
				{
					Name:              "gpt-4o",
					ProviderModelID:   "gpt-4o",
					InputCostPer1K:    0.005,
					OutputCostPer1K:   0.015,
					MaxContextWindow:  128000,
					MaxOutputTokens:   4096,
				},
				{
					Name:              "gpt-4o-mini",
					ProviderModelID:   "gpt-4o-mini",
					InputCostPer1K:    0.00015,
					OutputCostPer1K:   0.0006,
					MaxContextWindow:  128000,
					MaxOutputTokens:   16384,
				},
				{
					Name:              "gpt-3.5-turbo",
					ProviderModelID:   "gpt-3.5-turbo",
					InputCostPer1K:    0.0015,
					OutputCostPer1K:   0.002,
					MaxContextWindow:  16385,
					MaxOutputTokens:   4096,
				},
			},
			Timeout: 120 * time.Second,
		},
		Anthropic: &anthropic.AnthropicConfig{
			Models: []types.ModelInfo{
				{
					Name:              "claude-3-5-sonnet-20241022",
					ProviderModelID:   "claude-3-5-sonnet-20241022",
					InputCostPer1K:    0.003,
					OutputCostPer1K:   0.015,
					MaxContextWindow:  200000,
					MaxOutputTokens:   8192,
				},
				{
					Name:              "claude-3-haiku-20240307",
					ProviderModelID:   "claude-3-haiku-20240307",
					InputCostPer1K:    0.00025,
					OutputCostPer1K:   0.00125,
					MaxContextWindow:  200000,
					MaxOutputTokens:   4096,
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
	}
	
	if anthropicKey := os.Getenv("ANTHROPIC_API_KEY"); anthropicKey != "" {
		if c.Providers.Anthropic != nil {
			c.Providers.Anthropic.APIKey = anthropicKey
		}
	}
	
	// Logging configuration
	if level := os.Getenv("LLM_ROUTER_LOG_LEVEL"); level != "" {
		c.Logging.Level = level
	}
	
	if format := os.Getenv("LLM_ROUTER_LOG_FORMAT"); format != "" {
		c.Logging.Format = format
	}
	
	// Router configuration
	if strategy := os.Getenv("LLM_ROUTER_DEFAULT_STRATEGY"); strategy != "" {
		c.Router.DefaultStrategy = strategy
	}
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
	}
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
			MaxRequestSize:    10 * 1024 * 1024, // 10MB
			AllowedMethods:    c.Security.CORS.AllowedMethods,
			ContentTypes:      []string{"application/json", "text/plain"},
			MaxJSONDepth:      20,
			MaxFieldLength:    1024,
		},
		Audit: &security.AuditConfig{
			Enabled:     true,
			BufferSize:  1000,
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