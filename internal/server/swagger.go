package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/mux"
	"github.com/tributary-ai/llm-router-waf/docs"
	"gopkg.in/yaml.v3"
)

// swaggerUIAssets holds the vendored Swagger UI distribution plus our own
// stylesheet and bootstrap script. See swaggerui/README.md for provenance.
//
//go:embed swaggerui/swagger-ui.css swaggerui/swagger-ui-bundle.js swaggerui/custom.css swaggerui/init.js
var swaggerUIAssets embed.FS

// docsContentSecurityPolicy overrides the service-wide `default-src 'self'`
// for the documentation page only. Swagger UI needs style attributes on the
// elements it renders and data: URIs for its icons, neither of which bare
// 'self' permits — but scripts stay same-origin, so the vendored bundle is
// the only code that can run here.
const docsContentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self' data:; " +
	"connect-src 'self'; " +
	"base-uri 'none'; " +
	"frame-ancestors 'none'"

// openAPIJSON converts the embedded YAML spec to JSON once, on first request.
var openAPIJSON = sync.OnceValues(func() ([]byte, error) {
	var spec interface{}
	if err := yaml.Unmarshal(docs.OpenAPISpec, &spec); err != nil {
		return nil, fmt.Errorf("parsing embedded OpenAPI spec: %w", err)
	}
	return json.MarshalIndent(spec, "", "  ")
})

// setupSwaggerRoutes sets up Swagger UI routes for API documentation
func (s *Server) setupSwaggerRoutes(r *mux.Router) {
	// Serve OpenAPI spec
	r.HandleFunc("/docs/openapi.yaml", s.handleOpenAPISpec).Methods("GET")
	r.HandleFunc("/docs/openapi.json", s.handleOpenAPISpec).Methods("GET")

	// Serve the vendored Swagger UI bundle, stylesheet and bootstrap script.
	// Registered before the /docs/{path:.*} catch-all below, which would
	// otherwise answer every asset request with the index page.
	assets, err := fs.Sub(swaggerUIAssets, "swaggerui")
	if err != nil {
		// Only reachable if the embed directive and this path disagree,
		// which the build would have caught.
		panic(fmt.Sprintf("swagger UI assets: %v", err))
	}
	r.PathPrefix("/docs/ui/").Handler(
		http.StripPrefix("/docs/ui/", http.FileServer(http.FS(assets))),
	).Methods("GET")

	// Serve Swagger UI
	r.HandleFunc("/docs", s.handleSwaggerUI).Methods("GET")
	r.HandleFunc("/docs/", s.handleSwaggerUI).Methods("GET")
	r.HandleFunc("/docs/{path:.*}", s.handleSwaggerUI).Methods("GET")
}

// handleOpenAPISpec serves the embedded OpenAPI specification
func (s *Server) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Determine if JSON or YAML is requested
	if strings.HasSuffix(r.URL.Path, ".json") {
		jsonData, err := openAPIJSON()
		if err != nil {
			http.Error(w, "Error converting OpenAPI spec to JSON", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(jsonData)
		return
	}

	// Serve YAML spec
	w.Header().Set("Content-Type", "text/yaml")
	w.Write(docs.OpenAPISpec)
}

// handleSwaggerUI serves the Swagger UI interface
func (s *Server) handleSwaggerUI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/docs")

	// If requesting root docs path, serve the main UI
	if path == "" || path == "/" {
		s.serveSwaggerIndex(w, r)
		return
	}

	// Deep links like /docs/v1-chat-completions are client-side routes —
	// serve the index and let Swagger UI resolve them. Static assets never
	// reach here; /docs/ui/ is matched by the file server registered first.
	s.serveSwaggerIndex(w, r)
}

// serveSwaggerIndex serves the main Swagger UI HTML page.
//
// The markup carries no inline <style> or <script>: everything is loaded from
// /docs/ui/, so the page renders under the strict CSP set below without
// needing 'unsafe-inline' for scripts or a third-party origin.
func (s *Server) serveSwaggerIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	// Overrides the service-wide `default-src 'self'` from the security
	// middleware, which blocks the style attributes and data: icons Swagger
	// UI needs.
	w.Header().Set("Content-Security-Policy", docsContentSecurityPolicy)

	w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>LLM Router WAF - API Documentation</title>
    <link rel="stylesheet" type="text/css" href="/docs/ui/swagger-ui.css" />
    <link rel="stylesheet" type="text/css" href="/docs/ui/custom.css" />
</head>
<body>
    <div class="custom-header">
        <h1>LLM Router WAF API Documentation</h1>
        <p>
            Intelligent routing, security, and observability for Large Language Model APIs
            <span class="feature-highlight">🔄 Retry &amp; Fallback</span>
            <span class="feature-highlight">🛡️ Security</span>
            <span class="feature-highlight">📊 Observability</span>
        </p>
    </div>
    <div id="swagger-ui"></div>

    <script src="/docs/ui/swagger-ui-bundle.js"></script>
    <script src="/docs/ui/init.js"></script>
</body>
</html>`))
}
