package events

import (
	"encoding/json"
	"strings"
	"testing"
)

// The property that makes this audit trail safe to keep: an entry proving an
// SSN would have been redacted must never become the place that SSN is stored.
func TestFindingNeverCarriesTheMatchedValue(t *testing.T) {
	f := Finding{
		PatternID: "pii-ssn", Severity: "critical", Direction: "inbound",
		FieldPath: "messages[2].content", Offset: 412, Length: 11,
		ValuePreview: "***-**-6789", ValueHash: "sha256:abc123",
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	// There is no field that could hold it — asserted structurally rather than
	// by inspecting a value, so adding one later fails this test.
	for _, forbidden := range []string{`"value"`, `"raw"`, `"matched"`, `"text"`} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("finding serialises a raw-value field %s: %s", forbidden, b)
		}
	}
	if !strings.Contains(string(b), "***-**-6789") {
		t.Fatal("the masked preview should survive; it is what makes the entry legible")
	}
}

// Location is the difference between a count and evidence: it lets someone find
// the text without being shown it.
func TestFindingCarriesLocation(t *testing.T) {
	b, _ := json.Marshal(Finding{PatternID: "pii-ssn", FieldPath: "messages[2].content", Offset: 412, Length: 11})
	for _, want := range []string{"messages[2].content", "412", "11"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("location detail %q missing from %s", want, b)
		}
	}
}

// Direction matters: an SSN we sent is a disclosure risk, an SSN returned may
// be a leak from the model. They are different incidents.
func TestFindingDistinguishesDirection(t *testing.T) {
	in, _ := json.Marshal(Finding{PatternID: "pii-ssn", Direction: "inbound"})
	out, _ := json.Marshal(Finding{PatternID: "pii-ssn", Direction: "outbound"})
	if string(in) == string(out) {
		t.Fatal("inbound and outbound findings serialise identically")
	}
}

// Empty findings must not bloat every event with null fields.
func TestEmptyFindingIsCompact(t *testing.T) {
	b, _ := json.Marshal(Finding{PatternID: "x", Severity: "low", Direction: "inbound"})
	if strings.Contains(string(b), "offset") || strings.Contains(string(b), "frameworks") {
		t.Fatalf("unset optional fields are being serialised: %s", b)
	}
}

func TestCapIsBounded(t *testing.T) {
	// A pathological request can produce hundreds of findings; carrying them
	// all would let one caller inflate every downstream store.
	if MaxFindingsPerEvent <= 0 || MaxFindingsPerEvent > 200 {
		t.Fatalf("MaxFindingsPerEvent = %d, want a sane bound", MaxFindingsPerEvent)
	}
}
