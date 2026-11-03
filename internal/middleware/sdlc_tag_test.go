package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/sdlctag"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/tokens"
)

// sdlcStampHandler does what server.handleChatCompletion does for mode A:
// tag only inside AIQG mode, with the resolved tenant, and stamp the result.
type sdlcStampHandler struct {
	tagger *sdlctag.Tagger
	msgs   []types.Message
}

func (h sdlcStampHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.tagger != nil && RoutingFromContext(r.Context()) != nil {
		StampSDLCTurnStart(r.Context(), h.tagger.Tag(ResolvedTenantFromContext(r.Context()), h.msgs))
	}
	w.WriteHeader(http.StatusOK)
}

func runSDLC(t *testing.T, cfg sdlctag.Config, bearer string) (events.RequestEvent, events.ResponseEvent, []byte) {
	t.Helper()
	em := &events.MemoryEmitter{}
	mw := NewAIQG(AIQGConfig{
		Strict:  true,
		Logger:  silentLogger(),
		Emitter: em,
		Resolver: tokens.NewMapResolver([]tokens.ConfigToken{
			{TokenID: "tok_on", Token: "tas_qg_live_on", TenantID: "tenant-on", AIQGAccountID: "acct"},
			{TokenID: "tok_off", Token: "tas_qg_live_off", TenantID: "tenant-off", AIQGAccountID: "acct"},
		}),
	})
	h := sdlcStampHandler{
		tagger: sdlctag.New(cfg, silentLogger()),
		msgs: []types.Message{
			{Role: "assistant", Content: "Tests are green. Want me to open the PR?"},
			{Role: "user", Content: "yes go ahead"},
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("TAS-Auth", bearer)
	mw(h).ServeHTTP(httptest.NewRecorder(), req)
	if em.Len() != 1 {
		t.Fatalf("emit count=%d want=1", em.Len())
	}
	reqEv, respEv := em.Requests()[0].Data, em.Responses()[0].Data
	b, err := json.Marshal(em.Responses()[0])
	if err != nil {
		t.Fatal(err)
	}
	return reqEv, respEv, b
}

func TestSDLCTag_OffAddsNothingToTheEvent(t *testing.T) {
	cases := map[string]struct {
		cfg    sdlctag.Config
		bearer string
	}{
		"default config":            {sdlctag.Config{}, "tas_qg_live_on"},
		"tenant listed, global off": {sdlctag.Config{Tenants: []string{"tenant-on"}}, "tas_qg_live_on"},
		"global on, tenant not listed": {
			sdlctag.Config{Enabled: true, Tenants: []string{"tenant-on"}}, "tas_qg_live_off",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			reqEv, respEv, raw := runSDLC(t, c.cfg, c.bearer)
			if reqEv.SDLCTurnStart != nil || respEv.SDLCTurnStart != nil {
				t.Fatalf("mode A field present while off: req=%+v resp=%+v", reqEv.SDLCTurnStart, respEv.SDLCTurnStart)
			}
			if strings.Contains(string(raw), "sdlc_turn_start") {
				t.Fatalf("event JSON carries sdlc_turn_start while off: %s", raw)
			}
		})
	}
}

func TestSDLCTag_OnStampsBothEvents(t *testing.T) {
	reqEv, respEv, raw := runSDLC(t, sdlctag.Config{Enabled: true, Tenants: []string{"tenant-on"}}, "tas_qg_live_on")
	if reqEv.SDLCTurnStart == nil || respEv.SDLCTurnStart == nil {
		t.Fatal("mode A on for this tenant, but the events carry no turn-start flags")
	}
	ts := respEv.SDLCTurnStart
	if !ts.StartsApproval || !ts.PrevExplicitAsk || !ts.HasPrev {
		t.Errorf("flags = %+v", ts)
	}
	if !strings.Contains(string(raw), `"sdlc_turn_start"`) {
		t.Errorf("response JSON lacks sdlc_turn_start: %s", raw)
	}
	for _, word := range []string{"green", "open the PR", "go ahead"} {
		if strings.Contains(string(raw), word) {
			t.Errorf("event JSON contains message text %q", word)
		}
	}
}
