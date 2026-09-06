package promptcache

import (
	"strings"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// Prompt-cache control surface — docs/AIQG-PROMPT-CACHE-CONTROL.md §3.
//
// # What this fixes
//
// Until now the gateway DROPPED a client's cache_control silently: ChatRequest
// is a typed struct with no unknown-field passthrough, so encoding/json
// discarded it without error. A caller could ask for prompt caching, receive a
// perfectly normal response, and pay full price forever — while we read
// cache-token usage back and reported a saving we had never requested
// (tas-llm-router#100).
//
// # Why a mode, and not just passthrough
//
// Passthrough alone assumes every origin gets breakpoints right. They don't,
// and the failure is silent — no error, just full price. The documented
// mistakes are precisely the ones a gateway can see and an origin cannot: a
// breakpoint after a timestamp, a breakpoint covering the varying question, a
// prefix below the model's silent minimum, more than four breakpoints, or a
// long agentic turn exceeding the 20-block lookback. The gateway knows the
// model and the render order; each origin re-deriving that is how it gets got
// wrong N times.

// Mode selects how prompt-cache breakpoints are decided.
type Mode string

const (
	// ModeAuto lets the gateway place breakpoints, replacing anything the
	// client sent. The placement engine is P2 — see Available().
	ModeAuto Mode = "auto"
	// ModePassthrough honours exactly what the client sent and places nothing.
	ModePassthrough Mode = "passthrough"
	// ModeOff strips every breakpoint.
	ModeOff Mode = "off"
)

// DefaultMode is passthrough.
//
// The design (§5) leaves the default open, to be decided from the P0 probe's
// measured reuse. Measured 2026-08-20 over 30 days: 16 probe measurements, 2
// showing prefix reuse, mean cacheable prefix 613 tokens against a per-model
// minimum of 1024–4096. That is not evidence that caching would not pay — it is
// §10.1's third reading, "most traffic has no cacheable span at all" — but it
// is nowhere near enough to justify the gateway rewriting every caller's
// breakpoints by default. Passthrough it is, with auto available per route and
// per request, until a route with real prefix volume exists to measure.
const DefaultMode = ModePassthrough

// Header names. Deliberately distinct from TAS-Cache, which governs the
// RESPONSE cache — a different mechanism with a different failure mode, and
// conflating them in a header name would guarantee someone conflates them in
// their head.
const (
	HeaderMode = "TAS-Prompt-Cache"
	HeaderTTL  = "TAS-Prompt-Cache-TTL"
)

// ParseMode maps a header or config value to a Mode. Unrecognised values fall
// back to def and report ok=false, so a caller can surface the typo rather than
// silently applying a mode nobody asked for.
func ParseMode(s string, def Mode) (Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return def, true
	case "auto":
		return ModeAuto, true
	case "passthrough", "pass":
		return ModePassthrough, true
	case "off", "none", "disabled":
		return ModeOff, true
	}
	return def, false
}

// ParseTTL validates a TTL value. Only the two the API accepts are permitted;
// anything else falls back to the vendor default rather than being forwarded
// and rejected at the vendor with a less useful error.
func ParseTTL(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", true
	case "5m":
		return "5m", true
	case "1h":
		return "1h", true
	}
	return "", false
}

// Resolve combines a route-level default with a per-request header.
//
// An explicit `off` from the caller ALWAYS wins, including over a route that
// asks for auto. It is the escape hatch for someone who has found the
// gateway's placement wrong for their traffic, and an escape hatch a route
// default can override is not an escape hatch.
//
// Everything else follows normal precedence: header beats route, route beats
// the global default.
func Resolve(routeDefault Mode, header string) Mode {
	m, ok := ParseMode(header, "")
	if !ok || m == "" {
		if routeDefault == "" {
			return DefaultMode
		}
		return routeDefault
	}
	return m
}

// Available reports whether a mode can actually be applied today.
//
// The auto placement engine (§4.1/§4.2) has landed, so every mode is now
// available. Kept as a function because callers and the event still ask, and a
// future mode could reintroduce an unavailable state.
func Available(m Mode) bool { return true }

// Apply enforces the mode over a request's breakpoints and reports how many
// survive, for the event.
//
// Returns the mode actually applied, which differs from the requested one when
// auto is asked for before the placement engine exists.
func Apply(req *types.ChatRequest, m Mode) (applied Mode, breakpoints int) {
	switch m {
	case ModeOff:
		strip(req)
		return ModeOff, 0
	case ModeAuto:
		// Auto REPLACES whatever the client sent (§3): strip first so a caller's
		// breakpoints cannot combine with the gateway's placement into more than
		// four, and so the gateway's judgement — not the origin's — decides.
		strip(req)
		n := placeAuto(req)
		// Report auto honestly, including the no-op count. A non-Anthropic model
		// or a prefix below the minimum yields 0 breakpoints: the event then
		// shows mode=auto with 0, which is the truth (auto ran and placed
		// nothing) rather than a passthrough that never happened.
		return ModeAuto, n
	default:
		return ModePassthrough, count(req)
	}
}

// strip removes every breakpoint.
func strip(req *types.ChatRequest) {
	if req == nil {
		return
	}
	for i := range req.Tools {
		req.Tools[i].CacheControl = nil
	}
	for i := range req.Messages {
		req.Messages[i].CacheControl = nil
		if parts, ok := req.Messages[i].Content.([]types.ContentPart); ok {
			for j := range parts {
				parts[j].CacheControl = nil
			}
			req.Messages[i].Content = parts
		}
	}
}

// count reports how many breakpoints a request carries, so the event can show
// that a route believing itself cached is in fact sending none.
func count(req *types.ChatRequest) int {
	if req == nil {
		return 0
	}
	n := 0
	for _, t := range req.Tools {
		if t.CacheControl != nil {
			n++
		}
	}
	for _, m := range req.Messages {
		if m.CacheControl != nil {
			n++
		}
		if parts, ok := m.Content.([]types.ContentPart); ok {
			for _, p := range parts {
				if p.CacheControl != nil {
					n++
				}
			}
		}
	}
	return n
}

// MaxBreakpoints is the API's hard limit. A fifth is a 400, so exceeding it is
// not a degraded request but a failed one.
const MaxBreakpoints = 4

// Clamp removes breakpoints beyond the limit, keeping the EARLIEST ones.
//
// Earliest rather than latest because breakpoints are cumulative prefixes: an
// early breakpoint covers tools and system, which are the large stable spans
// worth caching, while a late one covers those plus a little more. Dropping
// early ones to keep late ones would discard the cheap, safe win to preserve a
// marginal one — and if the late blocks vary, preserve nothing at all.
func Clamp(req *types.ChatRequest) (removed int) {
	if req == nil {
		return 0
	}
	seen := 0
	for i := range req.Tools {
		if req.Tools[i].CacheControl == nil {
			continue
		}
		seen++
		if seen > MaxBreakpoints {
			req.Tools[i].CacheControl = nil
			removed++
		}
	}
	for i := range req.Messages {
		if parts, ok := req.Messages[i].Content.([]types.ContentPart); ok {
			for j := range parts {
				if parts[j].CacheControl == nil {
					continue
				}
				seen++
				if seen > MaxBreakpoints {
					parts[j].CacheControl = nil
					removed++
				}
			}
			req.Messages[i].Content = parts
		}
		if req.Messages[i].CacheControl == nil {
			continue
		}
		seen++
		if seen > MaxBreakpoints {
			req.Messages[i].CacheControl = nil
			removed++
		}
	}
	return removed
}
