package gatekeeper

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	routerTypes "github.com/tributary-ai/llm-router-waf/internal/types"
)

func redactMeta() ScanMeta {
	return ScanMeta{TenantID: "t", RequestID: "r", UserID: "u", Source: "llm_input"}
}

// End-to-end against the real Gatekeeper scanner + redaction engine: an email in
// a user message is masked, the finding is counted, and the original slice is
// left untouched (the caller assigns the returned copy).
func TestRedactMessages_MasksEmail(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	orig := []routerTypes.Message{
		{Role: "user", Content: "email me at alice@example.com please"},
	}
	out, n, err := client.RedactMessages(context.Background(), orig, redactMeta(), "mask")
	require.NoError(t, err)
	require.Greater(t, n, 0, "an obvious email must be detected and redacted")

	got, _ := out[0].Content.(string)
	assert.NotContains(t, got, "alice@example.com", "raw email survived redaction")
	// Original slice must be unchanged — redaction returns a copy.
	assert.Equal(t, "email me at alice@example.com please", orig[0].Content, "original message was mutated")
}

// Determinism is the cache-safety contract: identical input redacts to identical
// bytes, or a redacted prompt would break vendor prompt caching.
func TestRedactMessages_Deterministic(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	msgs := []routerTypes.Message{{Role: "user", Content: "reach bob@example.com now"}}
	out1, n1, err := client.RedactMessages(context.Background(), msgs, redactMeta(), "mask")
	require.NoError(t, err)
	out2, n2, err := client.RedactMessages(context.Background(), msgs, redactMeta(), "mask")
	require.NoError(t, err)

	assert.Equal(t, n1, n2)
	assert.Equal(t, out1[0].Content, out2[0].Content, "redaction is non-deterministic")
}

// Clean content: no findings, content unchanged, count 0.
func TestRedactMessages_CleanUnchanged(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	msgs := []routerTypes.Message{{Role: "user", Content: "what is the capital of France?"}}
	out, n, err := client.RedactMessages(context.Background(), msgs, redactMeta(), "mask")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, "what is the capital of France?", out[0].Content)
}

// Multimodal ([]interface{}) content is left unchanged in v1 (not redacted, not
// dropped) — a follow-up, but it must never corrupt the message.
func TestRedactMessages_MultimodalUnchanged(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	multimodal := []interface{}{
		map[string]interface{}{"type": "text", "text": "ssn 123-45-6789"},
	}
	msgs := []routerTypes.Message{{Role: "user", Content: multimodal}}
	out, n, err := client.RedactMessages(context.Background(), msgs, redactMeta(), "mask")
	require.NoError(t, err)
	assert.Equal(t, 0, n, "multimodal content is not redacted in v1")
	assert.Equal(t, multimodal, out[0].Content)
}

// The replace strategy substitutes a placeholder ([EMAIL]-style), distinct from
// mask — proves the strategy is honored, not hard-coded.
func TestRedactMessages_ReplaceStrategy(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	msgs := []routerTypes.Message{{Role: "user", Content: "write to carol@example.com"}}
	out, n, err := client.RedactMessages(context.Background(), msgs, redactMeta(), "replace")
	require.NoError(t, err)
	require.Greater(t, n, 0)
	got, _ := out[0].Content.(string)
	assert.NotContains(t, got, "carol@example.com")
	// mask keeps shape (has '@'); replace does not — a weak but real distinction.
	maskOut, _, _ := client.RedactMessages(context.Background(), msgs, redactMeta(), "mask")
	assert.NotEqual(t, out[0].Content, maskOut[0].Content, "replace and mask should differ")
}

// parseRedactionStrategy must never route to tokenize/remove, whatever the input.
func TestParseRedactionStrategy_SafeStrategiesOnly(t *testing.T) {
	for _, in := range []string{"tokenize", "remove", "", "bogus", "mask", "replace", "hash"} {
		got := string(parseRedactionStrategy(in))
		if got == "tokenize" || got == "remove" {
			t.Fatalf("parseRedactionStrategy(%q) = %q — must never be tokenize/remove", in, got)
		}
	}
	assert.Equal(t, "replace", string(parseRedactionStrategy("replace")))
	assert.Equal(t, "hash", string(parseRedactionStrategy("hash")))
	assert.Equal(t, "mask", string(parseRedactionStrategy("bogus")), "unknown → mask")
}

// A role the scan policy skips (assistant, by default) is not redacted.
func TestRedactMessages_HonorsScanPolicy(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()

	// Default policy never-scans "assistant"; its email must survive.
	msgs := []routerTypes.Message{
		{Role: "assistant", Content: "per our chat, dave@example.com"},
		{Role: "user", Content: "and eve@example.com"},
	}
	out, n, err := client.RedactMessages(context.Background(), msgs, redactMeta(), "mask")
	require.NoError(t, err)
	require.Greater(t, n, 0)
	assert.Contains(t, out[0].Content, "dave@example.com", "assistant message should not be scanned/redacted")
	got, _ := out[1].Content.(string)
	assert.NotContains(t, got, "eve@example.com", "user message should be redacted")
}

// The finding preview must stay log-safe even end-to-end (sanity that we never
// surface raw values through the count path — count is just an int, no value).
func TestRedactMessages_CountOnlyNoValueLeak(t *testing.T) {
	client := newTestClient(t)
	defer client.Close()
	msgs := []routerTypes.Message{{Role: "user", Content: "ssn 123-45-6789 and mail x@y.com"}}
	_, n, err := client.RedactMessages(context.Background(), msgs, redactMeta(), "mask")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 2, "both SSN and email should be found")
}
