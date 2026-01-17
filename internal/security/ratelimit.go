package security

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// RateLimiter defines the interface for rate limiting
type RateLimiter interface {
	Allow(ctx context.Context, key string) (*RateLimitResult, error)
	Reset(ctx context.Context, key string) error
	GetLimits(ctx context.Context, key string) (*RateLimitInfo, error)
}

// RateLimitResult contains the result of a rate limit check
type RateLimitResult struct {
	Allowed    bool          `json:"allowed"`
	Remaining  int           `json:"remaining"`
	ResetTime  time.Time     `json:"reset_time"`
	RetryAfter time.Duration `json:"retry_after"`
}

// RateLimitInfo contains current rate limit status
type RateLimitInfo struct {
	Limit     int       `json:"limit"`
	Used      int       `json:"used"`
	Remaining int       `json:"remaining"`
	ResetTime time.Time `json:"reset_time"`
}

// RateLimitConfig holds rate limiting configuration
type RateLimitConfig struct {
	Enabled           bool          `yaml:"enabled"`
	RequestsPerMinute int           `yaml:"requests_per_minute"`
	BurstSize         int           `yaml:"burst_size"`
	WindowDuration    time.Duration `yaml:"window_duration"`
	CleanupInterval   time.Duration `yaml:"cleanup_interval"`
	RedisURL          string        `yaml:"redis_url"`
}

// InMemoryRateLimiter implements rate limiting using in-memory storage
type InMemoryRateLimiter struct {
	config *RateLimitConfig
	logger *logrus.Logger
	
	// In-memory storage
	buckets map[string]*tokenBucket
	mutex   sync.RWMutex
	
	// Cleanup ticker
	cleanupTicker *time.Ticker
	stopCleanup   chan bool
	stopped       bool
}

// tokenBucket represents a token bucket for rate limiting
type tokenBucket struct {
	tokens    int
	lastRefill time.Time
	mutex     sync.Mutex
}

// NewInMemoryRateLimiter creates a new in-memory rate limiter
func NewInMemoryRateLimiter(config *RateLimitConfig, logger *logrus.Logger) *InMemoryRateLimiter {
	if config.WindowDuration == 0 {
		config.WindowDuration = time.Minute
	}
	if config.CleanupInterval == 0 {
		config.CleanupInterval = 5 * time.Minute
	}
	if config.BurstSize == 0 {
		config.BurstSize = config.RequestsPerMinute
	}
	
	rl := &InMemoryRateLimiter{
		config:      config,
		logger:      logger,
		buckets:     make(map[string]*tokenBucket),
		stopCleanup: make(chan bool),
	}
	
	// Start cleanup goroutine
	rl.startCleanup()
	
	return rl
}

// Allow checks if a request is allowed under the rate limit
func (rl *InMemoryRateLimiter) Allow(ctx context.Context, key string) (*RateLimitResult, error) {
	if !rl.config.Enabled {
		return &RateLimitResult{
			Allowed:   true,
			Remaining: rl.config.RequestsPerMinute,
			ResetTime: time.Now().Add(rl.config.WindowDuration),
		}, nil
	}
	
	now := time.Now()
	bucket := rl.getOrCreateBucket(key)
	
	bucket.mutex.Lock()
	defer bucket.mutex.Unlock()
	
	// Refill tokens based on elapsed time
	elapsed := now.Sub(bucket.lastRefill)
	if elapsed > 0 {
		tokensToAdd := int(elapsed.Minutes() * float64(rl.config.RequestsPerMinute))
		bucket.tokens = minInt(bucket.tokens+tokensToAdd, rl.config.BurstSize)
		bucket.lastRefill = now
	}
	
	// Check if request is allowed
	if bucket.tokens > 0 {
		bucket.tokens--
		return &RateLimitResult{
			Allowed:   true,
			Remaining: bucket.tokens,
			ResetTime: now.Add(rl.config.WindowDuration),
		}, nil
	}
	
	// Request denied
	retryAfter := time.Duration(float64(time.Minute) / float64(rl.config.RequestsPerMinute))
	
	rl.logger.WithFields(logrus.Fields{
		"key":         maskKey(key),
		"retry_after": retryAfter,
	}).Warn("Rate limit exceeded")
	
	return &RateLimitResult{
		Allowed:    false,
		Remaining:  0,
		ResetTime:  now.Add(retryAfter),
		RetryAfter: retryAfter,
	}, nil
}

// Reset resets the rate limit for a key
func (rl *InMemoryRateLimiter) Reset(ctx context.Context, key string) error {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	
	delete(rl.buckets, key)
	
	rl.logger.WithField("key", maskKey(key)).Info("Rate limit reset")
	return nil
}

// GetLimits returns current rate limit information for a key
func (rl *InMemoryRateLimiter) GetLimits(ctx context.Context, key string) (*RateLimitInfo, error) {
	bucket := rl.getOrCreateBucket(key)
	
	bucket.mutex.Lock()
	defer bucket.mutex.Unlock()
	
	now := time.Now()
	
	// Calculate current state
	elapsed := now.Sub(bucket.lastRefill)
	tokensToAdd := int(elapsed.Minutes() * float64(rl.config.RequestsPerMinute))
	currentTokens := minInt(bucket.tokens+tokensToAdd, rl.config.BurstSize)
	
	return &RateLimitInfo{
		Limit:     rl.config.RequestsPerMinute,
		Used:      rl.config.BurstSize - currentTokens,
		Remaining: currentTokens,
		ResetTime: now.Add(rl.config.WindowDuration),
	}, nil
}

// getOrCreateBucket gets or creates a token bucket for a key
func (rl *InMemoryRateLimiter) getOrCreateBucket(key string) *tokenBucket {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	
	bucket, exists := rl.buckets[key]
	if !exists {
		bucket = &tokenBucket{
			tokens:    rl.config.BurstSize,
			lastRefill: time.Now(),
		}
		rl.buckets[key] = bucket
	}
	
	return bucket
}

// startCleanup starts the cleanup goroutine to remove old buckets
func (rl *InMemoryRateLimiter) startCleanup() {
	rl.cleanupTicker = time.NewTicker(rl.config.CleanupInterval)
	
	go func() {
		for {
			select {
			case <-rl.cleanupTicker.C:
				rl.cleanup()
			case <-rl.stopCleanup:
				return
			}
		}
	}()
}

// cleanup removes buckets that haven't been used recently
func (rl *InMemoryRateLimiter) cleanup() {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	
	now := time.Now()
	cutoff := now.Add(-2 * rl.config.WindowDuration)
	
	removed := 0
	for key, bucket := range rl.buckets {
		bucket.mutex.Lock()
		if bucket.lastRefill.Before(cutoff) {
			delete(rl.buckets, key)
			removed++
		}
		bucket.mutex.Unlock()
	}
	
	if removed > 0 {
		rl.logger.WithField("removed_buckets", removed).Debug("Rate limit cleanup completed")
	}
}

// Stop stops the rate limiter and cleanup goroutine
func (rl *InMemoryRateLimiter) Stop() {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	
	if rl.stopped {
		return
	}
	
	rl.stopped = true
	if rl.cleanupTicker != nil {
		rl.cleanupTicker.Stop()
	}
	close(rl.stopCleanup)
}

// RateLimitMiddleware creates rate limiting middleware
func RateLimitMiddleware(rateLimiter RateLimiter, keyExtractor func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract rate limiting key
			key := keyExtractor(r)
			if key == "" {
				// If no key can be extracted, allow the request
				next.ServeHTTP(w, r)
				return
			}
			
			// Check rate limit
			result, err := rateLimiter.Allow(r.Context(), key)
			if err != nil {
				// Log error but allow request to proceed
				http.Error(w, "Rate limiting error", http.StatusInternalServerError)
				return
			}
			
			// Add rate limit headers
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(result.Remaining+1))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(result.Remaining))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(result.ResetTime.Unix(), 10))
			
			if !result.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(result.RetryAfter.Seconds())))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				
				response := fmt.Sprintf(`{
					"error": {
						"message": "Rate limit exceeded",
						"type": "rate_limit_error",
						"code": 429,
						"retry_after": %d
					},
					"timestamp": %d
				}`, int(result.RetryAfter.Seconds()), time.Now().Unix())
				
				w.Write([]byte(response))
				return
			}
			
			next.ServeHTTP(w, r)
		})
	}
}

// DefaultKeyExtractor extracts rate limiting key from request
func DefaultKeyExtractor(r *http.Request) string {
	// Try to get user ID from auth info
	if authInfo, ok := r.Context().Value("auth_info").(*AuthInfo); ok {
		return "user:" + authInfo.UserID
	}
	
	// Fall back to IP address
	return "ip:" + getClientIPFromRequest(r)
}

// APIKeyExtractor extracts rate limiting key from API key
func APIKeyExtractor(r *http.Request) string {
	token := extractToken(r)
	if token != "" {
		return "key:" + maskKey(token)
	}
	return "ip:" + getClientIPFromRequest(r)
}

// Helper functions

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****"
}