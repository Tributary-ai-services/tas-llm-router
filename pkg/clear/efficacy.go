package clear

// scoreEfficacy converts the vendor's finish_reason into a 0-100
// Efficacy score. This is the MVP — finish-reason-only — implementation
// of source-spec §2.2.2's Efficacy dimension. Full structural-validity
// + implicit-acceptance + groundedness sub-metrics need response-body
// capture (deferred slice).
//
// Vendor vocabulary is normalized first so the OpenAI ("stop", "length"
// …) and Anthropic ("end_turn", "max_tokens", "tool_use" …) families
// score the same outcome the same way. New vendors only need an entry
// in normalizeFinishReason — the score table stays one source of truth.
//
// Mapping (after normalization):
//
//	normalized        | score | rationale
//	"stop"            | 100   | clean completion — model finished naturally
//	"tool_calls"      | 100   | function-calling / tool-use success path
//	"length"          |  60   | truncated at max_tokens — partial output
//	"content_filter"  |   0   | vendor blocked output — policy violation
//	"" / unknown      |  nil  | not captured; dashboards flag for code update
//
// 60 for "length" sits in the Marginal band (50-74 per spec §2.2 default).
// 0 for "content_filter" mirrors how Assurance handles critical findings:
// hard policy outcomes shouldn't be diluted.
//
// HTTPStatus=0 (gateway-blocked) returns nil — no vendor response means
// no finish_reason means no Efficacy signal.
func scoreEfficacy(in Input) *Score {
	if in.HTTPStatus == 0 {
		return nil
	}
	if in.FinishReason == "" {
		return nil
	}
	var v Score
	switch normalizeFinishReason(in.FinishReason) {
	case "stop", "tool_calls":
		v = 100
	case "length":
		v = 60
	case "content_filter":
		v = 0
	default:
		// Unknown finish_reason after normalization — return nil so
		// dashboards can flag the request and prompt a code update.
		return nil
	}
	return &v
}

// normalizeFinishReason maps each vendor's native finish-reason
// vocabulary onto a single canonical set. Vendors don't agree:
//   - OpenAI: "stop", "length", "tool_calls", "function_call", "content_filter"
//   - Anthropic: "end_turn", "max_tokens", "stop_sequence", "tool_use"
//   - Google (future): "STOP", "MAX_TOKENS", "SAFETY", "RECITATION"
//
// Both case "stop_sequence" (user-defined stop hit) and "end_turn"
// (model decided it was done) collapse to "stop" — same outcome from
// the caller's POV (clean completion, full response). If we ever want
// to score "asked us to stop early" differently, split here.
func normalizeFinishReason(r string) string {
	switch r {
	// OpenAI passthrough
	case "stop", "length", "content_filter":
		return r
	case "tool_calls", "function_call":
		return "tool_calls"

	// Anthropic
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"

	default:
		return r
	}
}
