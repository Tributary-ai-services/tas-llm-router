package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/routing"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/breaker"
)

// breakerStatusServer builds a minimal Server whose router has a breaker
// constructed (or not) and a gateway default set, mirroring production where
// the breaker is construct-always and AIQG_BREAKER_ENABLED decides the default.
func breakerStatusServer(t *testing.T, construct, defaultEnabled bool) *Server {
	t.Helper()
	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)
	r := routing.NewRouter(logger)
	if construct {
		r.SetBreaker(breaker.New(breaker.NewMemoryStore(), breaker.Config{ConsecutiveErrors: 2}))
	}
	r.SetBreakerDefaultEnabled(defaultEnabled)
	return &Server{logger: logger, router: r}
}

func getBreakerStatus(t *testing.T, s *Server) map[string]interface{} {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/breaker", nil)
	s.handleBreakerStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	return body
}

// The regression at the heart of #185: with the breaker construct-always and
// the default OFF, the endpoint used to report enabled:true (object existence),
// which made the health panel's "not running" branch — keyed on !enabled —
// unreachable. It must now report the effective default, i.e. enabled:false.
func TestBreakerStatus_ConstructedDefaultOff_ReportsDisabled(t *testing.T) {
	s := breakerStatusServer(t, true /*construct*/, false /*default*/)
	body := getBreakerStatus(t, s)

	if body["enabled"] != false {
		t.Errorf("enabled = %v, want false (effective default is off)", body["enabled"])
	}
	if body["constructed"] != true {
		t.Errorf("constructed = %v, want true", body["constructed"])
	}
	if body["state"] != "off" {
		t.Errorf("state = %v, want \"off\"", body["state"])
	}
}

func TestBreakerStatus_ConstructedDefaultOn_ReportsEnabled(t *testing.T) {
	s := breakerStatusServer(t, true /*construct*/, true /*default*/)
	body := getBreakerStatus(t, s)

	if body["enabled"] != true {
		t.Errorf("enabled = %v, want true", body["enabled"])
	}
	if body["constructed"] != true {
		t.Errorf("constructed = %v, want true", body["constructed"])
	}
	if body["state"] != "on" {
		t.Errorf("state = %v, want \"on\"", body["state"])
	}
	// config travels with the enabled state so the panel need not read a manifest.
	if _, ok := body["config"]; !ok {
		t.Error("expected config block when constructed")
	}
}

func TestBreakerStatus_NotConstructed_ReportsUnavailable(t *testing.T) {
	s := breakerStatusServer(t, false /*construct*/, false /*default*/)
	body := getBreakerStatus(t, s)

	if body["enabled"] != false {
		t.Errorf("enabled = %v, want false", body["enabled"])
	}
	if body["constructed"] != false {
		t.Errorf("constructed = %v, want false", body["constructed"])
	}
	if body["state"] != "unavailable" {
		t.Errorf("state = %v, want \"unavailable\"", body["state"])
	}
}
