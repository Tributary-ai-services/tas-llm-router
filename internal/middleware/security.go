package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/tributary-ai/llm-router-waf/internal/security"
)

// SecurityMiddlewareConfig holds configuration for security middleware
type SecurityMiddlewareConfig struct {
	Auth       *security.Config           `yaml:"auth"`
	RateLimit  *security.RateLimitConfig  `yaml:"rate_limit"`
	Validation *security.ValidationConfig `yaml:"validation"`
	Audit      *security.AuditConfig      `yaml:"audit"`
}

// SecurityMiddleware combines all security middleware components
type SecurityMiddleware struct {
	authProvider    *security.DefaultAuthProvider
	rateLimiter     security.RateLimiter
	validator       *security.RequestValidator
	auditor         *security.AuditLogger
	logger          *logrus.Logger
}

// NewSecurityMiddleware creates a new security middleware stack
func NewSecurityMiddleware(config *SecurityMiddlewareConfig, logger *logrus.Logger) (*SecurityMiddleware, error) {
	// Initialize authentication provider
	var authProvider *security.DefaultAuthProvider
	if config.Auth != nil {
		authProvider = security.NewDefaultAuthProvider(config.Auth, logger)
	}
	
	// Initialize rate limiter
	var rateLimiter security.RateLimiter
	if config.RateLimit != nil && config.RateLimit.Enabled {
		rateLimiter = security.NewInMemoryRateLimiter(config.RateLimit, logger)
	}
	
	// Initialize request validator
	var validator *security.RequestValidator
	var err error
	if config.Validation != nil {
		validator, err = security.NewRequestValidator(config.Validation, logger)
		if err != nil {
			return nil, err
		}
	}
	
	// Initialize audit logger
	var auditor *security.AuditLogger
	if config.Audit != nil {
		auditor = security.NewAuditLogger(config.Audit, logger)
	}
	
	return &SecurityMiddleware{
		authProvider: authProvider,
		rateLimiter:  rateLimiter,
		validator:    validator,
		auditor:      auditor,
		logger:       logger,
	}, nil
}

// Handler creates the complete security middleware chain
func (s *SecurityMiddleware) Handler() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		// Build middleware chain in reverse order (innermost first)
		handler := next
		
		// 1. Audit logging (outermost - logs everything)
		if s.auditor != nil {
			handler = s.auditor.AuditMiddleware()(handler)
		}
		
		// 2. Authentication (before rate limiting to identify users)
		if s.authProvider != nil {
			handler = s.authProvider.AuthMiddleware()(handler)
		}
		
		// 3. Rate limiting (after auth to use user-based limits)
		if s.rateLimiter != nil {
			keyExtractor := security.DefaultKeyExtractor
			handler = security.RateLimitMiddleware(s.rateLimiter, keyExtractor)(handler)
		}
		
		// 4. Request validation (innermost - validates each request)
		if s.validator != nil {
			handler = s.validator.ValidationMiddleware()(handler)
		}
		
		// 5. Security headers (add security headers to all responses)
		handler = s.securityHeadersMiddleware()(handler)
		
		return handler
	}
}

// AuthenticationOnly returns only the authentication middleware
func (s *SecurityMiddleware) AuthenticationOnly() func(http.Handler) http.Handler {
	if s.authProvider != nil {
		return s.authProvider.AuthMiddleware()
	}
	return func(next http.Handler) http.Handler { return next }
}

// RateLimitingOnly returns only the rate limiting middleware
func (s *SecurityMiddleware) RateLimitingOnly() func(http.Handler) http.Handler {
	if s.rateLimiter != nil {
		keyExtractor := security.DefaultKeyExtractor
		return security.RateLimitMiddleware(s.rateLimiter, keyExtractor)
	}
	return func(next http.Handler) http.Handler { return next }
}

// ValidationOnly returns only the validation middleware
func (s *SecurityMiddleware) ValidationOnly() func(http.Handler) http.Handler {
	if s.validator != nil {
		return s.validator.ValidationMiddleware()
	}
	return func(next http.Handler) http.Handler { return next }
}

// AuditOnly returns only the audit logging middleware
func (s *SecurityMiddleware) AuditOnly() func(http.Handler) http.Handler {
	if s.auditor != nil {
		return s.auditor.AuditMiddleware()
	}
	return func(next http.Handler) http.Handler { return next }
}

// securityHeadersMiddleware adds security headers to responses
func (s *SecurityMiddleware) securityHeadersMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Security headers
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("X-XSS-Protection", "1; mode=block")
			w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
			w.Header().Set("Content-Security-Policy", "default-src 'self'")
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			
			// Remove server information
			w.Header().Del("Server")
			w.Header().Set("Server", "LLM-Router-WAF/1.0")
			
			// Add custom security headers
			w.Header().Set("X-API-Version", "1.0")
			w.Header().Set("X-Request-ID", r.Header.Get("X-Request-ID"))
			
			next.ServeHTTP(w, r)
		})
	}
}

// Stop gracefully stops all middleware components
func (s *SecurityMiddleware) Stop() {
	if s.auditor != nil {
		s.auditor.Stop()
	}
	
	if rateLimiter, ok := s.rateLimiter.(*security.InMemoryRateLimiter); ok {
		rateLimiter.Stop()
	}
}

// GetStats returns security middleware statistics
func (s *SecurityMiddleware) GetStats() map[string]interface{} {
	stats := make(map[string]interface{})
	
	// Add audit stats
	if s.auditor != nil {
		stats["audit_events_logged"] = s.auditor.GetEventCount()
	}
	
	// Add rate limiter stats (would need to implement this in rate limiter)
	stats["rate_limiter_enabled"] = s.rateLimiter != nil
	
	// Add validator stats
	stats["validation_enabled"] = s.validator != nil
	
	// Add auth stats
	stats["authentication_enabled"] = s.authProvider != nil
	
	return stats
}

// HealthCheck performs health checks on all security components
func (s *SecurityMiddleware) HealthCheck() error {
	// Check components are initialized
	if s.authProvider == nil {
		return fmt.Errorf("authentication provider not initialized")
	}
	
	// Additional health checks would go here
	// For example, check if external audit endpoint is reachable
	
	return nil
}

// LogSecurityEvent is a convenience method to log security events
func (s *SecurityMiddleware) LogSecurityEvent(ctx context.Context, eventType security.AuditEventType, message string, details map[string]interface{}) {
	if s.auditor != nil {
		s.auditor.LogEvent(ctx, eventType, message, details)
	}
}

// Custom middleware for specific security scenarios

// APIKeyOnlyMiddleware creates middleware that only accepts API key authentication
func (s *SecurityMiddleware) APIKeyOnlyMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract API key from headers
			apiKey := r.Header.Get("X-API-Key")
			if apiKey == "" {
				apiKey = r.Header.Get("API-Key")
			}
			
			if apiKey == "" {
				http.Error(w, "API key required", http.StatusUnauthorized)
				return
			}
			
			// Validate API key
			ctx := context.WithValue(r.Context(), "client_ip", getClientIPFromRequest(r))
			authInfo, err := s.authProvider.ValidateAPIKey(ctx, apiKey)
			if err != nil {
				s.logger.WithField("api_key_prefix", maskAPIKey(apiKey)).Warn("Invalid API key")
				http.Error(w, "Invalid API key", http.StatusUnauthorized)
				return
			}
			
			// Add auth info to context
			ctx = context.WithValue(r.Context(), "auth_info", authInfo)
			
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// JWTOnlyMiddleware creates middleware that only accepts JWT authentication
func (s *SecurityMiddleware) JWTOnlyMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract JWT from Authorization header
			authHeader := r.Header.Get("Authorization")
			if !strings.HasPrefix(authHeader, "Bearer ") {
				http.Error(w, "JWT token required", http.StatusUnauthorized)
				return
			}
			
			token := strings.TrimPrefix(authHeader, "Bearer ")
			
			// Validate JWT
			claims, err := s.authProvider.ValidateJWT(token)
			if err != nil {
				s.logger.WithError(err).Warn("Invalid JWT token")
				http.Error(w, "Invalid JWT token", http.StatusUnauthorized)
				return
			}
			
			// Create auth info from claims
			authInfo := &security.AuthInfo{
				UserID:      claims.UserID,
				Permissions: claims.Permissions,
				Metadata:    claims.Metadata,
				ExpiresAt:   &claims.ExpiresAt.Time,
			}
			
			// Add auth info to context
			ctx := context.WithValue(r.Context(), "auth_info", authInfo)
			
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// CORSMiddleware creates CORS middleware for cross-origin requests
func (s *SecurityMiddleware) CORSMiddleware(allowedOrigins []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			
			// Check if origin is allowed
			allowed := false
			for _, allowedOrigin := range allowedOrigins {
				if allowedOrigin == "*" || allowedOrigin == origin {
					allowed = true
					break
				}
			}
			
			if allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}
			
			// Handle preflight OPTIONS requests
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			
			next.ServeHTTP(w, r)
		})
	}
}

// Helper functions

func getClientIPFromRequest(r *http.Request) string {
	// Check X-Forwarded-For header
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		return strings.TrimSpace(ips[0])
	}
	
	// Check X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	
	// Fall back to RemoteAddr
	ip := r.RemoteAddr
	if colonIndex := strings.LastIndex(ip, ":"); colonIndex != -1 {
		ip = ip[:colonIndex]
	}
	
	return ip
}

func maskAPIKey(apiKey string) string {
	if len(apiKey) <= 8 {
		return "****"
	}
	return apiKey[:4] + "****" + apiKey[len(apiKey)-4:]
}