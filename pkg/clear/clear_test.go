package clear

import (
	"encoding/json"
	"testing"
)

func ptr64(v int64) *int64 { return &v }

func TestVersionPresent(t *testing.T) {
	if Version == "" {
		t.Fatalf("Version must not be empty — re-scoring jobs filter on it")
	}
}

func TestSLAMap(t *testing.T) {
	cases := []struct {
		workflow string
		want     int64
	}{
		{"single_turn_qa", 3000},
		{"classification_extraction", 5000},
		{"rag", 5000},
		{"summarization", 15000},
		{"code_generation", 30000},
		{"agentic", 30000},
		// Unknown workflow → 3s (was 10s). Tightened 2026-06-06 so
		// unclassified small-context chat requests produce visible
		// latency signal instead of clamping to 100.
		{"", 3000},
		{"unknown", 3000},
		{"misspelled_workflow_name", 3000},
	}
	for _, c := range cases {
		if got := slaMsForWorkflow(c.workflow); got != c.want {
			t.Errorf("slaMsForWorkflow(%q)=%d want=%d", c.workflow, got, c.want)
		}
	}
}

func TestLatencyCurve(t *testing.T) {
	// Use rag workflow (5s SLA = 5000ms target) for predictable math.
	const target int64 = 5000
	cases := []struct {
		name      string
		actualMs  int64
		wantScore Score
	}{
		{"at_target", target, 100},        // 100 - 50*(1-1) = 100
		{"under_target", 2500, 100},       // clamped to 100
		{"1.5x_target", 7500, 75},         // 100 - 50*(1.5-1) = 75 (boundary Healthy/Marginal)
		{"2x_target", 10000, 50},          // 100 - 50*(2-1) = 50 (boundary Marginal/Failing)
		{"2.5x_target", 12500, 25},        // 100 - 50*(2.5-1) = 25
		{"3x_target", 15000, 0},           // 100 - 50*(3-1) = 0 (just at the floor)
		{"way_over_target", 60000, 0},     // clamped to 0
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scoreLatency(Input{
				EndToEndMs: ptr64(c.actualMs),
				Workflow:   "rag",
				HTTPStatus: 200,
			})
			if s == nil {
				t.Fatalf("scoreLatency returned nil unexpectedly")
			}
			if *s != c.wantScore {
				t.Fatalf("scoreLatency(actual=%d)=%d want=%d", c.actualMs, *s, c.wantScore)
			}
		})
	}
}

func TestLatencyNilCases(t *testing.T) {
	if scoreLatency(Input{EndToEndMs: nil, Workflow: "rag", HTTPStatus: 200}) != nil {
		t.Errorf("scoreLatency should return nil when EndToEndMs is nil")
	}
	if scoreLatency(Input{EndToEndMs: ptr64(1000), Workflow: "rag", HTTPStatus: 0}) != nil {
		t.Errorf("scoreLatency should return nil when HTTPStatus=0 (gateway-blocked)")
	}
}

func TestCompositeAllDimensionsNil(t *testing.T) {
	s := &Scores{} // every dimension nil
	if got := composite(s); got != nil {
		t.Fatalf("composite of all-nil = %d, want nil", *got)
	}
}

func TestCompositeOneDimension(t *testing.T) {
	// Only Latency populated — composite should equal it exactly.
	latencyScore := Score(80)
	s := &Scores{Latency: &latencyScore}
	c := composite(s)
	if c == nil {
		t.Fatalf("composite nil with one dimension")
	}
	if *c != 80 {
		t.Errorf("composite=%d want=80", *c)
	}
}

func TestCompositeMultipleDimensions(t *testing.T) {
	cost := Score(50)
	latency := Score(80)
	efficacy := Score(70)
	s := &Scores{Cost: &cost, Latency: &latency, Efficacy: &efficacy}
	c := composite(s)
	if c == nil {
		t.Fatalf("composite nil")
	}
	// (50+80+70)/3 = 66 (int truncation)
	if *c != 66 {
		t.Errorf("composite=%d want=66", *c)
	}
}

// Compute end-to-end: only Latency input plumbed today, so Latency +
// Composite (= Latency) should be populated, others nil.
func TestComputeMVPHappyPath(t *testing.T) {
	s := Compute(Input{
		EndToEndMs: ptr64(2500), // under 5s SLA for rag
		Workflow:   "rag",
		HTTPStatus: 200,
	})
	if s == nil {
		t.Fatalf("Compute returned nil")
	}
	if s.Latency == nil || *s.Latency != 100 {
		t.Errorf("Latency=%v want=100", s.Latency)
	}
	if s.Cost != nil {
		t.Errorf("Cost should be nil when no vendor/model/tokens supplied, got %d", *s.Cost)
	}
	if s.Efficacy != nil {
		t.Errorf("Efficacy should be nil in MVP")
	}
	if s.Assurance != nil {
		t.Errorf("Assurance should be nil in MVP")
	}
	if s.Reliability != nil {
		t.Errorf("Reliability should be nil in MVP")
	}
	if s.Composite == nil || *s.Composite != 100 {
		t.Errorf("Composite=%v want=100", s.Composite)
	}
	if s.WeightsApplied == "" {
		t.Errorf("WeightsApplied should be set when Composite is populated")
	}
}

// Compute on a gateway-blocked request (HTTPStatus=0): Latency must be
// nil, composite consequently nil, WeightsApplied empty.
func TestComputeGatewayBlocked(t *testing.T) {
	s := Compute(Input{EndToEndMs: ptr64(50), HTTPStatus: 0, Workflow: "rag"})
	if s == nil {
		t.Fatalf("Compute returned nil")
	}
	if s.Latency != nil {
		t.Errorf("Latency should be nil on HTTPStatus=0")
	}
	if s.Composite != nil {
		t.Errorf("Composite should be nil when all dimensions are nil")
	}
	if s.WeightsApplied != "" {
		t.Errorf("WeightsApplied should be empty when Composite is nil")
	}
}

// JSON marshal: all-nil dimensions must be OMITTED via omitempty so
// the response event payload stays small and consumers don't see
// false "scored as 0" rows.
func TestScoresMarshalsOmitsNilDimensions(t *testing.T) {
	latencyScore := Score(75)
	s := &Scores{Latency: &latencyScore, Composite: &latencyScore, WeightsApplied: "equal-weight-non-nil"}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["latency"]; !ok {
		t.Errorf("latency missing from JSON")
	}
	if _, ok := m["composite"]; !ok {
		t.Errorf("composite missing from JSON")
	}
	for _, k := range []string{"cost", "efficacy", "assurance", "reliability"} {
		if _, ok := m[k]; ok {
			t.Errorf("%q should be omitted (nil pointer + omitempty)", k)
		}
	}
}

// Workflow-specific SLA matters: same actual latency scores differently
// per workflow because the SLA target differs.
func TestComputeWorkflowSensitivity(t *testing.T) {
	const actual int64 = 5000 // 5s
	// code_generation: SLA=30s → 5s is well under → 100
	cg := Compute(Input{EndToEndMs: ptr64(actual), Workflow: "code_generation", HTTPStatus: 200})
	if cg.Latency == nil || *cg.Latency != 100 {
		t.Errorf("code_generation@5s: latency=%v want=100", cg.Latency)
	}
	// single_turn_qa: SLA=3s → 5s is 1.67×target → 100-50*(1.67-1) = 67
	stq := Compute(Input{EndToEndMs: ptr64(actual), Workflow: "single_turn_qa", HTTPStatus: 200})
	if stq.Latency == nil || *stq.Latency < 60 || *stq.Latency > 70 {
		t.Errorf("single_turn_qa@5s: latency=%v want~67", stq.Latency)
	}
}

// Score is a type alias for int16 — verify the size is what callers
// (Postgres smallint storage) expect.
func TestScoreTypeAlias(t *testing.T) {
	var s Score = 100
	if _, ok := any(s).(int16); !ok {
		t.Fatalf("Score is not assignable to int16 — alias broken")
	}
}
