package promptcache

import (
	"encoding/json"
	"strings"

	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/clear"
)

// Auto-placement — docs/AIQG-PROMPT-CACHE-CONTROL.md §4.
//
// Render order is tools → system → messages, and a change at any level
// invalidates that level and everything after it. So the whole rule is: put the
// breakpoint at the last byte that is stable across requests.
//
// This file implements §4.1 (end of system — the big, safe win) and §4.2 (end
// of the last complete turn — multi-turn reuse), the budget/priority of §4.4,
// and the model-dependent minimum of §4.5. The §4.3 lookback fillers (an
// intermediate breakpoint every ~15 blocks inside a long agentic turn) are a
// follow-up; auto without them still places the two highest-value breakpoints.

// ephemeralBreakpoint is the only cache_control the API accepts. TTL is left
// empty (the 5m default) — the pinned SDK cannot express 1h anyway (§7).
func ephemeralBreakpoint() *types.CacheControl { return &types.CacheControl{Type: "ephemeral"} }

// minCacheTokens returns a model's minimum cacheable prefix (§4.5) and whether
// explicit breakpoints apply at all.
//
// applicable=false means the vendor caches with no control surface (OpenAI) or
// is one we do not place for — auto is then a no-op, not an error. For an
// unrecognised Claude model we return the highest known minimum: a too-high
// bar only skips a placement (safe — the prefix simply isn't cached), whereas a
// too-low one would spend a breakpoint slot on a span the API silently won't
// cache.
func minCacheTokens(model string) (min int, applicable bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if !strings.Contains(m, "claude") {
		return 0, false // OpenAI etc. cache automatically; nothing to place.
	}
	// Normalise separators so the two Anthropic naming orders both match — the
	// newer "sonnet-4-5" and the older "3-7-sonnet" — since the version and the
	// family swap places between generations.
	norm := strings.ReplaceAll(m, ".", "-")
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(norm, s) {
				return true
			}
		}
		return false
	}
	switch {
	case strings.Contains(norm, "opus"):
		return 4096, true // Every Opus tier in the table is 4096.
	case strings.Contains(norm, "haiku"):
		if has("haiku-4-5", "4-5-haiku") {
			return 4096, true
		}
		if has("haiku-3", "3-haiku", "3-5-haiku") {
			return 2048, true
		}
		return 4096, true // Unknown Haiku → conservative.
	case strings.Contains(norm, "fable"):
		return 2048, true
	case strings.Contains(norm, "sonnet"):
		if has("sonnet-4-6", "4-6-sonnet") {
			return 2048, true
		}
		if has("sonnet-4-5", "4-5-sonnet", "sonnet-4-1", "4-1-sonnet",
			"sonnet-4", "4-sonnet", "sonnet-3-7", "3-7-sonnet") {
			return 1024, true
		}
		return 2048, true // Unknown Sonnet → conservative (sonnets top out at 2048).
	default:
		return 4096, true // Unknown Claude family → most conservative.
	}
}

// placeAuto sets breakpoints per §4 and returns how many it placed. It assumes
// client breakpoints were already stripped (auto REPLACES them, §3). A
// non-applicable model (OpenAI) gets nothing — a no-op, not an error.
//
// Priority under the 4-breakpoint budget is system (§4.1) > last turn (§4.2);
// with at most two placements here the API's hard cap is never approached, and
// the server still runs Clamp as a backstop.
func placeAuto(req *types.ChatRequest) int {
	if req == nil {
		return 0
	}
	min, applicable := minCacheTokens(req.Model)
	if !applicable {
		return 0
	}

	toolsSystem := clear.TokensFromBytes(toolsBytes(req) + systemTextBytes(req))
	placed := 0

	// §4.1 — end of the last system block caches tools+system together. Only
	// worth a slot if that span clears the model's minimum; below it the API
	// silently does not cache, so the breakpoint would be a wasted slot.
	if idx := lastSystemIndex(req); idx >= 0 && toolsSystem >= min {
		req.Messages[idx].CacheControl = ephemeralBreakpoint()
		placed++
	}

	// §4.2 — end of the last COMPLETE turn (the last assistant message that is
	// followed by the current user question). Never after the question itself,
	// which is the classic shared-prefix mistake: every request would write a
	// unique entry and read nothing.
	if placed < MaxBreakpoints {
		if idx := lastCompleteTurnIndex(req); idx >= 0 {
			through := clear.TokensFromBytes(toolsBytes(req) + messagesTextBytesThrough(req, idx))
			if through >= min {
				req.Messages[idx].CacheControl = ephemeralBreakpoint()
				placed++
			}
		}
	}

	return placed
}

// lastSystemIndex is the index of the last system message, or -1 if none.
func lastSystemIndex(req *types.ChatRequest) int {
	idx := -1
	for i, m := range req.Messages {
		if m.Role == "system" {
			idx = i
		}
	}
	return idx
}

// lastCompleteTurnIndex is the index of the last assistant message that is
// followed by at least one later message (the pending user turn). -1 when the
// conversation has no completed assistant turn to reuse (e.g. a first request:
// system + user only).
func lastCompleteTurnIndex(req *types.ChatRequest) int {
	last := -1
	for i, m := range req.Messages {
		if m.Role == "assistant" {
			last = i
		}
	}
	if last >= 0 && last < len(req.Messages)-1 {
		return last
	}
	return -1
}

// toolsBytes estimates the byte size of the rendered tool block.
func toolsBytes(req *types.ChatRequest) int {
	n := 0
	for _, t := range req.Tools {
		n += len(t.Function.Name) + len(t.Function.Description)
		if t.Function.Parameters != nil {
			if b, err := json.Marshal(t.Function.Parameters); err == nil {
				n += len(b)
			}
		}
	}
	return n
}

// systemTextBytes is the byte size of all system message text.
func systemTextBytes(req *types.ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		if m.Role == "system" {
			n += contentTextBytes(m.Content)
		}
	}
	return n
}

// messagesTextBytesThrough is the byte size of every message's text from the
// start up to and including idx — the span a breakpoint at idx would cover
// (system messages included, since they render into the same prefix).
func messagesTextBytesThrough(req *types.ChatRequest, idx int) int {
	n := 0
	for i := 0; i <= idx && i < len(req.Messages); i++ {
		n += contentTextBytes(req.Messages[i].Content)
	}
	return n
}

// contentTextBytes flattens a message's content (string or multimodal parts) to
// its text byte size. Non-text (images) contribute nothing to the estimate.
func contentTextBytes(content interface{}) int {
	switch c := content.(type) {
	case string:
		return len(c)
	case []types.ContentPart:
		n := 0
		for _, p := range c {
			n += len(p.Text)
		}
		return n
	default:
		return 0
	}
}
