package gatekeeper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// BypassToken represents a stored bypass token.
type BypassToken struct {
	TokenID   string    `json:"token_id"`
	UserID    string    `json:"user_id"`
	TenantID  string    `json:"tenant_id"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked   bool      `json:"revoked"`
}

// BypassTokenStore defines the interface for token persistence.
type BypassTokenStore interface {
	// Store saves a bypass token keyed by its hash.
	Store(ctx context.Context, hash string, token *BypassToken, ttl time.Duration) error
	// Lookup retrieves a bypass token by its hash.
	Lookup(ctx context.Context, hash string) (*BypassToken, error)
	// Delete removes a bypass token by its token ID.
	Delete(ctx context.Context, tokenID string) error
	// ListByUser returns all active tokens for a user.
	ListByUser(ctx context.Context, userID string) ([]*BypassToken, error)
}

// BypassAuditLogger defines the interface for audit logging.
type BypassAuditLogger interface {
	LogBypassEvent(ctx context.Context, event BypassAuditEvent) error
}

// BypassAuditEvent represents a bypass audit event.
type BypassAuditEvent struct {
	EventType   string    `json:"event_type"`
	Severity    string    `json:"severity"`
	UserID      string    `json:"user_id"`
	TenantID    string    `json:"tenant_id"`
	TokenID     string    `json:"token_id"`
	RequestID   string    `json:"request_id"`
	ContentHash string    `json:"content_hash"`
	Reason      string    `json:"reason"`
	Timestamp   time.Time `json:"timestamp"`
}

// BypassManager manages scan bypass tokens.
type BypassManager struct {
	store  BypassTokenStore
	audit  BypassAuditLogger
	logger *logrus.Logger
}

// NewBypassManager creates a new bypass manager.
func NewBypassManager(store BypassTokenStore, audit BypassAuditLogger, logger *logrus.Logger) *BypassManager {
	if logger == nil {
		logger = logrus.New()
	}
	return &BypassManager{
		store:  store,
		audit:  audit,
		logger: logger,
	}
}

// GenerateToken creates a new bypass token for a user.
func (bm *BypassManager) GenerateToken(ctx context.Context, userID, tenantID, reason string, ttlHours int) (string, *BypassToken, error) {
	if ttlHours <= 0 || ttlHours > 24 {
		return "", nil, fmt.Errorf("ttl_hours must be between 1 and 24")
	}

	tokenID := uuid.New().String()
	rawToken := fmt.Sprintf("gk_bypass_%s", uuid.New().String())
	ttl := time.Duration(ttlHours) * time.Hour

	token := &BypassToken{
		TokenID:   tokenID,
		UserID:    userID,
		TenantID:  tenantID,
		Reason:    reason,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(ttl),
	}

	hash := hashToken(rawToken)
	if err := bm.store.Store(ctx, hash, token, ttl); err != nil {
		return "", nil, fmt.Errorf("failed to store bypass token: %w", err)
	}

	bm.logger.WithFields(logrus.Fields{
		"token_id":  tokenID,
		"user_id":   userID,
		"ttl_hours": ttlHours,
	}).Info("Bypass token generated")

	return rawToken, token, nil
}

// ValidateBypassToken checks if a bypass token is valid.
// Returns (userID, valid). Always logs the bypass attempt.
func (bm *BypassManager) ValidateBypassToken(ctx context.Context, rawToken, requestID string, contentHash string) (string, bool) {
	hash := hashToken(rawToken)

	token, err := bm.store.Lookup(ctx, hash)
	if err != nil || token == nil {
		bm.logger.WithField("request_id", requestID).Debug("Bypass token not found")
		return "", false
	}

	// Check expiry
	if time.Now().After(token.ExpiresAt) {
		bm.logger.WithFields(logrus.Fields{
			"token_id":   token.TokenID,
			"request_id": requestID,
		}).Debug("Bypass token expired")
		return "", false
	}

	// Check revocation
	if token.Revoked {
		bm.logger.WithFields(logrus.Fields{
			"token_id":   token.TokenID,
			"request_id": requestID,
		}).Debug("Bypass token revoked")
		return "", false
	}

	// Log audit event
	if bm.audit != nil {
		_ = bm.audit.LogBypassEvent(ctx, BypassAuditEvent{
			EventType:   "scan_bypass_used",
			Severity:    "warning",
			UserID:      token.UserID,
			TenantID:    token.TenantID,
			TokenID:     token.TokenID,
			RequestID:   requestID,
			ContentHash: contentHash,
			Reason:      token.Reason,
			Timestamp:   time.Now(),
		})
	}

	bm.logger.WithFields(logrus.Fields{
		"token_id":   token.TokenID,
		"user_id":    token.UserID,
		"request_id": requestID,
	}).Warn("Scan bypass token used")

	return token.UserID, true
}

// RevokeToken revokes a bypass token.
func (bm *BypassManager) RevokeToken(ctx context.Context, tokenID string) error {
	return bm.store.Delete(ctx, tokenID)
}

// ListTokens returns all active tokens for a user.
func (bm *BypassManager) ListTokens(ctx context.Context, userID string) ([]*BypassToken, error) {
	return bm.store.ListByUser(ctx, userID)
}

// hashToken computes the SHA-256 hash of a raw token.
func hashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// InMemoryBypassStore is a simple in-memory token store for development/testing.
type InMemoryBypassStore struct {
	tokens map[string]*BypassToken // keyed by hash
}

// NewInMemoryBypassStore creates a new in-memory bypass store.
func NewInMemoryBypassStore() *InMemoryBypassStore {
	return &InMemoryBypassStore{
		tokens: make(map[string]*BypassToken),
	}
}

func (s *InMemoryBypassStore) Store(_ context.Context, hash string, token *BypassToken, _ time.Duration) error {
	s.tokens[hash] = token
	return nil
}

func (s *InMemoryBypassStore) Lookup(_ context.Context, hash string) (*BypassToken, error) {
	token, ok := s.tokens[hash]
	if !ok {
		return nil, nil
	}
	return token, nil
}

func (s *InMemoryBypassStore) Delete(_ context.Context, tokenID string) error {
	for hash, token := range s.tokens {
		if token.TokenID == tokenID {
			delete(s.tokens, hash)
			return nil
		}
	}
	return nil
}

func (s *InMemoryBypassStore) ListByUser(_ context.Context, userID string) ([]*BypassToken, error) {
	var result []*BypassToken
	now := time.Now()
	for _, token := range s.tokens {
		if token.UserID == userID && !token.Revoked && now.Before(token.ExpiresAt) {
			result = append(result, token)
		}
	}
	return result, nil
}

// NoopAuditLogger is a no-op audit logger for testing.
type NoopAuditLogger struct {
	Events []BypassAuditEvent
}

func (l *NoopAuditLogger) LogBypassEvent(_ context.Context, event BypassAuditEvent) error {
	l.Events = append(l.Events, event)
	return nil
}

// TokenIDFromJSON extracts a token_id from a JSON token record for API responses.
func TokenIDFromJSON(data []byte) string {
	var t struct {
		TokenID string `json:"token_id"`
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return ""
	}
	return t.TokenID
}
