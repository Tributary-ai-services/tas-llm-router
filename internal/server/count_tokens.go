package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/providers"
)

// tokenCounter returns the first configured provider that implements exact
// token counting (Anthropic), or nil when none do.
func (s *Server) tokenCounter() providers.TokenCounter {
	for _, name := range s.router.ListProviders() {
		if p, ok := s.router.GetProvider(name); ok {
			if tc, ok := p.(providers.TokenCounter); ok {
				return tc
			}
		}
	}
	return nil
}

// handleCountTokens serves the native Anthropic POST /v1/messages/count_tokens.
// It parses the Anthropic body (max_tokens NOT required for counting), applies
// the BYOK upstream key, and returns {"input_tokens": N} from the vendor's exact
// count endpoint. Wrapped in AIQG for auth; a stock Anthropic SDK's
// client.messages.count_tokens() works against it.
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
		return
	}
	chatReq, err := parseAnthropicToChatRequest(body, false)
	if err != nil {
		s.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	tc := s.tokenCounter()
	if tc == nil {
		s.writeAnthropicError(w, http.StatusNotImplemented, "api_error", "no configured provider supports token counting")
		return
	}
	vendor := tc.GetProviderName()
	middleware.StampVendor(r.Context(), vendor)
	middleware.StampModel(r.Context(), chatReq.Model)

	if nr := s.applyBYOKKey(w, r, vendor); nr == nil {
		return
	} else {
		r = nr
	}

	n, err := tc.CountTokens(r.Context(), chatReq)
	if err != nil {
		s.writeAnthropicError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("count_tokens failed: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": n})
}
