package server

import (
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

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
