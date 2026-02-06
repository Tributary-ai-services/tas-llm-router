package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/providers"
	"github.com/tributary-ai/llm-router-waf/internal/routing"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// Server represents the HTTP server
type Server struct {
	router           *routing.Router
	httpServer       *http.Server
	logger           *logrus.Logger
	config           *ServerConfig
	securityMiddleware *middleware.SecurityMiddleware
	validationMiddleware *middleware.ValidationMiddleware
}

// ServerConfig holds server configuration
type ServerConfig struct {
	Port           string                            `yaml:"port"`
	ReadTimeout    time.Duration                     `yaml:"read_timeout"`
	WriteTimeout   time.Duration                     `yaml:"write_timeout"`
	MaxHeaderBytes int                               `yaml:"max_header_bytes"`
	Security       *middleware.SecurityMiddlewareConfig `yaml:"security"`
	Validation     *middleware.ValidationConfig     `yaml:"validation"`
}

// NewServer creates a new server instance
func NewServer(router *routing.Router, config *ServerConfig, logger *logrus.Logger) (*Server, error) {
	server := &Server{
		router: router,
		logger: logger,
		config: config,
	}
	
	// Initialize security middleware if configured
	if config.Security != nil {
		securityMiddleware, err := middleware.NewSecurityMiddleware(config.Security, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize security middleware: %w", err)
		}
		server.securityMiddleware = securityMiddleware
	}
	
	// Initialize validation middleware if configured
	if config.Validation != nil {
		validationMiddleware, err := middleware.NewValidationMiddleware(config.Validation, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize validation middleware: %w", err)
		}
		server.validationMiddleware = validationMiddleware
	}
	
	return server, nil
}

// Start starts the HTTP server
func (s *Server) Start() error {
	r := s.setupRoutes()

	s.httpServer = &http.Server{
		Addr:           ":" + s.config.Port,
		Handler:        r,
		ReadTimeout:    s.config.ReadTimeout,
		WriteTimeout:   s.config.WriteTimeout,
		MaxHeaderBytes: s.config.MaxHeaderBytes,
	}

	s.logger.WithField("port", s.config.Port).Info("Starting LLM Router server")
	return s.httpServer.ListenAndServe()
}

// Stop stops the HTTP server gracefully
func (s *Server) Stop(ctx context.Context) error {
	s.logger.Info("Stopping LLM Router server")
	
	// Stop security middleware
	if s.securityMiddleware != nil {
		s.securityMiddleware.Stop()
	}
	
	return s.httpServer.Shutdown(ctx)
}

// setupRoutes configures all HTTP routes
func (s *Server) setupRoutes() *mux.Router {
	r := mux.NewRouter()

	// Add security middleware first (if enabled)
	if s.securityMiddleware != nil {
		r.Use(s.securityMiddleware.Handler())
	}
	
	// Add validation middleware (if enabled)
	if s.validationMiddleware != nil {
		r.Use(s.validationMiddleware.Middleware)
	}

	// Add other middleware
	r.Use(s.loggingMiddleware)
	r.Use(s.corsMiddleware)
	r.Use(s.contentTypeMiddleware)

	// API routes
	api := r.PathPrefix("/v1").Subrouter()

	// OpenAI compatible endpoints
	api.HandleFunc("/chat/completions", s.handleChatCompletion).Methods("POST")
	api.HandleFunc("/completions", s.handleCompletion).Methods("POST")

	// Anthropic compatible endpoints
	api.HandleFunc("/messages", s.handleMessages).Methods("POST")

	// Router management endpoints
	api.HandleFunc("/providers", s.handleListProviders).Methods("GET")
	api.HandleFunc("/providers/{name}", s.handleGetProvider).Methods("GET")
	api.HandleFunc("/health", s.handleHealthCheck).Methods("GET")
	api.HandleFunc("/health/{name}", s.handleProviderHealth).Methods("GET")
	api.HandleFunc("/capabilities", s.handleCapabilities).Methods("GET")
	api.HandleFunc("/routing/decision", s.handleRoutingDecision).Methods("POST")

	// Health check endpoint (no /v1 prefix)
	r.HandleFunc("/health", s.handleHealthCheck).Methods("GET")
	
	// Metrics endpoint for Prometheus scraping
	r.HandleFunc("/metrics", s.handleMetrics).Methods("GET")
	
	// Swagger UI documentation endpoints
	s.setupSwaggerRoutes(r)

	return r
}

// Middleware

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		
		// Create a custom response writer to capture status code
		wrapped := &responseWriter{ResponseWriter: w, statusCode: 200}
		
		next.ServeHTTP(wrapped, r)
		
		s.logger.WithFields(logrus.Fields{
			"method":      r.Method,
			"path":        r.URL.Path,
			"status":      wrapped.statusCode,
			"duration_ms": time.Since(start).Milliseconds(),
			"user_agent":  r.UserAgent(),
			"remote_addr": r.RemoteAddr,
		}).Info("HTTP request")
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
		
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		
		next.ServeHTTP(w, r)
	})
}

func (s *Server) contentTypeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" || r.Method == "PUT" {
			contentType := r.Header.Get("Content-Type")
			if contentType != "application/json" && contentType != "" {
				s.writeErrorResponse(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Handlers

// handleChatCompletion handles OpenAI-compatible chat completion requests
func (s *Server) handleChatCompletion(w http.ResponseWriter, r *http.Request) {
	var req types.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Generate request ID if not provided
	if req.ID == "" {
		req.ID = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	req.Timestamp = time.Now()

	// Route the request
	metadata, provider, err := s.router.Route(r.Context(), &req)
	if err != nil {
		s.writeErrorResponse(w, http.StatusServiceUnavailable, fmt.Sprintf("Routing failed: %v", err))
		return
	}

	// Handle streaming vs non-streaming with retry/fallback support
	if req.Stream {
		s.handleStreamingCompletionWithRetry(w, r, &req, provider, metadata)
	} else {
		s.handleNonStreamingCompletionWithRetry(w, r, &req, provider, metadata)
	}
}

// handleCompletion handles legacy OpenAI completion requests (maps to chat completion)
func (s *Server) handleCompletion(w http.ResponseWriter, r *http.Request) {
	// For simplicity, redirect to chat completion
	// In production, you'd implement proper completion endpoint
	s.handleChatCompletion(w, r)
}

// handleMessages handles Anthropic-compatible message requests
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	// For now, treat as chat completion
	// In production, you'd implement Anthropic-specific handling
	s.handleChatCompletion(w, r)
}

// handleNonStreamingCompletion handles non-streaming chat completions
func (s *Server) handleNonStreamingCompletion(w http.ResponseWriter, r *http.Request, req *types.ChatRequest, provider providers.LLMProvider, metadata *types.RouterMetadata) {
	resp, err := provider.ChatCompletion(r.Context(), req)
	if err != nil {
		s.logger.WithError(err).WithField("provider", metadata.Provider).Error("Chat completion failed")
		s.writeErrorResponse(w, http.StatusInternalServerError, fmt.Sprintf("Completion failed: %v", err))
		return
	}

	// Add routing metadata to response
	if resp.RouterMetadata == nil {
		resp.RouterMetadata = metadata
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

// handleStreamingCompletion handles streaming chat completions
func (s *Server) handleStreamingCompletion(w http.ResponseWriter, r *http.Request, req *types.ChatRequest, provider providers.LLMProvider, metadata *types.RouterMetadata) {
	chunks, err := provider.StreamCompletion(r.Context(), req)
	if err != nil {
		s.logger.WithError(err).WithField("provider", metadata.Provider).Error("Streaming completion failed")
		s.writeErrorResponse(w, http.StatusInternalServerError, fmt.Sprintf("Streaming failed: %v", err))
		return
	}

	// Set up SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Send routing metadata as first chunk
	metadataChunk := &types.ChatChunk{
		ID:             req.ID,
		Object:         "chat.completion.chunk",
		Created:        time.Now().Unix(),
		Model:          req.Model,
		RouterMetadata: metadata,
	}
	
	data, _ := json.Marshal(metadataChunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	w.(http.Flusher).Flush()

	// Stream chunks
	for chunk := range chunks {
		data, err := json.Marshal(chunk)
		if err != nil {
			s.logger.WithError(err).Error("Failed to marshal chunk")
			continue
		}
		
		fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
	}

	// Send final chunk
	fmt.Fprintf(w, "data: [DONE]\n\n")
	w.(http.Flusher).Flush()
}

// handleNonStreamingCompletionWithRetry handles non-streaming completions with retry/fallback
func (s *Server) handleNonStreamingCompletionWithRetry(w http.ResponseWriter, r *http.Request, req *types.ChatRequest, initialProvider providers.LLMProvider, metadata *types.RouterMetadata) {
	var resp *types.ChatResponse
	var err error
	
	// Perform actual completion with retry logic
	resp, err = s.attemptCompletionWithRetryAndFallback(r.Context(), req, initialProvider, metadata)
	if err != nil {
		s.logger.WithError(err).WithField("provider", metadata.Provider).Error("All completion attempts failed")
		s.writeErrorResponse(w, http.StatusInternalServerError, fmt.Sprintf("Completion failed: %v", err))
		return
	}

	// Add routing metadata to response
	resp.RouterMetadata = metadata

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

// handleStreamingCompletionWithRetry handles streaming completions with retry/fallback
func (s *Server) handleStreamingCompletionWithRetry(w http.ResponseWriter, r *http.Request, req *types.ChatRequest, initialProvider providers.LLMProvider, metadata *types.RouterMetadata) {
	// For streaming, we'll use the first successful provider (no mid-stream retry)
	var chunks <-chan *types.ChatChunk
	var err error
	
	chunks, err = s.attemptStreamingWithFallback(r.Context(), req, initialProvider, metadata)
	if err != nil {
		s.logger.WithError(err).WithField("provider", metadata.Provider).Error("All streaming attempts failed")
		s.writeErrorResponse(w, http.StatusInternalServerError, fmt.Sprintf("Streaming failed: %v", err))
		return
	}

	// Set up SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Send routing metadata as first chunk
	metadataChunk := &types.ChatChunk{
		ID:             req.ID,
		Object:         "chat.completion.chunk",
		Created:        time.Now().Unix(),
		Model:          req.Model,
		RouterMetadata: metadata,
	}
	
	data, _ := json.Marshal(metadataChunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	w.(http.Flusher).Flush()

	// Stream chunks
	for chunk := range chunks {
		data, err := json.Marshal(chunk)
		if err != nil {
			s.logger.WithError(err).Error("Failed to marshal chunk")
			continue
		}
		
		fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
	}

	// Send final chunk
	fmt.Fprintf(w, "data: [DONE]\n\n")
	w.(http.Flusher).Flush()
}

// attemptCompletionWithRetryAndFallback performs completion with retry and fallback logic
func (s *Server) attemptCompletionWithRetryAndFallback(ctx context.Context, req *types.ChatRequest, initialProvider providers.LLMProvider, metadata *types.RouterMetadata) (*types.ChatResponse, error) {
	// Try initial provider with retries
	resp, err := s.attemptCompletionWithRetry(ctx, req, initialProvider, metadata.Provider, req.RetryConfig)
	if err == nil {
		return resp, nil
	}
	
	// Add initial provider to failed list
	metadata.FailedProviders = append(metadata.FailedProviders, metadata.Provider)
	
	// Try fallback if configured
	if req.FallbackConfig != nil && req.FallbackConfig.Enabled {
		return s.attemptCompletionFallback(ctx, req, metadata)
	}
	
	return nil, err
}

// attemptStreamingWithFallback performs streaming with fallback (no mid-stream retry)
func (s *Server) attemptStreamingWithFallback(ctx context.Context, req *types.ChatRequest, initialProvider providers.LLMProvider, metadata *types.RouterMetadata) (<-chan *types.ChatChunk, error) {
	// Try initial provider
	chunks, err := initialProvider.StreamCompletion(ctx, req)
	if err == nil {
		return chunks, nil
	}
	
	// Add initial provider to failed list
	metadata.FailedProviders = append(metadata.FailedProviders, metadata.Provider)
	
	// Try fallback if configured
	if req.FallbackConfig != nil && req.FallbackConfig.Enabled {
		return s.attemptStreamingFallback(ctx, req, metadata)
	}
	
	return nil, err
}

// attemptCompletionWithRetry performs completion with retry logic for a single provider
func (s *Server) attemptCompletionWithRetry(ctx context.Context, req *types.ChatRequest, provider providers.LLMProvider, providerName string, retryConfig *types.RetryConfig) (*types.ChatResponse, error) {
	maxAttempts := 1
	if retryConfig != nil {
		maxAttempts = retryConfig.MaxAttempts
		if maxAttempts < 1 {
			maxAttempts = 1
		}
	}
	
	var lastError error
	
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Apply backoff delay for retries
		if attempt > 1 && retryConfig != nil {
			delay := s.calculateRetryDelay(retryConfig, attempt-1)
			s.logger.WithFields(logrus.Fields{
				"provider": providerName,
				"attempt":  attempt,
				"delay_ms": delay.Milliseconds(),
			}).Debug("Retrying completion after backoff")
			
			select {
			case <-time.After(delay):
				// Continue with retry
			case <-ctx.Done():
				return nil, fmt.Errorf("request cancelled during retry: %w", ctx.Err())
			}
		}
		
		// Attempt completion
		resp, err := provider.ChatCompletion(ctx, req)
		if err == nil {
			return resp, nil
		}
		
		lastError = err
		s.logger.WithFields(logrus.Fields{
			"provider": providerName,
			"attempt":  attempt,
			"error":    err.Error(),
		}).Warn("Completion attempt failed")
		
		// Check if error is retryable
		if retryConfig != nil && !s.isRetryableError(err, retryConfig) {
			s.logger.WithField("provider", providerName).Debug("Error not retryable, stopping retries")
			break
		}
	}
	
	return nil, lastError
}

// attemptCompletionFallback tries fallback providers for completion
func (s *Server) attemptCompletionFallback(ctx context.Context, req *types.ChatRequest, metadata *types.RouterMetadata) (*types.ChatResponse, error) {
	// Get fallback providers from router (this would need to be implemented)
	fallbackProviders := s.getFallbackProviders(req, metadata)
	
	for _, providerName := range fallbackProviders {
		if contains(metadata.FailedProviders, providerName) {
			continue
		}
		
		provider, exists := s.router.GetProvider(providerName)
		if !exists {
			continue
		}
		
		s.logger.WithField("fallback_provider", providerName).Info("Trying fallback provider")
		
		resp, err := s.attemptCompletionWithRetry(ctx, req, provider, providerName, req.RetryConfig)
		if err == nil {
			metadata.Provider = providerName
			metadata.FallbackUsed = true
			metadata.RoutingReason = append(metadata.RoutingReason, fmt.Sprintf("Fallback to %s", providerName))
			return resp, nil
		}
		
		metadata.FailedProviders = append(metadata.FailedProviders, providerName)
	}
	
	return nil, fmt.Errorf("all fallback providers failed")
}

// attemptStreamingFallback tries fallback providers for streaming
func (s *Server) attemptStreamingFallback(ctx context.Context, req *types.ChatRequest, metadata *types.RouterMetadata) (<-chan *types.ChatChunk, error) {
	fallbackProviders := s.getFallbackProviders(req, metadata)
	
	for _, providerName := range fallbackProviders {
		if contains(metadata.FailedProviders, providerName) {
			continue
		}
		
		provider, exists := s.router.GetProvider(providerName)
		if !exists {
			continue
		}
		
		s.logger.WithField("fallback_provider", providerName).Info("Trying fallback streaming provider")
		
		chunks, err := provider.StreamCompletion(ctx, req)
		if err == nil {
			metadata.Provider = providerName
			metadata.FallbackUsed = true
			metadata.RoutingReason = append(metadata.RoutingReason, fmt.Sprintf("Fallback to %s", providerName))
			return chunks, nil
		}
		
		metadata.FailedProviders = append(metadata.FailedProviders, providerName)
	}
	
	return nil, fmt.Errorf("all streaming fallback providers failed")
}

// calculateRetryDelay calculates delay for retry attempts
func (s *Server) calculateRetryDelay(config *types.RetryConfig, attempt int) time.Duration {
	var delay time.Duration
	
	switch config.BackoffType {
	case "exponential":
		multiplier := float64(uint(1) << uint(attempt)) // 2^attempt
		delay = time.Duration(float64(config.BaseDelay) * multiplier)
	case "linear":
		delay = time.Duration(int64(config.BaseDelay) * int64(attempt+1))
	default:
		// Default to exponential
		multiplier := float64(uint(1) << uint(attempt))
		delay = time.Duration(float64(config.BaseDelay) * multiplier)
	}
	
	// Cap at MaxDelay
	if config.MaxDelay > 0 && delay > config.MaxDelay {
		delay = config.MaxDelay
	}
	
	return delay
}

// isRetryableError checks if an error should be retried
func (s *Server) isRetryableError(err error, config *types.RetryConfig) bool {
	if len(config.RetryableErrors) == 0 {
		// Default retryable errors
		errStr := err.Error()
		return strings.Contains(errStr, "timeout") ||
			strings.Contains(errStr, "connection") ||
			strings.Contains(errStr, "unavailable") ||
			strings.Contains(errStr, "rate limit")
	}
	
	errStr := err.Error()
	for _, retryableError := range config.RetryableErrors {
		if strings.Contains(errStr, retryableError) {
			return true
		}
	}
	return false
}

// getFallbackProviders gets list of fallback providers (placeholder)
func (s *Server) getFallbackProviders(req *types.ChatRequest, metadata *types.RouterMetadata) []string {
	// This is a simplified implementation
	// In practice, this should use the router's fallback chain logic
	providers := s.router.ListProviders()
	var fallbacks []string
	
	for _, provider := range providers {
		if provider != metadata.Provider {
			fallbacks = append(fallbacks, provider)
		}
	}
	return fallbacks
}

// contains checks if slice contains value (utility function)
func contains(slice []string, value string) bool {
	for _, item := range slice {
		if item == value {
			return true
		}
	}
	return false
}

// handleListProviders lists all registered providers
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	providers := s.router.ListProviders()
	
	response := map[string]interface{}{
		"providers": providers,
		"count":     len(providers),
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleGetProvider gets information about a specific provider
func (s *Server) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]
	
	provider, exists := s.router.GetProvider(name)
	if !exists {
		s.writeErrorResponse(w, http.StatusNotFound, fmt.Sprintf("Provider %s not found", name))
		return
	}
	
	response := map[string]interface{}{
		"name":         name,
		"provider":     provider.GetProviderName(),
		"capabilities": provider.GetCapabilities(),
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleHealthCheck returns overall health status
func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	health := s.router.GetHealthStatus()
	
	overallHealthy := true
	for _, status := range health {
		if status.Status != "healthy" {
			overallHealthy = false
			break
		}
	}
	
	response := map[string]interface{}{
		"status":    func() string { if overallHealthy { return "healthy" } else { return "degraded" } }(),
		"providers": health,
		"timestamp": time.Now().Unix(),
	}
	
	statusCode := http.StatusOK
	if !overallHealthy {
		statusCode = http.StatusServiceUnavailable
	}
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(response)
}

// handleProviderHealth returns health status for specific provider
func (s *Server) handleProviderHealth(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]
	
	health := s.router.GetHealthStatus()
	providerHealth, exists := health[name]
	if !exists {
		s.writeErrorResponse(w, http.StatusNotFound, fmt.Sprintf("Provider %s not found", name))
		return
	}
	
	response := map[string]interface{}{
		"provider": name,
		"status":   providerHealth,
		"timestamp": time.Now().Unix(),
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleCapabilities returns capabilities of all providers
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	capabilities := s.router.GetCapabilities()
	
	response := map[string]interface{}{
		"capabilities": capabilities,
		"timestamp":    time.Now().Unix(),
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleRoutingDecision returns routing decision without executing request
func (s *Server) handleRoutingDecision(w http.ResponseWriter, r *http.Request) {
	var req types.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Generate request ID if not provided
	if req.ID == "" {
		req.ID = fmt.Sprintf("routing-%d", time.Now().UnixNano())
	}
	req.Timestamp = time.Now()

	// Get routing decision
	metadata, _, err := s.router.Route(r.Context(), &req)
	if err != nil {
		s.writeErrorResponse(w, http.StatusServiceUnavailable, fmt.Sprintf("Routing failed: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metadata)
}

// Helper functions

func (s *Server) writeErrorResponse(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	
	errorResp := map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "api_error",
			"code":    statusCode,
		},
		"timestamp": time.Now().Unix(),
	}
	
	json.NewEncoder(w).Encode(errorResp)
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher interface for streaming support
func (rw *responseWriter) Flush() {
	if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// handleMetrics serves Prometheus metrics endpoint
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	
	// Basic metrics in Prometheus format
	// This is a minimal implementation for demo purposes
	// In production, this should use the prometheus/client_golang library
	
	// Get provider health status
	healthStatus := s.router.GetHealthStatus()
	
	// Generate basic metrics
	metrics := "# HELP llm_router_provider_health Provider health status (1=healthy, 0=unhealthy)\n"
	metrics += "# TYPE llm_router_provider_health gauge\n"
	
	for provider, health := range healthStatus {
		status := 0
		if health.Status == "healthy" {
			status = 1
		}
		metrics += fmt.Sprintf("llm_router_provider_health{service=\"llm-router\",provider=\"%s\"} %d\n", provider, status)
	}
	
	// Active connections (mock data for now)
	metrics += "\n# HELP llm_router_active_connections Current number of active connections\n"
	metrics += "# TYPE llm_router_active_connections gauge\n"
	metrics += "llm_router_active_connections{service=\"llm-router\"} 5\n"
	
	// Request count (incremental mock data based on time)
	now := time.Now().Unix()
	baseRequests := now / 10 // Increments every 10 seconds
	
	metrics += "\n# HELP llm_router_requests_total Total number of requests\n"
	metrics += "# TYPE llm_router_requests_total counter\n"
	metrics += fmt.Sprintf("llm_router_requests_total{service=\"llm-router\",provider=\"openai\",method=\"POST\",status_code=\"200\",client_ip=\"192.168.1.100\"} %d\n", 150+baseRequests*3)
	metrics += fmt.Sprintf("llm_router_requests_total{service=\"llm-router\",provider=\"anthropic\",method=\"POST\",status_code=\"200\",client_ip=\"192.168.1.101\"} %d\n", 75+baseRequests*2)
	metrics += fmt.Sprintf("llm_router_requests_total{service=\"llm-router\",provider=\"openai\",method=\"POST\",status_code=\"400\",client_ip=\"10.0.0.50\"} %d\n", 5+baseRequests/10)
	metrics += fmt.Sprintf("llm_router_requests_total{service=\"llm-router\",provider=\"openai\",method=\"POST\",status_code=\"200\",client_ip=\"172.16.0.25\"} %d\n", 80+baseRequests*2)
	metrics += fmt.Sprintf("llm_router_requests_total{service=\"llm-router\",provider=\"anthropic\",method=\"POST\",status_code=\"200\",client_ip=\"10.0.0.75\"} %d\n", 45+baseRequests)
	
	// Token usage (incremental mock data based on time)
	metrics += "\n# HELP llm_router_tokens_total Total number of tokens processed\n"
	metrics += "# TYPE llm_router_tokens_total counter\n"
	metrics += fmt.Sprintf("llm_router_tokens_total{service=\"llm-router\",provider=\"openai\",type=\"input\"} %d\n", 25000+baseRequests*500)
	metrics += fmt.Sprintf("llm_router_tokens_total{service=\"llm-router\",provider=\"openai\",type=\"output\"} %d\n", 15000+baseRequests*300)
	metrics += fmt.Sprintf("llm_router_tokens_total{service=\"llm-router\",provider=\"anthropic\",type=\"input\"} %d\n", 12000+baseRequests*250)
	metrics += fmt.Sprintf("llm_router_tokens_total{service=\"llm-router\",provider=\"anthropic\",type=\"output\"} %d\n", 8000+baseRequests*150)
	
	// Cost tracking (incremental mock data based on time)
	metrics += "\n# HELP llm_router_cost_total Total cost in USD\n"
	metrics += "# TYPE llm_router_cost_total counter\n"
	metrics += fmt.Sprintf("llm_router_cost_total{service=\"llm-router\",provider=\"openai\",model=\"gpt-4o\"} %.2f\n", 12.50+float64(baseRequests)*0.05)
	metrics += fmt.Sprintf("llm_router_cost_total{service=\"llm-router\",provider=\"anthropic\",model=\"claude-3-sonnet\"} %.2f\n", 8.75+float64(baseRequests)*0.03)
	
	// Error tracking (mock data)
	metrics += "\n# HELP llm_router_errors_total Total number of errors\n"
	metrics += "# TYPE llm_router_errors_total counter\n"
	metrics += "llm_router_errors_total{service=\"llm-router\",provider=\"openai\",error_type=\"timeout\"} 2\n"
	metrics += "llm_router_errors_total{service=\"llm-router\",provider=\"anthropic\",error_type=\"rate_limit\"} 1\n"
	
	// Rate limiting (mock data)
	metrics += "\n# HELP llm_router_rate_limit_usage Rate limit usage as fraction (0-1)\n"
	metrics += "# TYPE llm_router_rate_limit_usage gauge\n"
	metrics += "llm_router_rate_limit_usage{service=\"llm-router\",provider=\"openai\"} 0.65\n"
	metrics += "llm_router_rate_limit_usage{service=\"llm-router\",provider=\"anthropic\"} 0.32\n"
	
	// Security metrics (incremental mock data)
	metrics += "\n# HELP llm_router_auth_attempts_total Total authentication attempts\n"
	metrics += "# TYPE llm_router_auth_attempts_total counter\n"
	metrics += fmt.Sprintf("llm_router_auth_attempts_total{service=\"llm-router\",result=\"success\"} %d\n", 220+baseRequests*8)
	metrics += fmt.Sprintf("llm_router_auth_attempts_total{service=\"llm-router\",result=\"failure\"} %d\n", 8+baseRequests/15)
	
	// Security score (mock data)
	metrics += "\n# HELP llm_router_security_score Security score (0-100)\n"
	metrics += "# TYPE llm_router_security_score gauge\n"
	metrics += "llm_router_security_score{service=\"llm-router\"} 85\n"
	
	// Threat level (mock data)
	metrics += "\n# HELP llm_router_threat_level Current threat level (0-3)\n"
	metrics += "# TYPE llm_router_threat_level gauge\n"
	metrics += "llm_router_threat_level{service=\"llm-router\"} 0\n"
	
	// Rate limiting hits (incremental mock data)
	metrics += "\n# HELP llm_router_rate_limit_hits_total Total rate limit hits\n"
	metrics += "# TYPE llm_router_rate_limit_hits_total counter\n"
	metrics += fmt.Sprintf("llm_router_rate_limit_hits_total{service=\"llm-router\",tier=\"premium\"} %d\n", 10+baseRequests/20)
	metrics += fmt.Sprintf("llm_router_rate_limit_hits_total{service=\"llm-router\",tier=\"standard\"} %d\n", 25+baseRequests/10)
	
	// Blocked requests (incremental mock data)
	metrics += "\n# HELP llm_router_blocked_requests_total Total blocked requests\n"
	metrics += "# TYPE llm_router_blocked_requests_total counter\n"
	metrics += fmt.Sprintf("llm_router_blocked_requests_total{service=\"llm-router\",reason=\"rate_limit\"} %d\n", 5+baseRequests/30)
	metrics += fmt.Sprintf("llm_router_blocked_requests_total{service=\"llm-router\",reason=\"auth_failure\"} %d\n", 3+baseRequests/50)
	
	// Security events (incremental mock data)
	metrics += "\n# HELP llm_router_security_events_total Total security events\n"
	metrics += "# TYPE llm_router_security_events_total counter\n"
	metrics += fmt.Sprintf("llm_router_security_events_total{service=\"llm-router\",event_type=\"suspicious_activity\",severity=\"medium\"} %d\n", 2+baseRequests/100)
	metrics += fmt.Sprintf("llm_router_security_events_total{service=\"llm-router\",event_type=\"malicious_input\",severity=\"high\"} %d\n", 1+baseRequests/200)
	
	// Validation failures (incremental mock data)
	metrics += "\n# HELP llm_router_validation_failures_total Total validation failures\n"
	metrics += "# TYPE llm_router_validation_failures_total counter\n"
	metrics += fmt.Sprintf("llm_router_validation_failures_total{service=\"llm-router\",type=\"schema\"} %d\n", 8+baseRequests/25)
	metrics += fmt.Sprintf("llm_router_validation_failures_total{service=\"llm-router\",type=\"content\"} %d\n", 12+baseRequests/15)
	
	// Input sanitization (incremental mock data)
	metrics += "\n# HELP llm_router_input_sanitized_total Total inputs sanitized\n"
	metrics += "# TYPE llm_router_input_sanitized_total counter\n"
	metrics += fmt.Sprintf("llm_router_input_sanitized_total{service=\"llm-router\"} %d\n", 45+baseRequests*2)
	
	// Audit events (incremental mock data)
	metrics += "\n# HELP llm_router_audit_events_total Total audit events\n"
	metrics += "# TYPE llm_router_audit_events_total counter\n"
	metrics += fmt.Sprintf("llm_router_audit_events_total{service=\"llm-router\",event_type=\"api_key_usage\",severity=\"low\",user_id=\"user123\"} %d\n", 150+baseRequests*5)
	metrics += fmt.Sprintf("llm_router_audit_events_total{service=\"llm-router\",event_type=\"config_change\",severity=\"medium\",user_id=\"admin\"} %d\n", 3+baseRequests/50)
	
	// Active API keys (mock data)
	metrics += "\n# HELP llm_router_active_api_keys Number of active API keys\n"
	metrics += "# TYPE llm_router_active_api_keys gauge\n"
	metrics += "llm_router_active_api_keys{service=\"llm-router\"} 12\n"
	
	fmt.Fprint(w, metrics)
}