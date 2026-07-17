package middleware

import (
	"context"
	"errors"
	"testing"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/tokens"
)

// fakeProbe records calls and can be made to fail, so we can assert the
// best-effort contract without a Redis.
type fakeProbe struct {
	calls []string // tenantID|hash
	seen  bool
	err   error
}

func (f *fakeProbe) Observe(_ context.Context, tenantID, hash string) (bool, error) {
	f.calls = append(f.calls, tenantID+"|"+hash)
	return f.seen, f.err
}

func routingWith(hash string) *Routing {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)
	StampCachePrefixHash(ctx, hash)
	return r
}

func TestProbePromptCache_ObservesStampedHashUnderTenant(t *testing.T) {
	p := &fakeProbe{seen: true}
	cfg := AIQGConfig{Logger: silentLogger(), PromptCache: p}
	probePromptCache(cfg, routingWith("deadbeef"), &tokens.Token{TenantID: "tenant-1"})

	if len(p.calls) != 1 {
		t.Fatalf("Observe called %d times, want 1", len(p.calls))
	}
	if got, want := p.calls[0], "tenant-1|deadbeef"; got != want {
		t.Fatalf("Observe(%q), want %q — the probe must be tenant-scoped, since vendor caches are per-account", got, want)
	}
}

// A request with nothing cacheable is not a datapoint. Probing it would key
// every bare chat onto one shared hash and report a reuse rate that is pure
// artifact — the number this whole exercise exists to trust.
func TestProbePromptCache_SkipsWhenNothingCacheable(t *testing.T) {
	p := &fakeProbe{}
	cfg := AIQGConfig{Logger: silentLogger(), PromptCache: p}
	probePromptCache(cfg, routingWith(""), &tokens.Token{TenantID: "t1"})

	if len(p.calls) != 0 {
		t.Fatalf("Observe called %d times for a request with no cacheable prefix, want 0", len(p.calls))
	}
}

// Measurement must never be able to break a served request. This runs from a
// deferred path after the response is sent, so every degenerate input and every
// store failure has to be a silent no-op.
func TestProbePromptCache_BestEffort(t *testing.T) {
	t.Run("nil probe (Redis unconfigured)", func(t *testing.T) {
		probePromptCache(AIQGConfig{Logger: silentLogger()}, routingWith("h"), &tokens.Token{TenantID: "t"})
	})
	t.Run("nil routing", func(t *testing.T) {
		probePromptCache(AIQGConfig{Logger: silentLogger(), PromptCache: &fakeProbe{}}, nil, &tokens.Token{TenantID: "t"})
	})
	t.Run("nil token (unauthenticated)", func(t *testing.T) {
		probePromptCache(AIQGConfig{Logger: silentLogger(), PromptCache: &fakeProbe{}}, routingWith("h"), nil)
	})
	t.Run("probe error is logged, not propagated", func(t *testing.T) {
		p := &fakeProbe{err: errors.New("redis down")}
		probePromptCache(AIQGConfig{Logger: silentLogger(), PromptCache: p}, routingWith("h"), &tokens.Token{TenantID: "t"})
		if len(p.calls) != 1 {
			t.Fatalf("Observe called %d times, want 1", len(p.calls))
		}
	})
}

func TestStampCachePrefixHash_FirstWriteWinsAndEmptyIsNoop(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	if got := r.Snapshot().CachePrefixHash; got != "" {
		t.Fatalf("unstamped CachePrefixHash = %q, want \"\"", got)
	}

	StampCachePrefixHash(ctx, "")
	if got := r.Snapshot().CachePrefixHash; got != "" {
		t.Fatalf("empty stamp set CachePrefixHash = %q, want \"\"", got)
	}

	StampCachePrefixHash(ctx, "first")
	StampCachePrefixHash(ctx, "second")
	if got := r.Snapshot().CachePrefixHash; got != "first" {
		t.Fatalf("CachePrefixHash = %q, want \"first\" (first-write-wins, mirroring the other stamps)", got)
	}
}

func TestStampCachePrefixHash_NoRoutingInContextIsSafe(t *testing.T) {
	StampCachePrefixHash(context.Background(), "h") // must not panic
}
