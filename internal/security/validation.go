package security

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sirupsen/logrus"
)

// ValidationConfig holds request validation configuration
type ValidationConfig struct {
	MaxRequestSize    int64             `yaml:"max_request_size"`
	AllowedMethods    []string          `yaml:"allowed_methods"`
	RequiredHeaders   []string          `yaml:"required_headers"`
	BlockedPatterns   []string          `yaml:"blocked_patterns"`
	ContentTypes      []string          `yaml:"allowed_content_types"`
	MaxJSONDepth      int               `yaml:"max_json_depth"`
	MaxFieldLength    int               `yaml:"max_field_length"`
	IPWhitelist       []string          `yaml:"ip_whitelist"`
	IPBlacklist       []string          `yaml:"ip_blacklist"`
	UserAgentPatterns []string          `yaml:"user_agent_patterns"`
}

// RequestValidator handles request validation and sanitization
type RequestValidator struct {
	config         *ValidationConfig
	logger         *logrus.Logger
	blockedRegexes []*regexp.Regexp
	uaRegexes      []*regexp.Regexp
}

// ValidationResult contains the result of request validation
type ValidationResult struct {
	Valid        bool     `json:"valid"`
	Errors       []string `json:"errors,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
	SanitizedURL string   `json:"sanitized_url,omitempty"`
}

// NewRequestValidator creates a new request validator
func NewRequestValidator(config *ValidationConfig, logger *logrus.Logger) (*RequestValidator, error) {
	if config.MaxRequestSize == 0 {
		config.MaxRequestSize = 10 * 1024 * 1024 // 10MB default
	}
	if config.MaxJSONDepth == 0 {
		config.MaxJSONDepth = 20
	}
	if config.MaxFieldLength == 0 {
		config.MaxFieldLength = 1024
	}

	validator := &RequestValidator{
		config: config,
		logger: logger,
	}

	// Compile blocked patterns
	for _, pattern := range config.BlockedPatterns {
		regex, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid blocked pattern '%s': %w", pattern, err)
		}
		validator.blockedRegexes = append(validator.blockedRegexes, regex)
	}

	// Compile user agent patterns
	for _, pattern := range config.UserAgentPatterns {
		regex, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid user agent pattern '%s': %w", pattern, err)
		}
		validator.uaRegexes = append(validator.uaRegexes, regex)
	}

	return validator, nil
}

// ValidateRequest validates an incoming HTTP request
func (v *RequestValidator) ValidateRequest(ctx context.Context, r *http.Request) (*ValidationResult, error) {
	result := &ValidationResult{
		Valid:    true,
		Errors:   []string{},
		Warnings: []string{},
	}

	// Method validation
	if !v.isAllowedMethod(r.Method) {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("Method %s not allowed", r.Method))
	}

	// Content-Length validation
	if r.ContentLength > v.config.MaxRequestSize {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("Request size %d exceeds maximum %d", r.ContentLength, v.config.MaxRequestSize))
	}

	// Content-Type validation
	if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" {
		contentType := r.Header.Get("Content-Type")
		if !v.isAllowedContentType(contentType) {
			result.Valid = false
			result.Errors = append(result.Errors, fmt.Sprintf("Content-Type %s not allowed", contentType))
		}
	}

	// Required headers validation
	for _, header := range v.config.RequiredHeaders {
		if r.Header.Get(header) == "" {
			result.Valid = false
			result.Errors = append(result.Errors, fmt.Sprintf("Required header %s missing", header))
		}
	}

	// IP validation
	clientIP := getClientIPFromRequest(r)
	if !v.isAllowedIP(clientIP) {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("IP %s not allowed", clientIP))
	}

	if v.isBlockedIP(clientIP) {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("IP %s is blocked", clientIP))
	}

	// User-Agent validation
	userAgent := r.UserAgent()
	if !v.isValidUserAgent(userAgent) {
		result.Warnings = append(result.Warnings, "Suspicious user agent detected")
	}

	// URL validation and sanitization
	sanitizedURL := v.sanitizeURL(r.URL.String())
	result.SanitizedURL = sanitizedURL

	// Check for blocked patterns in URL
	if v.containsBlockedPattern(sanitizedURL) {
		result.Valid = false
		result.Errors = append(result.Errors, "Request contains blocked patterns")
	}

	// Log validation results
	if !result.Valid {
		v.logger.WithFields(logrus.Fields{
			"method":    r.Method,
			"url":       r.URL.String(),
			"client_ip": clientIP,
			"errors":    result.Errors,
		}).Warn("Request validation failed")
	}

	return result, nil
}

// ValidateJSON validates JSON request body
func (v *RequestValidator) ValidateJSON(ctx context.Context, body []byte) (*ValidationResult, error) {
	result := &ValidationResult{
		Valid:    true,
		Errors:   []string{},
		Warnings: []string{},
	}

	// Check if body is valid UTF-8
	if !utf8.Valid(body) {
		result.Valid = false
		result.Errors = append(result.Errors, "Request body contains invalid UTF-8")
		return result, nil
	}

	// Parse JSON to validate structure
	var jsonData interface{}
	if err := json.Unmarshal(body, &jsonData); err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("Invalid JSON: %s", err.Error()))
		return result, nil
	}

	// Check JSON depth
	depth := v.getJSONDepth(jsonData)
	if depth > v.config.MaxJSONDepth {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("JSON depth %d exceeds maximum %d", depth, v.config.MaxJSONDepth))
	}

	// Check field lengths
	if err := v.validateJSONFields(jsonData); err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, err.Error())
	}

	// Check for blocked patterns in JSON content
	bodyStr := string(body)
	if v.containsBlockedPattern(bodyStr) {
		result.Valid = false
		result.Errors = append(result.Errors, "Request body contains blocked patterns")
	}

	return result, nil
}

// SanitizeInput sanitizes user input to prevent injection attacks
func (v *RequestValidator) SanitizeInput(input string) string {
	// Remove null bytes
	input = strings.ReplaceAll(input, "\x00", "")
	
	// Remove control characters except newline and tab
	var sanitized strings.Builder
	for _, r := range input {
		if r >= 32 || r == '\n' || r == '\t' {
			sanitized.WriteRune(r)
		}
	}
	
	return sanitized.String()
}

// ValidationMiddleware creates request validation middleware
func (v *RequestValidator) ValidationMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Validate request
			result, err := v.ValidateRequest(r.Context(), r)
			if err != nil {
				http.Error(w, "Validation error", http.StatusInternalServerError)
				return
			}

			if !result.Valid {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				
				response := map[string]interface{}{
					"error": map[string]interface{}{
						"message": "Request validation failed",
						"type":    "validation_error",
						"code":    http.StatusBadRequest,
						"details": result.Errors,
					},
					"timestamp": time.Now().Unix(),
				}
				
				json.NewEncoder(w).Encode(response)
				return
			}

			// Add validation warnings to response headers if any
			if len(result.Warnings) > 0 {
				w.Header().Set("X-Validation-Warnings", strings.Join(result.Warnings, "; "))
			}

			next.ServeHTTP(w, r)
		})
	}
}

// Helper methods

func (v *RequestValidator) isAllowedMethod(method string) bool {
	if len(v.config.AllowedMethods) == 0 {
		return true // Allow all if none specified
	}
	
	for _, allowed := range v.config.AllowedMethods {
		if strings.EqualFold(method, allowed) {
			return true
		}
	}
	return false
}

func (v *RequestValidator) isAllowedContentType(contentType string) bool {
	if len(v.config.ContentTypes) == 0 {
		return true // Allow all if none specified
	}
	
	// Extract main content type (ignore charset, etc.)
	mainType := strings.Split(contentType, ";")[0]
	mainType = strings.TrimSpace(mainType)
	
	for _, allowed := range v.config.ContentTypes {
		if strings.EqualFold(mainType, allowed) {
			return true
		}
	}
	return false
}

func (v *RequestValidator) isAllowedIP(ip string) bool {
	if len(v.config.IPWhitelist) == 0 {
		return true // Allow all if no whitelist
	}
	
	for _, allowed := range v.config.IPWhitelist {
		if v.matchesIPPattern(ip, allowed) {
			return true
		}
	}
	return false
}

func (v *RequestValidator) isBlockedIP(ip string) bool {
	for _, blocked := range v.config.IPBlacklist {
		if v.matchesIPPattern(ip, blocked) {
			return true
		}
	}
	return false
}

func (v *RequestValidator) matchesIPPattern(ip, pattern string) bool {
	// Simple IP matching (in production, use proper CIDR matching)
	if ip == pattern {
		return true
	}
	
	// Check for CIDR notation
	if strings.Contains(pattern, "/") {
		// This is a simplified check - use net.ParseCIDR in production
		parts := strings.Split(pattern, "/")
		if len(parts) == 2 {
			return strings.HasPrefix(ip, parts[0][:strings.LastIndex(parts[0], ".")])
		}
	}
	
	return false
}

func (v *RequestValidator) isValidUserAgent(userAgent string) bool {
	if len(v.uaRegexes) == 0 {
		return true // No patterns means all are valid
	}
	
	for _, regex := range v.uaRegexes {
		if regex.MatchString(userAgent) {
			return true
		}
	}
	return false
}

func (v *RequestValidator) containsBlockedPattern(text string) bool {
	for _, regex := range v.blockedRegexes {
		if regex.MatchString(text) {
			return true
		}
	}
	return false
}

func (v *RequestValidator) sanitizeURL(url string) string {
	// Basic URL sanitization
	url = strings.TrimSpace(url)
	
	// Remove dangerous URL schemes
	dangerousSchemes := []string{"javascript:", "data:", "vbscript:", "file:"}
	lowerURL := strings.ToLower(url)
	
	for _, scheme := range dangerousSchemes {
		if strings.HasPrefix(lowerURL, scheme) {
			return ""
		}
	}
	
	return url
}

func (rv *RequestValidator) getJSONDepth(data interface{}) int {
	switch d := data.(type) {
	case map[string]interface{}:
		maxDepth := 0
		for _, value := range d {
			depth := rv.getJSONDepth(value)
			if depth > maxDepth {
				maxDepth = depth
			}
		}
		return maxDepth + 1
	case []interface{}:
		maxDepth := 0
		for _, value := range d {
			depth := rv.getJSONDepth(value)
			if depth > maxDepth {
				maxDepth = depth
			}
		}
		return maxDepth + 1
	default:
		return 1
	}
}

func (rv *RequestValidator) validateJSONFields(data interface{}) error {
	switch d := data.(type) {
	case map[string]interface{}:
		for key, value := range d {
			if len(key) > rv.config.MaxFieldLength {
				if len(key) > 50 {
					return fmt.Errorf("field key length exceeds maximum: %s", key[:50]+"...")
				}
				return fmt.Errorf("field key length exceeds maximum: %s", key+"...")
			}
			if err := rv.validateJSONFields(value); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, value := range d {
			if err := rv.validateJSONFields(value); err != nil {
				return err
			}
		}
	case string:
		if len(d) > rv.config.MaxFieldLength {
			if len(d) > 50 {
				return fmt.Errorf("string field length exceeds maximum: %s", d[:50]+"...")
			}
			return fmt.Errorf("string field length exceeds maximum: %s", d+"...")
		}
	}
	return nil
}