package middleware

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestAuthRejection_AnthropicWireShape(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard) // silence

	// /v1/messages → Anthropic error envelope
	t.Run("messages path -> anthropic error", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		rejectPathA(log, w, r, "TAS-Auth")
		if w.Code != 401 {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		var body struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("bad json: %v (%s)", err, w.Body.String())
		}
		if body.Type != "error" || body.Error.Type != "authentication_error" {
			t.Errorf("not Anthropic-shaped: %s", w.Body.String())
		}
	})

	// chat/completions → keep the standard (OpenAI-ish) envelope
	t.Run("chat path -> standard error", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		rejectPathA(log, w, r, "TAS-Auth")
		if w.Code != 401 {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		if body.Error.Code != "path_a_auth_required" {
			t.Errorf("expected standard envelope, got: %s", w.Body.String())
		}
	})
}

func TestRecoverGatewayToken(t *testing.T) {
	tok := authTokenPrefix + "abc123"

	tests := []struct {
		name       string
		authHeader string
		xAPIKey    string
		wantTok    string
		wantFrom   tokenSource
	}{
		{
			name:       "openai sdk bearer",
			authHeader: "Bearer " + tok,
			wantTok:    tok,
			wantFrom:   tokenFromAuthorization,
		},
		{
			name:       "bare authorization",
			authHeader: tok,
			wantTok:    tok,
			wantFrom:   tokenFromAuthorization,
		},
		{
			name:     "anthropic sdk x-api-key",
			xAPIKey:  tok,
			wantTok:  tok,
			wantFrom: tokenFromXAPIKey,
		},
		{
			name:       "real openai vendor key ignored",
			authHeader: "Bearer sk-proj-realkey",
			wantTok:    "",
			wantFrom:   tokenFromNone,
		},
		{
			name:     "real anthropic vendor key ignored",
			xAPIKey:  "sk-ant-realkey",
			wantTok:  "",
			wantFrom: tokenFromNone,
		},
		{
			name:     "nothing",
			wantTok:  "",
			wantFrom: tokenFromNone,
		},
		{
			name:       "authorization wins over x-api-key",
			authHeader: "Bearer " + tok,
			xAPIKey:    authTokenPrefix + "other",
			wantTok:    tok,
			wantFrom:   tokenFromAuthorization,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			if tt.authHeader != "" {
				r.Header.Set("Authorization", tt.authHeader)
			}
			if tt.xAPIKey != "" {
				r.Header.Set("X-Api-Key", tt.xAPIKey)
			}
			gotTok, gotFrom := recoverGatewayToken(r)
			if gotTok != tt.wantTok {
				t.Errorf("token = %q, want %q", gotTok, tt.wantTok)
			}
			if gotFrom != tt.wantFrom {
				t.Errorf("source = %v, want %v", gotFrom, tt.wantFrom)
			}
		})
	}
}
