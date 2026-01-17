package security

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func TestNewAuditLogger(t *testing.T) {
	config := &AuditConfig{
		Enabled:       true,
		BufferSize:    100,
		FlushInterval: 5 * time.Second,
	}
	logger := logrus.New()

	auditor := NewAuditLogger(config, logger)

	assert.NotNil(t, auditor)
	assert.Equal(t, config, auditor.config)
	assert.Equal(t, logger, auditor.logger)
	assert.NotNil(t, auditor.buffer)
	assert.NotNil(t, auditor.stopChan)

	// Clean up
	auditor.Stop()
}

func TestNewAuditLogger_WithDefaults(t *testing.T) {
	config := &AuditConfig{
		Enabled: true,
		// Leave other fields empty to test defaults
	}
	logger := logrus.New()

	auditor := NewAuditLogger(config, logger)

	assert.Equal(t, 1000, auditor.config.BufferSize)
	assert.Equal(t, 10*time.Second, auditor.config.FlushInterval)
	assert.Equal(t, int64(100*1024*1024), auditor.config.MaxFileSize)
	assert.Equal(t, 10, auditor.config.MaxFiles)

	// Clean up
	auditor.Stop()
}

func TestAuditLogger_LogEvent_Disabled(t *testing.T) {
	config := &AuditConfig{
		Enabled: false,
	}
	logger := logrus.New()
	auditor := NewAuditLogger(config, logger)

	ctx := context.Background()
	details := map[string]interface{}{"key": "value"}

	// Should not panic or block when disabled
	auditor.LogEvent(ctx, AuthenticationSuccess, "test message", details)

	// Event count should remain 0
	assert.Equal(t, int64(0), auditor.GetEventCount())
}

func TestAuditLogger_LogEvent_WithContext(t *testing.T) {
	config := &AuditConfig{
		Enabled:       true,
		BufferSize:    10,
		FlushInterval: 1 * time.Second,
	}
	logger := logrus.New()
	auditor := NewAuditLogger(config, logger)
	defer auditor.Stop()

	// Create context with various values
	ctx := context.WithValue(context.Background(), "request_id", "req-123")
	ctx = context.WithValue(ctx, "client_ip", "192.168.1.100")
	authInfo := &AuthInfo{UserID: "user-123"}
	ctx = context.WithValue(ctx, "auth_info", authInfo)

	details := map[string]interface{}{
		"action": "login",
		"result": "success",
	}

	auditor.LogEvent(ctx, AuthenticationSuccess, "User logged in", details)

	// Wait a moment for async processing
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, int64(1), auditor.GetEventCount())
}

func TestAuditLogger_LogAuthenticationAttempt(t *testing.T) {
	config := &AuditConfig{
		Enabled:       true,
		BufferSize:    10,
		FlushInterval: 1 * time.Second,
	}
	logger := logrus.New()
	auditor := NewAuditLogger(config, logger)
	defer auditor.Stop()

	ctx := context.Background()

	// Test successful authentication
	auditor.LogAuthenticationAttempt(ctx, "user123", "api_key", true, nil)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(1), auditor.GetEventCount())

	// Test failed authentication
	auditor.LogAuthenticationAttempt(ctx, "user123", "api_key", false, map[string]interface{}{
		"reason": "invalid_key",
	})
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(2), auditor.GetEventCount())
}

func TestAuditLogger_LogAPIKeyUsage(t *testing.T) {
	config := &AuditConfig{
		Enabled:       true,
		BufferSize:    10,
		FlushInterval: 1 * time.Second,
	}
	logger := logrus.New()
	auditor := NewAuditLogger(config, logger)
	defer auditor.Stop()

	ctx := context.Background()

	auditor.LogAPIKeyUsage(ctx, "sk-1234567890abcdef", "/v1/chat/completions", 200)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(1), auditor.GetEventCount())
}

func TestAuditLogger_LogSecurityViolation(t *testing.T) {
	config := &AuditConfig{
		Enabled:       true,
		BufferSize:    10,
		FlushInterval: 1 * time.Second,
	}
	logger := logrus.New()
	auditor := NewAuditLogger(config, logger)
	defer auditor.Stop()

	ctx := context.Background()

	auditor.LogSecurityViolation(ctx, "xss_attempt", "Script tag detected", map[string]interface{}{
		"blocked_content": "<script>alert(1)</script>",
	})
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(1), auditor.GetEventCount())
}

func TestAuditLogger_LogSuspiciousActivity(t *testing.T) {
	config := &AuditConfig{
		Enabled:       true,
		BufferSize:    10,
		FlushInterval: 1 * time.Second,
	}
	logger := logrus.New()
	auditor := NewAuditLogger(config, logger)
	defer auditor.Stop()

	ctx := context.Background()

	auditor.LogSuspiciousActivity(ctx, "brute_force", "Multiple failed login attempts", map[string]interface{}{
		"attempt_count": 5,
		"time_window":   "1 minute",
	})
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(1), auditor.GetEventCount())
}

func TestAuditLogger_SanitizeDetails(t *testing.T) {
	config := &AuditConfig{
		Enabled: true,
		SensitiveFields: []string{"custom_secret"},
	}
	logger := logrus.New()
	auditor := NewAuditLogger(config, logger)
	defer auditor.Stop()

	details := map[string]interface{}{
		"user":          "john",
		"password":      "secret123",
		"token":         "abc123",
		"custom_secret": "sensitive_data",
		"safe_field":    "public_data",
	}

	sanitized := auditor.sanitizeDetails(details)

	assert.Equal(t, "john", sanitized["user"])
	assert.Equal(t, "***REDACTED***", sanitized["password"])
	assert.Equal(t, "***REDACTED***", sanitized["token"])
	assert.Equal(t, "***REDACTED***", sanitized["custom_secret"])
	assert.Equal(t, "public_data", sanitized["safe_field"])
}

func TestAuditLogger_IsSensitiveField(t *testing.T) {
	config := &AuditConfig{
		SensitiveFields: []string{"custom_field"},
	}
	logger := logrus.New()
	auditor := NewAuditLogger(config, logger)

	tests := []struct {
		field    string
		expected bool
	}{
		{"password", true},
		{"token", true},
		{"secret", true},
		{"key", true},
		{"authorization", true},
		{"x-api-key", true},
		{"custom_field", true},
		{"CUSTOM_FIELD", true}, // Case insensitive
		{"username", false},
		{"data", false},
	}

	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			result := auditor.isSensitiveField(tt.field)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestAuditLogger_GetSeverity(t *testing.T) {
	config := &AuditConfig{Enabled: true}
	logger := logrus.New()
	auditor := NewAuditLogger(config, logger)

	tests := []struct {
		eventType AuditEventType
		expected  string
	}{
		{SecurityViolation, "critical"},
		{UnauthorizedAccess, "critical"},
		{AuthenticationFailure, "high"},
		{AuthorizationFailure, "high"},
		{SuspiciousActivity, "high"},
		{RateLimitExceeded, "medium"},
		{ValidationFailure, "medium"},
		{AuthenticationSuccess, "low"},
		{APIKeyUsage, "low"},
	}

	for _, tt := range tests {
		t.Run(string(tt.eventType), func(t *testing.T) {
			result := auditor.getSeverity(tt.eventType)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestAuditLogger_BufferOverflow(t *testing.T) {
	config := &AuditConfig{
		Enabled:       true,
		BufferSize:    2, // Very small buffer
		FlushInterval: 1 * time.Second,
	}
	logger := logrus.New()
	auditor := NewAuditLogger(config, logger)
	defer auditor.Stop()

	ctx := context.Background()

	// Fill the buffer beyond capacity
	for i := 0; i < 5; i++ {
		auditor.LogEvent(ctx, AuthenticationSuccess, "test event", nil)
	}

	// Should not hang or crash, but some events may be dropped
	time.Sleep(100 * time.Millisecond)
	
	// The exact count may vary due to async processing and buffer overflow
	count := auditor.GetEventCount()
	assert.LessOrEqual(t, count, int64(5))
}

func TestAuditLogger_Stop(t *testing.T) {
	config := &AuditConfig{
		Enabled:       true,
		BufferSize:    10,
		FlushInterval: 100 * time.Millisecond,
	}
	logger := logrus.New()
	auditor := NewAuditLogger(config, logger)

	ctx := context.Background()

	// Add some events
	auditor.LogEvent(ctx, AuthenticationSuccess, "test event 1", nil)
	auditor.LogEvent(ctx, AuthenticationSuccess, "test event 2", nil)

	// Wait a bit to ensure events are processed
	time.Sleep(50 * time.Millisecond)

	// Stop should not hang and should flush remaining events
	auditor.Stop()

	// After stop, we don't test logging as it may panic (expected behavior)
	// The test just verifies that Stop() completes successfully
}

func TestGenerateEventID(t *testing.T) {
	id1 := generateEventID()
	id2 := generateEventID()

	assert.NotEmpty(t, id1)
	assert.NotEmpty(t, id2)
	assert.NotEqual(t, id1, id2)
	assert.True(t, strings.HasPrefix(id1, "audit_"))
	assert.True(t, strings.HasPrefix(id2, "audit_"))
}

func TestGenerateRequestID(t *testing.T) {
	id1 := generateRequestID()
	id2 := generateRequestID()

	assert.NotEmpty(t, id1)
	assert.NotEmpty(t, id2)
	assert.NotEqual(t, id1, id2)
	assert.True(t, strings.HasPrefix(id1, "req_"))
	assert.True(t, strings.HasPrefix(id2, "req_"))
}

func TestResponseWriterWrapper(t *testing.T) {
	// Create a proper ResponseWriter for the wrapper
	w := httptest.NewRecorder()
	recorder := &responseWriterWrapper{
		ResponseWriter: w,
		statusCode:     200,
	}

	assert.Equal(t, 200, recorder.statusCode)

	// Test WriteHeader
	recorder.WriteHeader(404)
	assert.Equal(t, 404, recorder.statusCode)
}