package middleware

import (
	"net/http/httptest"
	"testing"
)

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
