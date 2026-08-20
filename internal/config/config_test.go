package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfig_Defaults(t *testing.T) {
	// Set test API keys to satisfy validation
	os.Setenv("OPENAI_API_KEY", "test-openai-key")
	os.Setenv("ANTHROPIC_API_KEY", "test-anthropic-key")
	defer func() {
		os.Unsetenv("OPENAI_API_KEY")
		os.Unsetenv("ANTHROPIC_API_KEY")
	}()

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	// Test default values
	if cfg.Server.Port != "8080" {
		t.Errorf("Expected default port '8080', got %s", cfg.Server.Port)
	}

	if cfg.Router.DefaultStrategy != "cost_optimized" {
		t.Errorf("Expected default strategy 'cost_optimized', got %s", cfg.Router.DefaultStrategy)
	}

	if cfg.Logging.Level != "info" {
		t.Errorf("Expected default log level 'info', got %s", cfg.Logging.Level)
	}

	if cfg.Server.ReadTimeout != 30*time.Second {
		t.Errorf("Expected default read timeout 30s, got %v", cfg.Server.ReadTimeout)
	}
}

func TestLoadConfig_EnvironmentOverride(t *testing.T) {
	// Set environment variables
	os.Setenv("LLM_ROUTER_PORT", "9090")
	os.Setenv("OPENAI_API_KEY", "test-openai-key")
	os.Setenv("ANTHROPIC_API_KEY", "test-anthropic-key")
	os.Setenv("LLM_ROUTER_LOG_LEVEL", "debug")
	os.Setenv("LLM_ROUTER_LOG_FORMAT", "text")
	os.Setenv("LLM_ROUTER_DEFAULT_STRATEGY", "performance")

	defer func() {
		os.Unsetenv("LLM_ROUTER_PORT")
		os.Unsetenv("OPENAI_API_KEY")
		os.Unsetenv("ANTHROPIC_API_KEY")
		os.Unsetenv("LLM_ROUTER_LOG_LEVEL")
		os.Unsetenv("LLM_ROUTER_LOG_FORMAT")
		os.Unsetenv("LLM_ROUTER_DEFAULT_STRATEGY")
	}()

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	// Test environment overrides
	if cfg.Server.Port != "9090" {
		t.Errorf("Expected port '9090', got %s", cfg.Server.Port)
	}

	if cfg.Logging.Level != "debug" {
		t.Errorf("Expected log level 'debug', got %s", cfg.Logging.Level)
	}

	if cfg.Logging.Format != "text" {
		t.Errorf("Expected log format 'text', got %s", cfg.Logging.Format)
	}

	if cfg.Router.DefaultStrategy != "performance" {
		t.Errorf("Expected strategy 'performance', got %s", cfg.Router.DefaultStrategy)
	}
}

func TestLoadConfig_Validation(t *testing.T) {
	tests := []struct {
		name    string
		setup   func()
		cleanup func()
		wantErr bool
		errMsg  string
	}{
		{
			name: "Missing API keys",
			setup: func() {
				os.Unsetenv("OPENAI_API_KEY")
				os.Unsetenv("ANTHROPIC_API_KEY")
			},
			cleanup: func() {},
			wantErr: true,
			// LoadConfig now requires at least one provider rather than
			// specifically OpenAI; the message reflects that.
			errMsg: "at least one provider must be configured",
		},
		{
			name: "Invalid log level",
			setup: func() {
				os.Setenv("OPENAI_API_KEY", "test-key")
				os.Setenv("ANTHROPIC_API_KEY", "test-key")
				os.Setenv("LLM_ROUTER_LOG_LEVEL", "invalid")
			},
			cleanup: func() {
				os.Unsetenv("OPENAI_API_KEY")
				os.Unsetenv("ANTHROPIC_API_KEY")
				os.Unsetenv("LLM_ROUTER_LOG_LEVEL")
			},
			wantErr: true,
			errMsg:  "invalid log level",
		},
		{
			name: "Invalid strategy",
			setup: func() {
				os.Setenv("OPENAI_API_KEY", "test-key")
				os.Setenv("ANTHROPIC_API_KEY", "test-key")
				os.Setenv("LLM_ROUTER_DEFAULT_STRATEGY", "invalid_strategy")
			},
			cleanup: func() {
				os.Unsetenv("OPENAI_API_KEY")
				os.Unsetenv("ANTHROPIC_API_KEY")
				os.Unsetenv("LLM_ROUTER_DEFAULT_STRATEGY")
			},
			wantErr: true,
			errMsg:  "invalid default strategy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup()
			defer tt.cleanup()

			_, err := LoadConfig("")

			if tt.wantErr {
				if err == nil {
					t.Error("Expected error but got none")
				} else if !containsString(err.Error(), tt.errMsg) {
					t.Errorf("Expected error containing %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error but got: %v", err)
				}
			}
		})
	}
}

func TestLoadConfig_FileLoading(t *testing.T) {
	// Create a temporary config file
	configContent := `
server:
  port: "3000"
  read_timeout: 60s

router:
  default_strategy: "round_robin"

logging:
  level: "warn"
  format: "text"

providers:
  openai:
    api_key: "file-openai-key"
  anthropic:
    api_key: "file-anthropic-key"
`

	tmpFile, err := os.CreateTemp("", "test_config_*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(configContent); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	tmpFile.Close()

	// Load config from file
	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	// Verify file values override defaults
	if cfg.Server.Port != "3000" {
		t.Errorf("Expected port '3000', got %s", cfg.Server.Port)
	}

	if cfg.Server.ReadTimeout != 60*time.Second {
		t.Errorf("Expected read timeout 60s, got %v", cfg.Server.ReadTimeout)
	}

	if cfg.Router.DefaultStrategy != "round_robin" {
		t.Errorf("Expected strategy 'round_robin', got %s", cfg.Router.DefaultStrategy)
	}

	if cfg.Logging.Level != "warn" {
		t.Errorf("Expected log level 'warn', got %s", cfg.Logging.Level)
	}

	if cfg.Providers.OpenAI.APIKey != "file-openai-key" {
		t.Errorf("Expected OpenAI key 'file-openai-key', got %s", cfg.Providers.OpenAI.APIKey)
	}
}

func TestConfig_GetEnabledProviders(t *testing.T) {
	tests := []struct {
		name          string
		openaiKey     string
		anthropicKey  string
		expectedCount int
		expectedNames []string
	}{
		{
			name:          "Both providers enabled",
			openaiKey:     "openai-test-key",
			anthropicKey:  "anthropic-test-key",
			expectedCount: 2,
			expectedNames: []string{"openai", "anthropic"},
		},
		{
			name:          "Only OpenAI enabled",
			openaiKey:     "openai-test-key",
			anthropicKey:  "",
			expectedCount: 1,
			expectedNames: []string{"openai"},
		},
		{
			name:          "Only Anthropic enabled",
			openaiKey:     "",
			anthropicKey:  "anthropic-test-key",
			expectedCount: 1,
			expectedNames: []string{"anthropic"},
		},
		{
			name:          "No providers enabled",
			openaiKey:     "",
			anthropicKey:  "",
			expectedCount: 0,
			expectedNames: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.setDefaults()

			// Set API keys
			if tt.openaiKey != "" {
				cfg.Providers.OpenAI.APIKey = tt.openaiKey
			}
			if tt.anthropicKey != "" {
				cfg.Providers.Anthropic.APIKey = tt.anthropicKey
			}

			enabledProviders := cfg.GetEnabledProviders()

			if len(enabledProviders) != tt.expectedCount {
				t.Errorf("Expected %d enabled providers, got %d", tt.expectedCount, len(enabledProviders))
			}

			// Check that expected providers are present
			providerMap := make(map[string]bool)
			for _, provider := range enabledProviders {
				providerMap[provider] = true
			}

			for _, expected := range tt.expectedNames {
				if !providerMap[expected] {
					t.Errorf("Expected provider %s not found in enabled providers", expected)
				}
			}
		})
	}
}

func TestConfig_ToServerConfig(t *testing.T) {
	cfg := &Config{}
	cfg.setDefaults()
	cfg.Server.Port = "9999"
	cfg.Server.ReadTimeout = 45 * time.Second
	cfg.Server.WriteTimeout = 50 * time.Second
	cfg.Server.MaxHeaderBytes = 2048

	serverConfig := cfg.ToServerConfig()

	if serverConfig.Port != "9999" {
		t.Errorf("Expected port '9999', got %s", serverConfig.Port)
	}

	if serverConfig.ReadTimeout != 45*time.Second {
		t.Errorf("Expected read timeout 45s, got %v", serverConfig.ReadTimeout)
	}

	if serverConfig.WriteTimeout != 50*time.Second {
		t.Errorf("Expected write timeout 50s, got %v", serverConfig.WriteTimeout)
	}

	if serverConfig.MaxHeaderBytes != 2048 {
		t.Errorf("Expected max header bytes 2048, got %d", serverConfig.MaxHeaderBytes)
	}
}

// AIQG disabled (the safe default) maps to a nil server.AIQGServerConfig
// so the middleware doesn't mount.
func TestConfig_ToAIQGServerConfig_Disabled(t *testing.T) {
	cfg := &Config{}
	cfg.setDefaults()
	// AIQG.Enabled is false by default.

	if got := cfg.ToAIQGServerConfig(); got != nil {
		t.Errorf("expected nil AIQG config when disabled, got %#v", got)
	}
	if got := cfg.ToServerConfig().AIQG; got != nil {
		t.Errorf("ToServerConfig should also yield nil AIQG when disabled, got %#v", got)
	}
}

// When enabled, AIQG values + tokens copy through to the server config.
func TestConfig_ToAIQGServerConfig_EnabledWithTokens(t *testing.T) {
	cfg := &Config{}
	cfg.setDefaults()
	cfg.AIQG.Enabled = true
	cfg.AIQG.Strict = true
	cfg.AIQG.Region = "us-west"
	cfg.AIQG.Tokens = []AIQGTokenConfig{
		{
			TokenID:       "tok_1",
			Token:         "tas_qg_live_abc",
			TenantID:      "tenant-a",
			AIQGAccountID: "account-a",
			SourceApp:     "billing",
			Suspended:     false,
		},
		{
			TokenID:   "tok_2",
			Token:     "tas_qg_live_def",
			Suspended: true,
		},
	}

	got := cfg.ToAIQGServerConfig()
	if got == nil {
		t.Fatalf("expected non-nil AIQG config when enabled")
	}
	if !got.Enabled || !got.Strict {
		t.Errorf("flags not propagated: enabled=%v strict=%v", got.Enabled, got.Strict)
	}
	if got.Region != "us-west" {
		t.Errorf("Region=%q", got.Region)
	}
	if len(got.Tokens) != 2 {
		t.Fatalf("token count=%d want=2", len(got.Tokens))
	}
	if got.Tokens[0].Token != "tas_qg_live_abc" || got.Tokens[0].TenantID != "tenant-a" {
		t.Errorf("token[0] mismatch: %#v", got.Tokens[0])
	}
	if !got.Tokens[1].Suspended {
		t.Errorf("token[1].Suspended not propagated")
	}
}

// AIQG_TOKENS_FILE pointing at a valid YAML replaces any config.yaml tokens.
func TestLoadConfig_AIQGTokensFile(t *testing.T) {
	dir := t.TempDir()
	tokensFile := dir + "/tokens.yaml"
	yaml := `tokens:
  - token_id: "tok-1"
    token: "tas_qg_live_abc"
    tenant_id: "tenant-a"
    aiqg_account_id: "account-a"
    source_app: "billing"
    suspended: false
  - token_id: "tok-2"
    token: "tas_qg_live_def"
    tenant_id: "tenant-b"
    aiqg_account_id: "account-b"
    suspended: true
`
	if err := os.WriteFile(tokensFile, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write tokens file: %v", err)
	}

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("AIQG_ENABLED", "true")
	t.Setenv("AIQG_TOKENS_FILE", tokensFile)

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.AIQG.Tokens) != 2 {
		t.Fatalf("token count=%d want=2", len(cfg.AIQG.Tokens))
	}
	if cfg.AIQG.Tokens[0].TenantID != "tenant-a" || cfg.AIQG.Tokens[0].Token != "tas_qg_live_abc" {
		t.Errorf("token[0] mismatch: %#v", cfg.AIQG.Tokens[0])
	}
	if !cfg.AIQG.Tokens[1].Suspended {
		t.Errorf("token[1].Suspended not propagated")
	}
}

// File takes precedence over config.yaml tokens (replace, not merge).
func TestLoadConfig_AIQGTokensFileReplacesYAML(t *testing.T) {
	dir := t.TempDir()
	tokensFile := dir + "/tokens.yaml"
	if err := os.WriteFile(tokensFile, []byte("tokens:\n  - token: \"tas_qg_live_from_file\"\n    tenant_id: \"tenant-from-file\"\n"), 0o600); err != nil {
		t.Fatalf("write tokens file: %v", err)
	}

	c := &Config{}
	c.setDefaults()
	c.AIQG.Tokens = []AIQGTokenConfig{{Token: "tas_qg_live_from_yaml", TenantID: "tenant-from-yaml"}}

	if err := c.loadAIQGTokensFromFile(tokensFile); err != nil {
		t.Fatalf("loadAIQGTokensFromFile: %v", err)
	}
	if len(c.AIQG.Tokens) != 1 {
		t.Fatalf("len=%d want=1", len(c.AIQG.Tokens))
	}
	if c.AIQG.Tokens[0].Token != "tas_qg_live_from_file" {
		t.Errorf("file did not override YAML: %#v", c.AIQG.Tokens[0])
	}
}

// Missing AIQG_TOKENS_FILE is logged but does NOT fail LoadConfig.
// The middleware just runs in the no-resolver permissive path.
func TestLoadConfig_AIQGTokensFileMissingDoesNotFail(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("AIQG_TOKENS_FILE", "/this/path/does/not/exist.yaml")

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig should not fail on missing tokens file: %v", err)
	}
	if len(cfg.AIQG.Tokens) != 0 {
		t.Errorf("expected empty tokens, got %d", len(cfg.AIQG.Tokens))
	}
}

// Malformed YAML in the tokens file is logged but doesn't fail startup.
func TestLoadConfig_AIQGTokensFileMalformedDoesNotFail(t *testing.T) {
	dir := t.TempDir()
	tokensFile := dir + "/tokens.yaml"
	if err := os.WriteFile(tokensFile, []byte("this is not\n valid: : yaml: :"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("AIQG_TOKENS_FILE", tokensFile)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig should not fail on malformed tokens file: %v", err)
	}
	if len(cfg.AIQG.Tokens) != 0 {
		t.Errorf("malformed yaml should leave tokens empty, got %d", len(cfg.AIQG.Tokens))
	}
}

// Env-var overrides flip the YAML defaults.
func TestLoadConfig_AIQGEnvOverrides(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key") // satisfy provider validation
	t.Setenv("AIQG_ENABLED", "true")
	t.Setenv("AIQG_STRICT", "1")
	t.Setenv("AIQG_REGION", "eu")

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.AIQG.Enabled {
		t.Errorf("AIQG_ENABLED=true did not flip Enabled")
	}
	if !cfg.AIQG.Strict {
		t.Errorf("AIQG_STRICT=1 did not flip Strict")
	}
	if cfg.AIQG.Region != "eu" {
		t.Errorf("Region=%q", cfg.AIQG.Region)
	}
}

func TestConfig_SaveToFile(t *testing.T) {
	// Create a config
	cfg := &Config{}
	cfg.setDefaults()
	cfg.Server.Port = "4000"

	// Create temp file
	tmpFile, err := os.CreateTemp("", "test_save_*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	// Save config
	if err := cfg.SaveToFile(tmpFile.Name()); err != nil {
		t.Fatalf("SaveToFile failed: %v", err)
	}

	// Read back and verify
	data, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to read saved file: %v", err)
	}

	content := string(data)
	if !containsString(content, "port: \"4000\"") {
		t.Error("Saved config should contain the custom port")
	}

	if !containsString(content, "default_strategy: cost_optimized") {
		t.Error("Saved config should contain default strategy")
	}
}

// Helper functions
func containsString(s, substr string) bool {
	return len(substr) <= len(s) && (substr == s || containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Benchmark tests
func BenchmarkLoadConfig_Defaults(b *testing.B) {
	os.Setenv("OPENAI_API_KEY", "test-key")
	os.Setenv("ANTHROPIC_API_KEY", "test-key")
	defer func() {
		os.Unsetenv("OPENAI_API_KEY")
		os.Unsetenv("ANTHROPIC_API_KEY")
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = LoadConfig("")
	}
}

func BenchmarkConfig_GetEnabledProviders(b *testing.B) {
	cfg := &Config{}
	cfg.setDefaults()
	cfg.Providers.OpenAI.APIKey = "test-key"
	cfg.Providers.Anthropic.APIKey = "test-key"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cfg.GetEnabledProviders()
	}
}
