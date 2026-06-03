package tokens

import (
	"context"
	"errors"
	"testing"
)

func newResolverWithFixtures() *MapResolver {
	return NewMapResolver([]ConfigToken{
		{
			TokenID:       "tok_uuid_1",
			Token:         "tas_qg_live_abc",
			TenantID:      "tenant-a",
			AIQGAccountID: "account-a",
			SourceApp:     "billing-api-prod",
		},
		{
			TokenID:       "tok_uuid_2",
			Token:         "tas_qg_live_def",
			TenantID:      "tenant-b",
			AIQGAccountID: "account-b",
			SourceApp:     "claims-api-prod",
			Suspended:     true,
		},
	})
}

func TestMapResolver_Hit(t *testing.T) {
	r := newResolverWithFixtures()
	tok, err := r.Resolve(context.Background(), "tas_qg_live_abc")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if tok.TenantID != "tenant-a" {
		t.Errorf("TenantID=%q", tok.TenantID)
	}
	if tok.AIQGAccountID != "account-a" {
		t.Errorf("AIQGAccountID=%q", tok.AIQGAccountID)
	}
	if tok.TokenID != "tok_uuid_1" {
		t.Errorf("TokenID=%q", tok.TokenID)
	}
	if tok.SourceApp != "billing-api-prod" {
		t.Errorf("SourceApp=%q", tok.SourceApp)
	}
	if tok.Suspended {
		t.Errorf("Suspended=true for active token")
	}
}

func TestMapResolver_NotFound(t *testing.T) {
	r := newResolverWithFixtures()
	tok, err := r.Resolve(context.Background(), "tas_qg_live_not_in_config")
	if !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("expected ErrTokenNotFound, got %v", err)
	}
	if tok != nil {
		t.Errorf("Token should be nil on not-found: %#v", tok)
	}
}

// Suspended accounts return BOTH the populated token AND ErrTokenSuspended
// so the middleware can log the resolved tenant for triage even as it
// rejects the request.
func TestMapResolver_Suspended(t *testing.T) {
	r := newResolverWithFixtures()
	tok, err := r.Resolve(context.Background(), "tas_qg_live_def")
	if !errors.Is(err, ErrTokenSuspended) {
		t.Fatalf("expected ErrTokenSuspended, got %v", err)
	}
	if tok == nil {
		t.Fatalf("Token should be populated even on suspended account (for logging)")
	}
	if tok.TenantID != "tenant-b" {
		t.Errorf("TenantID=%q", tok.TenantID)
	}
}

func TestMapResolver_EmptyTokenString(t *testing.T) {
	r := newResolverWithFixtures()
	_, err := r.Resolve(context.Background(), "")
	if !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("empty string should be ErrTokenNotFound, got %v", err)
	}
}

func TestMapResolver_Len(t *testing.T) {
	if got := newResolverWithFixtures().Len(); got != 2 {
		t.Errorf("Len()=%d want=2", got)
	}
	if got := NewMapResolver(nil).Len(); got != 0 {
		t.Errorf("Len()=%d want=0 for nil config", got)
	}
}

func TestContextRoundTrip(t *testing.T) {
	src := &Token{TenantID: "t1", AIQGAccountID: "a1", TokenID: "tok-1"}
	ctx := WithToken(context.Background(), src)
	got := FromContext(ctx)
	if got != src {
		t.Errorf("FromContext did not round-trip the token")
	}

	if FromContext(context.Background()) != nil {
		t.Errorf("FromContext on bare ctx should return nil")
	}
	var nilCtx context.Context
	if FromContext(nilCtx) != nil {
		t.Errorf("FromContext(nil ctx) should return nil")
	}
}

// Duplicate token entries — the LAST wins, documented behavior.
func TestMapResolver_DuplicateLastWins(t *testing.T) {
	r := NewMapResolver([]ConfigToken{
		{Token: "tas_qg_live_dup", TenantID: "first-tenant", TokenID: "first"},
		{Token: "tas_qg_live_dup", TenantID: "second-tenant", TokenID: "second"},
	})
	tok, err := r.Resolve(context.Background(), "tas_qg_live_dup")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if tok.TenantID != "second-tenant" {
		t.Errorf("expected last to win, got TenantID=%q", tok.TenantID)
	}
}
