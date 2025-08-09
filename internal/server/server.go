package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
}

// ServerConfig holds server configuration
type ServerConfig struct {
	Port           string                            `yaml:"port"`
	ReadTimeout    time.Duration                     `yaml:"read_timeout"`
	WriteTimeout   time.Duration                     `yaml:"write_timeout"`
	MaxHeaderBytes int                               `yaml:"max_header_bytes"`
	Security       *middleware.SecurityMiddlewareConfig `yaml:"security"`
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

	// Handle streaming vs non-streaming
	if req.Stream {
		s.handleStreamingCompletion(w, r, &req, provider, metadata)
	} else {
		s.handleNonStreamingCompletion(w, r, &req, provider, metadata)
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