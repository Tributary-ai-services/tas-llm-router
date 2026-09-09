package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/gorilla/mux"

	routermetrics "github.com/tributary-ai/llm-router-waf/internal/metrics"
	"github.com/tributary-ai/llm-router-waf/internal/registry"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// writeRegistryJSON writes v as a JSON response with the given status.
func writeRegistryJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Admin + observability endpoints for the model registry (epic #2, Phase 5).
// All are no-ops returning 503 when the registry is disabled, so they are safe
// to mount unconditionally.

// syncMinInterval rate-limits the manual sync endpoint: a discovery pass makes
// live provider calls, so a tight loop of POSTs would hammer the vendors. One
// manual sync per interval; the periodic engine is unaffected.
const syncMinInterval = 10 * time.Second

// SetRegistry wires the model registry and its sync engine so the admin
// endpoints can read and drive them. Call once at startup.
func (s *Server) SetRegistry(reg *registry.Registry, engine *registry.SyncEngine) {
	s.modelRegistry = reg
	s.modelRegSync = engine
}

// registryEnabled reports whether the registry is wired.
func (s *Server) registryEnabled() bool { return s.modelRegistry != nil }

// recordRegistryResolution emits the alias/fallback metrics for what the router
// did to a request's model, from the RouterMetadata. Called once on the primary
// serving path.
func recordRegistryResolution(meta *types.RouterMetadata) {
	if meta == nil || meta.OriginalModel == "" {
		return
	}
	if meta.FallbackUsed && meta.FallbackReason == "model_unavailable" {
		routermetrics.ObserveModelFallback(meta.OriginalModel, meta.Model)
		return
	}
	if meta.ResolvedModel != "" {
		routermetrics.ObserveAliasResolution(meta.OriginalModel, meta.ResolvedModel)
	}
}

// modelStatusSamples adapts the registry's contents to the metrics collector's
// sample shape. Safe for concurrent use (reads the in-memory snapshot).
func (s *Server) ModelStatusSamples() []routermetrics.ModelStatusSample {
	if s.modelRegistry == nil {
		return nil
	}
	var out []routermetrics.ModelStatusSample
	for _, provider := range s.modelRegistry.Providers() {
		models, err := s.modelRegistry.ListModels(provider)
		if err != nil {
			continue
		}
		for _, m := range models {
			status := string(m.Status)
			if status == "" {
				status = string(types.ModelStatusActive)
			}
			out = append(out, routermetrics.ModelStatusSample{Provider: provider, Model: m.Name, Status: status})
		}
	}
	return out
}

func (s *Server) registryDisabled(w http.ResponseWriter, r *http.Request) bool {
	if s.registryEnabled() {
		return false
	}
	s.writeErrorCtx(w, r, http.StatusServiceUnavailable, "model registry is not enabled")
	return true
}

// handleRegistrySync triggers a synchronous discovery pass and returns its
// summary. Rate-limited to one per syncMinInterval.
func (s *Server) handleRegistrySync(w http.ResponseWriter, r *http.Request) {
	if s.registryDisabled(w, r) {
		return
	}
	s.regSyncMu.Lock()
	since := time.Since(s.regSyncLast)
	if since < syncMinInterval {
		retry := (syncMinInterval - since).Seconds()
		s.regSyncMu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(int(retry)+1))
		s.writeErrorCtx(w, r, http.StatusTooManyRequests, "registry sync was run too recently; try again shortly")
		return
	}
	s.regSyncLast = time.Now()
	s.regSyncMu.Unlock()

	stats := s.modelRegSync.SyncNow(r.Context())
	writeRegistryJSON(w, http.StatusOK, stats)
}

// handleListRegistryModels lists all registered models grouped by provider.
func (s *Server) handleListRegistryModels(w http.ResponseWriter, r *http.Request) {
	if s.registryDisabled(w, r) {
		return
	}
	out := map[string][]types.ModelInfo{}
	for _, provider := range s.modelRegistry.Providers() {
		if models, err := s.modelRegistry.ListModels(provider); err == nil {
			out[provider] = models
		}
	}
	writeRegistryJSON(w, http.StatusOK, map[string]interface{}{"providers": out})
}

// handleListProviderModels lists one provider's models.
func (s *Server) handleListProviderModels(w http.ResponseWriter, r *http.Request) {
	if s.registryDisabled(w, r) {
		return
	}
	provider := mux.Vars(r)["provider"]
	models, err := s.modelRegistry.ListModels(provider)
	if err != nil {
		s.writeErrorCtx(w, r, http.StatusInternalServerError, "failed to list models: "+err.Error())
		return
	}
	if len(models) == 0 && !s.providerKnown(provider) {
		s.writeErrorCtx(w, r, http.StatusNotFound, "unknown provider: "+provider)
		return
	}
	writeRegistryJSON(w, http.StatusOK, map[string]interface{}{"provider": provider, "models": models})
}

func (s *Server) providerKnown(provider string) bool {
	return slices.Contains(s.modelRegistry.Providers(), provider)
}

// handleRegistryStatus reports the last sync summary and health.
func (s *Server) handleRegistryStatus(w http.ResponseWriter, r *http.Request) {
	if s.registryDisabled(w, r) {
		return
	}
	stats := s.modelRegSync.Stats()
	writeRegistryJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":   true,
		"providers": s.modelRegistry.Providers(),
		"last_sync": stats,
	})
}

// handleValidateModel validates a specific model's availability via its
// provider adapter (a live probe). Body: {"provider": "...", "model": "..."}.
func (s *Server) handleValidateModel(w http.ResponseWriter, r *http.Request) {
	if s.registryDisabled(w, r) {
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Provider == "" || body.Model == "" {
		s.writeErrorCtx(w, r, http.StatusBadRequest, "body must be {\"provider\":\"...\",\"model\":\"...\"}")
		return
	}
	available, err := s.modelRegistry.ValidateModel(r.Context(), body.Provider, body.Model)
	result := "available"
	switch {
	case err != nil:
		result = "error"
	case !available:
		result = "unavailable"
	}
	routermetrics.ObserveModelValidation(body.Provider, body.Model, result)
	resp := map[string]interface{}{"provider": body.Provider, "model": body.Model, "available": available}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeRegistryJSON(w, http.StatusOK, resp)
}
