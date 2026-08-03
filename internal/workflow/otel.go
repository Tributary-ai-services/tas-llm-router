package workflow

import "strings"

// OTelOperationMapVersion versions the gen_ai.operation.name -> workflow_type
// mapping so historical classification drift stays interpretable across
// changes to the table below. See
// aether-shared/data-models/aiqg/otel-genai-ingestion.md §3.
const OTelOperationMapVersion = "otel-map-v1"

// agenticOperations are the OTel gen_ai.operation.name values that map
// unambiguously to the `agentic` workflow type and are confident enough to
// override the heuristic classifier.
var agenticOperations = map[string]struct{}{
	"invoke_agent": {},
	"execute_tool": {},
	"create_agent": {},
}

// excludedOperations are OTel operations that are not chat-completion
// workloads (e.g. embeddings). They never produce a declared workflow and
// are distinguished from generic-fallthrough ops only for auditability.
var excludedOperations = map[string]struct{}{
	"embeddings": {},
}

// OperationToWorkflow maps an OTel gen_ai.operation.name to an AIQG
// workflow_type. It returns (type, true) ONLY for operations confident
// enough to override the heuristic classifier (invoke_agent / execute_tool /
// create_agent -> agentic). For generic ops (chat / text_completion /
// generate_content), excluded ops (embeddings), and unknown/future ops it
// returns ("", false) so the caller falls through to the heuristic
// classifier. Case-insensitive; trims surrounding whitespace.
//
// This is a partial map + heuristic fallthrough by design: OTel's op-name
// set is coarser than the 6-type taxonomy, so most ops cannot be classified
// from the op-name alone.
func OperationToWorkflow(op string) (string, bool) {
	op = strings.ToLower(strings.TrimSpace(op))
	if op == "" {
		return "", false
	}
	if _, ok := agenticOperations[op]; ok {
		return WorkflowAgentic, true
	}
	return "", false
}

// OperationExcluded reports whether an OTel operation is a non-chat-completion
// workload that should not be classified at all (e.g. embeddings). Used to
// distinguish "excluded" from "generic fallthrough" when recording the raw
// op-name for classification drift.
func OperationExcluded(op string) bool {
	op = strings.ToLower(strings.TrimSpace(op))
	_, ok := excludedOperations[op]
	return ok
}
