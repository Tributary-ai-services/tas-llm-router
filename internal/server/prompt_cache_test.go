package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/promptcache"
)

// #100 (P1 finish): the gateway-wide prompt_cache.default_mode must participate
// in resolution. Before this, applyPromptCacheMode passed a hardcoded "" route
// default, so a configured default of `off` could never strip breakpoints and
// `auto` could never be the standing mode without a header on every request.

func pcLogger() *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.FatalLevel)
	return l
}

// reqOneBreakpoint returns a request carrying exactly one breakpoint (a
// system-block cache_control).
func reqOneBreakpoint() *types.ChatRequest {
	return &types.ChatRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []types.Message{
			{Role: "system", Content: "big stable system", CacheControl: &types.CacheControl{Type: "ephemeral"}},
			{Role: "user", Content: "hi"},
		},
	}
}

// applyWithDefault runs applyPromptCacheMode for a given configured default +
// header and returns the stamped outcome plus whether the request's breakpoint
// survived.
func applyWithDefault(t *testing.T, def promptcache.Mode, header string) (mode string, breakpoints int, survived bool) {
	t.Helper()
	s := &Server{logger: pcLogger(), promptCacheDefault: def}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if header != "" {
		r.Header.Set(promptcache.HeaderMode, header)
	}
	rt := middleware.NewRouting()
	r = r.WithContext(middleware.WithRouting(r.Context(), rt))

	req := reqOneBreakpoint()
	s.applyPromptCacheMode(r, req)

	snap := rt.Snapshot()
	return snap.PromptCacheMode, snap.PromptCacheBreakpoints, req.Messages[0].CacheControl != nil
}

// A configured default of `off` strips breakpoints with no header at all — the
// behaviour that was impossible while the route default was hardcoded "".
func TestPromptCacheDefaultOffStripsWithoutHeader(t *testing.T) {
	mode, bps, survived := applyWithDefault(t, promptcache.ModeOff, "")
	if mode != string(promptcache.ModeOff) {
		t.Errorf("mode = %q, want off", mode)
	}
	if bps != 0 {
		t.Errorf("breakpoints = %d, want 0", bps)
	}
	if survived {
		t.Error("breakpoint survived a default-off route; it should have been stripped")
	}
}

// A header always beats the configured default.
func TestPromptCacheHeaderBeatsDefault(t *testing.T) {
	mode, bps, survived := applyWithDefault(t, promptcache.ModeOff, "passthrough")
	if mode != string(promptcache.ModePassthrough) {
		t.Errorf("mode = %q, want passthrough (header overrides default off)", mode)
	}
	if bps != 1 || !survived {
		t.Errorf("breakpoint should survive when the header asks for passthrough; bps=%d survived=%v", bps, survived)
	}
}

// An explicit `off` from the caller wins over a non-off default — the escape
// hatch must not be overridable by a route/global default.
func TestPromptCacheHeaderOffBeatsPassthroughDefault(t *testing.T) {
	mode, bps, survived := applyWithDefault(t, promptcache.ModePassthrough, "off")
	if mode != string(promptcache.ModeOff) {
		t.Errorf("mode = %q, want off (caller off must win)", mode)
	}
	if bps != 0 || survived {
		t.Errorf("breakpoint should be stripped by an explicit off; bps=%d survived=%v", bps, survived)
	}
}

// No configured default and no header → the built-in default (passthrough):
// unchanged behaviour, so existing deployments are unaffected.
func TestPromptCacheNoDefaultNoHeaderIsPassthrough(t *testing.T) {
	mode, bps, survived := applyWithDefault(t, "", "")
	if mode != string(promptcache.ModePassthrough) {
		t.Errorf("mode = %q, want passthrough (built-in default)", mode)
	}
	if bps != 1 || !survived {
		t.Errorf("breakpoint should survive under the built-in default; bps=%d survived=%v", bps, survived)
	}
}
