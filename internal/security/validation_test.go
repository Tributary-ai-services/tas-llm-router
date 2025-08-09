package security

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRequestValidator(t *testing.T) {
	config := &ValidationConfig{
		MaxRequestSize:      1024,
		AllowedMethods:      []string{"GET", "POST"},
		BlockedPatterns:     []string{"(?i)script"},
		MaxJSONDepth:        10,
		MaxFieldLength:      100,
		UserAgentPatterns:   []string{"MyApp/.*"},
	}
	logger := logrus.New()

	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)
	assert.NotNil(t, validator)
	assert.Equal(t, config, validator.config)
	assert.Len(t, validator.blockedRegexes, 1)
	assert.Len(t, validator.uaRegexes, 1)
}

func TestNewRequestValidator_InvalidPattern(t *testing.T) {
	config := &ValidationConfig{
		BlockedPatterns: []string{"[invalid regex"},
	}
	logger := logrus.New()

	validator, err := NewRequestValidator(config, logger)
	assert.Error(t, err)
	assert.Nil(t, validator)
	assert.Contains(t, err.Error(), "invalid blocked pattern")
}

func TestRequestValidator_ValidateRequest_ValidRequest(t *testing.T) {
	config := &ValidationConfig{
		MaxRequestSize:    1024,
		AllowedMethods:    []string{"GET", "POST"},
		ContentTypes:      []string{"application/json"},
		RequiredHeaders:   []string{"Content-Type"},
		IPWhitelist:       []string{"192.168.1.0/24"},
	}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	req := httptest.NewRequest("POST", "/test", strings.NewReader(`{"test": "data"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.168.1.100:12345"
	req.ContentLength = 15

	result, err := validator.ValidateRequest(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, result.Valid)
	assert.Empty(t, result.Errors)
}

func TestRequestValidator_ValidateRequest_InvalidMethod(t *testing.T) {
	config := &ValidationConfig{
		AllowedMethods: []string{"GET", "POST"},
	}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	req := httptest.NewRequest("DELETE", "/test", nil)

	result, err := validator.ValidateRequest(context.Background(), req)
	require.NoError(t, err)
	assert.False(t, result.Valid)
	assert.Contains(t, result.Errors, "Method DELETE not allowed")
}

func TestRequestValidator_ValidateRequest_RequestTooLarge(t *testing.T) {
	config := &ValidationConfig{
		MaxRequestSize: 100,
	}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	req := httptest.NewRequest("POST", "/test", nil)
	req.ContentLength = 200

	result, err := validator.ValidateRequest(context.Background(), req)
	require.NoError(t, err)
	assert.False(t, result.Valid)
	assert.Contains(t, result.Errors, "Request size 200 exceeds maximum 100")
}

func TestRequestValidator_ValidateRequest_InvalidContentType(t *testing.T) {
	config := &ValidationConfig{
		ContentTypes: []string{"application/json"},
	}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	req := httptest.NewRequest("POST", "/test", nil)
	req.Header.Set("Content-Type", "text/plain")

	result, err := validator.ValidateRequest(context.Background(), req)
	require.NoError(t, err)
	assert.False(t, result.Valid)
	assert.Contains(t, result.Errors, "Content-Type text/plain not allowed")
}

func TestRequestValidator_ValidateRequest_MissingRequiredHeader(t *testing.T) {
	config := &ValidationConfig{
		RequiredHeaders: []string{"Authorization"},
	}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/test", nil)

	result, err := validator.ValidateRequest(context.Background(), req)
	require.NoError(t, err)
	assert.False(t, result.Valid)
	assert.Contains(t, result.Errors, "Required header Authorization missing")
}

func TestRequestValidator_ValidateRequest_BlockedPattern(t *testing.T) {
	config := &ValidationConfig{
		BlockedPatterns: []string{"(?i)script"},
	}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/test?param=<script>alert(1)</script>", nil)

	result, err := validator.ValidateRequest(context.Background(), req)
	require.NoError(t, err)
	assert.False(t, result.Valid)
	assert.Contains(t, result.Errors, "Request contains blocked patterns")
}

func TestRequestValidator_ValidateJSON_ValidJSON(t *testing.T) {
	config := &ValidationConfig{
		MaxJSONDepth:   5,
		MaxFieldLength: 100,
	}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	jsonData := []byte(`{"name": "test", "value": 123, "nested": {"key": "value"}}`)

	result, err := validator.ValidateJSON(context.Background(), jsonData)
	require.NoError(t, err)
	assert.True(t, result.Valid)
	assert.Empty(t, result.Errors)
}

func TestRequestValidator_ValidateJSON_InvalidJSON(t *testing.T) {
	config := &ValidationConfig{}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	jsonData := []byte(`{"name": "test", invalid json}`)

	result, err := validator.ValidateJSON(context.Background(), jsonData)
	require.NoError(t, err)
	assert.False(t, result.Valid)
	assert.NotEmpty(t, result.Errors)
	assert.Contains(t, result.Errors[0], "Invalid JSON")
}

func TestRequestValidator_ValidateJSON_TooDeep(t *testing.T) {
	config := &ValidationConfig{
		MaxJSONDepth: 2,
	}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	// JSON with depth 3
	jsonData := []byte(`{"level1": {"level2": {"level3": "value"}}}`)

	result, err := validator.ValidateJSON(context.Background(), jsonData)
	require.NoError(t, err)
	assert.False(t, result.Valid)
	assert.Contains(t, result.Errors, "JSON depth 4 exceeds maximum 2")
}

func TestRequestValidator_ValidateJSON_FieldTooLong(t *testing.T) {
	config := &ValidationConfig{
		MaxFieldLength: 10,
	}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	jsonData := []byte(`{"name": "this string is longer than 10 characters"}`)

	result, err := validator.ValidateJSON(context.Background(), jsonData)
	require.NoError(t, err)
	assert.False(t, result.Valid)
	assert.Contains(t, result.Errors[0], "string field length exceeds maximum")
}

func TestRequestValidator_ValidateJSON_InvalidUTF8(t *testing.T) {
	config := &ValidationConfig{}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	// Invalid UTF-8 bytes
	jsonData := []byte{0xff, 0xfe, 0xfd}

	result, err := validator.ValidateJSON(context.Background(), jsonData)
	require.NoError(t, err)
	assert.False(t, result.Valid)
	assert.Contains(t, result.Errors[0], "invalid UTF-8")
}

func TestRequestValidator_SanitizeInput(t *testing.T) {
	config := &ValidationConfig{}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "normal string",
			input: "Hello World",
			want:  "Hello World",
		},
		{
			name:  "with null bytes",
			input: "Hello\x00World",
			want:  "HelloWorld",
		},
		{
			name:  "with control characters",
			input: "Hello\x01\x02World",
			want:  "HelloWorld",
		},
		{
			name:  "keep newlines and tabs",
			input: "Hello\n\tWorld",
			want:  "Hello\n\tWorld",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validator.SanitizeInput(tt.input)
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestRequestValidator_IsAllowedMethod(t *testing.T) {
	config := &ValidationConfig{
		AllowedMethods: []string{"GET", "POST"},
	}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	assert.True(t, validator.isAllowedMethod("GET"))
	assert.True(t, validator.isAllowedMethod("POST"))
	assert.True(t, validator.isAllowedMethod("get")) // Case insensitive
	assert.False(t, validator.isAllowedMethod("DELETE"))

	// Test with no allowed methods (should allow all)
	config.AllowedMethods = []string{}
	validator, err = NewRequestValidator(config, logger)
	require.NoError(t, err)
	assert.True(t, validator.isAllowedMethod("DELETE"))
}

func TestRequestValidator_IsAllowedContentType(t *testing.T) {
	config := &ValidationConfig{
		ContentTypes: []string{"application/json", "text/plain"},
	}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	assert.True(t, validator.isAllowedContentType("application/json"))
	assert.True(t, validator.isAllowedContentType("application/json; charset=utf-8"))
	assert.True(t, validator.isAllowedContentType("text/plain"))
	assert.False(t, validator.isAllowedContentType("text/html"))

	// Test with no content types (should allow all)
	config.ContentTypes = []string{}
	validator, err = NewRequestValidator(config, logger)
	require.NoError(t, err)
	assert.True(t, validator.isAllowedContentType("text/html"))
}

func TestRequestValidator_ContainsBlockedPattern(t *testing.T) {
	config := &ValidationConfig{
		BlockedPatterns: []string{"(?i)script", "javascript:"},
	}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	assert.True(t, validator.containsBlockedPattern("<script>alert(1)</script>"))
	assert.True(t, validator.containsBlockedPattern("<SCRIPT>alert(1)</SCRIPT>"))
	assert.True(t, validator.containsBlockedPattern("javascript:alert(1)"))
	assert.False(t, validator.containsBlockedPattern("normal text"))
}

func TestRequestValidator_SanitizeURL(t *testing.T) {
	config := &ValidationConfig{}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "normal URL",
			url:  "https://example.com/path",
			want: "https://example.com/path",
		},
		{
			name: "javascript URL",
			url:  "javascript:alert(1)",
			want: "",
		},
		{
			name: "data URL",
			url:  "data:text/html,<script>alert(1)</script>",
			want: "",
		},
		{
			name: "URL with spaces",
			url:  "  https://example.com/path  ",
			want: "https://example.com/path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validator.sanitizeURL(tt.url)
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestRequestValidator_GetJSONDepth(t *testing.T) {
	config := &ValidationConfig{}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	tests := []struct {
		name string
		data interface{}
		want int
	}{
		{
			name: "simple object",
			data: map[string]interface{}{"key": "value"},
			want: 2,
		},
		{
			name: "nested object",
			data: map[string]interface{}{
				"level1": map[string]interface{}{
					"level2": "value",
				},
			},
			want: 3,
		},
		{
			name: "array",
			data: []interface{}{"item1", "item2"},
			want: 2,
		},
		{
			name: "nested array",
			data: []interface{}{
				[]interface{}{"nested", "array"},
			},
			want: 3,
		},
		{
			name: "primitive",
			data: "string",
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validator.getJSONDepth(tt.data)
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestRequestValidator_ValidationMiddleware(t *testing.T) {
	config := &ValidationConfig{
		AllowedMethods: []string{"GET", "POST"},
	}
	logger := logrus.New()
	validator, err := NewRequestValidator(config, logger)
	require.NoError(t, err)

	// Test valid request
	handler := validator.ValidationMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "OK", w.Body.String())

	// Test invalid request
	req = httptest.NewRequest("DELETE", "/test", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "validation_error")
}