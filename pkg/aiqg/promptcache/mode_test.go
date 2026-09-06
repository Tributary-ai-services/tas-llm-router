package promptcache

import (
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func cc() *types.CacheControl { return &types.CacheControl{Type: "ephemeral"} }

// reqWith builds a request with a breakpoint at each named place.
func reqWith(system, part, tool bool) *types.ChatRequest {
	r := &types.ChatRequest{
		Messages: []types.Message{
			{Role: "system", Content: "you are helpful"},
			{Role: "user", Content: []types.ContentPart{{Type: "text", Text: "hi"}}},
		},
		Tools: []types.Tool{{Type: "function"}},
	}
	if system {
		r.Messages[0].CacheControl = cc()
	}
	if part {
		parts := r.Messages[1].Content.([]types.ContentPart)
		parts[0].CacheControl = cc()
		r.Messages[1].Content = parts
	}
	if tool {
		r.Tools[0].CacheControl = cc()
	}
	return r
}

// ---------------------------------------------------------------------------
// The bug this package closes: a client's cache_control was silently dropped.
// ---------------------------------------------------------------------------

func TestPassthroughPreservesEveryBreakpoint(t *testing.T) {
	r := reqWith(true, true, true)
	applied, n := Apply(r, ModePassthrough)
	if applied != ModePassthrough {
		t.Fatalf("applied = %v, want passthrough", applied)
	}
	if n != 3 {
		t.Fatalf("breakpoints = %d, want 3 — passthrough must not drop what the caller sent", n)
	}
	if r.Messages[0].CacheControl == nil || r.Tools[0].CacheControl == nil {
		t.Fatal("passthrough mutated the request")
	}
}

func TestOffStripsEverything(t *testing.T) {
	r := reqWith(true, true, true)
	applied, n := Apply(r, ModeOff)
	if applied != ModeOff || n != 0 {
		t.Fatalf("applied=%v breakpoints=%d, want off/0", applied, n)
	}
	if r.Messages[0].CacheControl != nil || r.Tools[0].CacheControl != nil {
		t.Fatal("off left a breakpoint in place")
	}
	parts := r.Messages[1].Content.([]types.ContentPart)
	if parts[0].CacheControl != nil {
		t.Fatal("off left a content-part breakpoint in place")
	}
}

// `off` is the escape hatch for a caller who finds our placement wrong. An
// escape hatch a route default can override is not an escape hatch.
func TestExplicitOffBeatsAnyRouteDefault(t *testing.T) {
	for _, routeDefault := range []Mode{ModeAuto, ModePassthrough, ModeOff} {
		if got := Resolve(routeDefault, "off"); got != ModeOff {
			t.Errorf("Resolve(%v, \"off\") = %v, want off", routeDefault, got)
		}
	}
}

func TestHeaderBeatsRouteDefault(t *testing.T) {
	if got := Resolve(ModePassthrough, "auto"); got != ModeAuto {
		t.Fatalf("header did not override the route default: %v", got)
	}
	if got := Resolve(ModeAuto, ""); got != ModeAuto {
		t.Fatalf("empty header should leave the route default in force, got %v", got)
	}
	if got := Resolve("", ""); got != DefaultMode {
		t.Fatalf("with nothing set the global default should apply, got %v", got)
	}
}

// A typo'd header must be reported, not silently applied as something else —
// the caller would otherwise believe they had changed the mode.
func TestUnrecognisedModeIsReported(t *testing.T) {
	m, ok := ParseMode("cahce", ModePassthrough)
	if ok {
		t.Fatal("a misspelled mode was accepted")
	}
	if m != ModePassthrough {
		t.Fatalf("fallback = %v, want the supplied default", m)
	}
	// An unrecognised header must not silently become something the route
	// did not ask for either.
	if got := Resolve(ModeOff, "cahce"); got != ModeOff {
		t.Fatalf("an invalid header changed the effective mode to %v", got)
	}
}

func TestParseTTLAcceptsOnlyWhatTheAPITakes(t *testing.T) {
	for _, good := range []string{"", "5m", "1h", "5M"} {
		if _, ok := ParseTTL(good); !ok {
			t.Errorf("ParseTTL(%q) rejected", good)
		}
	}
	for _, bad := range []string{"10m", "30s", "forever", "1"} {
		if _, ok := ParseTTL(bad); ok {
			t.Errorf("ParseTTL(%q) accepted; only 5m and 1h are valid", bad)
		}
	}
}

// auto is now available and actively places (§4). Detailed placement behaviour
// lives in place_test.go; here we assert only the mode-layer contract: auto is
// available, and Apply(auto) REPLACES the caller's breakpoints (§3) rather than
// honouring them.
func TestAutoIsAvailableAndReplacesClientBreakpoints(t *testing.T) {
	if !Available(ModeAuto) || !Available(ModePassthrough) || !Available(ModeOff) {
		t.Fatal("every mode should be available now that the placement engine has landed")
	}
	r := reqWith(true, false, false) // client breakpoint on the system message; model unset
	applied, n := Apply(r, ModeAuto)
	if applied != ModeAuto {
		t.Fatalf("applied = %v, want auto", applied)
	}
	// Model is unset (non-Anthropic) so placement is a no-op — and the caller's
	// breakpoint must be gone regardless, because auto replaces, not honours.
	if n != 0 {
		t.Fatalf("placed %d breakpoints on a non-Anthropic model; want 0 (no-op)", n)
	}
	if r.Messages[0].CacheControl != nil {
		t.Fatal("auto did not strip the caller's breakpoint; it must replace, not honour")
	}
}

// ---------------------------------------------------------------------------
// Clamping. A fifth breakpoint is a 400, so this converts a failed request
// into a working one.
// ---------------------------------------------------------------------------

func TestClampKeepsTheEarliestBreakpoints(t *testing.T) {
	parts := make([]types.ContentPart, 6)
	for i := range parts {
		parts[i] = types.ContentPart{Type: "text", Text: "x", CacheControl: cc()}
	}
	r := &types.ChatRequest{Messages: []types.Message{{Role: "user", Content: parts}}}

	removed := Clamp(r)
	if removed != 2 {
		t.Fatalf("removed = %d, want 2 (6 breakpoints, limit %d)", removed, MaxBreakpoints)
	}
	got := r.Messages[0].Content.([]types.ContentPart)
	// Earliest kept: an early breakpoint covers the large stable prefix, a
	// late one covers that plus a little more. Keeping late ones would discard
	// the cheap safe win to preserve a marginal one.
	for i := 0; i < MaxBreakpoints; i++ {
		if got[i].CacheControl == nil {
			t.Fatalf("breakpoint %d was dropped; the earliest must be kept", i)
		}
	}
	for i := MaxBreakpoints; i < len(got); i++ {
		if got[i].CacheControl != nil {
			t.Fatalf("breakpoint %d survived past the limit", i)
		}
	}
}

func TestClampIsANoOpUnderTheLimit(t *testing.T) {
	r := reqWith(true, true, true)
	if removed := Clamp(r); removed != 0 {
		t.Fatalf("removed %d breakpoints when under the limit", removed)
	}
}

func TestCountAcrossEveryPlacement(t *testing.T) {
	if got := count(reqWith(false, false, false)); got != 0 {
		t.Fatalf("count with no breakpoints = %d", got)
	}
	if got := count(reqWith(true, true, true)); got != 3 {
		t.Fatalf("count = %d, want 3 (system + content part + tool)", got)
	}
}

func TestNilRequestIsSafe(t *testing.T) {
	// Callers should never need a nil check on a best-effort control surface.
	if _, n := Apply(nil, ModeOff); n != 0 {
		t.Fatal("Apply(nil) misbehaved")
	}
	if Clamp(nil) != 0 {
		t.Fatal("Clamp(nil) misbehaved")
	}
}
