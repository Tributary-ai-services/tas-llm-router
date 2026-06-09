package middleware

// nist_classifier.go — maps Gatekeeper finding pattern IDs onto the
// four NIST AI RMF (AI Risk Management Framework) characteristics
// the AIQG dashboard reports under "Trustworthiness":
//
//   - Secure & Resilient   — resists adversarial input (injection, jailbreak)
//   - Privacy-Enhanced     — protects sensitive personal data
//   - Valid & Reliable     — produces accurate, consistent outputs
//   - Safe                 — refuses unsafe / non-compliant requests
//
// The mapping lives here (tas-llm-router) rather than inside
// Gatekeeper because it's AIQG-specific and the rest of TAS doesn't
// need to know about it. Other Gatekeeper consumers see only raw
// findings; AIQG enriches them with NIST classification before the
// event is emitted.
//
// Categorization scheme: a single PatternID maps to exactly one
// characteristic. When a future matcher straddles two characteristics
// (e.g. a "leaked PII via injection" pattern), pick the more
// actionable bucket — Privacy-Enhanced for PII exposure, since the
// remediation is data protection.

// NIST AI RMF characteristic constants. Use these strings as map
// keys throughout the pipeline so the dashboard stays in sync.
const (
	NISTSecureResilient  = "secure_resilient"
	NISTPrivacyEnhanced  = "privacy_enhanced"
	NISTValidReliable    = "valid_reliable"
	NISTSafe             = "safe"

	// nistOther is a fallback bucket for findings whose pattern ID
	// isn't in the table (e.g. a new matcher landed before the
	// mapping was extended). Surfaces in logs so the gap is visible
	// but never inflates the headline characteristic counts.
	nistOther = "other"
)

// nistByPatternID maps every known Gatekeeper matcher ID to its
// NIST AI RMF characteristic. Keep alphabetized by ID for easy
// audit. When a new matcher lands in Gatekeeper, add a row here
// in the same PR so findings never silently fall into nistOther.
var nistByPatternID = map[string]string{
	// AIQG quality matchers (Gatekeeper#8 + #9). Bloated context and
	// refusal both surface as Valid & Reliable failures — degraded
	// input quality drives degraded output. Role-claim is a soft
	// prompt injection → Secure & Resilient.
	"aiqg-bloated-context":     NISTValidReliable,
	"aiqg-refusal":             NISTValidReliable,
	"aiqg-role-claim":          NISTSecureResilient,
	// Outbound quality matchers (Gatekeeper#9). All three signal
	// degraded model output → Valid & Reliable.
	"aiqg-repetition":          NISTValidReliable,
	"aiqg-hallucination-hedge": NISTValidReliable,
	"aiqg-malformed-output":    NISTValidReliable,
	// Citation marker (Gatekeeper#12, Phase 2.4). Presence of citations
	// in a response is a positive signal for groundedness — a request
	// that grounded its output in retrieved context is more reliable
	// than one that didn't. Lives in Valid & Reliable for two reasons:
	// (a) citation absence on a RAG response is the meaningful signal,
	// and absence is an unreliability proxy; (b) keeps the Trustworthiness
	// panel on the report scoring this as quality rather than safety.
	"aiqg-citation-marker": NISTValidReliable,

	// Safety / policy matchers (Gatekeeper#10). Populate the Safe
	// characteristic — until this PR landed it was always 0 on the
	// Day-1 Report Trustworthiness panel.
	"aiqg-harm-request":            NISTSafe,
	"aiqg-credential-solicitation": NISTSafe,
	"aiqg-explicit-jailbreak":      NISTSafe,

	// Inbound input-quality antipatterns (Gatekeeper#11). Vague /
	// overloaded / unbounded prompts produce unreliable outputs
	// regardless of intent → Valid & Reliable.
	"aiqg-vague-prompt":         NISTValidReliable,
	"aiqg-instruction-stuffing": NISTValidReliable,
	"aiqg-unbounded-loop":       NISTValidReliable,

	// Credential exposure — leaked secrets are a privacy issue
	// (someone's secret in an LLM transcript = data leak).
	"cred-api-key":          NISTPrivacyEnhanced,
	"cred-aws-access-key":   NISTPrivacyEnhanced,
	"cred-aws-secret-key":   NISTPrivacyEnhanced,
	"cred-azure-key":        NISTPrivacyEnhanced,
	"cred-connection-string": NISTPrivacyEnhanced,
	"cred-gcp-key":          NISTPrivacyEnhanced,
	"cred-jwt-token":        NISTPrivacyEnhanced,
	"cred-oauth-token":      NISTPrivacyEnhanced,
	"cred-private-key":      NISTPrivacyEnhanced,

	// Injection — by definition adversarial input.
	"injection-prompt": NISTSecureResilient,
	"injection-sql":    NISTSecureResilient,
	"injection-xss":    NISTSecureResilient,

	// PII — every type is a privacy finding.
	"pii-address":          NISTPrivacyEnhanced,
	"pii-bank-account":     NISTPrivacyEnhanced,
	"pii-credit-card":      NISTPrivacyEnhanced,
	"pii-dob":              NISTPrivacyEnhanced,
	"pii-drivers-license":  NISTPrivacyEnhanced,
	"pii-email":            NISTPrivacyEnhanced,
	"pii-ip-address":       NISTPrivacyEnhanced,
	"pii-mrn":              NISTPrivacyEnhanced,
	"pii-name":             NISTPrivacyEnhanced,
	"pii-passport":         NISTPrivacyEnhanced,
	"pii-phone":            NISTPrivacyEnhanced,
	"pii-ssn":              NISTPrivacyEnhanced,
}

// MapPatternToNIST returns the NIST characteristic for a Gatekeeper
// pattern ID. Unknown IDs return nistOther — caller should log so
// the mapping can be extended.
func MapPatternToNIST(patternID string) string {
	if c, ok := nistByPatternID[patternID]; ok {
		return c
	}
	return nistOther
}

// AllNISTCharacteristics returns the four canonical characteristic
// strings in display order. Used by the dashboard handler and tests
// so the Trustworthiness panel's section order is stable.
func AllNISTCharacteristics() []string {
	return []string{
		NISTSecureResilient,
		NISTPrivacyEnhanced,
		NISTValidReliable,
		NISTSafe,
	}
}
