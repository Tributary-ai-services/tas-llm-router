package clear

// scoreReliability is the MVP — gateway-fulfillment-proxy —
// implementation of source-spec §2.2.4's Reliability dimension. The
// spec's canonical Reliability metric is pass@k (probability of k
// consecutive identical-input successes), which requires cross-request
// session state the gateway doesn't yet track. Without that, we use
// the routing layer's per-request retry/fallback signals as a
// gateway-side proxy for "how reliably did the gateway fulfill this
// request":
//
//	attempt_count | fallback_used | score | tier
//	1             | false         | 100   | Healthy   (clean first try)
//	1             | true          |  75   | Marginal  (shouldn't happen but defensive)
//	2             | false         |  75   | Marginal  (one in-provider retry)
//	2             | true          |  50   | Failing   (had to switch providers)
//	3             | false         |  50   | Failing   (multiple in-provider retries)
//	3             | true          |  25   | Failing
//	≥4            | any           |   0   | Failing   (chronic instability)
//
// Honest framing for dashboards: this measures the gateway's ability
// to deliver a vendor response on first attempt, NOT the spec's
// "model behavior consistency across runs". Once the retry-detection
// slice lands (60s session windows, similarity matching), this scorer
// will absorb those signals too.
//
// Returns nil when:
//   - HTTPStatus is 0 (gateway-blocked before forwarding — no attempt
//     was made, so the dimension has no signal)
//   - AttemptCount is unset (zero — handler never stamped, the routing
//     layer didn't surface metadata)
func scoreReliability(in Input) *Score {
	if in.HTTPStatus == 0 {
		return nil
	}
	if in.AttemptCount <= 0 {
		return nil
	}

	var base Score
	switch in.AttemptCount {
	case 1:
		base = 100
	case 2:
		base = 75
	case 3:
		base = 50
	default:
		base = 0
	}
	// Fallback costs an extra tier on top of attempt-count scoring.
	if in.FallbackUsed && base > 0 {
		base -= 25
		if base < 0 {
			base = 0
		}
	}
	return &base
}
