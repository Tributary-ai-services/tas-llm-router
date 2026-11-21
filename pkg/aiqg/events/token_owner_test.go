package events

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/instrumentation"
)

// Plan #22 P1: the response event says which token sent the request and whose
// it is, because the aggregator reads response events only.
func TestBuild_StampsTokenAndOwnerOnResponse(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	tv := TokenView{TenantID: "tenant-a", AIQGAccountID: "acct-a",
		TASAuthTokenID: "11111111-2222-4333-8444-555555555555", OwnerSub: "kc-owner"}
	reqEnv, respEnv := Build(r, AIQGHeadersView{}, RoutingView{}, tv, instrumentation.Snapshot{}, BuildOptions{HTTPStatus: 200})

	if respEnv.Data.TASAuthTokenID != tv.TASAuthTokenID || respEnv.Data.TokenOwnerSub != "kc-owner" {
		t.Errorf("response event token/owner = (%q, %q)", respEnv.Data.TASAuthTokenID, respEnv.Data.TokenOwnerSub)
	}
	if reqEnv.Data.TASAuthTokenID != tv.TASAuthTokenID {
		t.Errorf("request event lost tas_auth_token_id: %q", reqEnv.Data.TASAuthTokenID)
	}
	b, _ := json.Marshal(respEnv.Data)
	if !strings.Contains(string(b), `"token_owner_sub":"kc-owner"`) || !strings.Contains(string(b), `"tas_auth_token_id":"11111111-`) {
		t.Errorf("Kafka payload lacks the fields: %s", b)
	}
}

// An unowned token, or no resolver at all, must not grow empty keys.
func TestBuild_OmitsOwnerWhenUnowned(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, respEnv := Build(r, AIQGHeadersView{}, RoutingView{}, TokenView{}, instrumentation.Snapshot{}, BuildOptions{HTTPStatus: 200})
	b, _ := json.Marshal(respEnv.Data)
	for _, k := range []string{"token_owner_sub", "tas_auth_token_id"} {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Errorf("%s emitted with no token: %s", k, b)
		}
	}
}

func TestEmitter_PromotesTokenAndOwner(t *testing.T) {
	resp := ResponseEnvelope{}
	resp.Data.TASAuthTokenID = "11111111-2222-4333-8444-555555555555"
	resp.Data.TokenOwnerSub = "kc-owner"
	m := emitAndParse(t, resp)
	if m["tas_auth_token_id"] != resp.Data.TASAuthTokenID || m["token_owner_sub"] != "kc-owner" {
		t.Errorf("not promoted: token=%v owner=%v", m["tas_auth_token_id"], m["token_owner_sub"])
	}

	m = emitAndParse(t, ResponseEnvelope{})
	for _, k := range []string{"tas_auth_token_id", "token_owner_sub"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s promoted when empty", k)
		}
	}
}
