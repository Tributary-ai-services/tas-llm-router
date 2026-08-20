package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/breaker"
)

// handleBreakerStatus reports passive outlier-detection state for every known
// routing target.
//
// This exists because an ejection nobody can see is indistinguishable from an
// outage. When a provider stops receiving traffic the operator's first
// question is "is the vendor down, or did WE stop sending?" — and without this
// endpoint the two look identical from the outside. Every field here answers
// part of that question: state says which, reason says why, since says when,
// and ejected_until says when it will next be tried.
//
// Unauthenticated like /v1/providers and /health, and deliberately carries no
// tenant data: it is provider-fleet metadata, identical for every caller.
func (s *Server) handleBreakerStatus(w http.ResponseWriter, r *http.Request) {
	b := s.router.Breaker()
	if b == nil || !b.Enabled() {
		// Distinguish "enabled and nothing ejected" from "not running at all".
		// Reporting an empty list for both would let a disabled breaker read
		// as a perfectly healthy one.
		writeBreakerJSON(w, map[string]interface{}{
			"enabled": false,
			"targets": []breaker.Status{},
		})
		return
	}

	statuses, err := b.AllStatus(r.Context())
	if err != nil {
		s.writeErrorCtx(w, r, http.StatusInternalServerError, "breaker status unavailable: "+err.Error())
		return
	}
	if statuses == nil {
		statuses = []breaker.Status{}
	}

	cfg := b.Config()
	writeBreakerJSON(w, map[string]interface{}{
		"enabled": true,
		"targets": statuses,
		// The effective config travels with the state so the panel can explain
		// a decision in terms of the thresholds that produced it, rather than
		// making the operator cross-reference a deployment manifest.
		"config": map[string]interface{}{
			"consecutive_errors": cfg.ConsecutiveErrors,
			"error_rate_percent": cfg.ErrorRatePercent,
			"min_requests":       cfg.MinRequests,
			"window_seconds":     int(cfg.Window.Seconds()),
			"eject_for_seconds":  int(cfg.EjectFor.Seconds()),
			"retry_ratio":        cfg.RetryRatio,
			"min_retries":        cfg.MinRetries,
		},
	})
}

func writeBreakerJSON(w http.ResponseWriter, body map[string]interface{}) {
	body["timestamp"] = time.Now().Unix()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(body)
}
