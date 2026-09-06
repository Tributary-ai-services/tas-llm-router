package server

import (
	"net/http"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/promptcache"
)

// Prompt-cache mode at the request boundary — docs/AIQG-PROMPT-CACHE-CONTROL.md
// §3, work-breakdown P1.
//
// Before this, a client's cache_control was discarded silently by JSON
// decoding: no error, no caching, full price forever, while we read cache-token
// usage back and reported savings we had never requested (#100). The types now
// carry it; this decides whether it survives to the vendor.

// applyPromptCacheMode resolves the effective mode for a request and enforces
// it, stamping what actually happened onto the event.
//
// The stamped values describe the OUTCOME, not the request: mode is what was
// applied (which differs from what was asked when auto is requested before the
// placement engine exists), and breakpoints is how many actually survive to the
// vendor. A route that believes caching is on while sending zero breakpoints is
// exactly the silent failure this feature exists to end, and it is only visible
// if the event reports the count rather than the intent.
func (s *Server) applyPromptCacheMode(r *http.Request, req *types.ChatRequest) {
	if req == nil {
		return
	}
	header := r.Header.Get(promptcache.HeaderMode)
	requested, ok := promptcache.ParseMode(header, "")
	if !ok {
		// An unrecognised value is surfaced rather than silently ignored: a
		// typo'd header would otherwise leave the caller believing they had
		// changed something.
		s.logger.WithField("header", header).
			Warn("Unrecognised TAS-Prompt-Cache value; using the route default")
	}
	_ = requested

	// Precedence: an explicit header wins; absent a header the gateway-wide
	// default (prompt_cache.default_mode, parsed once at construction) applies;
	// absent that, the built-in default. A per-route default from dashboard-be
	// resolution will slot between header and global once that block exists —
	// Resolve already models header > route > global.
	mode := promptcache.Resolve(s.promptCacheDefault, header)

	applied, breakpoints := promptcache.Apply(req, mode)
	if removed := promptcache.Clamp(req); removed > 0 {
		// A fifth breakpoint is a 400 from the vendor, so clamping converts a
		// failed request into a working one — but silently dropping what the
		// caller asked for would be its own trap, hence the warning.
		s.logger.WithFields(logrus.Fields{
			"removed": removed,
			"limit":   promptcache.MaxBreakpoints,
		}).Warn("Clamped prompt-cache breakpoints to the API limit")
		breakpoints = promptcache.MaxBreakpoints
	}

	if mode == promptcache.ModeAuto && !promptcache.Available(mode) {
		// Asked for placement we cannot yet do. Saying so is the difference
		// between "we did nothing" and "we did nothing and told you we did
		// something".
		s.logger.Debug("TAS-Prompt-Cache: auto requested; placement engine not yet available, honouring caller breakpoints")
	}

	middleware.StampPromptCache(r.Context(), string(applied), breakpoints)
}
