package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tributary-ai/llm-router-waf/internal/security"
)

func TestNewSecurityMiddleware(t *testing.T) {
	config := &SecurityMiddlewareConfig{
		Auth: &security.Config{
			APIKeys:     []string{"test-key"},
			RequireAuth: true,
		},
		RateLimit: &security.RateLimitConfig{
			Enabled:           true,
			RequestsPerMinute: 60,
			BurstSize:         10,
		},
		Validation: &security.ValidationConfig{
			MaxRequestSize: 1024,
			AllowedMethods: []string{"GET", "POST"},
		},
		Audit: &security.AuditConfig{
			Enabled: true,
		},
	}
	logger := logrus.New()

	middleware, err := NewSecurityMiddleware(config, logger)
	require.NoError(t, err)
	assert.NotNil(t, middleware)
	assert.NotNil(t, middleware.authProvider)
	assert.NotNil(t, middleware.rateLimiter)
	assert.NotNil(t, middleware.validator)
	assert.NotNil(t, middleware.auditor)

	// Clean up
	middleware.Stop()
}

func TestNewSecurityMiddleware_ValidationError(t *testing.T) {
	config := &SecurityMiddlewareConfig{
		Auth: &security.Config{
			APIKeys: []string{"test"},
		},
		Validation: &security.ValidationConfig{
			BlockedPatterns: []string{"[invalid regex"},
		},
	}
	logger := logrus.New()

	middleware, err := NewSecurityMiddleware(config, logger)
	assert.Error(t, err)
	assert.Nil(t, middleware)
	assert.Contains(t, err.Error(), "invalid blocked pattern")
}

func TestSecurityMiddleware_Handler(t *testing.T) {
	config := &SecurityMiddlewareConfig{
		Auth: &security.Config{
			APIKeys:     []string{"valid-key"},
			RequireAuth: false, // Disable for easier testing
		},
		RateLimit: &security.RateLimitConfig{
			Enabled: false, // Disable for easier testing
		},
		Validation: &security.ValidationConfig{
			AllowedMethods: []string{"GET", "POST"},
		},
		Audit: &security.AuditConfig{
			Enabled: true,
		},
	}
	logger := logrus.New()
	middleware, err := NewSecurityMiddleware(config, logger)
	require.NoError(t, err)
	defer middleware.Stop()

	// Create test handler
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	// Wrap with security middleware
	handler := middleware.Handler()(testHandler)

	// Test valid request
	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "success", w.Body.String())

	// Check security headers were added
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
	assert.Equal(t, "LLM-Router-WAF/1.0", w.Header().Get("Server"))
}

func TestSecurityMiddleware_Handler_InvalidMethod(t *testing.T) {
	config := &SecurityMiddlewareConfig{
		Auth: &security.Config{
			RequireAuth: false,
		},
		RateLimit: &security.RateLimitConfig{
			Enabled: false,
		},
		Validation: &security.ValidationConfig{
			AllowedMethods: []string{"GET", "POST"},
		},
		Audit: &security.AuditConfig{
			Enabled: false, // Disable for cleaner test
		},
	}
	logger := logrus.New()
	middleware, err := NewSecurityMiddleware(config, logger)
	require.NoError(t, err)
	defer middleware.Stop()

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	handler := middleware.Handler()(testHandler)

	// Test invalid method
	req := httptest.NewRequest("DELETE", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "validation_error")
}

func TestSecurityMiddleware_AuthenticationOnly(t *testing.T) {
	config := &SecurityMiddlewareConfig{
		Auth: &security.Config{
			APIKeys:     []string{"valid-key"},
			RequireAuth: true,
		},
	}
	logger := logrus.New()
	middleware, err := NewSecurityMiddleware(config, logger)
	require.NoError(t, err)
	defer middleware.Stop()

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("authenticated"))
	})

	handler := middleware.AuthenticationOnly()(testHandler)

	// Test without API key
	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Test with valid API key
	req = httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-API-Key", "valid-key")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "authenticated", w.Body.String())
}

func TestSecurityMiddleware_RateLimitingOnly(t *testing.T) {
	config := &SecurityMiddlewareConfig{
		RateLimit: &security.RateLimitConfig{
			Enabled:           true,
			RequestsPerMinute: 2, // Very low for easy testing
			BurstSize:         2,
		},
	}
	logger := logrus.New()
	middleware, err := NewSecurityMiddleware(config, logger)
	require.NoError(t, err)
	defer middleware.Stop()

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	handler := middleware.RateLimitingOnly()(testHandler)

	// First requests should succeed
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "192.168.1.100:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	req = httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "192.168.1.100:12345"
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Third request should be rate limited
	req = httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "192.168.1.100:12345"
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), "Rate limit exceeded")
}

func TestSecurityMiddleware_ValidationOnly(t *testing.T) {
	config := &SecurityMiddlewareConfig{
		Validation: &security.ValidationConfig{
			MaxRequestSize: 100,
		},
	}
	logger := logrus.New()
	middleware, err := NewSecurityMiddleware(config, logger)
	require.NoError(t, err)
	defer middleware.Stop()

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	handler := middleware.ValidationOnly()(testHandler)

	// Valid request
	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Request too large
	req = httptest.NewRequest("POST", "/test", nil)
	req.ContentLength = 200 // Exceeds limit of 100
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSecurityMiddleware_AuditOnly(t *testing.T) {
	config := &SecurityMiddlewareConfig{
		Audit: &security.AuditConfig{
			Enabled: true,
		},
	}
	logger := logrus.New()
	middleware, err := NewSecurityMiddleware(config, logger)
	require.NoError(t, err)
	defer middleware.Stop()

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	handler := middleware.AuditOnly()(testHandler)

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "success", w.Body.String())

	// Wait for async audit logging
	time.Sleep(100 * time.Millisecond)
	
	// Verify audit event was logged
	assert.Greater(t, middleware.auditor.GetEventCount(), int64(0))
}

func TestSecurityMiddleware_GetStats(t *testing.T) {
	config := &SecurityMiddlewareConfig{
		Auth:       &security.Config{APIKeys: []string{"test"}},
		RateLimit:  &security.RateLimitConfig{Enabled: true},
		Validation: &security.ValidationConfig{},
		Audit:      &security.AuditConfig{Enabled: true},
	}
	logger := logrus.New()
	middleware, err := NewSecurityMiddleware(config, logger)
	require.NoError(t, err)
	defer middleware.Stop()

	stats := middleware.GetStats()

	assert.Contains(t, stats, "audit_events_logged")
	assert.Contains(t, stats, "rate_limiter_enabled")
	assert.Contains(t, stats, "validation_enabled")
	assert.Contains(t, stats, "authentication_enabled")
	
	assert.True(t, stats["rate_limiter_enabled"].(bool))
	assert.True(t, stats["validation_enabled"].(bool))
	assert.True(t, stats["authentication_enabled"].(bool))
}

func TestSecurityMiddleware_HealthCheck(t *testing.T) {
	config := &SecurityMiddlewareConfig{
		Auth: &security.Config{APIKeys: []string{"test"}},
	}
	logger := logrus.New()
	middleware, err := NewSecurityMiddleware(config, logger)
	require.NoError(t, err)
	defer middleware.Stop()

	err = middleware.HealthCheck()
	assert.NoError(t, err)
}

func TestSecurityMiddleware_Stop(t *testing.T) {
	config := &SecurityMiddlewareConfig{
		Auth: &security.Config{APIKeys: []string{"test"}},
		RateLimit: &security.RateLimitConfig{
			Enabled:           true,
			RequestsPerMinute: 60,
		},
		Audit: &security.AuditConfig{Enabled: true},
	}
	logger := logrus.New()
	middleware, err := NewSecurityMiddleware(config, logger)
	require.NoError(t, err)

	// Stop should not panic and should clean up resources
	middleware.Stop()
	
	// Multiple stops should be safe
	middleware.Stop()
}

func TestSecurityMiddleware_CORSMiddleware(t *testing.T) {
	config := &SecurityMiddlewareConfig{}
	logger := logrus.New()
	middleware, err := NewSecurityMiddleware(config, logger)
	require.NoError(t, err)
	defer middleware.Stop()

	allowedOrigins := []string{"https://example.com", "https://*.example.com"}
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	handler := middleware.CORSMiddleware(allowedOrigins)(testHandler)

	// Test allowed origin
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "https://example.com", w.Header().Get("Access-Control-Allow-Origin"))
	assert.NotEmpty(t, w.Header().Get("Access-Control-Allow-Methods"))

	// Test preflight request
	req = httptest.NewRequest("OPTIONS", "/test", nil)
	req.Header.Set("Origin", "https://example.com")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "https://example.com", w.Header().Get("Access-Control-Allow-Origin"))
}

func TestGetClientIPFromRequest(t *testing.T) {
	tests := []struct {
		name           string
		setupRequest   func() *http.Request
		expectedIP     string
	}{
		{
			name: "X-Forwarded-For header",
			setupRequest: func() *http.Request {
				req := httptest.NewRequest("GET", "/test", nil)
				req.Header.Set("X-Forwarded-For", "203.0.113.1, 192.168.1.1")
				req.RemoteAddr = "192.168.1.100:12345"
				return req
			},
			expectedIP: "203.0.113.1",
		},
		{
			name: "X-Real-IP header",
			setupRequest: func() *http.Request {
				req := httptest.NewRequest("GET", "/test", nil)
				req.Header.Set("X-Real-IP", "203.0.113.2")
				req.RemoteAddr = "192.168.1.100:12345"
				return req
			},
			expectedIP: "203.0.113.2",
		},
		{
			name: "RemoteAddr fallback",
			setupRequest: func() *http.Request {
				req := httptest.NewRequest("GET", "/test", nil)
				req.RemoteAddr = "192.168.1.100:12345"
				return req
			},
			expectedIP: "192.168.1.100",
		},
		{
			name: "RemoteAddr without port",
			setupRequest: func() *http.Request {
				req := httptest.NewRequest("GET", "/test", nil)
				req.RemoteAddr = "192.168.1.100"
				return req
			},
			expectedIP: "192.168.1.100",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.setupRequest()
			ip := getClientIPFromRequest(req)
			assert.Equal(t, tt.expectedIP, ip)
		})
	}
}

func TestMaskAPIKey(t *testing.T) {
	tests := []struct {
		name   string
		apiKey string
		want   string
	}{
		{
			name:   "normal API key",
			apiKey: "sk-1234567890abcdef",
			want:   "sk-1****cdef",
		},
		{
			name:   "short API key",
			apiKey: "short",
			want:   "****",
		},
		{
			name:   "exactly 8 chars",
			apiKey: "12345678",
			want:   "****",
		},
		{
			name:   "empty string",
			apiKey: "",
			want:   "****",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := maskAPIKey(tt.apiKey)
			assert.Equal(t, tt.want, result)
		})
	}
}