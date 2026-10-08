package routing

import "strings"

// Health-failure reasons, a CLOSED set.
//
// Closed deliberately: these become a Prometheus label, and vendor error text
// is unbounded -- "Your credit balance is too low", a request id, a workspace
// id. Using it raw would be a cardinality bomb in the same metric an operator
// reaches for during an outage.
const (
	ReasonCreditExhausted   = "credit_exhausted"
	ReasonAuth              = "auth"
	ReasonRateLimited       = "rate_limited"
	ReasonVendorUnavailable = "vendor_unavailable"
	ReasonNetwork           = "network"
	ReasonOther             = "other"
	ReasonHealthy           = ""
)

// classifyHealthFailure turns a provider health-check error into one of the
// reasons above.
//
// OPS-55: callers were told `provider anthropic is not healthy`, which reads as
// "Anthropic is down" when the truth was "our prepaid account is out of money"
// -- and the upstream body that said so exactly ("Your credit balance is too
// low to access the Anthropic API") was captured on HealthStatus.ErrorMessage
// and then used nowhere. Ten minutes of the 2026-10-05 incident were spent
// looking in the wrong place because of it.
//
// Substring matching on vendor prose is admittedly brittle, and it is a
// deliberate trade: the alternative is no classification at all, since the
// health-check path has only an error value to work from. A reason that
// degrades to "other" is strictly better than a message that misdirects, and
// ReasonOther is counted rather than hidden so a growing "other" share is the
// signal to add a case.
func classifyHealthFailure(err error) string {
	if err == nil {
		return ReasonHealthy
	}
	s := strings.ToLower(err.Error())
	switch {
	// Checked before the generic auth case: a spend limit and an exhausted
	// balance both arrive as a 400, not a 401, and both mean "pay us".
	case strings.Contains(s, "credit balance"),
		strings.Contains(s, "spend limit"),
		strings.Contains(s, "quota"),
		strings.Contains(s, "insufficient_quota"),
		strings.Contains(s, "billing"):
		return ReasonCreditExhausted
	case strings.Contains(s, "429"), strings.Contains(s, "rate limit"), strings.Contains(s, "overloaded"):
		return ReasonRateLimited
	case strings.Contains(s, "401"), strings.Contains(s, "403"),
		strings.Contains(s, "unauthorized"), strings.Contains(s, "authentication"),
		strings.Contains(s, "invalid x-api-key"), strings.Contains(s, "invalid api key"):
		return ReasonAuth
	case strings.Contains(s, "500"), strings.Contains(s, "502"), strings.Contains(s, "503"),
		strings.Contains(s, "529"), strings.Contains(s, "internal server error"):
		return ReasonVendorUnavailable
	case strings.Contains(s, "dial"), strings.Contains(s, "timeout"),
		strings.Contains(s, "connection refused"), strings.Contains(s, "no such host"),
		strings.Contains(s, "eof"), strings.Contains(s, "context deadline"):
		return ReasonNetwork
	default:
		return ReasonOther
	}
}

// humanHealthReason is what a CALLER is told. The classified reason only --
// never the vendor's raw text, which carries request ids, workspace ids and our
// account's billing state, and this message can reach the public edge.
func humanHealthReason(reason string) string {
	switch reason {
	case ReasonCreditExhausted:
		return "the gateway's upstream account has no credit or has hit a spend limit"
	case ReasonAuth:
		return "the gateway's upstream credentials were rejected"
	case ReasonRateLimited:
		return "the upstream provider is rate-limiting or overloaded"
	case ReasonVendorUnavailable:
		return "the upstream provider returned a server error"
	case ReasonNetwork:
		return "the upstream provider is unreachable from the gateway"
	default:
		return "the last upstream health check failed"
	}
}
