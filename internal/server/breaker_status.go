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
	constructed := b != nil && b.Enabled()
	// The breaker is now constructed unconditionally (a store is always
	// attached), so b.Enabled() — object existence — is permanently true in
	// production and cannot answer "is ejection actually in effect?". The value
	// that decides for a request carrying no tenant control is the gateway-wide
	// default, the third term of router.breakerEnabled. Report THAT as
	// `enabled`, so the health panel's `!enabled` "not running" branch is
	// reachable again when the default is off (issue #185).
	defaultEnabled := s.router.BreakerDefaultEnabled()

	// `state` carries the finer distinction the panel wants, which a single
	// boolean cannot:
	//   unavailable — no store attached; the breaker cannot act for anyone.
	//   off         — constructed but off by default. An empty target list here
	//                 is NOT a clean bill of health: nothing is watched for a
	//                 tenant that has not forced cooldown on.
	//   on          — constructed and on by default, fleet-wide.
	state := "unavailable"
	if constructed {
		if defaultEnabled {
			state = "on"
		} else {
			state = "off"
		}
	}

	if !constructed {
		writeBreakerJSON(w, map[string]interface{}{
			"enabled":     false,
			"constructed": false,
			"state":       state,
			"targets":     []breaker.Status{},
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
		// `enabled` is the effective default, not object existence. `targets`
		// and `config` are still reported when constructed — a tenant that
		// forces cooldown on gets ejection even when the default is off, and
		// those ejections belong in the surface — but `enabled`/`state` now tell
		// the operator whether the empty case means "watched, nothing ejected"
		// or "not watched by default".
		"enabled":     defaultEnabled,
		"constructed": true,
		"state":       state,
		"targets":     statuses,
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
