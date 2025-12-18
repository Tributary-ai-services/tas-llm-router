package server

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gorilla/mux"
	"gopkg.in/yaml.v2"
)

// setupSwaggerRoutes sets up Swagger UI routes for API documentation
func (s *Server) setupSwaggerRoutes(r *mux.Router) {
	// Serve OpenAPI spec
	r.HandleFunc("/docs/openapi.yaml", s.handleOpenAPISpec).Methods("GET")
	r.HandleFunc("/docs/openapi.json", s.handleOpenAPISpec).Methods("GET")
	
	// Serve Swagger UI
	r.HandleFunc("/docs", s.handleSwaggerUI).Methods("GET")
	r.HandleFunc("/docs/", s.handleSwaggerUI).Methods("GET")
	r.HandleFunc("/docs/{path:.*}", s.handleSwaggerUI).Methods("GET")
}

// handleOpenAPISpec serves the OpenAPI specification
func (s *Server) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	// Determine if JSON or YAML is requested
	path := r.URL.Path
	isJSON := strings.HasSuffix(path, ".json")
	
	if isJSON {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		
		// Read and convert YAML to JSON
		specPath := filepath.Join("docs", "openapi.yaml")
		yamlData, err := ioutil.ReadFile(specPath)
		if err != nil {
			http.Error(w, "OpenAPI spec not found", http.StatusNotFound)
			return
		}
		
		// Parse YAML
		var spec interface{}
		if err := yaml.Unmarshal(yamlData, &spec); err != nil {
			http.Error(w, "Error parsing OpenAPI spec", http.StatusInternalServerError)
			return
		}
		
		// Convert to JSON
		jsonData, err := json.MarshalIndent(spec, "", "  ")
		if err != nil {
			http.Error(w, "Error converting to JSON", http.StatusInternalServerError)
			return
		}
		
		w.Write(jsonData)
		return
	}
	
	// Serve YAML spec
	w.Header().Set("Content-Type", "text/yaml")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	// Read the OpenAPI spec file
	specPath := filepath.Join("docs", "openapi.yaml")
	http.ServeFile(w, r, specPath)
}

// handleSwaggerUI serves the Swagger UI interface
func (s *Server) handleSwaggerUI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/docs")
	
	// If requesting root docs path, serve the main UI
	if path == "" || path == "/" {
		s.serveSwaggerIndex(w, r)
		return
	}
	
	// For now, serve a simple HTML page
	// In production, you'd serve static Swagger UI assets
	s.serveSwaggerIndex(w, r)
}

// serveSwaggerIndex serves the main Swagger UI HTML page
func (s *Server) serveSwaggerIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	
	// Get the base URL for the API spec
	baseURL := getBaseURL(r)
	specURL := fmt.Sprintf("%s/docs/openapi.yaml", baseURL)
	
	html := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>LLM Router WAF - API Documentation</title>
    <link rel="stylesheet" type="text/css" href="https://unpkg.com/swagger-ui-dist@5.9.0/swagger-ui.css" />
    <style>
        html {
            box-sizing: border-box;
            overflow: -moz-scrollbars-vertical;
            overflow-y: scroll;
        }
        *, *:before, *:after {
            box-sizing: inherit;
        }
        body {
            margin:0;
            background: #fafafa;
        }
        .swagger-ui .topbar { display: none; }
        .custom-header {
            background: #1f2937;
            color: white;
            padding: 1rem 2rem;
            margin-bottom: 2rem;
        }
        .custom-header h1 {
            margin: 0;
            font-size: 1.5rem;
        }
        .custom-header p {
            margin: 0.5rem 0 0 0;
            opacity: 0.8;
        }
        .feature-highlight {
            background: #10b981;
            color: white;
            padding: 0.25rem 0.5rem;
            border-radius: 0.25rem;
            font-size: 0.875rem;
            margin-left: 0.5rem;
        }
    </style>
</head>
<body>
    <div class="custom-header">
        <h1>LLM Router WAF API Documentation</h1>
        <p>
            Intelligent routing, security, and observability for Large Language Model APIs
            <span class="feature-highlight">🔄 Retry & Fallback</span>
            <span class="feature-highlight">🛡️ Security</span>
            <span class="feature-highlight">📊 Observability</span>
        </p>
    </div>
    <div id="swagger-ui"></div>
    
    <script src="https://unpkg.com/swagger-ui-dist@5.9.0/swagger-ui-bundle.js"></script>
    <script src="https://unpkg.com/swagger-ui-dist@5.9.0/swagger-ui-standalone-preset.js"></script>
    <script>
        window.onload = function() {
            const ui = SwaggerUIBundle({
                url: '%s',
                dom_id: '#swagger-ui',
                deepLinking: true,
                presets: [
                    SwaggerUIBundle.presets.apis,
                    SwaggerUIStandalonePreset
                ],
                plugins: [
                    SwaggerUIBundle.plugins.DownloadUrl
                ],
                layout: "StandaloneLayout",
                defaultModelsExpandDepth: 0,
                defaultModelExpandDepth: 3,
                docExpansion: "list",
                filter: true,
                showRequestHeaders: true,
                supportedSubmitMethods: ['get', 'post', 'put', 'delete', 'patch'],
                validatorUrl: null,
                onComplete: function() {
                    // Add custom styling or behavior after load
                    console.log('LLM Router WAF API Documentation loaded');
                },
                requestInterceptor: function(request) {
                    // Add default headers or modify requests
                    if (!request.headers['X-API-Key'] && !request.headers['Authorization']) {
                        request.headers['X-API-Key'] = 'your-api-key-here';
                    }
                    return request;
                }
            });
        };
    </script>
</body>
</html>`, specURL)
	
	w.Write([]byte(html))
}

// getBaseURL extracts the base URL from the request
func getBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	
	// Check for forwarded headers (common in reverse proxy setups)
	if forwardedProto := r.Header.Get("X-Forwarded-Proto"); forwardedProto != "" {
		scheme = forwardedProto
	}
	
	host := r.Host
	if forwardedHost := r.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
		host = forwardedHost
	}
	
	return fmt.Sprintf("%s://%s", scheme, host)
}