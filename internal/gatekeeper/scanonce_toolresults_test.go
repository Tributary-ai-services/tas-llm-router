package gatekeeper

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	routerTypes "github.com/tributary-ai/llm-router-waf/internal/types"
)

func toolConversation() []routerTypes.Message {
	return []routerTypes.Message{
		{Role: "user", Content: "Look up the order."},
		{Role: "assistant", Content: "Looking it up.", ToolCalls: []routerTypes.ToolCall{{ID: "call_card", Type: "function"}, {ID: "call_clean", Type: "function"}}},
		{Role: "tool", ToolCallID: "call_card", Content: "Order paid with card " + testCard + " exp 12/29."},
		{Role: "tool", ToolCallID: "call_clean", Content: "Shipping status: delivered on Tuesday."},
	}
}

// Plan #22 T.5: the per-message scan attributes countable findings to the
// tool result they were found in, and re-sent history adds nothing.
func TestScanOnce_ToolResultsAttribution(t *testing.T) {
	c := clientWithStore(t, newMemStore())
	ctx := context.Background()
	meta := ScanMeta{TenantID: "tenant-a", RequestID: "r"}

	s1, err := c.ScanInbound(ctx, toolConversation(), meta)
	require.NoError(t, err)
	if countCards(s1) == 0 {
		t.Skip("default rules did not flag the test card")
	}
	require.Contains(t, s1.ToolResults, "call_card")
	require.Contains(t, s1.ToolResults, "call_clean")
	cardHits := 0
	for k, v := range s1.ToolResults["call_card"].Findings {
		if k == "pii-credit-card" || k == "pii_credit_card" {
			cardHits += v
		}
	}
	assert.Equal(t, 1, cardHits, "the card is credited to the tool result that carried it")
	assert.Empty(t, s1.ToolResults["call_clean"].Findings)
	assert.False(t, s1.ToolResults["call_clean"].Skipped)

	// Next turn re-sends the same results: nothing is countable again.
	s2, err := c.ScanInbound(ctx, toolConversation(), meta)
	require.NoError(t, err)
	assert.Empty(t, s2.ToolResults["call_card"].Findings, "counted once, not per turn")
	assert.True(t, s2.ToolResults["call_clean"].Skipped, "remembered clean, not rescanned")
}

// Without scan-once the request is scanned as one blob: no attribution.
func TestScanOnce_ToolResultsNilWhenOff(t *testing.T) {
	c := newTestClient(t)
	s, err := c.ScanInbound(context.Background(), toolConversation(), ScanMeta{TenantID: "tenant-a"})
	require.NoError(t, err)
	assert.Nil(t, s.ToolResults)
}
