package clear

import "testing"

func TestScoreAssurance_NilWhenScanDidNotRun(t *testing.T) {
	s := scoreAssurance(Input{HTTPStatus: 200, AssuranceScanRan: false})
	if s != nil {
		t.Errorf("expected nil when AssuranceScanRan=false, got %d", *s)
	}
}

func TestScoreAssurance_HealthyOnCleanScan(t *testing.T) {
	s := scoreAssurance(Input{
		HTTPStatus:       200,
		AssuranceScanRan: true,
	})
	if s == nil || *s != 100 {
		t.Errorf("score=%v want=100 (clean scan)", s)
	}
}

func TestScoreAssurance_Bucketing(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]int
		out  map[string]int
		want Score
	}{
		{"low_only", map[string]int{SeverityLow: 5}, nil, 95},
		{"medium_inbound", map[string]int{SeverityMedium: 1}, nil, 80},
		{"medium_outbound", nil, map[string]int{SeverityMedium: 2}, 80},
		{"high_inbound_low_outbound_picks_high", map[string]int{SeverityHigh: 1}, map[string]int{SeverityLow: 5}, 50},
		{"critical_inbound", map[string]int{SeverityCritical: 1}, nil, 0},
		{"critical_outbound_only", nil, map[string]int{SeverityCritical: 1}, 0},
		{"low_plus_medium_picks_medium", map[string]int{SeverityLow: 3, SeverityMedium: 1}, nil, 80},
		{"high_plus_critical_picks_critical", map[string]int{SeverityHigh: 1, SeverityCritical: 1}, nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scoreAssurance(Input{
				HTTPStatus:                 200,
				AssuranceScanRan:           true,
				InboundFindingsBySeverity:  c.in,
				OutboundFindingsBySeverity: c.out,
			})
			if s == nil {
				t.Fatalf("score nil")
			}
			if *s != c.want {
				t.Errorf("score=%d want=%d", *s, c.want)
			}
		})
	}
}

// Per source-spec §2.2.3, Assurance uses stricter tier boundaries:
// Healthy ≥90, Marginal 75-89, Failing <75. Verify the bucketed
// scores land in the documented tiers.
func TestScoreAssurance_TierBoundaries(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]int
		tier string // "healthy" | "marginal" | "failing"
	}{
		{"clean → healthy", nil, "healthy"},                          // 100
		{"low → healthy", map[string]int{SeverityLow: 1}, "healthy"}, // 95
		{"medium → marginal", map[string]int{SeverityMedium: 1}, "marginal"}, // 80
		{"high → failing", map[string]int{SeverityHigh: 1}, "failing"},       // 50
		{"critical → failing", map[string]int{SeverityCritical: 1}, "failing"}, // 0
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scoreAssurance(Input{HTTPStatus: 200, AssuranceScanRan: true, InboundFindingsBySeverity: c.in})
			if s == nil {
				t.Fatalf("nil")
			}
			actual := tierName(*s)
			if actual != c.tier {
				t.Errorf("score=%d → %s, want %s", *s, actual, c.tier)
			}
		})
	}
}

func tierName(s Score) string {
	switch {
	case s >= 90:
		return "healthy"
	case s >= 75:
		return "marginal"
	default:
		return "failing"
	}
}

func TestCompute_PopulatesAssurance(t *testing.T) {
	in := Input{
		HTTPStatus:                200,
		AssuranceScanRan:          true,
		InboundFindingsBySeverity: map[string]int{SeverityMedium: 2},
	}
	s := Compute(in)
	if s.Assurance == nil || *s.Assurance != 80 {
		t.Errorf("Assurance=%v want=80", s.Assurance)
	}
	if s.Composite == nil {
		t.Fatalf("Composite nil")
	}
}
