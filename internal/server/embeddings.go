package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/providers"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// embeddingProvider returns the first configured provider that implements the
// embeddings endpoint (OpenAI), or nil when none do.
func (s *Server) embeddingProvider() providers.EmbeddingProvider {
	for _, name := range s.router.ListProviders() {
		if p, ok := s.router.GetProvider(name); ok {
			if ep, ok := p.(providers.EmbeddingProvider); ok {
				return ep
			}
		}
	}
	return nil
}

// handleEmbeddings serves the OpenAI-compatible POST /v1/embeddings. Wrapped in
// the AIQG ingress middleware (auth + event emission), it applies the BYOK
// upstream key and forwards to the embeddings-capable provider. It uses a lean
// path — no chat-specific classification/caching — but stamps vendor/model/usage
// so the AIQG event still carries attribution and token accounting.
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req types.EmbeddingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if req.Model == "" {
		s.writeErrorResponse(w, http.StatusBadRequest, "field 'model' is required")
		return
	}
	if req.Input == nil {
		s.writeErrorResponse(w, http.StatusBadRequest, "field 'input' is required")
		return
	}

	prov := s.embeddingProvider()
	if prov == nil {
		s.writeErrorResponse(w, http.StatusNotImplemented, "no configured provider supports embeddings")
		return
	}
	vendor := prov.GetProviderName()
	middleware.StampVendor(r.Context(), vendor)
	middleware.StampModel(r.Context(), req.Model)

	// BYOK: resolve the effective upstream key for the vendor (stored → shared).
	// Returns nil after writing a 402 for a BYOK-only tenant with no key.
	if nr := s.applyBYOKKey(w, r, vendor); nr == nil {
		return
	} else {
		r = nr
	}

	resp, err := prov.Embeddings(r.Context(), &req)
	if err != nil {
		s.writeErrorResponse(w, http.StatusBadGateway, fmt.Sprintf("Embeddings failed: %v", err))
		return
	}
	if resp.Usage != nil {
		// Embeddings bill only input tokens; stamp them so the AIQG event
		// carries token accounting (completion tokens are always 0 here).
		middleware.StampTokenUsage(r.Context(), resp.Usage.PromptTokens, 0, 0, 0)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
