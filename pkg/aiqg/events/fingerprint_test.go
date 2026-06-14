package events

import "testing"

// The `fingerprinted` tier (§B): a per-(tenant, principal) surrogate minted
// from the request's structural signature. It promotes identity only when
// nothing stronger resolved, is always recorded for the asserted-vs-inferred
// cross-check, and never collides across tenants/principals.
func TestBuildAgentContext_Fingerprinted(t *testing.T) {
	tok := TokenView{TenantID: "t1", TASAuthTokenID: "tok-1"} // principal present
	const fp = "sig-abc"

	t.Run("untagged request with a fingerprint becomes fingerprinted", func(t *testing.T) {
		ac := buildAgentContext(AIQGHeadersView{}, tok, "", Linkage{StepID: "r1", Fingerprint: fp})
		if ac == nil {
			t.Fatal("nil agent context")
		}
		if ac.IdentitySource != "fingerprinted" {
			t.Errorf("identity_source = %q, want fingerprinted", ac.IdentitySource)
		}
		if ac.AgentSurrogateID == "" || ac.AgentID != ac.AgentSurrogateID {
			t.Errorf("surrogate should fill agent_id: %+v", ac)
		}
	})

	t.Run("surrogate is stable per signature and scoped per principal", func(t *testing.T) {
		a := buildAgentContext(AIQGHeadersView{}, tok, "", Linkage{Fingerprint: fp})
		b := buildAgentContext(AIQGHeadersView{}, tok, "", Linkage{Fingerprint: fp})
		if a.AgentSurrogateID != b.AgentSurrogateID {
			t.Error("same (tenant, principal, signature) must yield the same surrogate")
		}
		// Different principal → different surrogate (no cross-customer collision).
		other := buildAgentContext(AIQGHeadersView{}, TokenView{TenantID: "t1", TASAuthTokenID: "tok-2"}, "", Linkage{Fingerprint: fp})
		if other.AgentSurrogateID == a.AgentSurrogateID {
			t.Error("different principal must yield a different surrogate")
		}
		// Different tenant → different surrogate.
		otherT := buildAgentContext(AIQGHeadersView{}, TokenView{TenantID: "t2", TASAuthTokenID: "tok-1"}, "", Linkage{Fingerprint: fp})
		if otherT.AgentSurrogateID == a.AgentSurrogateID {
			t.Error("different tenant must yield a different surrogate")
		}
	})

	t.Run("stronger tier wins WHO but surrogate is still recorded for cross-check", func(t *testing.T) {
		// Baggage user wins identity_source, but a fingerprint present must
		// still be stamped so an impersonation mismatch can be surfaced (§E).
		h := AIQGHeadersView{BaggageUserID: "u_alice"}
		ac := buildAgentContext(h, tok, "", Linkage{Fingerprint: fp})
		if ac.IdentitySource != "baggage" {
			t.Errorf("identity_source = %q, want baggage", ac.IdentitySource)
		}
		if ac.AgentSurrogateID == "" {
			t.Error("surrogate should be recorded even under a stronger tier")
		}
		if ac.AgentID != "" {
			t.Errorf("surrogate must NOT overwrite agent_id under a stronger tier: %+v", ac)
		}
	})

	t.Run("asserted agent keeps its id; surrogate recorded alongside", func(t *testing.T) {
		h := AIQGHeadersView{AgentID: "agent-real"}
		ac := buildAgentContext(h, tok, "", Linkage{Fingerprint: fp})
		if ac.IdentitySource != "asserted" || ac.AgentID != "agent-real" {
			t.Errorf("asserted agent must be preserved: %+v", ac)
		}
		if ac.AgentSurrogateID == "" {
			t.Error("surrogate should still be computed for the cross-check")
		}
	})

	t.Run("no fingerprint leaves principal tier untouched", func(t *testing.T) {
		ac := buildAgentContext(AIQGHeadersView{}, tok, "", Linkage{StepID: "r1"})
		if ac.IdentitySource != "principal" {
			t.Errorf("identity_source = %q, want principal", ac.IdentitySource)
		}
		if ac.AgentSurrogateID != "" || ac.AgentID != "" {
			t.Errorf("no surrogate without a fingerprint: %+v", ac)
		}
	})
}
