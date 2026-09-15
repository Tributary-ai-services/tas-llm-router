package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/upstreamkey"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/credentials"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
)

// These pin whose key an evaluation call bills (#184). The gateway's judge and
// replay calls always used the gateway's own key, which is wrong for a BYOK
// tenant twice over: the spend lands on the wrong account, and a customer who
// brought their own key never agreed to the gateway spending its key on their
// traffic.
//
// credentials.Resolver is a concrete type, so these drive the REAL resolver
// against an httptest server rather than widening an interface for tests. A
// fresh resolver per case keeps its TTL cache from leaking between them.

func credResolverFor(t *testing.T, status int, body map[string]any) *credentials.Resolver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return credentials.NewResolver(srv.URL, "internal-token", time.Minute)
}

func evalAttrFor(path string) *evalAttribution {
	return &evalAttribution{TenantID: "tenant-1", Path: path}
}

func TestResolveEvalKey_StoredKeyBillsTheTenant(t *testing.T) {
	before := testutil.ToFloat64(metrics.EvalCredentialSourceTotal.WithLabelValues("judge", metrics.EvalCredTenantStored))

	jr := &judgeRunner{
		creds: credResolverFor(t, http.StatusOK, map[string]any{
			"found": true, "api_key": "sk-tenant-own", "credential_id": "cred-1", "allow_shared_fallback": false,
		}),
		log: logrus.New(),
	}

	dec := jr.resolveEvalKey(context.Background(), evalAttrFor("judge"), "anthropic")

	if dec.skip {
		t.Fatal("a tenant with a stored key must be evaluated, not skipped")
	}
	if got := upstreamkey.From(dec.ctx); got != "sk-tenant-own" {
		t.Errorf("upstream key = %q, want the tenant's own key", got)
	}
	if got := testutil.ToFloat64(metrics.EvalCredentialSourceTotal.WithLabelValues("judge", metrics.EvalCredTenantStored)); got != before+1 {
		t.Errorf("tenant_stored delta = %v, want 1", got-before)
	}
}

func TestResolveEvalKey_SharedFallbackIsConsent(t *testing.T) {
	before := testutil.ToFloat64(metrics.EvalCredentialSourceTotal.WithLabelValues("judge", metrics.EvalCredTASShared))

	jr := &judgeRunner{
		creds: credResolverFor(t, http.StatusOK, map[string]any{
			"found": false, "allow_shared_fallback": true,
		}),
		log: logrus.New(),
	}

	dec := jr.resolveEvalKey(context.Background(), evalAttrFor("judge"), "anthropic")

	// The tenant declared the shared key acceptable, so using it is consent
	// rather than assumption.
	if dec.skip {
		t.Fatal("a tenant allowing shared fallback should still be evaluated")
	}
	if got := upstreamkey.From(dec.ctx); got != "" {
		t.Errorf("no override expected when falling back, got %q", got)
	}
	if got := testutil.ToFloat64(metrics.EvalCredentialSourceTotal.WithLabelValues("judge", metrics.EvalCredTASShared)); got != before+1 {
		t.Errorf("tas_shared delta = %v, want 1", got-before)
	}
}

// The decision this PR exists for: a BYOK-only tenant with no stored key is
// never evaluated on the gateway's key.
func TestResolveEvalKey_BYOKOnlyWithNoKeyIsSkippedAndCounted(t *testing.T) {
	before := testutil.ToFloat64(metrics.JudgeExcludedTotal.WithLabelValues(metrics.JudgeExcludedBYOKOnly))
	sharedBefore := testutil.ToFloat64(metrics.EvalCredentialSourceTotal.WithLabelValues("judge", metrics.EvalCredTASShared))

	jr := &judgeRunner{
		creds: credResolverFor(t, http.StatusOK, map[string]any{
			"found": false, "allow_shared_fallback": false,
		}),
		log: logrus.New(),
	}

	dec := jr.resolveEvalKey(context.Background(), evalAttrFor("judge"), "anthropic")

	if !dec.skip {
		t.Fatal("a BYOK-only tenant with no stored key must not be evaluated on the gateway's key")
	}
	// Skipped, not silently absent — otherwise a BYOK-only tenant's
	// evaluations vanish from coverage with nothing indicating why.
	if got := testutil.ToFloat64(metrics.JudgeExcludedTotal.WithLabelValues(metrics.JudgeExcludedBYOKOnly)); got != before+1 {
		t.Errorf("byok_only_no_key delta = %v, want 1", got-before)
	}
	// And it must not also register as having used the shared key.
	if got := testutil.ToFloat64(metrics.EvalCredentialSourceTotal.WithLabelValues("judge", metrics.EvalCredTASShared)); got != sharedBefore {
		t.Errorf("a skip must not record shared-key use: delta %v", got-sharedBefore)
	}
}

// A resolver blip must not drop the evaluation — same degrade the request path
// takes.
func TestResolveEvalKey_ResolverErrorDegradesToConfiguredKey(t *testing.T) {
	before := testutil.ToFloat64(metrics.EvalCredentialSourceTotal.WithLabelValues("shadow_replay", metrics.EvalCredResolverErr))

	jr := &judgeRunner{
		creds: credResolverFor(t, http.StatusInternalServerError, nil),
		log:   logrus.New(),
	}

	dec := jr.resolveEvalKey(context.Background(), evalAttrFor("shadow_replay"), "openai")

	if dec.skip {
		t.Fatal("a resolver blip should degrade, not drop the evaluation")
	}
	if got := upstreamkey.From(dec.ctx); got != "" {
		t.Errorf("no override expected on a resolver error, got %q", got)
	}
	if got := testutil.ToFloat64(metrics.EvalCredentialSourceTotal.WithLabelValues("shadow_replay", metrics.EvalCredResolverErr)); got != before+1 {
		t.Errorf("resolver_error delta = %v, want 1", got-before)
	}
}

// BYOK unconfigured or the call unattributed: pre-#184 behaviour, and nothing
// recorded, because there was no tenant who could have preferred otherwise.
func TestResolveEvalKey_UnconfiguredOrUnattributedIsANoOp(t *testing.T) {
	cases := []struct {
		name string
		jr   *judgeRunner
		attr *evalAttribution
	}{
		{"nil runner", nil, evalAttrFor("judge")},
		{"BYOK unconfigured", &judgeRunner{log: logrus.New()}, evalAttrFor("judge")},
		{"no attribution", &judgeRunner{log: logrus.New()}, nil},
		{"empty tenant", &judgeRunner{log: logrus.New()}, &evalAttribution{Path: "judge"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := tc.jr.resolveEvalKey(context.Background(), tc.attr, "anthropic")
			if dec.skip {
				t.Error("must not skip when there is no BYOK policy to respect")
			}
			if upstreamkey.From(dec.ctx) != "" {
				t.Error("no key override expected")
			}
		})
	}
}
