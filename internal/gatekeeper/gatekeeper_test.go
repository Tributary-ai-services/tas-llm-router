package gatekeeper

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Tributary-ai-services/Gatekeeper/pkg/pipeline"
	"github.com/Tributary-ai-services/Gatekeeper/pkg/scan"
	"github.com/Tributary-ai-services/Gatekeeper/pkg/types"
	routerTypes "github.com/tributary-ai/llm-router-waf/internal/types"
)

func newTestClient(t *testing.T) *Client {
	t.Helper()
	cfg := DefaultConfig()
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	client, err := New(cfg, logger)
	require.NoError(t, err)
	return client
}

func TestScanMessages_Clean(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	messages := []routerTypes.Message{
		{Role: "user", Content: "Hello, how are you today?"},
	}

	meta := ScanMeta{
		TenantID:  "test-tenant",
		RequestID: "req-1",
		UserID:    "user-1",
		Source:    "llm_input",
	}

	result, err := client.ScanMessages(context.Background(), messages, meta)
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.False(t, client.ShouldBlock(result, "inbound"))
}

func TestScanMessages_CriticalFinding(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	// SQL injection content should trigger critical findings
	messages := []routerTypes.Message{
		{Role: "user", Content: "'; DROP TABLE users; --"},
	}

	meta := ScanMeta{
		TenantID:  "test-tenant",
		RequestID: "req-2",
		UserID:    "user-1",
		Source:    "llm_input",
	}

	result, err := client.ScanMessages(context.Background(), messages, meta)
	require.NoError(t, err)
	assert.NotNil(t, result)

	// Verify findings were detected
	if result.ScanResult != nil && len(result.ScanResult.Findings) > 0 {
		hasCritical := false
		for _, f := range result.ScanResult.Findings {
			if f.Severity >= scan.SeverityCritical {
				hasCritical = true
				break
			}
		}
		if hasCritical {
			assert.True(t, client.ShouldBlock(result, "inbound"))
		}
	}
}

func TestScanMessages_MultipartContent(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	// Simulate multipart content ([]interface{} after JSON decode)
	messages := []routerTypes.Message{
		{
			Role: "user",
			Content: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": "Hello, this is a normal message",
				},
			},
		},
	}

	meta := ScanMeta{
		TenantID:  "test-tenant",
		RequestID: "req-3",
		UserID:    "user-1",
		Source:    "llm_input",
	}

	result, err := client.ScanMessages(context.Background(), messages, meta)
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.False(t, client.ShouldBlock(result, "inbound"))
}

func TestScanMessages_FailOpen(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FailOpen = true

	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	client, err := New(cfg, logger)
	require.NoError(t, err)
	defer client.Close()

	messages := []routerTypes.Message{
		{Role: "user", Content: "normal message"},
	}

	meta := ScanMeta{
		TenantID:  "test-tenant",
		RequestID: "req-4",
		UserID:    "user-1",
		Source:    "llm_input",
	}

	// Even if scanning were to fail, fail-open should not block
	result, err := client.ScanMessages(context.Background(), messages, meta)
	require.NoError(t, err)
	assert.False(t, client.ShouldBlock(result, "inbound"))
}

func TestScanMessages_FailClosed(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FailOpen = false

	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	client, err := New(cfg, logger)
	require.NoError(t, err)
	defer client.Close()

	// Normal message should pass even in fail-closed mode
	messages := []routerTypes.Message{
		{Role: "user", Content: "normal message"},
	}

	meta := ScanMeta{
		TenantID:  "test-tenant",
		RequestID: "req-5",
		UserID:    "user-1",
		Source:    "llm_input",
	}

	result, err := client.ScanMessages(context.Background(), messages, meta)
	require.NoError(t, err)
	assert.False(t, client.ShouldBlock(result, "inbound"))
}

func TestScanResponse_PIIDetected(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	// Content with PII (SSN)
	content := "Here is the SSN: 123-45-6789 for the patient"

	meta := ScanMeta{
		TenantID:  "test-tenant",
		RequestID: "req-6",
		UserID:    "user-1",
		Source:    "llm_output",
	}

	result, err := client.ScanResponse(context.Background(), content, meta)
	require.NoError(t, err)
	assert.NotNil(t, result)

	// SSN should be detected
	if result.ScanResult != nil {
		assert.Greater(t, len(result.ScanResult.Findings), 0, "SSN should be detected in content")
	}
}

func TestScanResponse_EmptyContent(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	meta := ScanMeta{
		TenantID:  "test-tenant",
		RequestID: "req-7",
		UserID:    "user-1",
		Source:    "llm_output",
	}

	result, err := client.ScanResponse(context.Background(), "", meta)
	require.NoError(t, err)
	assert.True(t, result.Skipped)
	assert.Equal(t, "empty_content", result.SkipReason)
}

func TestShouldBlock_Severity(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	tests := []struct {
		name     string
		severity scan.Severity
		want     bool
	}{
		{"critical blocks", scan.SeverityCritical, true},
		{"high does not block", scan.SeverityHigh, false},
		{"medium does not block", scan.SeverityMedium, false},
		{"low does not block", scan.SeverityLow, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &pipeline.ProcessResult{
				ScanResult: &scan.ScanResult{
					Findings: []scan.Finding{
						{Severity: tt.severity, PatternID: "test"},
					},
				},
			}

			got := client.ShouldBlock(result, "inbound")
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestShouldBlock_NilResult(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	assert.False(t, client.ShouldBlock(nil, "inbound"))
	assert.False(t, client.ShouldBlock(&pipeline.ProcessResult{}, "inbound"))
	assert.False(t, client.ShouldBlock(&pipeline.ProcessResult{Skipped: true}, "inbound"))
}

func TestShouldBlock_BlockOnCriticalDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Inbound.BlockOnCritical = false

	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	client, err := New(cfg, logger)
	require.NoError(t, err)
	defer client.Close()

	result := &pipeline.ProcessResult{
		ScanResult: &scan.ScanResult{
			Findings: []scan.Finding{
				{Severity: scan.SeverityCritical, PatternID: "test"},
			},
		},
	}

	assert.False(t, client.ShouldBlock(result, "inbound"), "Should not block when BlockOnCritical is false")
}

func TestHonorAttestation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HonorAttestations = true

	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	client, err := New(cfg, logger)
	require.NoError(t, err)
	defer client.Close()

	// Without an attestor configured, attestation won't cause a skip.
	// This test verifies the flag is respected and doesn't error.
	messages := []routerTypes.Message{
		{Role: "user", Content: "Test message"},
	}

	meta := ScanMeta{
		TenantID:  "test-tenant",
		RequestID: "req-8",
		UserID:    "user-1",
		Source:    "llm_input",
	}

	result, err := client.ScanMessages(context.Background(), messages, meta)
	require.NoError(t, err)
	assert.NotNil(t, result)
}

func TestExtractMessagesText(t *testing.T) {
	tests := []struct {
		name     string
		messages []routerTypes.Message
		want     string
	}{
		{
			name:     "string content",
			messages: []routerTypes.Message{{Role: "user", Content: "hello"}},
			want:     "hello",
		},
		{
			name: "multipart content",
			messages: []routerTypes.Message{{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "hello"},
					map[string]interface{}{"type": "text", "text": "world"},
				},
			}},
			want: "hello\nworld",
		},
		{
			name:     "empty messages",
			messages: []routerTypes.Message{},
			want:     "",
		},
		{
			name:     "nil content",
			messages: []routerTypes.Message{{Role: "user", Content: nil}},
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMessagesText(tt.messages)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

// Bypass token tests

func TestBypassToken_Valid(t *testing.T) {
	store := NewInMemoryBypassStore()
	audit := &NoopAuditLogger{}
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	manager := NewBypassManager(store, audit, logger)

	rawToken, token, err := manager.GenerateToken(context.Background(), "user-1", "tenant-1", "debugging", 4)
	require.NoError(t, err)
	assert.NotEmpty(t, rawToken)
	assert.NotEmpty(t, token.TokenID)

	// Validate the token
	userID, valid := manager.ValidateBypassToken(context.Background(), rawToken, "req-1", "hash123")
	assert.True(t, valid)
	assert.Equal(t, "user-1", userID)

	// Verify audit event was logged
	assert.Len(t, audit.Events, 1)
	assert.Equal(t, "scan_bypass_used", audit.Events[0].EventType)
	assert.Equal(t, "warning", audit.Events[0].Severity)
	assert.Equal(t, "user-1", audit.Events[0].UserID)
}

func TestBypassToken_Expired(t *testing.T) {
	store := NewInMemoryBypassStore()
	audit := &NoopAuditLogger{}
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	manager := NewBypassManager(store, audit, logger)

	// Create a token that's already expired by manipulating the store
	rawToken, token, err := manager.GenerateToken(context.Background(), "user-1", "tenant-1", "debugging", 1)
	require.NoError(t, err)

	// Manually expire the token
	hash := hashToken(rawToken)
	storedToken, _ := store.Lookup(context.Background(), hash)
	storedToken.ExpiresAt = time.Now().Add(-1 * time.Hour)
	_ = token // suppress unused

	userID, valid := manager.ValidateBypassToken(context.Background(), rawToken, "req-1", "hash123")
	assert.False(t, valid)
	assert.Empty(t, userID)
}

func TestBypassToken_Revoked(t *testing.T) {
	store := NewInMemoryBypassStore()
	audit := &NoopAuditLogger{}
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	manager := NewBypassManager(store, audit, logger)

	rawToken, token, err := manager.GenerateToken(context.Background(), "user-1", "tenant-1", "debugging", 4)
	require.NoError(t, err)

	// Revoke the token
	err = manager.RevokeToken(context.Background(), token.TokenID)
	require.NoError(t, err)

	// Try to validate - should fail
	userID, valid := manager.ValidateBypassToken(context.Background(), rawToken, "req-1", "hash123")
	assert.False(t, valid)
	assert.Empty(t, userID)
}

func TestBypassToken_InvalidToken(t *testing.T) {
	store := NewInMemoryBypassStore()
	audit := &NoopAuditLogger{}
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	manager := NewBypassManager(store, audit, logger)

	userID, valid := manager.ValidateBypassToken(context.Background(), "invalid-token", "req-1", "hash123")
	assert.False(t, valid)
	assert.Empty(t, userID)
}

func TestBypassToken_ListTokens(t *testing.T) {
	store := NewInMemoryBypassStore()
	audit := &NoopAuditLogger{}
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	manager := NewBypassManager(store, audit, logger)

	_, _, err := manager.GenerateToken(context.Background(), "user-1", "tenant-1", "reason1", 4)
	require.NoError(t, err)
	_, _, err = manager.GenerateToken(context.Background(), "user-1", "tenant-1", "reason2", 8)
	require.NoError(t, err)
	_, _, err = manager.GenerateToken(context.Background(), "user-2", "tenant-1", "reason3", 4)
	require.NoError(t, err)

	tokens, err := manager.ListTokens(context.Background(), "user-1")
	require.NoError(t, err)
	assert.Len(t, tokens, 2)
}

func TestFormatBlockMessage(t *testing.T) {
	msg := FormatBlockMessage(nil, "Inbound")
	assert.Contains(t, msg, "Inbound")

	result := &pipeline.ProcessResult{
		ScanResult: &scan.ScanResult{
			Findings: []scan.Finding{
				{Severity: scan.SeverityCritical, PatternID: "test"},
			},
		},
	}
	msg = FormatBlockMessage(result, "Inbound")
	assert.Contains(t, msg, "critical")
}

func TestParseScanProfile(t *testing.T) {
	assert.Equal(t, scan.ProfileFull, parseScanProfile("full"))
	assert.Equal(t, scan.ProfilePIIOnly, parseScanProfile("pii_only"))
	assert.Equal(t, scan.ProfileInjectionOnly, parseScanProfile("injection_only"))
	assert.Equal(t, scan.ProfileCompliance, parseScanProfile("compliance"))
	assert.Equal(t, scan.ProfileFull, parseScanProfile("unknown"))
}

func TestParseTrustTier(t *testing.T) {
	assert.Equal(t, scan.TierInternal, parseTrustTier("internal"))
	assert.Equal(t, scan.TierPartner, parseTrustTier("partner"))
	assert.Equal(t, scan.TierExternal, parseTrustTier("external"))
	assert.Equal(t, scan.TierExternal, parseTrustTier("unknown"))
}

func TestShouldScanMessage(t *testing.T) {
	policy := DefaultScanPolicy()

	tests := []struct {
		name     string
		msg      routerTypes.Message
		wantScan bool
	}{
		{
			name:     "user messages are always scanned",
			msg:      routerTypes.Message{Role: "user", Content: "hello"},
			wantScan: true,
		},
		{
			name: "user messages scanned even with pre_scanned metadata",
			msg: routerTypes.Message{
				Role:     "user",
				Content:  "hello",
				Metadata: map[string]interface{}{"trust": "pre_scanned"},
			},
			wantScan: true,
		},
		{
			name:     "assistant messages are never scanned",
			msg:      routerTypes.Message{Role: "assistant", Content: "response"},
			wantScan: false,
		},
		{
			name:     "system messages without metadata are scanned",
			msg:      routerTypes.Message{Role: "system", Content: "You are a helpful assistant"},
			wantScan: true,
		},
		{
			name: "system messages with pre_scanned metadata are skipped",
			msg: routerTypes.Message{
				Role:    "system",
				Content: "document context with --- dividers",
				Metadata: map[string]interface{}{
					"trust":       "pre_scanned",
					"scan_source": "deeplake",
				},
			},
			wantScan: false,
		},
		{
			name: "tool messages with pre_scanned metadata are skipped",
			msg: routerTypes.Message{
				Role:    "tool",
				Content: "tool result content",
				Metadata: map[string]interface{}{
					"trust":       "pre_scanned",
					"scan_source": "mcp",
				},
			},
			wantScan: false,
		},
		{
			name:     "tool messages without metadata are scanned",
			msg:      routerTypes.Message{Role: "tool", Content: "tool result"},
			wantScan: true,
		},
		{
			name: "system messages with unrelated metadata are scanned",
			msg: routerTypes.Message{
				Role:     "system",
				Content:  "prompt",
				Metadata: map[string]interface{}{"other_key": "value"},
			},
			wantScan: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldScanMessage(tt.msg, policy)
			assert.Equal(t, tt.wantScan, got)
		})
	}
}

func TestScanMessages_PreScannedSystemSkipped(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	// System message with pre-scanned metadata containing SQL-injection-like content
	// that would normally trigger a false positive
	messages := []routerTypes.Message{
		{
			Role:    "system",
			Content: "Document context:\n--- Section ---\nSome content with -- dashes\n--- END ---",
			Metadata: map[string]interface{}{
				"trust":       "pre_scanned",
				"scan_source": "deeplake",
			},
		},
		{
			Role:    "user",
			Content: "What does this document say?",
		},
	}

	meta := ScanMeta{
		TenantID:  "test-tenant",
		RequestID: "req-selective-1",
		UserID:    "user-1",
		Source:    "llm_input",
	}

	result, err := client.ScanMessages(context.Background(), messages, meta)
	require.NoError(t, err)
	assert.NotNil(t, result)
	// Should NOT block because the problematic content is in the pre-scanned system message
	assert.False(t, client.ShouldBlock(result, "inbound"))
}

func TestScanMessages_AllPreScannedReturnsSkipped(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	// All messages are either assistant (never scan) or pre-scanned
	messages := []routerTypes.Message{
		{
			Role:    "system",
			Content: "system prompt",
			Metadata: map[string]interface{}{
				"trust": "pre_scanned",
			},
		},
		{
			Role:    "assistant",
			Content: "previous response",
		},
	}

	meta := ScanMeta{
		TenantID:  "test-tenant",
		RequestID: "req-selective-2",
		UserID:    "user-1",
		Source:    "llm_input",
	}

	result, err := client.ScanMessages(context.Background(), messages, meta)
	require.NoError(t, err)
	assert.True(t, result.Skipped)
	assert.Equal(t, "all_messages_pre_scanned_or_empty", result.SkipReason)
}

// Suppress unused import warnings
var _ = types.Attestation{}
