// Package tokens resolves AIQG bearer tokens (`tas_qg_live_*`) to
// tenant + account identifiers. Per build-vs-reuse §1.2 the production
// resolver will query aiqg-dashboard-be's token store; for MVP this
// package ships an in-memory MapResolver constructed from server config
// so deployments can start emitting tenant-scoped events without
// blocking on the dashboard-backend rollout.
//
// The Resolver interface keeps the production swap a one-line change:
//
//	type DashboardResolver struct { client *dashboard.Client }
//	func (r *DashboardResolver) Resolve(ctx, token) (*Token, error) { ... }
//
// Errors:
//   - ErrTokenNotFound  → middleware emits 401 (spec request-event.md §564)
//   - ErrTokenSuspended → middleware emits 403 (account suspended)
//
// Both errors bypass event emission per spec §273 — pre-validation auth
// failures are counted in Prometheus auth-failure metrics only because
// there is no resolved tenant to attribute an event to.
package tokens

import (
	"context"
	"errors"
)

// Token is the resolved view of a `tas_qg_live_*` bearer. Field names
// mirror aether-shared/data-models/aiqg/account.md so plumbing the
// dashboard-backend resolver later is a copy of the same struct.
type Token struct {
	// TokenID is the UUID FK to the aiqg_token table managed by
	// aiqg-dashboard-be. Stamped on request_event.tas_auth_token_id
	// (request-event.md §"Field reference").
	TokenID string

	// TenantID is the customer-scoped isolation boundary. Hot index
	// for tenant-scoped dashboard queries.
	TenantID string

	// AIQGAccountID is 1:1 with TenantID but stored explicitly for
	// join convenience (per spec field reference).
	AIQGAccountID string

	// SourceApp is the token's claimed source-app identifier. Per
	// request-event.md §80 the customer-supplied TAS-Source-App
	// header overrides this when both are present.
	SourceApp string

	// Suspended marks accounts that should receive 403, not 401.
	// Token presence + recognition is fine; the account itself is
	// suspended.
	Suspended bool
}

// ErrTokenNotFound means the bearer wasn't recognized by the resolver.
// Distinct from ErrTokenSuspended (which means recognized but blocked).
var ErrTokenNotFound = errors.New("aiqg/tokens: token not found")

// ErrTokenSuspended means the token resolved to an account that is
// currently suspended. Maps to HTTP 403 per spec.
var ErrTokenSuspended = errors.New("aiqg/tokens: token's account is suspended")

// Resolver turns a `tas_qg_live_*` bearer into a Token. The MVP
// MapResolver is in-process; production swaps to a backend-client
// implementation against aiqg-dashboard-be.
type Resolver interface {
	Resolve(ctx context.Context, bearer string) (*Token, error)
}

// MapResolver looks tokens up in an in-memory map. Constructed from
// server config; safe for concurrent reads (no writes after
// construction).
type MapResolver struct {
	byBearer map[string]*Token
}

// NewMapResolver builds a resolver from a slice of token definitions.
// Duplicate bearer entries take the LAST occurrence — config-loader
// validation should reject duplicates before reaching here, but the
// behavior is documented so the silent collision is not surprising.
func NewMapResolver(tokens []ConfigToken) *MapResolver {
	m := &MapResolver{byBearer: make(map[string]*Token, len(tokens))}
	for _, t := range tokens {
		m.byBearer[t.Token] = &Token{
			TokenID:       t.TokenID,
			TenantID:      t.TenantID,
			AIQGAccountID: t.AIQGAccountID,
			SourceApp:     t.SourceApp,
			Suspended:     t.Suspended,
		}
	}
	return m
}

// Resolve returns the Token for bearer, or ErrTokenNotFound. Suspended
// accounts return their populated Token AND ErrTokenSuspended — the
// middleware needs both: the Token to log the resolved account, the
// error to gate the response.
func (m *MapResolver) Resolve(_ context.Context, bearer string) (*Token, error) {
	tok, ok := m.byBearer[bearer]
	if !ok {
		return nil, ErrTokenNotFound
	}
	if tok.Suspended {
		return tok, ErrTokenSuspended
	}
	return tok, nil
}

// Len returns the number of tokens in the resolver. Useful for startup
// log lines confirming the config loaded correctly.
func (m *MapResolver) Len() int {
	return len(m.byBearer)
}

// ConfigToken is the YAML-mappable shape consumed by NewMapResolver.
// Lives in this package so the server-config struct can reference it
// without pulling middleware/events deps into config parsing.
type ConfigToken struct {
	TokenID       string `yaml:"token_id"`
	Token         string `yaml:"token"`
	TenantID      string `yaml:"tenant_id"`
	AIQGAccountID string `yaml:"aiqg_account_id"`
	SourceApp     string `yaml:"source_app"`
	Suspended     bool   `yaml:"suspended"`
}

// Context plumbing — same shape as middleware.WithHeaders. Lets
// downstream consumers (audit logger, policy resolver) read the
// resolved token without re-running the resolver.

type ctxKey struct{}

var tokenKey = ctxKey{}

// WithToken attaches a resolved Token to ctx.
func WithToken(ctx context.Context, t *Token) context.Context {
	return context.WithValue(ctx, tokenKey, t)
}

// FromContext returns the Token attached to ctx, or nil. Callers that
// don't need the token (non-AIQG paths) get nil and can skip
// tenant-scoped work entirely.
func FromContext(ctx context.Context) *Token {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(tokenKey).(*Token)
	return t
}
