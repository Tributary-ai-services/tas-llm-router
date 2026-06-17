package server

import (
	"encoding/json"
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func TestParseOverrideReduction(t *testing.T) {
	t.Run("nil for empty / model-only override", func(t *testing.T) {
		if parseOverrideReduction(nil) != nil {
			t.Error("nil override should yield nil")
		}
		if parseOverrideReduction(json.RawMessage(`{"model":"gpt-4o-mini"}`)) != nil {
			t.Error("model-only override should yield nil reduction")
		}
	})
	t.Run("parses reduction block", func(t *testing.T) {
		rp := parseOverrideReduction(json.RawMessage(`{"reduction":{"mode":"active","steps":{"relevance":{"enabled":true,"top_k":20}}}}`))
		if rp == nil {
			t.Fatal("expected a parsed reduction policy")
		}
		if !rp.Applies() {
			t.Errorf("mode active should apply, got %q", rp.Mode)
		}
		if rp.Steps.Relevance == nil || rp.Steps.Relevance.TopK != 20 {
			t.Errorf("relevance top_k not parsed: %+v", rp.Steps.Relevance)
		}
	})
}

func TestLargestReducibleMessageIndex(t *testing.T) {
	t.Run("picks largest non-last-user message", func(t *testing.T) {
		msgs := []types.Message{
			{Role: "system", Content: "short"},
			{Role: "user", Content: "this is a very long context block with lots of detail to reduce"}, // largest, but not last user
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "the question?"}, // last user — must be excluded
		}
		got := largestReducibleMessageIndex(msgs)
		if got != 1 {
			t.Errorf("expected index 1 (long context), got %d", got)
		}
	})
	t.Run("excludes the lone question", func(t *testing.T) {
		msgs := []types.Message{{Role: "user", Content: "only a question, nothing to reduce"}}
		if got := largestReducibleMessageIndex(msgs); got != -1 {
			t.Errorf("expected -1 for lone question, got %d", got)
		}
	})
	t.Run("falls back to a non-user large message when last is user", func(t *testing.T) {
		msgs := []types.Message{
			{Role: "system", Content: "a moderately sized system preamble with content"},
			{Role: "user", Content: "tiny q"},
		}
		if got := largestReducibleMessageIndex(msgs); got != 0 {
			t.Errorf("expected index 0 (system), got %d", got)
		}
	})
}

func TestMessageContentString(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"string", "hello world", "hello world"},
		{"contentparts", []types.ContentPart{
			{Type: "text", Text: "a"},
			{Type: "image_url"},
			{Type: "text", Text: "b"},
		}, "ab"},
		{"json-decoded", []interface{}{
			map[string]interface{}{"type": "text", "text": "x"},
			map[string]interface{}{"type": "image_url"},
			map[string]interface{}{"type": "text", "text": "y"},
		}, "xy"},
		{"nil", nil, ""},
		{"number", 42, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := messageContentString(c.in); got != c.want {
				t.Errorf("messageContentString(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestMessagesByteLenAndLastUser(t *testing.T) {
	msgs := []types.Message{
		{Role: "system", Content: "sys"},      // 3
		{Role: "user", Content: "first ask"},  // 9
		{Role: "assistant", Content: "reply"}, // 5
		{Role: "user", Content: "final ask"},  // 9
	}
	if got, want := messagesByteLen(msgs), 26; got != want {
		t.Errorf("messagesByteLen = %d, want %d", got, want)
	}
	if got, want := lastUserText(msgs), "final ask"; got != want {
		t.Errorf("lastUserText = %q, want %q", got, want)
	}
	if got := lastUserText([]types.Message{{Role: "system", Content: "x"}}); got != "" {
		t.Errorf("lastUserText with no user = %q, want empty", got)
	}
}

func TestSampleHit(t *testing.T) {
	// rate <= 0 and >= 1 are deterministic; the middle is probabilistic.
	if !sampleHit(0) {
		t.Error("sampleHit(0) should always hit (measure all eligible)")
	}
	if !sampleHit(1) {
		t.Error("sampleHit(1) should always hit")
	}
	if !sampleHit(2) {
		t.Error("sampleHit(>1) should always hit")
	}
	// rate=0.5 over many trials should land strictly between 0 and N.
	hits := 0
	const n = 2000
	for i := 0; i < n; i++ {
		if sampleHit(0.5) {
			hits++
		}
	}
	if hits == 0 || hits == n {
		t.Errorf("sampleHit(0.5) produced %d/%d hits — not sampling", hits, n)
	}
}
