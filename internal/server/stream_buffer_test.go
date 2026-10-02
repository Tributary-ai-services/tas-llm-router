package server

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
)

func deltaChunk(texts ...string) *types.ChatChunk {
	c := &types.ChatChunk{}
	for i, t := range texts {
		c.Choices = append(c.Choices, types.ChoiceChunk{
			Index: i,
			Delta: &types.Message{Content: t},
		})
	}
	return c
}

func TestStreamBuffer_AccumulatesDeltas(t *testing.T) {
	sb := newStreamBuffer(0)
	sb.append(deltaChunk("Hello"))
	sb.append(deltaChunk(", "))
	sb.append(deltaChunk("world"))

	if got := sb.text(); got != "Hello, world" {
		t.Errorf("text() = %q, want %q", got, "Hello, world")
	}
	if sb.truncated {
		t.Error("truncated = true well under the cap")
	}
	if sb.incomplete {
		t.Error("incomplete = true with no error frame")
	}
}

func TestStreamBuffer_DefaultCapApplies(t *testing.T) {
	for _, max := range []int{0, -1} {
		if got := newStreamBuffer(max).max; got != defaultStreamBufferMaxBytes {
			t.Errorf("newStreamBuffer(%d).max = %d, want the default %d", max, got, defaultStreamBufferMaxBytes)
		}
	}
}

// The cap is the only thing bounding memory here — llm-router-aiqg declares no
// container memory limit — so this asserts appends actually STOP rather than
// the buffer growing and being sliced afterwards.
func TestStreamBuffer_CapStopsAppending(t *testing.T) {
	before := testutil.ToFloat64(metrics.StreamBufferTruncatedTotal)

	sb := newStreamBuffer(10)
	sb.append(deltaChunk("12345"))
	sb.append(deltaChunk("678901234567890")) // crosses the cap
	sb.append(deltaChunk("ignored entirely"))
	sb.append(deltaChunk("also ignored"))

	if got := sb.text(); got != "1234567890" {
		t.Errorf("text() = %q, want exactly the first 10 bytes", got)
	}
	if n := len(sb.text()); n > 10 {
		t.Errorf("buffer grew past the cap to %d bytes — the bound is not enforced", n)
	}
	if !sb.truncated {
		t.Error("truncated = false after crossing the cap")
	}

	// Counted ONCE, not once per subsequent chunk, or the metric reads as a
	// chunk count rather than a response count.
	if delta := testutil.ToFloat64(metrics.StreamBufferTruncatedTotal) - before; delta != 1 {
		t.Errorf("StreamBufferTruncatedTotal delta = %v, want exactly 1 across 4 appends", delta)
	}
}

func TestStreamBuffer_ExactFitIsNotTruncated(t *testing.T) {
	sb := newStreamBuffer(5)
	sb.append(deltaChunk("12345"))
	if sb.truncated {
		t.Error("truncated = true on an exact fit; the cap is inclusive")
	}
	if got := sb.text(); got != "12345" {
		t.Errorf("text() = %q, want %q", got, "12345")
	}
}

func TestStreamBuffer_MultiChoiceAndNonStringContent(t *testing.T) {
	sb := newStreamBuffer(0)
	sb.append(deltaChunk("a", "b")) // two choices in one chunk

	// Structured (non-string) content carries no assistant prose, and a nil
	// delta is a role announcement or ping — both must be skipped rather than
	// panicking or stringifying.
	sb.append(&types.ChatChunk{Choices: []types.ChoiceChunk{
		{Delta: &types.Message{Content: []any{map[string]any{"type": "image"}}}},
		{Delta: nil},
		{Delta: &types.Message{Content: ""}},
	}})
	sb.append(deltaChunk("c"))

	if got := sb.text(); got != "abc" {
		t.Errorf("text() = %q, want %q", got, "abc")
	}
}

func TestStreamBuffer_NilSafeAndEmpty(t *testing.T) {
	var sb *streamBuffer // buffering disabled
	sb.append(deltaChunk("x"))
	sb.markIncomplete()
	sb.setUsage(&types.Usage{PromptTokens: 1})
	if got := sb.text(); got != "" {
		t.Errorf("nil buffer text() = %q, want empty", got)
	}

	empty := newStreamBuffer(0)
	empty.append(nil)
	if got := empty.text(); got != "" {
		t.Errorf("empty buffer text() = %q, want empty", got)
	}
}

func TestStreamBuffer_IncompleteAndUsage(t *testing.T) {
	sb := newStreamBuffer(0)
	sb.append(deltaChunk("partial"))
	sb.markIncomplete()
	sb.setUsage(&types.Usage{PromptTokens: 7, CompletionTokens: 3})
	sb.setUsage(nil) // must not clobber a real reading

	if !sb.incomplete {
		t.Error("incomplete = false after markIncomplete")
	}
	if sb.usage == nil || sb.usage.PromptTokens != 7 {
		t.Errorf("usage = %+v, want the last non-nil reading", sb.usage)
	}
	if got := sb.text(); got != "partial" {
		t.Errorf("text() = %q — an incomplete stream still keeps what arrived", got)
	}
}

// The buffer must never alter what the caller receives.
func TestStreamBuffer_DoesNotMutateChunks(t *testing.T) {
	chunk := deltaChunk("hello")
	sb := newStreamBuffer(3) // forces truncation, the likeliest place to slice in place
	sb.append(chunk)

	if got, _ := chunk.Choices[0].Delta.Content.(string); got != "hello" {
		t.Errorf("forwarded chunk content = %q, want %q unchanged", got, "hello")
	}
	if got := sb.text(); got != "hel" {
		t.Errorf("buffered text = %q, want %q", got, "hel")
	}
}

func TestStreamBuffer_LargeResponseUnderDefaultCap(t *testing.T) {
	sb := newStreamBuffer(0)
	piece := strings.Repeat("x", 4096)
	for i := 0; i < 16; i++ { // 64 KiB, a big but realistic completion
		sb.append(deltaChunk(piece))
	}
	if sb.truncated {
		t.Error("a 64 KiB response truncated under the 256 KiB default — the cap is too tight")
	}
	if n := len(sb.text()); n != 16*4096 {
		t.Errorf("buffered %d bytes, want %d", n, 16*4096)
	}
}
