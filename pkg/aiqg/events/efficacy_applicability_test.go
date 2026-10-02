package events

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/instrumentation"
)

func buildWithApplicability(t *testing.T, rv RoutingView) ResponseEnvelope {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	_, respEnv := Build(r, AIQGHeadersView{}, rv, TokenView{},
		instrumentation.Snapshot{}, BuildOptions{HTTPStatus: 200})
	return respEnv
}

// An omitted field means "never observed"; a present false means "observed, and
// the caller asked for nothing". Collapsing those would corrupt the
// applicability rate these flags exist to measure (§6.3, nil is a value).
func TestBuild_ApplicabilityOmittedWhenNeverStamped(t *testing.T) {
	resp := buildWithApplicability(t, RoutingView{})

	if resp.Data.SchemaRequested != nil {
		t.Errorf("SchemaRequested = %v, want nil when the sidecar was never stamped", *resp.Data.SchemaRequested)
	}
	if resp.Data.ToolsDeclared != nil {
		t.Errorf("ToolsDeclared = %v, want nil when the sidecar was never stamped", *resp.Data.ToolsDeclared)
	}

	// And the keys must be absent from the wire form, not present as null —
	// the Spark job and the LogQL aggregation both read presence.
	b, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["schema_requested"]; ok {
		t.Errorf("schema_requested present in JSON when unobserved; want omitted")
	}
	if _, ok := m["tools_declared"]; ok {
		t.Errorf("tools_declared present in JSON when unobserved; want omitted")
	}
}

func TestBuild_ApplicabilityStampedFalseIsEmitted(t *testing.T) {
	resp := buildWithApplicability(t, RoutingView{ApplicabilitySet: true})

	if resp.Data.SchemaRequested == nil || *resp.Data.SchemaRequested {
		t.Errorf("SchemaRequested = %v, want a non-nil false", resp.Data.SchemaRequested)
	}
	if resp.Data.ToolsDeclared == nil || *resp.Data.ToolsDeclared {
		t.Errorf("ToolsDeclared = %v, want a non-nil false", resp.Data.ToolsDeclared)
	}

	b, _ := json.Marshal(resp.Data)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// omitempty does not drop a non-nil *bool pointing at false.
	if v, ok := m["schema_requested"]; !ok || v != false {
		t.Errorf("schema_requested in JSON = (%v, present=%v), want (false, true)", v, ok)
	}
}

func TestBuild_ApplicabilityStampedTrue(t *testing.T) {
	resp := buildWithApplicability(t, RoutingView{
		SchemaRequested:  true,
		ToolsDeclared:    true,
		ApplicabilitySet: true,
	})

	if resp.Data.SchemaRequested == nil || !*resp.Data.SchemaRequested {
		t.Errorf("SchemaRequested = %v, want true", resp.Data.SchemaRequested)
	}
	if resp.Data.ToolsDeclared == nil || !*resp.Data.ToolsDeclared {
		t.Errorf("ToolsDeclared = %v, want true", resp.Data.ToolsDeclared)
	}
}

// Guards against a future edit that forgets ApplicabilitySet and lets the raw
// booleans through — that would publish flags nobody observed.
func TestBuild_ApplicabilityRequiresTheSetFlag(t *testing.T) {
	resp := buildWithApplicability(t, RoutingView{
		SchemaRequested: true,
		ToolsDeclared:   true,
		// ApplicabilitySet deliberately false.
	})
	if resp.Data.SchemaRequested != nil || resp.Data.ToolsDeclared != nil {
		t.Errorf("flags emitted without ApplicabilitySet: (%v, %v); the set flag must gate both",
			resp.Data.SchemaRequested, resp.Data.ToolsDeclared)
	}
}

func TestBoolPtrIf(t *testing.T) {
	if got := boolPtrIf(false, true); got != nil {
		t.Errorf("boolPtrIf(false, true) = %v, want nil", *got)
	}
	if got := boolPtrIf(true, false); got == nil || *got {
		t.Errorf("boolPtrIf(true, false) = %v, want non-nil false", got)
	}
	if got := boolPtrIf(true, true); got == nil || !*got {
		t.Errorf("boolPtrIf(true, true) = %v, want non-nil true", got)
	}
}
