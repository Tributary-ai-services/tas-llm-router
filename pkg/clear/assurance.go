package clear

// Severity strings the Assurance scorer recognizes. Match the four
// values Gatekeeper emits (pkg/scan.Severity in the Gatekeeper repo)
// without importing Gatekeeper types here — keeps pkg/clear standalone.
const (
	SeverityLow      = "low"
	SeverityMedium   = "medium"
	SeverityHigh     = "high"
	SeverityCritical = "critical"
)

// scoreAssurance buckets the request's worst Gatekeeper finding into
// a 0-100 score per source-spec §2.2.3, which carries stricter tier
// boundaries than the other dimensions because assurance failures have
// asymmetric consequences (one unauthorized disclosure invalidates
// otherwise perfect performance):
//
//	Healthy   ≥ 90  → no findings, or low-only
//	Marginal  75-89 → any medium
//	Failing   < 75  → any high or critical
//
// Concrete mapping:
//
//	no findings   → 100
//	only "low"    → 95
//	any "medium"  → 80
//	any "high"    → 50
//	any "critical"→ 0
//
// Bucketing on worst-severity (not count) matches the spec's
// asymmetric-consequence framing — one critical finding shouldn't be
// diluted by 99 clean checks. Counts still ride on the AssuranceSummary
// for dashboards that want to slice by frequency.
//
// Returns nil when no scan ran at all (FindingsSet=false on the
// routing snapshot). Distinct from "scan ran, found nothing" which
// is the explicit Healthy=100 case.
func scoreAssurance(in Input) *Score {
	if !in.AssuranceScanRan {
		return nil
	}
	worst := worstSeverity(in.InboundFindingsBySeverity, in.OutboundFindingsBySeverity)
	var v Score
	switch worst {
	case SeverityCritical:
		v = 0
	case SeverityHigh:
		v = 50
	case SeverityMedium:
		v = 80
	case SeverityLow:
		v = 95
	default:
		// No findings at any severity → Healthy.
		v = 100
	}
	return &v
}

// worstSeverity returns the highest severity present across either
// direction. Empty string means no findings.
func worstSeverity(in, out map[string]int) string {
	// Order matters — check critical first, then high, etc.
	for _, sev := range []string{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow} {
		if in[sev] > 0 || out[sev] > 0 {
			return sev
		}
	}
	return ""
}
