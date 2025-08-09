package security

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDefaultAuthProvider(t *testing.T) {
	config := &Config{
		APIKeys:   []string{"test-key-1", "test-key-2"},
		JWTSecret: "test-secret",
		JWTExpiry: 24 * time.Hour,
	}
	logger := logrus.New()

	provider := NewDefaultAuthProvider(config, logger)

	assert.NotNil(t, provider)
	assert.Equal(t, config, provider.config)
	assert.Equal(t, logger, provider.logger)
}

func TestDefaultAuthProvider_ValidateAPIKey(t *testing.T) {
	config := &Config{
		APIKeys: []string{"valid-key-1", "valid-key-2"},
	}
	logger := logrus.New()
	provider := NewDefaultAuthProvider(config, logger)
	ctx := context.Background()

	tests := []struct {
		name    string
		apiKey  string
		wantErr bool
	}{
		{
			name:    "valid API key 1",
			apiKey:  "valid-key-1",
			wantErr: false,
		},
		{
			name:    "valid API key 2",
			apiKey:  "valid-key-2",
			wantErr: false,
		},
		{
			name:    "invalid API key",
			apiKey:  "invalid-key",
			wantErr: true,
		},
		{
			name:    "empty API key",
			apiKey:  "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authInfo, err := provider.ValidateAPIKey(ctx, tt.apiKey)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, authInfo)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, authInfo)
				assert.NotEmpty(t, authInfo.UserID)
				assert.Equal(t, tt.apiKey, authInfo.APIKey)
				assert.Contains(t, authInfo.Permissions, "api:access")
				assert.Equal(t, "api_key", authInfo.Metadata["auth_type"])
			}
		})
	}
}

func TestDefaultAuthProvider_GenerateAndValidateJWT(t *testing.T) {
	config := &Config{
		JWTSecret: "test-secret-key-for-jwt-signing-must-be-long-enough",
		JWTExpiry: 1 * time.Hour,
	}
	logger := logrus.New()
	provider := NewDefaultAuthProvider(config, logger)

	userID := "test-user"
	claims := map[string]interface{}{
		"permissions": []string{"api:access", "admin:read"},
		"organization": "test-org",
	}

	// Generate JWT
	token, err := provider.GenerateJWT(userID, claims)
	require.NoError(t, err)
	assert.NotEmpty(t, token)

	// Validate JWT
	jwtClaims, err := provider.ValidateJWT(token)
	require.NoError(t, err)
	assert.NotNil(t, jwtClaims)
	assert.Equal(t, userID, jwtClaims.UserID)
	assert.Equal(t, []string{"api:access", "admin:read"}, jwtClaims.Permissions)
	assert.Equal(t, "test-org", jwtClaims.Metadata["organization"])
	assert.Equal(t, "llm-router-waf", jwtClaims.Issuer)
}

func TestDefaultAuthProvider_ValidateJWT_InvalidToken(t *testing.T) {
	config := &Config{
		JWTSecret: "test-secret-key-for-jwt-signing-must-be-long-enough",
		JWTExpiry: 1 * time.Hour,
	}
	logger := logrus.New()
	provider := NewDefaultAuthProvider(config, logger)

	tests := []struct {
		name  string
		token string
	}{
		{
			name:  "empty token",
			token: "",
		},
		{
			name:  "invalid token format",
			token: "not.a.jwt",
		},
		{
			name:  "malformed token",
			token: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.invalid.signature",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims, err := provider.ValidateJWT(tt.token)
			assert.Error(t, err)
			assert.Nil(t, claims)
		})
	}
}

func TestDefaultAuthProvider_Authenticate(t *testing.T) {
	config := &Config{
		APIKeys:   []string{"api-key-test"},
		JWTSecret: "test-secret-key-for-jwt-signing-must-be-long-enough",
		JWTExpiry: 1 * time.Hour,
	}
	logger := logrus.New()
	provider := NewDefaultAuthProvider(config, logger)
	ctx := context.Background()

	// Test with API key
	authInfo, err := provider.Authenticate(ctx, "api-key-test")
	assert.NoError(t, err)
	assert.NotNil(t, authInfo)
	assert.Equal(t, "api-key-test", authInfo.APIKey)

	// Test with JWT
	jwtToken, err := provider.GenerateJWT("test-user", map[string]interface{}{
		"permissions": []string{"api:access"},
	})
	require.NoError(t, err)

	authInfo, err = provider.Authenticate(ctx, jwtToken)
	assert.NoError(t, err)
	assert.NotNil(t, authInfo)
	assert.Equal(t, "test-user", authInfo.UserID)
	assert.Contains(t, authInfo.Permissions, "api:access")

	// Test with invalid token
	authInfo, err = provider.Authenticate(ctx, "invalid-token")
	assert.Error(t, err)
	assert.Nil(t, authInfo)
}

func TestGenerateUserID(t *testing.T) {
	tests := []struct {
		name   string
		apiKey string
		want   string
	}{
		{
			name:   "normal API key",
			apiKey: "sk-1234567890abcdef",
			want:   "user_sk-12345",
		},
		{
			name:   "short API key",
			apiKey: "short",
			want:   "user_short",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := generateUserID(tt.apiKey)
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestMaskAPIKey(t *testing.T) {
	tests := []struct {
		name   string
		apiKey string
		want   string
	}{
		{
			name:   "normal API key",
			apiKey: "sk-1234567890abcdef",
			want:   "sk-1****cdef",
		},
		{
			name:   "short API key",
			apiKey: "short",
			want:   "****",
		},
		{
			name:   "exactly 8 chars",
			apiKey: "12345678",
			want:   "****",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := maskAPIKey(tt.apiKey)
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestGetAuthInfo(t *testing.T) {
	// Test with valid auth info
	authInfo := &AuthInfo{
		UserID:      "test-user",
		Permissions: []string{"api:access"},
	}
	ctx := context.WithValue(context.Background(), "auth_info", authInfo)

	result, ok := GetAuthInfo(ctx)
	assert.True(t, ok)
	assert.Equal(t, authInfo, result)

	// Test with no auth info
	emptyCtx := context.Background()
	result, ok = GetAuthInfo(emptyCtx)
	assert.False(t, ok)
	assert.Nil(t, result)

	// Test with wrong type
	wrongCtx := context.WithValue(context.Background(), "auth_info", "not-auth-info")
	result, ok = GetAuthInfo(wrongCtx)
	assert.False(t, ok)
	assert.Nil(t, result)
}