package events

// Per-finding audit detail — routing-decision.md step 8.
//
// # Why counts were not enough
//
// Until now the gateway collapsed every scan result to per-severity and
// per-pattern COUNTS. That answers "how many" and nothing else. The moment
// enforcement can block a request, "why was this blocked?" stops being a
// curiosity and becomes a support question — and "a pii-ssn rule fired
// somewhere in your prompt" does not survive contact with a customer.
//
// Gatekeeper already produced all of this per finding; the gateway threw it
// away at the event boundary. So this is plumbing, not new detection.
//
// # Never the matched value
//
// Gatekeeper's own type carries the comment "never log actual PII", and it
// provides ValuePreview ("j***@***.com") and ValueHash for exactly this reason.
// An audit trail proving you would have redacted an SSN must not become the
// place the SSN is stored — that would be the leak it documents.
//
// So: preview and hash travel, the value never does. The hash still allows the
// useful correlation — the same secret appearing across many requests — without
// holding the secret.

// Finding is one detected pattern, with enough detail to explain an
// enforcement decision.
type Finding struct {
	PatternID string `json:"pattern_id"`
	Severity  string `json:"severity"`
	// Direction is inbound (the prompt) or outbound (the response). The same
	// pattern means different things in each: an SSN we sent is a disclosure
	// risk, an SSN we received back may be a leak from the model.
	Direction string `json:"direction"`

	// FieldPath and Offset locate the match — "messages[2].content" at
	// characters 412-423. This is the difference between a count and evidence:
	// it lets someone find the text without being shown it.
	FieldPath string `json:"field_path,omitempty"`
	Offset    int    `json:"offset,omitempty"`
	Length    int    `json:"length,omitempty"`

	// ValuePreview is a masked rendering, never the value itself.
	ValuePreview string `json:"value_preview,omitempty"`
	// ValueHash correlates the same secret across requests without storing it.
	ValueHash string `json:"value_hash,omitempty"`

	Confidence float64 `json:"confidence,omitempty"`
	// Redacted records whether the gateway actually acted, so an audit entry
	// distinguishes "detected" from "detected and handled".
	Redacted bool `json:"redacted,omitempty"`
	// Frameworks names the regimes this violates, so a compliance question is
	// answerable from the event rather than by re-deriving the mapping.
	Frameworks []string `json:"frameworks,omitempty"`
}

// MaxFindingsPerEvent caps how many findings ride on one event.
//
// A pathological request can produce hundreds; carrying them all would let one
// caller inflate every downstream store. The counts remain complete and
// authoritative — only the detail is capped — and FindingsTruncated says how
// many were dropped, because a truncated list that does not admit it reads as
// the complete set.
const MaxFindingsPerEvent = 50
