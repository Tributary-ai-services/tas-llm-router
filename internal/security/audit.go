package security

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// AuditEventType represents different types of security events
type AuditEventType string

const (
	AuthenticationAttempt AuditEventType = "authentication_attempt"
	AuthenticationSuccess AuditEventType = "authentication_success"
	AuthenticationFailure AuditEventType = "authentication_failure"
	AuthorizationFailure  AuditEventType = "authorization_failure"
	RateLimitExceeded     AuditEventType = "rate_limit_exceeded"
	ValidationFailure     AuditEventType = "validation_failure"
	SuspiciousActivity    AuditEventType = "suspicious_activity"
	SecurityViolation     AuditEventType = "security_violation"
	APIKeyUsage           AuditEventType = "api_key_usage"
	JWTTokenIssued        AuditEventType = "jwt_token_issued"
	JWTTokenExpired       AuditEventType = "jwt_token_expired"
	PasswordReset         AuditEventType = "password_reset"
	AccountLocked         AuditEventType = "account_locked"
	UnauthorizedAccess    AuditEventType = "unauthorized_access"
)

// AuditEvent represents a security audit event
type AuditEvent struct {
	ID          string                 `json:"id"`
	Timestamp   time.Time              `json:"timestamp"`
	EventType   AuditEventType         `json:"event_type"`
	UserID      string                 `json:"user_id,omitempty"`
	SessionID   string                 `json:"session_id,omitempty"`
	IPAddress   string                 `json:"ip_address"`
	UserAgent   string                 `json:"user_agent,omitempty"`
	Resource    string                 `json:"resource,omitempty"`
	Action      string                 `json:"action,omitempty"`
	Method      string                 `json:"method,omitempty"`
	StatusCode  int                    `json:"status_code,omitempty"`
	Message     string                 `json:"message"`
	Details     map[string]interface{} `json:"details,omitempty"`
	Severity    string                 `json:"severity"`
	Source      string                 `json:"source"`
	RequestID   string                 `json:"request_id,omitempty"`
}

// AuditConfig holds audit logging configuration
type AuditConfig struct {
	Enabled         bool          `yaml:"enabled"`
	LogFile         string        `yaml:"log_file"`
	MaxFileSize     int64         `yaml:"max_file_size"`
	MaxFiles        int           `yaml:"max_files"`
	BufferSize      int           `yaml:"buffer_size"`
	FlushInterval   time.Duration `yaml:"flush_interval"`
	IncludeRequest  bool          `yaml:"include_request"`
	IncludeResponse bool          `yaml:"include_response"`
	SensitiveFields []string      `yaml:"sensitive_fields"`
	RemoteEndpoint  string        `yaml:"remote_endpoint"`
	RemoteToken     string        `yaml:"remote_token"`
}

// AuditLogger handles security audit logging
type AuditLogger struct {
	config     *AuditConfig
	logger     *logrus.Logger
	buffer     chan *AuditEvent
	stopChan   chan bool
	wg         sync.WaitGroup
	eventCount int64
	mu         sync.RWMutex
	stopped    bool
}

// NewAuditLogger creates a new audit logger
func NewAuditLogger(config *AuditConfig, logger *logrus.Logger) *AuditLogger {
	if config.BufferSize == 0 {
		config.BufferSize = 1000
	}
	if config.FlushInterval == 0 {
		config.FlushInterval = 10 * time.Second
	}
	if config.MaxFileSize == 0 {
		config.MaxFileSize = 100 * 1024 * 1024 // 100MB
	}
	if config.MaxFiles == 0 {
		config.MaxFiles = 10
	}

	auditor := &AuditLogger{
		config:   config,
		logger:   logger,
		buffer:   make(chan *AuditEvent, config.BufferSize),
		stopChan: make(chan bool),
	}

	if config.Enabled {
		auditor.start()
	}

	return auditor
}

// LogEvent logs a security audit event
func (a *AuditLogger) LogEvent(ctx context.Context, eventType AuditEventType, message string, details map[string]interface{}) {
	a.mu.RLock()
	enabled := a.config.Enabled
	stopped := a.stopped
	a.mu.RUnlock()
	
	if !enabled || stopped {
		return
	}

	event := &AuditEvent{
		ID:        generateEventID(),
		Timestamp: time.Now().UTC(),
		EventType: eventType,
		Message:   message,
		Details:   a.sanitizeDetails(details),
		Severity:  a.getSeverity(eventType),
		Source:    "llm-router-waf",
	}

	// Extract context information if available
	if requestID, ok := ctx.Value("request_id").(string); ok {
		event.RequestID = requestID
	}

	if authInfo, ok := ctx.Value("auth_info").(*AuthInfo); ok {
		event.UserID = authInfo.UserID
	}

	if clientIP, ok := ctx.Value("client_ip").(string); ok {
		event.IPAddress = clientIP
	}

	// Try to add event to buffer
	select {
	case a.buffer <- event:
		a.mu.Lock()
		a.eventCount++
		a.mu.Unlock()
	default:
		// Buffer full, log warning and drop event
		a.logger.Warn("Audit buffer full, dropping event")
	}
}

// LogAuthenticationAttempt logs authentication attempts
func (a *AuditLogger) LogAuthenticationAttempt(ctx context.Context, userID, method string, success bool, details map[string]interface{}) {
	eventType := AuthenticationSuccess
	message := fmt.Sprintf("User %s authenticated successfully using %s", userID, method)
	
	if !success {
		eventType = AuthenticationFailure
		message = fmt.Sprintf("Authentication failed for user %s using %s", userID, method)
	}
	
	if details == nil {
		details = make(map[string]interface{})
	}
	details["auth_method"] = method
	details["success"] = success
	
	a.LogEvent(ctx, eventType, message, details)
}

// LogAPIKeyUsage logs API key usage
func (a *AuditLogger) LogAPIKeyUsage(ctx context.Context, apiKey, endpoint string, statusCode int) {
	details := map[string]interface{}{
		"api_key_prefix": maskAPIKey(apiKey),
		"endpoint":       endpoint,
		"status_code":    statusCode,
	}
	
	message := fmt.Sprintf("API key used for %s (status: %d)", endpoint, statusCode)
	a.LogEvent(ctx, APIKeyUsage, message, details)
}

// LogSecurityViolation logs security violations
func (a *AuditLogger) LogSecurityViolation(ctx context.Context, violationType, description string, details map[string]interface{}) {
	if details == nil {
		details = make(map[string]interface{})
	}
	details["violation_type"] = violationType
	details["description"] = description
	
	message := fmt.Sprintf("Security violation detected: %s - %s", violationType, description)
	a.LogEvent(ctx, SecurityViolation, message, details)
}

// LogSuspiciousActivity logs suspicious activities
func (a *AuditLogger) LogSuspiciousActivity(ctx context.Context, activity, reason string, details map[string]interface{}) {
	if details == nil {
		details = make(map[string]interface{})
	}
	details["activity"] = activity
	details["reason"] = reason
	
	message := fmt.Sprintf("Suspicious activity detected: %s - %s", activity, reason)
	a.LogEvent(ctx, SuspiciousActivity, message, details)
}

// AuditMiddleware creates audit logging middleware
func (a *AuditLogger) AuditMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			startTime := time.Now()
			
			// Create a response writer wrapper to capture status code
			wrapper := &responseWriterWrapper{
				ResponseWriter: w,
				statusCode:     200,
			}
			
			// Add request ID to context
			requestID := generateRequestID()
			ctx := context.WithValue(r.Context(), "request_id", requestID)
			ctx = context.WithValue(ctx, "client_ip", getClientIPFromRequest(r))
			
			// Process request
			next.ServeHTTP(wrapper, r.WithContext(ctx))
			
			// Log the request
			duration := time.Since(startTime)
			
			details := map[string]interface{}{
				"method":      r.Method,
				"url":         r.URL.String(),
				"status_code": wrapper.statusCode,
				"duration_ms": duration.Milliseconds(),
				"user_agent":  r.UserAgent(),
				"referer":     r.Referer(),
			}
			
			// Add request headers if configured
			if a.config.IncludeRequest {
				headers := make(map[string]string)
				for key, values := range r.Header {
					if !a.isSensitiveField(key) {
						headers[key] = strings.Join(values, ", ")
					}
				}
				details["request_headers"] = headers
			}
			
			// Add auth info if available
			if authInfo, ok := ctx.Value("auth_info").(*AuthInfo); ok {
				details["user_id"] = authInfo.UserID
				details["auth_type"] = authInfo.Metadata["auth_type"]
			}
			
			// Determine event type based on status code
			eventType := AuthenticationSuccess
			message := fmt.Sprintf("%s %s - %d", r.Method, r.URL.Path, wrapper.statusCode)
			
			if wrapper.statusCode >= 400 {
				if wrapper.statusCode == 401 {
					eventType = AuthenticationFailure
				} else if wrapper.statusCode == 403 {
					eventType = AuthorizationFailure
				} else if wrapper.statusCode == 429 {
					eventType = RateLimitExceeded
				} else if wrapper.statusCode >= 400 && wrapper.statusCode < 500 {
					eventType = ValidationFailure
				}
			}
			
			a.LogEvent(ctx, eventType, message, details)
		})
	}
}

// GetEventCount returns the number of events logged
func (a *AuditLogger) GetEventCount() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.eventCount
}

// Stop stops the audit logger
func (a *AuditLogger) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	
	if !a.config.Enabled || a.stopped {
		return
	}
	
	a.stopped = true
	close(a.stopChan)
	a.wg.Wait()
	close(a.buffer)
	
	// Flush remaining events
	for event := range a.buffer {
		a.writeEvent(event)
	}
}

// Private methods

func (a *AuditLogger) start() {
	a.wg.Add(1)
	go a.eventProcessor()
}

func (a *AuditLogger) eventProcessor() {
	defer a.wg.Done()
	
	ticker := time.NewTicker(a.config.FlushInterval)
	defer ticker.Stop()
	
	events := make([]*AuditEvent, 0, 100)
	
	for {
		select {
		case event := <-a.buffer:
			events = append(events, event)
			
			// Flush if buffer is full
			if len(events) >= 100 {
				a.flushEvents(events)
				events = events[:0]
			}
			
		case <-ticker.C:
			// Periodic flush
			if len(events) > 0 {
				a.flushEvents(events)
				events = events[:0]
			}
			
		case <-a.stopChan:
			// Final flush on shutdown
			if len(events) > 0 {
				a.flushEvents(events)
			}
			return
		}
	}
}

func (a *AuditLogger) flushEvents(events []*AuditEvent) {
	for _, event := range events {
		a.writeEvent(event)
	}
}

func (a *AuditLogger) writeEvent(event *AuditEvent) {
	// Write to structured log
	fields := logrus.Fields{
		"audit_event":  true,
		"event_type":   event.EventType,
		"event_id":     event.ID,
		"user_id":      event.UserID,
		"ip_address":   event.IPAddress,
		"resource":     event.Resource,
		"action":       event.Action,
		"status_code":  event.StatusCode,
		"severity":     event.Severity,
		"request_id":   event.RequestID,
		"timestamp":    event.Timestamp,
	}
	
	// Add details
	for key, value := range event.Details {
		fields[fmt.Sprintf("detail_%s", key)] = value
	}
	
	entry := a.logger.WithFields(fields)
	
	// Log at appropriate level based on severity
	switch event.Severity {
	case "critical":
		entry.Error(event.Message)
	case "high":
		entry.Warn(event.Message)
	case "medium":
		entry.Info(event.Message)
	default:
		entry.Debug(event.Message)
	}
	
	// Send to remote endpoint if configured
	if a.config.RemoteEndpoint != "" {
		go a.sendToRemoteEndpoint(event)
	}
}

func (a *AuditLogger) sendToRemoteEndpoint(event *AuditEvent) {
	// Implementation would send to external SIEM/logging system
	// This is a placeholder for the actual implementation
	a.logger.Debug("Would send audit event to remote endpoint", event.ID)
}

func (a *AuditLogger) sanitizeDetails(details map[string]interface{}) map[string]interface{} {
	if details == nil {
		return nil
	}
	
	sanitized := make(map[string]interface{})
	for key, value := range details {
		if a.isSensitiveField(key) {
			sanitized[key] = "***REDACTED***"
		} else {
			sanitized[key] = value
		}
	}
	
	return sanitized
}

func (a *AuditLogger) isSensitiveField(field string) bool {
	fieldLower := strings.ToLower(field)
	
	// Default sensitive fields
	defaultSensitive := []string{
		"password", "token", "secret", "key", "auth", "credential",
		"authorization", "x-api-key", "api-key", "bearer",
	}
	
	// Check default sensitive fields
	for _, sensitive := range defaultSensitive {
		if strings.Contains(fieldLower, sensitive) {
			return true
		}
	}
	
	// Check configured sensitive fields
	for _, sensitive := range a.config.SensitiveFields {
		if strings.EqualFold(field, sensitive) {
			return true
		}
	}
	
	return false
}

func (a *AuditLogger) getSeverity(eventType AuditEventType) string {
	switch eventType {
	case SecurityViolation, UnauthorizedAccess:
		return "critical"
	case AuthenticationFailure, AuthorizationFailure, SuspiciousActivity:
		return "high"
	case RateLimitExceeded, ValidationFailure:
		return "medium"
	default:
		return "low"
	}
}

// Helper types and functions

type responseWriterWrapper struct {
	http.ResponseWriter
	statusCode int
}

func (w *responseWriterWrapper) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func generateEventID() string {
	return fmt.Sprintf("audit_%d_%d", time.Now().Unix(), time.Now().Nanosecond())
}

func generateRequestID() string {
	return fmt.Sprintf("req_%d_%d", time.Now().Unix(), time.Now().Nanosecond())
}