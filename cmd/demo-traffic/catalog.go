// Matcher catalog for the AIQG demo-traffic generator.
//
// Each entry mirrors a Gatekeeper pattern_id that the production
// pipeline can fire on a request. The generator injects these as
// cross-cutting attributes (compliance / vague / hedging / bloat /
// refusal) so the dashboard's tag, NIST, Assurance, and avoidable-cost
// panels light up with a coherent story.
//
// Coherence rule: when a matcher is attached to an event we
//  1. emit a `tag_<sanitized>` count (drives /metrics/tags + the
//     avoidable-cost categories in handlers/avoidable_cost.go),
//  2. add its NIST characteristic to nist_<characteristic> (drives the
//     Day-1 Trustworthiness query),
//  3. record a finding at its severity in the Assurance severity maps
//     so clear.Compute scores the Assurance dimension consistently
//     (assurance.go: critical=0, high=50, medium=80, low=95).
//
// The pattern_ids and the hyphen→underscore sanitization match
// pkg/aiqg/events/emitter.go and aiqg-dashboard-be's metrics.go /
// avoidable_cost.go exactly. Keep this list in sync when Gatekeeper
// matchers change.
package main

// matcher is one Gatekeeper pattern with the metadata the generator
// needs to emit a coherent finding.
type matcher struct {
	PatternID string // e.g. "aiqg-bloated-context"; hyphens, as Gatekeeper emits
	Severity  string // "low" | "medium" | "high" | "critical"
	NIST      string // NIST AI RMF characteristic: valid_reliable | privacy_enhanced | secure_resilient | safe
}

// Quality matchers — the four avoidable-cost categories in
// aiqg-dashboard-be/internal/handlers/avoidable_cost.go map onto these
// pattern_ids. Severities are low/medium because a quality defect is a
// cost/utility problem, not a safety breach.
var (
	mBloatedContext     = matcher{"aiqg-bloated-context", severityMedium, nistValidReliable}
	mInstructionStuffing = matcher{"aiqg-instruction-stuffing", severityLow, nistValidReliable}
	mHallucinationHedge = matcher{"aiqg-hallucination-hedge", severityLow, nistValidReliable}
	mRepetition         = matcher{"aiqg-repetition", severityLow, nistValidReliable}
	mVaguePrompt        = matcher{"aiqg-vague-prompt", severityLow, nistValidReliable}
	mUnboundedLoop      = matcher{"aiqg-unbounded-loop", severityMedium, nistValidReliable}
	mRefusal            = matcher{"aiqg-refusal", severityMedium, nistValidReliable}
	mMalformedOutput    = matcher{"aiqg-malformed-output", severityLow, nistValidReliable}
	mRoleClaim          = matcher{"aiqg-role-claim", severityLow, nistValidReliable}
)

// Compliance matchers — PII, credentials, injection, and safety
// patterns. These carry real risk so they score high/critical and tank
// the Assurance dimension, which is the only way to exercise it.
var (
	mPIISSN        = matcher{"pii-ssn", severityCritical, nistPrivacyEnhanced}
	mPIICreditCard = matcher{"pii-credit-card", severityHigh, nistPrivacyEnhanced}
	mPIIEmail      = matcher{"pii-email", severityMedium, nistPrivacyEnhanced}
	mPIIPhone      = matcher{"pii-phone", severityMedium, nistPrivacyEnhanced}

	mCredAPIKey       = matcher{"cred-api-key", severityCritical, nistSecureResilient}
	mCredAWSSecretKey = matcher{"cred-aws-secret-key", severityCritical, nistSecureResilient}

	mInjectionPrompt = matcher{"injection-prompt", severityHigh, nistSecureResilient}
	mInjectionSQL    = matcher{"injection-sql", severityHigh, nistSecureResilient}

	mHarmRequest    = matcher{"aiqg-harm-request", severityCritical, nistSafe}
	mExplicitJail   = matcher{"aiqg-explicit-jailbreak", severityCritical, nistSafe}
	mCredSolicit    = matcher{"aiqg-credential-solicitation", severityHigh, nistSafe}
)

const (
	severityLow      = "low"
	severityMedium   = "medium"
	severityHigh     = "high"
	severityCritical = "critical"

	nistValidReliable   = "valid_reliable"
	nistPrivacyEnhanced = "privacy_enhanced"
	nistSecureResilient = "secure_resilient"
	nistSafe            = "safe"
)

// hedgingMatchers, vagueMatchers, complianceMatchers are the pools the
// cross-cutting injectors draw from. Hedging and vague map to the
// like-named avoidable-cost categories; compliance spans PII /
// credentials / injection / safety.
var (
	hedgingMatchers    = []matcher{mHallucinationHedge, mRepetition}
	vagueMatchers      = []matcher{mVaguePrompt, mUnboundedLoop}
	complianceMatchers = []matcher{
		mPIISSN, mPIICreditCard, mPIIEmail, mPIIPhone,
		mCredAPIKey, mCredAWSSecretKey,
		mInjectionPrompt, mInjectionSQL,
		mHarmRequest, mExplicitJail, mCredSolicit,
	}
)

// severityRank lets us merge findings and keep the worst — matches the
// ordering in assurance.go's worstSeverity.
func severityRank(s string) int {
	switch s {
	case severityCritical:
		return 4
	case severityHigh:
		return 3
	case severityMedium:
		return 2
	case severityLow:
		return 1
	default:
		return 0
	}
}
