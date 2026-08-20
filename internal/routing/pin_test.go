package routing

import (
	"context"
	"testing"
)

// The pin is the mechanism that turns policy resolution into steering.

func TestPinRoundTrips(t *testing.T) {
	ctx := WithPinnedProvider(context.Background(), "anthropic")
	if got := PinnedProviderFrom(ctx); got != "anthropic" {
		t.Fatalf("PinnedProviderFrom = %q, want anthropic", got)
	}
}

func TestNoPinIsInert(t *testing.T) {
	if got := PinnedProviderFrom(context.Background()); got != "" {
		t.Fatalf("bare context reported a pin: %q", got)
	}
	// An empty pin must not store a value that later reads as set — otherwise
	// "no override" would be indistinguishable from a pin to the empty string.
	ctx := WithPinnedProvider(context.Background(), "")
	if got := PinnedProviderFrom(ctx); got != "" {
		t.Fatalf("empty pin stored as %q — absent must stay absent", got)
	}
}

// A pin must survive context derivation, since the request context is wrapped
// several times between resolution and routing.
func TestPinSurvivesDerivedContext(t *testing.T) {
	ctx := WithPinnedProvider(context.Background(), "openai")
	derived, cancel := context.WithCancel(ctx)
	defer cancel()
	if got := PinnedProviderFrom(derived); got != "openai" {
		t.Fatalf("pin lost through context derivation: %q", got)
	}
}
