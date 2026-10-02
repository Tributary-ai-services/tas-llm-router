package server

import (
	"strings"

	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
)

// defaultStreamBufferMaxBytes caps the assembled text kept for one streamed
// response. 256 KiB holds any realistic completion — a 128k-token output is
// roughly 512 KB of text, and nothing we serve approaches that — while keeping
// the worst case bounded at concurrent-streams × this value.
//
// The cap matters more here than it looks: llm-router-aiqg declares no memory
// limit at all (BestEffort, per the cluster's resource policy), so there is no
// container ceiling to catch an unbounded buffer. This constant IS the ceiling.
const defaultStreamBufferMaxBytes = 256 * 1024

// streamBuffer accumulates the text of a streamed response so the quality
// layer can read it after the stream closes.
//
// Why this exists: before it, a streamed response left NO text anywhere.
// streamChunks forwarded each chunk to the wire and kept only token usage and
// finish_reason, so no *types.ChatResponse was ever assembled — which is why
// maybeJudge has only ever run on the non-streaming path, and why structural
// Efficacy sub-metrics had nothing to parse. That was never a decision; it was
// an absence.
//
// Deliberately NOT safe for concurrent use. It is owned by the single
// `for range chunks` goroutine in streamChunks, so a mutex would be cost
// without a reader. Anything that starts sharing it must add one.
type streamBuffer struct {
	b   strings.Builder
	max int

	// truncated: the cap was reached and later content was dropped. The text
	// held is a PREFIX of what the caller received.
	truncated bool
	// incomplete: the stream terminated on an error frame, so the text is a
	// fragment of an answer the caller never fully got either.
	incomplete bool
	// usage is the vendor's last reported token usage, carried so the quality
	// layer can price an evaluation against the same numbers the event does.
	usage *types.Usage
}

// newStreamBuffer returns a buffer capped at max bytes. max <= 0 selects the
// default; a negative max is handled by the caller (buffering off) rather than
// here, so this never has to represent "disabled".
func newStreamBuffer(max int) *streamBuffer {
	if max <= 0 {
		max = defaultStreamBufferMaxBytes
	}
	return &streamBuffer{max: max}
}

// append adds one chunk's content deltas, stopping at the cap.
//
// Once the cap is hit it stops appending and sets truncated — it does not keep
// growing and slice afterwards, which would defeat the bound this type exists
// to enforce. The truncation is counted once, not once per subsequent chunk.
func (sb *streamBuffer) append(chunk *types.ChatChunk) {
	if sb == nil || chunk == nil {
		return
	}
	for _, c := range chunk.Choices {
		if c.Delta == nil {
			continue
		}
		// Mirrors extractResponseContent: only string content is text. A
		// structured content block carries no assistant prose to judge.
		text, ok := c.Delta.Content.(string)
		if !ok || text == "" {
			continue
		}
		if sb.truncated {
			return
		}
		if room := sb.max - sb.b.Len(); len(text) > room {
			if room > 0 {
				sb.b.WriteString(text[:room])
			}
			sb.truncated = true
			metrics.StreamBufferTruncatedTotal.Inc()
			return
		}
		sb.b.WriteString(text)
	}
}

// markIncomplete records that the stream ended on an error frame.
func (sb *streamBuffer) markIncomplete() {
	if sb != nil {
		sb.incomplete = true
	}
}

// setUsage records the vendor's reported usage (last write wins, matching how
// streamChunks already tracks it for the cost metrics).
func (sb *streamBuffer) setUsage(u *types.Usage) {
	if sb != nil && u != nil {
		sb.usage = u
	}
}

// text returns the accumulated response text. Empty for a tool-call-only or
// content-free stream, which the quality layer treats the same way it already
// treats an empty non-streaming response.
func (sb *streamBuffer) text() string {
	if sb == nil {
		return ""
	}
	return sb.b.String()
}
