package policy

import (
	"encoding/json"
	"testing"
)

func TestParseReduction(t *testing.T) {
	raw := json.RawMessage(`{"mode":"Shadow","min_tokens":8000,"sample_rate":0.05,"steps":{"relevance":{"enabled":true,"threshold":0.3,"top_k":100},"slm":{"enabled":false}}}`)
	rp, err := ParseReduction(raw)
	if err != nil {
		t.Fatal(err)
	}
	if rp == nil {
		t.Fatal("expected policy")
	}
	if rp.Mode != ReductionShadow { // lower-cased + trimmed
		t.Errorf("mode=%q want shadow", rp.Mode)
	}
	if rp.MinTokens != 8000 || rp.SampleRate != 0.05 {
		t.Errorf("min=%d rate=%v", rp.MinTokens, rp.SampleRate)
	}
	if rp.Steps.Relevance == nil || !rp.Steps.Relevance.Enabled || rp.Steps.Relevance.TopK != 100 {
		t.Errorf("relevance step not parsed: %+v", rp.Steps.Relevance)
	}
}

func TestParseReduction_EmptyAndMalformed(t *testing.T) {
	if rp, err := ParseReduction(nil); rp != nil || err != nil {
		t.Errorf("empty → (nil,nil), got (%v,%v)", rp, err)
	}
	if _, err := ParseReduction(json.RawMessage(`not json`)); err == nil {
		t.Error("malformed should error")
	}
	// Mode defaults to projected when absent.
	rp, _ := ParseReduction(json.RawMessage(`{"min_tokens":1}`))
	if rp == nil || rp.Mode != ReductionProjected {
		t.Errorf("absent mode should default projected, got %+v", rp)
	}
}

func TestReductionModeHelpers(t *testing.T) {
	var nilrp *ReductionPolicy
	if nilrp.RunsExtractor() || nilrp.Applies() || nilrp.EligibleBySize(99999) {
		t.Error("nil policy must be inert")
	}
	off := &ReductionPolicy{Mode: ReductionOff}
	if off.RunsExtractor() || off.Applies() {
		t.Error("off must not run/apply")
	}
	shadow := &ReductionPolicy{Mode: ReductionShadow}
	if !shadow.RunsExtractor() || shadow.Applies() {
		t.Error("shadow runs the extractor but does not apply")
	}
	active := &ReductionPolicy{Mode: ReductionActive}
	if !active.RunsExtractor() || !active.Applies() {
		t.Error("active runs + applies")
	}
	// MinTokens floor.
	sz := &ReductionPolicy{Mode: ReductionActive, MinTokens: 8000}
	if sz.EligibleBySize(100) {
		t.Error("below floor should be ineligible")
	}
	if !sz.EligibleBySize(8000) {
		t.Error("at floor should be eligible")
	}
}
