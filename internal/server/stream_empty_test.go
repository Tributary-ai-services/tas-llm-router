package server

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
)

// nullEncoder swallows everything; these tests are about what gets STAMPED,
// not about the wire format, which anthropic_messages_test covers.
type nullEncoder struct{ errs int }

func (n *nullEncoder) writeChunk(*types.ChatChunk)     {}
func (n *nullEncoder) writeError(e *types.StreamError) { n.errs++ }
func (n *nullEncoder) done()                           {}

func runStream(t *testing.T, chunks []*types.ChatChunk) middleware.RoutingSnapshot {
	t.Helper()
	s := &Server{logger: logrus.New()}
	r := middleware.NewRouting()
	ctx := middleware.WithRouting(context.Background(), r)
	ch := make(chan *types.ChatChunk, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	s.streamChunks(ctx, &nullEncoder{}, ch, "anthropic", "claude-sonnet-5-5")
	return r.Snapshot()
}

func textChunk(s string) *types.ChatChunk {
	return &types.ChatChunk{Choices: []types.ChoiceChunk{{Delta: &types.Message{Content: s}}}}
}

// AIQG-56: the measured failure. The vendor answers, sends a handful of empty
// chunks, and closes cleanly. 5 of 102 real dogfood turns looked like this and
// every one was recorded as a success.
func TestStreamChunks_EmptyStreamIsNotASuccess(t *testing.T) {
	snap := runStream(t, []*types.ChatChunk{
		{Usage: &types.Usage{PromptTokens: 158612, CacheReadTokens: 64406}},
		{Choices: []types.ChoiceChunk{{Delta: &types.Message{}}}},
		{Choices: []types.ChoiceChunk{{Delta: &types.Message{}}}},
	})
	if snap.Outcome != events.StatusVendorError {
		t.Errorf("Outcome = %q, want %q — a stream that produced nothing was recorded as a success",
			snap.Outcome, events.StatusVendorError)
	}
	// "error" is what pkg/clear/efficacy.go scores 0 undiluted; NULL efficacy
	// is what let these flatter the average by 4 points.
	if snap.FinishReason != "error" {
		t.Errorf("FinishReason = %q, want \"error\" so efficacy scores it 0", snap.FinishReason)
	}
}

// The trap this fix had to avoid: a tool-only turn carries NO text, and 47 of
// 102 measured dogfood turns finished tool_use. Flagging those would invent a
// 46% failure rate.
func TestStreamChunks_ToolOnlyStreamIsASuccess(t *testing.T) {
	snap := runStream(t, []*types.ChatChunk{
		{Usage: &types.Usage{PromptTokens: 1000}},
		{Choices: []types.ChoiceChunk{{Delta: &types.Message{
			ToolCalls: []types.ToolCall{{ID: "t1", Type: "function"}},
		}}}},
		{Choices: []types.ChoiceChunk{{Delta: &types.Message{}, FinishReason: "tool_use"}}},
	})
	if snap.Outcome != "" {
		t.Errorf("Outcome = %q, want empty — a tool-only turn is a legitimate success", snap.Outcome)
	}
	if snap.FinishReason != "tool_use" {
		t.Errorf("FinishReason = %q, want tool_use", snap.FinishReason)
	}
}

func TestStreamChunks_TextStreamIsASuccess(t *testing.T) {
	snap := runStream(t, []*types.ChatChunk{
		textChunk("hello "), textChunk("world"),
		{Choices: []types.ChoiceChunk{{Delta: &types.Message{}, FinishReason: "end_turn"}}},
	})
	if snap.Outcome != "" {
		t.Errorf("Outcome = %q, want empty for a normal text stream", snap.Outcome)
	}
}

// A vendor-reported reason must never be overwritten: StampFinishReason is
// first-non-empty-wins, so an end_turn with no content keeps end_turn and
// still gets the failure OUTCOME. Stated as a known limit in the code: its
// efficacy will read 100.
func TestStreamChunks_EmptyButVendorReportedAReason(t *testing.T) {
	snap := runStream(t, []*types.ChatChunk{
		{Choices: []types.ChoiceChunk{{Delta: &types.Message{}, FinishReason: "end_turn"}}},
	})
	if snap.FinishReason != "end_turn" {
		t.Errorf("FinishReason = %q — a vendor-reported reason was overwritten", snap.FinishReason)
	}
	if snap.Outcome != events.StatusVendorError {
		t.Errorf("Outcome = %q, want %q — zero content is still a failure", snap.Outcome, events.StatusVendorError)
	}
}

// An error frame must keep #263's behaviour, not be reclassified by this path.
func TestStreamChunks_ErrorFrameStillWins(t *testing.T) {
	snap := runStream(t, []*types.ChatChunk{
		{Error: &types.StreamError{Message: "boom", Type: "upstream_stream_error", UpstreamStatus: 529}},
	})
	if snap.Outcome != events.StatusVendorError || snap.FinishReason != "error" {
		t.Errorf("error frame mishandled: outcome=%q finish=%q", snap.Outcome, snap.FinishReason)
	}
	if snap.VendorErrorStatus != 529 {
		t.Errorf("VendorErrorStatus = %d, want 529", snap.VendorErrorStatus)
	}
}
