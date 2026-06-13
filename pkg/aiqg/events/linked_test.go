package events

import "testing"

// The `linked` tier: a tool_call_id echo proves flow/step topology even when
// the caller sent no TAS-Flow-Id. "asserted wins for WHO, linked wins for
// SHAPE" — step/parent are always applied; LinkedFlowID only fills an
// otherwise-empty FlowID, and `linked` ranks below asserted/baggage/trace.
func TestBuildAgentContext_Linked(t *testing.T) {
	tok := TokenView{TASAuthTokenID: "tok-1"} // principal always present in AIQG mode

	t.Run("untagged request linked by tool_call echo", func(t *testing.T) {
		lk := Linkage{StepID: "resp-2", ParentStepID: "resp-1", LinkedFlowID: "flow-X"}
		ac := buildAgentContext(AIQGHeadersView{}, tok, "", lk)
		if ac == nil {
			t.Fatal("nil agent context")
		}
		if ac.IdentitySource != "linked" {
			t.Errorf("identity_source = %q, want linked", ac.IdentitySource)
		}
		if ac.FlowID != "flow-X" || ac.ParentStepID != "resp-1" || ac.StepID != "resp-2" {
			t.Errorf("linkage not applied: %+v", ac)
		}
	})

	t.Run("asserted agent keeps WHO, linkage supplies SHAPE", func(t *testing.T) {
		// Agent named via header (asserted) but no TAS-Flow-Id; linkage fills the flow.
		h := AIQGHeadersView{AgentID: "agent-7"}
		lk := Linkage{StepID: "resp-9", ParentStepID: "resp-8", LinkedFlowID: "flow-Y"}
		ac := buildAgentContext(h, tok, "", lk)
		if ac.IdentitySource != "asserted" {
			t.Errorf("identity_source = %q, want asserted (agent named wins WHO)", ac.IdentitySource)
		}
		if ac.FlowID != "flow-Y" || ac.ParentStepID != "resp-8" {
			t.Errorf("linked shape not applied under asserted: %+v", ac)
		}
	})

	t.Run("explicit TAS-Flow-Id wins over linked flow", func(t *testing.T) {
		h := AIQGHeadersView{FlowID: "header-flow"}
		lk := Linkage{StepID: "resp-3", LinkedFlowID: "flow-Z"}
		ac := buildAgentContext(h, tok, "", lk)
		if ac.FlowID != "header-flow" {
			t.Errorf("FlowID = %q, want the asserted header flow", ac.FlowID)
		}
	})

	t.Run("step id alone keeps principal tier", func(t *testing.T) {
		ac := buildAgentContext(AIQGHeadersView{}, tok, "", Linkage{StepID: "resp-5"})
		if ac.IdentitySource != "principal" {
			t.Errorf("identity_source = %q, want principal (no link, token present)", ac.IdentitySource)
		}
		if ac.StepID != "resp-5" {
			t.Errorf("step id not stamped: %+v", ac)
		}
	})
}
