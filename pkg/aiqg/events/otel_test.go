package events

import (
	"encoding/json"
	"testing"
)

// Workflow precedence: explicit TAS-Workflow header > OTel-declared > heuristic.
// See otel-genai-ingestion.md §4.1.
func TestPreferredWorkflow_Precedence(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		otel       string
		classified string
		want       string
	}{
		{"header wins over all", "rag", "agentic", "single_turn_qa", "rag"},
		{"otel beats heuristic", "", "agentic", "single_turn_qa", "agentic"},
		{"heuristic when no declared", "", "", "single_turn_qa", "single_turn_qa"},
		{"header wins even if only header", "summarization", "", "", "summarization"},
		{"all empty stays empty", "", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := preferredWorkflow(c.header, c.otel, c.classified); got != c.want {
				t.Errorf("preferredWorkflow(%q,%q,%q) = %q, want %q",
					c.header, c.otel, c.classified, got, c.want)
			}
		})
	}
}

// The `otel` identity tier: gen_ai.agent.* fills agent_id/name when no
// TAS-Agent-* is present, sits below asserted and above trace/fingerprinted.
// See otel-genai-ingestion.md §4.2.
func TestBuildAgentContext_OTelTier(t *testing.T) {
	tok := TokenView{TenantID: "t1", TASAuthTokenID: "tok-1"}

	t.Run("otel agent fills id/name; identity_source=otel", func(t *testing.T) {
		h := AIQGHeadersView{OTelAgentID: "agent-otel", OTelAgentName: "Planner"}
		ac := buildAgentContext(h, tok, "", Linkage{})
		if ac == nil {
			t.Fatal("nil agent context")
		}
		if ac.IdentitySource != "otel" {
			t.Errorf("identity_source = %q, want otel", ac.IdentitySource)
		}
		if ac.AgentID != "agent-otel" || ac.AgentName != "Planner" {
			t.Errorf("otel agent not folded: %+v", ac)
		}
	})

	t.Run("TAS-Agent-* beats OTel (asserted wins)", func(t *testing.T) {
		h := AIQGHeadersView{AgentID: "agent-tas", OTelAgentID: "agent-otel"}
		ac := buildAgentContext(h, tok, "", Linkage{})
		if ac.IdentitySource != "asserted" {
			t.Errorf("identity_source = %q, want asserted", ac.IdentitySource)
		}
		if ac.AgentID != "agent-tas" {
			t.Errorf("asserted agent must win, got %q", ac.AgentID)
		}
	})

	t.Run("otel beats trace", func(t *testing.T) {
		h := AIQGHeadersView{OTelAgentID: "agent-otel", TraceID: "trace-xyz"}
		ac := buildAgentContext(h, tok, "", Linkage{})
		if ac.IdentitySource != "otel" {
			t.Errorf("identity_source = %q, want otel (beats trace)", ac.IdentitySource)
		}
	})

	t.Run("otel beats fingerprint; surrogate does not overwrite otel agent", func(t *testing.T) {
		h := AIQGHeadersView{OTelAgentID: "agent-otel"}
		ac := buildAgentContext(h, tok, "", Linkage{Fingerprint: "sig-abc"})
		if ac.IdentitySource != "otel" {
			t.Errorf("identity_source = %q, want otel (beats fingerprinted)", ac.IdentitySource)
		}
		if ac.AgentID != "agent-otel" {
			t.Errorf("fingerprint surrogate must not overwrite the otel agent id, got %q", ac.AgentID)
		}
		if ac.AgentSurrogateID == "" {
			t.Error("surrogate should still be recorded for the cross-check")
		}
	})

	t.Run("otel conversation fills when no header/baggage", func(t *testing.T) {
		h := AIQGHeadersView{OTelConversationID: "conv-otel"}
		ac := buildAgentContext(h, tok, "", Linkage{})
		if ac.ConversationID != "conv-otel" {
			t.Errorf("conversation_id = %q, want conv-otel", ac.ConversationID)
		}
		if ac.IdentitySource != "otel" {
			t.Errorf("identity_source = %q, want otel", ac.IdentitySource)
		}
	})

	t.Run("TAS conversation header beats otel conversation", func(t *testing.T) {
		h := AIQGHeadersView{ConversationID: "conv-tas", OTelConversationID: "conv-otel"}
		ac := buildAgentContext(h, tok, "", Linkage{})
		if ac.ConversationID != "conv-tas" {
			t.Errorf("conversation_id = %q, want conv-tas (header wins)", ac.ConversationID)
		}
	})
}

// Round-trip pins the snake_case JSON keys on AgentContext, including the
// identity_source value written by the otel tier. Fills the coverage gap
// noted in the plan (only a CloudEvents-shape test existed before).
func TestAgentContext_JSONKeysRoundTrip(t *testing.T) {
	ac := buildAgentContext(
		AIQGHeadersView{OTelAgentID: "agent-otel", OTelAgentName: "Planner", OTelConversationID: "conv-otel"},
		TokenView{TenantID: "t1", TASAuthTokenID: "tok-1"},
		"",
		Linkage{StepID: "r1"},
	)
	if ac == nil {
		t.Fatal("nil agent context")
	}
	b, err := json.Marshal(ac)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"agent_id", "agent_name", "conversation_id", "identity_source"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing snake_case key %q in %s", key, b)
		}
	}
	if m["identity_source"] != "otel" {
		t.Errorf("identity_source = %v, want otel", m["identity_source"])
	}
}
