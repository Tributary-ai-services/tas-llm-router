package workflow

import "testing"

// OperationToWorkflow is a partial map + heuristic fallthrough: only the three
// agentic ops override the classifier; everything else (generic ops, embeddings,
// unknown/future ops) returns ("", false) so the caller uses the heuristic.
// See aether-shared/data-models/aiqg/otel-genai-ingestion.md §3.
func TestOperationToWorkflow(t *testing.T) {
	cases := []struct {
		op       string
		wantType string
		wantOK   bool
	}{
		// Agentic ops override the heuristic.
		{"invoke_agent", WorkflowAgentic, true},
		{"execute_tool", WorkflowAgentic, true},
		{"create_agent", WorkflowAgentic, true},
		// Case-insensitive + trims.
		{"  INVOKE_AGENT  ", WorkflowAgentic, true},
		{"Execute_Tool", WorkflowAgentic, true},
		// Generic ops fall through to the heuristic.
		{"chat", "", false},
		{"text_completion", "", false},
		{"generate_content", "", false},
		// Excluded op — never classified.
		{"embeddings", "", false},
		// Unknown/future op — forward-compatible fallthrough.
		{"some_future_op", "", false},
		// Empty.
		{"", "", false},
		{"   ", "", false},
	}
	for _, c := range cases {
		gotType, gotOK := OperationToWorkflow(c.op)
		if gotType != c.wantType || gotOK != c.wantOK {
			t.Errorf("OperationToWorkflow(%q) = (%q, %v), want (%q, %v)",
				c.op, gotType, gotOK, c.wantType, c.wantOK)
		}
	}
}

func TestOperationExcluded(t *testing.T) {
	if !OperationExcluded("embeddings") {
		t.Error("embeddings must be excluded")
	}
	if !OperationExcluded("  Embeddings ") {
		t.Error("excluded check must be case-insensitive + trim")
	}
	for _, op := range []string{"chat", "invoke_agent", "", "unknown"} {
		if OperationExcluded(op) {
			t.Errorf("%q must not be excluded", op)
		}
	}
}
