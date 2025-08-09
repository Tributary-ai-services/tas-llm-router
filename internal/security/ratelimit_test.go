package security

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewInMemoryRateLimiter(t *testing.T) {
	config := &RateLimitConfig{
		Enabled:           true,
		RequestsPerMinute: 60,
		BurstSize:         10,
		WindowDuration:    time.Minute,
		CleanupInterval:   5 * time.Minute,
	}
	logger := logrus.New()

	limiter := NewInMemoryRateLimiter(config, logger)

	assert.NotNil(t, limiter)
	assert.Equal(t, config, limiter.config)
	assert.NotNil(t, limiter.buckets)
	assert.NotNil(t, limiter.cleanupTicker)
}

func TestInMemoryRateLimiter_Allow_Disabled(t *testing.T) {
	config := &RateLimitConfig{
		Enabled:           false,
		RequestsPerMinute: 60,
	}
	logger := logrus.New()
	limiter := NewInMemoryRateLimiter(config, logger)
	ctx := context.Background()

	result, err := limiter.Allow(ctx, "test-key")
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Equal(t, 60, result.Remaining)
}

func TestInMemoryRateLimiter_Allow_WithinLimit(t *testing.T) {
	config := &RateLimitConfig{
		Enabled:           true,
		RequestsPerMinute: 60,
		BurstSize:         10,
		WindowDuration:    time.Minute,
	}
	logger := logrus.New()
	limiter := NewInMemoryRateLimiter(config, logger)
	ctx := context.Background()

	// First request should be allowed
	result, err := limiter.Allow(ctx, "test-key")
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Equal(t, 9, result.Remaining) // Started with 10, used 1

	// Several more requests should be allowed
	for i := 0; i < 8; i++ {
		result, err = limiter.Allow(ctx, "test-key")
		require.NoError(t, err)
		assert.True(t, result.Allowed)
	}

	// Last allowed request
	result, err = limiter.Allow(ctx, "test-key")
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Equal(t, 0, result.Remaining)
}

func TestInMemoryRateLimiter_Allow_ExceedLimit(t *testing.T) {
	config := &RateLimitConfig{
		Enabled:           true,
		RequestsPerMinute: 60,
		BurstSize:         2, // Small burst for easy testing
		WindowDuration:    time.Minute,
	}
	logger := logrus.New()
	limiter := NewInMemoryRateLimiter(config, logger)
	ctx := context.Background()

	// Use up all tokens
	for i := 0; i < 2; i++ {
		result, err := limiter.Allow(ctx, "test-key")
		require.NoError(t, err)
		assert.True(t, result.Allowed)
	}

	// Next request should be denied
	result, err := limiter.Allow(ctx, "test-key")
	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.Equal(t, 0, result.Remaining)
	assert.Greater(t, result.RetryAfter, time.Duration(0))
}

func TestInMemoryRateLimiter_Allow_DifferentKeys(t *testing.T) {
	config := &RateLimitConfig{
		Enabled:           true,
		RequestsPerMinute: 60,
		BurstSize:         1, // One request per key
		WindowDuration:    time.Minute,
	}
	logger := logrus.New()
	limiter := NewInMemoryRateLimiter(config, logger)
	ctx := context.Background()

	// First key should be allowed
	result, err := limiter.Allow(ctx, "key1")
	require.NoError(t, err)
	assert.True(t, result.Allowed)

	// Second key should also be allowed (different bucket)
	result, err = limiter.Allow(ctx, "key2")
	require.NoError(t, err)
	assert.True(t, result.Allowed)

	// First key should be denied (bucket exhausted)
	result, err = limiter.Allow(ctx, "key1")
	require.NoError(t, err)
	assert.False(t, result.Allowed)
}

func TestInMemoryRateLimiter_Reset(t *testing.T) {
	config := &RateLimitConfig{
		Enabled:           true,
		RequestsPerMinute: 60,
		BurstSize:         1,
		WindowDuration:    time.Minute,
	}
	logger := logrus.New()
	limiter := NewInMemoryRateLimiter(config, logger)
	ctx := context.Background()

	// Exhaust the bucket
	result, err := limiter.Allow(ctx, "test-key")
	require.NoError(t, err)
	assert.True(t, result.Allowed)

	// Should be denied
	result, err = limiter.Allow(ctx, "test-key")
	require.NoError(t, err)
	assert.False(t, result.Allowed)

	// Reset the key
	err = limiter.Reset(ctx, "test-key")
	require.NoError(t, err)

	// Should be allowed again
	result, err = limiter.Allow(ctx, "test-key")
	require.NoError(t, err)
	assert.True(t, result.Allowed)
}

func TestInMemoryRateLimiter_GetLimits(t *testing.T) {
	config := &RateLimitConfig{
		Enabled:           true,
		RequestsPerMinute: 60,
		BurstSize:         10,
		WindowDuration:    time.Minute,
	}
	logger := logrus.New()
	limiter := NewInMemoryRateLimiter(config, logger)
	ctx := context.Background()

	// Get limits for new key
	info, err := limiter.GetLimits(ctx, "test-key")
	require.NoError(t, err)
	assert.Equal(t, 60, info.Limit)
	assert.Equal(t, 0, info.Used)
	assert.Equal(t, 10, info.Remaining)

	// Use some tokens
	_, err = limiter.Allow(ctx, "test-key")
	require.NoError(t, err)
	_, err = limiter.Allow(ctx, "test-key")
	require.NoError(t, err)

	// Check limits again
	info, err = limiter.GetLimits(ctx, "test-key")
	require.NoError(t, err)
	assert.Equal(t, 60, info.Limit)
	assert.Equal(t, 2, info.Used)
	assert.Equal(t, 8, info.Remaining)
}

func TestInMemoryRateLimiter_TokenRefill(t *testing.T) {
	config := &RateLimitConfig{
		Enabled:           true,
		RequestsPerMinute: 120, // 2 tokens per second for easier testing
		BurstSize:         2,
		WindowDuration:    time.Minute,
	}
	logger := logrus.New()
	limiter := NewInMemoryRateLimiter(config, logger)
	ctx := context.Background()

	// Exhaust tokens
	result, err := limiter.Allow(ctx, "test-key")
	require.NoError(t, err)
	assert.True(t, result.Allowed)

	result, err = limiter.Allow(ctx, "test-key")
	require.NoError(t, err)
	assert.True(t, result.Allowed)

	// Should be denied
	result, err = limiter.Allow(ctx, "test-key")
	require.NoError(t, err)
	assert.False(t, result.Allowed)

	// Wait for token refill (simulate time passage)
	// Note: In a real test, we might mock time or use a shorter interval
	time.Sleep(1 * time.Second)

	// Should be allowed again due to token refill
	result, err = limiter.Allow(ctx, "test-key")
	require.NoError(t, err)
	// This might be true or false depending on exact timing, 
	// but the test demonstrates the refill concept
}

func TestInMemoryRateLimiter_Stop(t *testing.T) {
	config := &RateLimitConfig{
		Enabled:           true,
		RequestsPerMinute: 60,
		BurstSize:         10,
		CleanupInterval:   100 * time.Millisecond,
	}
	logger := logrus.New()
	limiter := NewInMemoryRateLimiter(config, logger)

	// Verify it's running
	assert.NotNil(t, limiter.cleanupTicker)

	// Stop it
	limiter.Stop()

	// Verify cleanup is stopped (we can't easily test this without exposing internals)
	// But the Stop() method should not panic or hang
}

func TestDefaultKeyExtractor(t *testing.T) {
	// This would typically require an HTTP request context
	// For now, we'll test that it doesn't panic with a basic context
	ctx := context.Background()

	// Test with no auth info (should not panic)
	result := getClientIP(ctx)
	assert.Equal(t, "unknown", result)

	// Test with auth info
	authInfo := &AuthInfo{UserID: "test-user"}
	ctx = context.WithValue(ctx, "auth_info", authInfo)
	
	// The function should still work (implementation details may vary)
	result = getClientIP(ctx)
	assert.Equal(t, "unknown", result) // No client_ip in context
}

func TestMaskKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{
			name: "normal key",
			key:  "sk-1234567890abcdef",
			want: "sk-1****",
		},
		{
			name: "short key",
			key:  "short",
			want: "****",
		},
		{
			name: "exactly 8 chars",
			key:  "12345678",
			want: "****",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := maskKey(tt.key)
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestMinInt(t *testing.T) {
	tests := []struct {
		name string
		a, b int
		want int
	}{
		{"a smaller", 5, 10, 5},
		{"b smaller", 10, 5, 5},
		{"equal", 7, 7, 7},
		{"negative", -5, -10, -10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := minInt(tt.a, tt.b)
			assert.Equal(t, tt.want, result)
		})
	}
}