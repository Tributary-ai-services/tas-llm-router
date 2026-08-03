package middleware

import "testing"

// gen_ai.* explicit headers are ingested into AIQGHeaders. Go canonicalizes
// header names on both store and lookup, so any client casing of the dotted
// gen_ai.* names resolves. See otel-genai-ingestion.md §2.
func TestParseHeaders_OTelGenAI(t *testing.T) {
	req := newReq(t, map[string]string{
		"gen_ai.operation.name":  "invoke_agent",
		"gen_ai.agent.id":        "agent-otel-1",
		"gen_ai.agent.name":      "Planner",
		"gen_ai.conversation.id": "conv-otel-9",
		"gen_ai.system":          "anthropic",
	})
	h, err := ParseHeaders(req)
	if err != nil {
		t.Fatalf("ParseHeaders err: %v", err)
	}
	if h.OTelOperation != "invoke_agent" {
		t.Errorf("OTelOperation=%q", h.OTelOperation)
	}
	if h.OTelAgentID != "agent-otel-1" {
		t.Errorf("OTelAgentID=%q", h.OTelAgentID)
	}
	if h.OTelAgentName != "Planner" {
		t.Errorf("OTelAgentName=%q", h.OTelAgentName)
	}
	if h.OTelConversationID != "conv-otel-9" {
		t.Errorf("OTelConversationID=%q", h.OTelConversationID)
	}
	if h.OTelSystem != "anthropic" {
		t.Errorf("OTelSystem=%q", h.OTelSystem)
	}
}

// Mixed-case client header names still resolve (canonicalization lowercases
// everything after the first letter for these dotted names).
func TestParseHeaders_OTelGenAI_CaseInsensitive(t *testing.T) {
	req := newReq(t, map[string]string{
		"Gen_AI.Operation.Name": "execute_tool",
	})
	h, err := ParseHeaders(req)
	if err != nil {
		t.Fatalf("ParseHeaders err: %v", err)
	}
	if h.OTelOperation != "execute_tool" {
		t.Errorf("mixed-case gen_ai header not resolved: OTelOperation=%q", h.OTelOperation)
	}
}

// gen_ai.* are TAS-internal attribution signals and MUST be stripped before
// the vendor (unlike traceparent). See otel-genai-ingestion.md §5.
func TestStripFromOutbound_OTelGenAI(t *testing.T) {
	req := newReq(t, map[string]string{
		"gen_ai.operation.name":  "invoke_agent",
		"gen_ai.agent.id":        "agent-otel-1",
		"gen_ai.agent.name":      "Planner",
		"gen_ai.conversation.id": "conv-otel-9",
		"gen_ai.system":          "anthropic",
		"traceparent":            "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
	})
	StripFromOutbound(req)
	for _, name := range []string{
		"gen_ai.operation.name", "gen_ai.agent.id", "gen_ai.agent.name",
		"gen_ai.conversation.id", "gen_ai.system",
	} {
		if req.Header.Get(name) != "" {
			t.Errorf("%s must be stripped before vendor", name)
		}
	}
	if req.Header.Get("traceparent") == "" {
		t.Error("traceparent must NOT be stripped (standard header)")
	}
}
