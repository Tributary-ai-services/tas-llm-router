package server

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// anthropicSDKRequest reports whether the caller is the Anthropic SDK, detected
// by the anthropic-version header it always sends. Used on the shared
// /v1/models routes so the Anthropic SDK's models.list()/retrieve() receives
// Anthropic's native model shape rather than the OpenAI list shape.
func anthropicSDKRequest(r *http.Request) bool {
	return r.Header.Get("anthropic-version") != ""
}

// writeAnthropicModelList renders the router's models in Anthropic's native
// list shape ({data:[{type:"model",id,display_name}],has_more,...}). Only
// Anthropic-owned models are listed — an Anthropic SDK caller expects Claude
// models, not the full cross-provider catalog.
func (s *Server) writeAnthropicModelList(w http.ResponseWriter, caps map[string]types.ProviderCapabilities) {
	models := s.anthropicModels(caps)
	list := types.AnthropicModelList{Data: models, HasMore: false}
	if len(models) > 0 {
		list.FirstID = models[0].ID
		list.LastID = models[len(models)-1].ID
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(list)
}

// anthropicModels collects the Anthropic-owned models across providers, deduped
// and sorted by id, in Anthropic's model shape.
func (s *Server) anthropicModels(caps map[string]types.ProviderCapabilities) []types.AnthropicModel {
	seen := make(map[string]struct{})
	var out []types.AnthropicModel
	for pName, pc := range caps {
		owner := pc.ProviderName
		if owner == "" {
			owner = pName
		}
		if owner != "anthropic" {
			continue
		}
		for _, m := range pc.SupportedModels {
			if m.Name == "" {
				continue
			}
			if _, dup := seen[m.Name]; dup {
				continue
			}
			seen[m.Name] = struct{}{}
			display := m.DisplayName
			if display == "" {
				display = m.Name
			}
			out = append(out, types.AnthropicModel{Type: "model", ID: m.Name, DisplayName: display})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// writeAnthropicModel renders a single model in Anthropic's shape, or false when
// no Anthropic-owned provider offers it (caller then 404s).
func (s *Server) writeAnthropicModel(w http.ResponseWriter, caps map[string]types.ProviderCapabilities, id string) bool {
	for pName, pc := range caps {
		owner := pc.ProviderName
		if owner == "" {
			owner = pName
		}
		if owner != "anthropic" {
			continue
		}
		for _, m := range pc.SupportedModels {
			if m.Name == id {
				display := m.DisplayName
				if display == "" {
					display = m.Name
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(types.AnthropicModel{Type: "model", ID: m.Name, DisplayName: display})
				return true
			}
		}
	}
	return false
}
