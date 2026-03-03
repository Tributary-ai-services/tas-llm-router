package gatekeeper

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/security"
)

// BypassHandler handles HTTP endpoints for bypass token management.
type BypassHandler struct {
	manager *BypassManager
	logger  *logrus.Logger
}

// NewBypassHandler creates a new bypass handler.
func NewBypassHandler(manager *BypassManager, logger *logrus.Logger) *BypassHandler {
	return &BypassHandler{
		manager: manager,
		logger:  logger,
	}
}

// RegisterRoutes registers bypass token API routes on a subrouter.
func (h *BypassHandler) RegisterRoutes(r *mux.Router) {
	r.HandleFunc("/v1/scan/bypass-tokens", h.handleCreate).Methods("POST")
	r.HandleFunc("/v1/scan/bypass-tokens", h.handleList).Methods("GET")
	r.HandleFunc("/v1/scan/bypass-tokens/{id}", h.handleRevoke).Methods("DELETE")
}

type createTokenRequest struct {
	TTLHours int    `json:"ttl_hours"`
	Reason   string `json:"reason"`
}

type createTokenResponse struct {
	Token     string    `json:"token"`
	TokenID   string    `json:"token_id"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedBy string    `json:"created_by"`
}

type listTokensResponse struct {
	Tokens []tokenInfo `json:"tokens"`
}

type tokenInfo struct {
	TokenID   string    `json:"token_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Reason    string    `json:"reason"`
}

func (h *BypassHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	authInfo, _ := security.GetAuthInfo(r.Context())
	if authInfo == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}

	var req createTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.Reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reason is required"})
		return
	}
	if req.TTLHours <= 0 || req.TTLHours > 24 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ttl_hours must be between 1 and 24"})
		return
	}

	tenantID := ""
	if authInfo.Metadata != nil {
		tenantID = authInfo.Metadata["tenant_id"]
	}

	rawToken, token, err := h.manager.GenerateToken(r.Context(), authInfo.UserID, tenantID, req.Reason, req.TTLHours)
	if err != nil {
		h.logger.WithError(err).Error("Failed to generate bypass token")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to generate token"})
		return
	}

	writeJSON(w, http.StatusCreated, createTokenResponse{
		Token:     rawToken,
		TokenID:   token.TokenID,
		ExpiresAt: token.ExpiresAt,
		CreatedBy: authInfo.UserID,
	})
}

func (h *BypassHandler) handleList(w http.ResponseWriter, r *http.Request) {
	authInfo, _ := security.GetAuthInfo(r.Context())
	if authInfo == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}

	tokens, err := h.manager.ListTokens(r.Context(), authInfo.UserID)
	if err != nil {
		h.logger.WithError(err).Error("Failed to list bypass tokens")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list tokens"})
		return
	}

	resp := listTokensResponse{Tokens: make([]tokenInfo, 0, len(tokens))}
	for _, t := range tokens {
		resp.Tokens = append(resp.Tokens, tokenInfo{
			TokenID:   t.TokenID,
			CreatedAt: t.CreatedAt,
			ExpiresAt: t.ExpiresAt,
			Reason:    t.Reason,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *BypassHandler) handleRevoke(w http.ResponseWriter, r *http.Request) {
	authInfo, _ := security.GetAuthInfo(r.Context())
	if authInfo == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}

	vars := mux.Vars(r)
	tokenID := vars["id"]

	if err := h.manager.RevokeToken(r.Context(), tokenID); err != nil {
		h.logger.WithError(err).Error("Failed to revoke bypass token")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to revoke token"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
