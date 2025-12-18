package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/sirupsen/logrus"
)

// ValidationMiddleware provides OpenAPI schema validation
type ValidationMiddleware struct {
	router  routers.Router
	logger  *logrus.Logger
	enabled bool
}

// ValidationConfig configures the validation middleware
type ValidationConfig struct {
	Enabled    bool   `yaml:"enabled"`
	SpecPath   string `yaml:"spec_path"`
	StrictMode bool   `yaml:"strict_mode"`
}

// NewValidationMiddleware creates a new validation middleware
func NewValidationMiddleware(config *ValidationConfig, logger *logrus.Logger) (*ValidationMiddleware, error) {
	if config == nil {
		config = &ValidationConfig{
			Enabled:    false,
			SpecPath:   "docs/openapi.yaml",
			StrictMode: false,
		}
	}

	vm := &ValidationMiddleware{
		logger:  logger,
		enabled: config.Enabled,
	}

	if !config.Enabled {
		logger.Info("API validation middleware disabled")
		return vm, nil
	}

	// Load OpenAPI specification
	if err := vm.loadOpenAPISpec(config.SpecPath, config.StrictMode); err != nil {
		return nil, fmt.Errorf("failed to load OpenAPI spec: %w", err)
	}

	logger.WithField("spec_path", config.SpecPath).Info("API validation middleware enabled")
	return vm, nil
}

// loadOpenAPISpec loads and parses the OpenAPI specification
func (vm *ValidationMiddleware) loadOpenAPISpec(specPath string, strictMode bool) error {
	// Load the OpenAPI spec
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true

	// Try to load from relative path
	doc, err := loader.LoadFromFile(specPath)
	if err != nil {
		// If relative path fails, try from project root
		rootPath := filepath.Join("..", "..", specPath)
		doc, err = loader.LoadFromFile(rootPath)
		if err != nil {
			return fmt.Errorf("failed to load OpenAPI spec from %s or %s: %w", specPath, rootPath, err)
		}
	}

	// Validate the document
	ctx := context.Background()
	if err := doc.Validate(ctx); err != nil {
		return fmt.Errorf("invalid OpenAPI spec: %w", err)
	}

	// Create router for path matching
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		return fmt.Errorf("failed to create OpenAPI router: %w", err)
	}

	vm.router = router
	return nil
}

// Middleware returns the HTTP middleware function
func (vm *ValidationMiddleware) Middleware(next http.Handler) http.Handler {
	if !vm.enabled {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validate request
		if err := vm.validateRequest(r); err != nil {
			vm.logger.WithError(err).WithFields(logrus.Fields{
				"method": r.Method,
				"path":   r.URL.Path,
			}).Warn("Request validation failed")

			// Return validation error
			vm.writeValidationError(w, err)
			return
		}

		// Continue to next handler
		next.ServeHTTP(w, r)
	})
}

// validateRequest validates an HTTP request against the OpenAPI spec
func (vm *ValidationMiddleware) validateRequest(r *http.Request) error {
	// Find the route
	route, pathParams, err := vm.router.FindRoute(r)
	if err != nil {
		// If route not found in spec, allow it to pass through
		// This handles routes not documented in OpenAPI (like /metrics, /health)
		if strings.Contains(err.Error(), "not found") {
			return nil
		}
		return fmt.Errorf("route lookup failed: %w", err)
	}

	// Read request body
	var body []byte
	if r.Body != nil {
		body, err = ioutil.ReadAll(r.Body)
		if err != nil {
			return fmt.Errorf("failed to read request body: %w", err)
		}
		// Restore the body for downstream handlers
		r.Body = ioutil.NopCloser(bytes.NewBuffer(body))
	}

	// Create validation input
	requestValidationInput := &openapi3filter.RequestValidationInput{
		Request:    r,
		PathParams: pathParams,
		Route:      route,
	}

	// Set body if present
	if len(body) > 0 {
		requestValidationInput.Request.Body = ioutil.NopCloser(bytes.NewBuffer(body))
	}

	// Validate request
	ctx := context.Background()
	if err := openapi3filter.ValidateRequest(ctx, requestValidationInput); err != nil {
		return fmt.Errorf("request validation failed: %w", err)
	}

	return nil
}

// writeValidationError writes a validation error response
func (vm *ValidationMiddleware) writeValidationError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)

	// Parse validation error for better formatting
	errorDetail := vm.parseValidationError(err)

	response := map[string]interface{}{
		"error": map[string]interface{}{
			"message": errorDetail.Message,
			"type":    "validation_error",
			"code":    "400",
			"details": errorDetail.Details,
		},
		"timestamp": getCurrentTimestamp(),
	}

	json.NewEncoder(w).Encode(response)
}

// ValidationErrorDetail contains parsed validation error information
type ValidationErrorDetail struct {
	Message string                 `json:"message"`
	Details map[string]interface{} `json:"details,omitempty"`
}

// parseValidationError parses validation errors for better user experience
func (vm *ValidationMiddleware) parseValidationError(err error) *ValidationErrorDetail {
	errorStr := err.Error()
	
	// Try to extract meaningful information from the error
	detail := &ValidationErrorDetail{
		Message: "Request validation failed",
		Details: make(map[string]interface{}),
	}

	// Handle different types of validation errors
	switch {
	case strings.Contains(errorStr, "request body"):
		detail.Message = "Invalid request body format"
		detail.Details["field"] = "request body"
	case strings.Contains(errorStr, "required"):
		detail.Message = "Missing required field"
		detail.Details["error"] = errorStr
	case strings.Contains(errorStr, "type"):
		detail.Message = "Invalid field type"
		detail.Details["error"] = errorStr
	case strings.Contains(errorStr, "enum"):
		detail.Message = "Invalid enum value"
		detail.Details["error"] = errorStr
	default:
		detail.Message = errorStr
	}

	return detail
}

// getCurrentTimestamp returns current Unix timestamp
func getCurrentTimestamp() int64 {
	return time.Now().Unix()
}

// ValidateResponse validates an HTTP response against the OpenAPI spec (optional)
func (vm *ValidationMiddleware) ValidateResponse(w http.ResponseWriter, r *http.Request, response *http.Response) error {
	if !vm.enabled {
		return nil
	}

	// Find the route
	route, pathParams, err := vm.router.FindRoute(r)
	if err != nil {
		return nil // Skip validation for undocumented routes
	}

	// Create validation input
	responseValidationInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request:    r,
			PathParams: pathParams,
			Route:      route,
		},
		Status: response.StatusCode,
		Header: response.Header,
	}

	// Add response body if present
	if response.Body != nil {
		body, err := ioutil.ReadAll(response.Body)
		if err != nil {
			return fmt.Errorf("failed to read response body: %w", err)
		}
		responseValidationInput.Body = ioutil.NopCloser(bytes.NewBuffer(body))
		// Restore body
		response.Body = ioutil.NopCloser(bytes.NewBuffer(body))
	}

	// Validate response
	ctx := context.Background()
	if err := openapi3filter.ValidateResponse(ctx, responseValidationInput); err != nil {
		vm.logger.WithError(err).Warn("Response validation failed")
		return fmt.Errorf("response validation failed: %w", err)
	}

	return nil
}