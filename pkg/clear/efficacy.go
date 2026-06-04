package clear

// scoreEfficacy converts the vendor's finish_reason into a 0-100
// Efficacy score. This is the MVP — finish-reason-only — implementation
// of source-spec §2.2.2's Efficacy dimension. Full structural-validity
// + implicit-acceptance + groundedness sub-metrics need response-body
// capture (deferred slice).
//
// Mapping:
//
//	finish_reason   | score | rationale
//	"stop"          | 100   | clean completion — vendor signaled the model finished naturally
//	"tool_calls"    | 100   | function-calling success path
//	"function_call" | 100   | legacy OpenAI function-call path
//	"length"        |  60   | truncated at max_tokens — output is partial; usable but degraded
//	"content_filter"|   0   | vendor blocked output — content policy violation
//	""              |  nil  | finish_reason not captured (streaming truncation, gateway block, vendor outage)
//
// 60 for "length" sits in the Marginal band (50-74 per spec §2.2 default)
// — partial output isn't a hard failure but it's not Healthy either.
// 0 for "content_filter" mirrors how Assurance handles critical
// findings: hard policy outcomes shouldn't be diluted.
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
	switch in.FinishReason {
	case "stop", "tool_calls", "function_call":
		v = 100
	case "length":
		v = 60
	case "content_filter":
		v = 0
	default:
		// Unknown finish_reason — emerging values like "max_tokens" or
		// "end_turn" we haven't seen yet. Return nil rather than guess
		// so dashboards can flag the request and prompt a code update.
		return nil
	}
	return &v
}
