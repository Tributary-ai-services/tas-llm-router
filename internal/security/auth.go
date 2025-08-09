package security

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/sirupsen/logrus"
)

// AuthProvider defines the interface for authentication providers
type AuthProvider interface {
	Authenticate(ctx context.Context, token string) (*AuthInfo, error)
	ValidateAPIKey(ctx context.Context, apiKey string) (*AuthInfo, error)
	GenerateJWT(userID string, claims map[string]interface{}) (string, error)
	ValidateJWT(tokenString string) (*JWTClaims, error)
}

// AuthInfo contains authenticated user information
type AuthInfo struct {
	UserID      string            `json:"user_id"`
	APIKey      string            `json:"api_key,omitempty"`
	Permissions []string          `json:"permissions"`
	Metadata    map[string]string `json:"metadata"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
}

// JWTClaims represents JWT token claims
type JWTClaims struct {
	UserID      string            `json:"user_id"`
	Permissions []string          `json:"permissions"`
	Metadata    map[string]string `json:"metadata"`
	jwt.RegisteredClaims
}

// Config holds authentication configuration
type Config struct {
	APIKeys          []string      `yaml:"api_keys"`
	JWTSecret        string        `yaml:"jwt_secret"`
	JWTExpiry        time.Duration `yaml:"jwt_expiry"`
	RequireAuth      bool          `yaml:"require_auth"`
	AllowedOrigins   []string      `yaml:"allowed_origins"`
	TrustedProxies   []string      `yaml:"trusted_proxies"`
}

// DefaultAuthProvider implements the AuthProvider interface
type DefaultAuthProvider struct {
	config *Config
	logger *logrus.Logger
}

// NewDefaultAuthProvider creates a new authentication provider
func NewDefaultAuthProvider(config *Config, logger *logrus.Logger) *DefaultAuthProvider {
	if config.JWTExpiry == 0 {
		config.JWTExpiry = 24 * time.Hour
	}
	
	return &DefaultAuthProvider{
		config: config,
		logger: logger,
	}
}

// Authenticate validates a token (API key or JWT)
func (a *DefaultAuthProvider) Authenticate(ctx context.Context, token string) (*AuthInfo, error) {
	// Try API key first
	if authInfo, err := a.ValidateAPIKey(ctx, token); err == nil {
		return authInfo, nil
	}
	
	// Try JWT token
	if claims, err := a.ValidateJWT(token); err == nil {
		return &AuthInfo{
			UserID:      claims.UserID,
			Permissions: claims.Permissions,
			Metadata:    claims.Metadata,
			ExpiresAt:   &claims.ExpiresAt.Time,
		}, nil
	}
	
	return nil, errors.New("invalid authentication token")
}

// ValidateAPIKey validates an API key
func (a *DefaultAuthProvider) ValidateAPIKey(ctx context.Context, apiKey string) (*AuthInfo, error) {
	if apiKey == "" {
		return nil, errors.New("API key is required")
	}
	
	// Use constant-time comparison to prevent timing attacks
	for i, validKey := range a.config.APIKeys {
		if subtle.ConstantTimeCompare([]byte(apiKey), []byte(validKey)) == 1 {
			return &AuthInfo{
				UserID:      generateUserID(apiKey),
				APIKey:      apiKey,
				Permissions: []string{"api:access"},
				Metadata: map[string]string{
					"key_index": string(rune(i)),
					"auth_type": "api_key",
				},
			}, nil
		}
	}
	
	a.logger.WithFields(logrus.Fields{
		"api_key_prefix": maskAPIKey(apiKey),
		"remote_ip":      getClientIP(ctx),
	}).Warn("Invalid API key attempted")
	
	return nil, errors.New("invalid API key")
}

// GenerateJWT generates a new JWT token
func (a *DefaultAuthProvider) GenerateJWT(userID string, claims map[string]interface{}) (string, error) {
	now := time.Now()
	
	jwtClaims := &JWTClaims{
		UserID: userID,
		Metadata: make(map[string]string),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "llm-router-waf",
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(a.config.JWTExpiry)),
			NotBefore: jwt.NewNumericDate(now),
		},
	}
	
	// Add custom claims
	for key, value := range claims {
		switch key {
		case "permissions":
			if perms, ok := value.([]string); ok {
				jwtClaims.Permissions = perms
			}
		default:
			if strVal, ok := value.(string); ok {
				jwtClaims.Metadata[key] = strVal
			}
		}
	}
	
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwtClaims)
	return token.SignedString([]byte(a.config.JWTSecret))
}

// ValidateJWT validates a JWT token
func (a *DefaultAuthProvider) ValidateJWT(tokenString string) (*JWTClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &JWTClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(a.config.JWTSecret), nil
	})
	
	if err != nil {
		return nil, err
	}
	
	if claims, ok := token.Claims.(*JWTClaims); ok && token.Valid {
		return claims, nil
	}
	
	return nil, errors.New("invalid JWT token")
}

// AuthMiddleware creates authentication middleware
func (a *DefaultAuthProvider) AuthMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip auth for health check endpoints
			if strings.HasPrefix(r.URL.Path, "/health") {
				next.ServeHTTP(w, r)
				return
			}
			
			// Skip auth if not required
			if !a.config.RequireAuth {
				next.ServeHTTP(w, r)
				return
			}
			
			// Extract token from Authorization header or API-Key header
			token := extractToken(r)
			if token == "" {
				a.writeUnauthorized(w, "Missing authentication token")
				return
			}
			
			// Authenticate token
			ctx := context.WithValue(r.Context(), "client_ip", getClientIPFromRequest(r))
			authInfo, err := a.Authenticate(ctx, token)
			if err != nil {
				a.logger.WithFields(logrus.Fields{
					"error":     err.Error(),
					"path":      r.URL.Path,
					"method":    r.Method,
					"remote_ip": getClientIPFromRequest(r),
					"user_agent": r.UserAgent(),
				}).Warn("Authentication failed")
				
				a.writeUnauthorized(w, "Invalid authentication token")
				return
			}
			
			// Add auth info to request context
			ctx = context.WithValue(r.Context(), "auth_info", authInfo)
			
			// Log successful authentication
			a.logger.WithFields(logrus.Fields{
				"user_id":    authInfo.UserID,
				"auth_type":  authInfo.Metadata["auth_type"],
				"path":       r.URL.Path,
				"method":     r.Method,
				"remote_ip":  getClientIPFromRequest(r),
			}).Debug("Authentication successful")
			
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Helper functions

func extractToken(r *http.Request) string {
	// Try Authorization header first (Bearer token)
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer ")
	}
	
	// Try API-Key header
	if apiKey := r.Header.Get("X-API-Key"); apiKey != "" {
		return apiKey
	}
	
	// Try API-Key header (alternative)
	if apiKey := r.Header.Get("API-Key"); apiKey != "" {
		return apiKey
	}
	
	return ""
}

func generateUserID(apiKey string) string {
	// Generate a consistent user ID from API key (first 8 chars + hash)
	if len(apiKey) >= 8 {
		return "user_" + apiKey[:8]
	}
	return "user_" + apiKey
}

func maskAPIKey(apiKey string) string {
	if len(apiKey) <= 8 {
		return "****"
	}
	return apiKey[:4] + "****" + apiKey[len(apiKey)-4:]
}

func getClientIP(ctx context.Context) string {
	if ip, ok := ctx.Value("client_ip").(string); ok {
		return ip
	}
	return "unknown"
}

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

func (a *DefaultAuthProvider) writeUnauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	
	// Simple JSON response without using the json package to keep it lightweight
	timestamp := time.Now().Unix()
	response := fmt.Sprintf(`{"error":{"message":"%s","type":"authentication_error","code":401},"timestamp":%d}`, message, timestamp)
	w.Write([]byte(response))
}

// GetAuthInfo extracts authentication info from request context
func GetAuthInfo(ctx context.Context) (*AuthInfo, bool) {
	if authInfo, ok := ctx.Value("auth_info").(*AuthInfo); ok {
		return authInfo, true
	}
	return nil, false
}