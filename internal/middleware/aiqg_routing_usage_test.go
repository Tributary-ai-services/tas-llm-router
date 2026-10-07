package middleware

import (
	"context"
	"testing"
)

// The shapes below are the two usage blocks a real Anthropic stream produces,
// taken from internal/providers/anthropic/provider.go: message_start reports
// the input and cache counts with output_tokens: 0 (:456-467), and the closing
// chunk reports all four from the accumulated message (:319-325).
const (
	wantPrompt        = 100
	wantCompletion    = 357
	wantCacheCreation = 50
	wantCacheRead     = 200
)

func stampMessageStart(ctx context.Context) {
	StampTokenUsage(ctx, wantPrompt, 0, wantCacheCreation, wantCacheRead)
}

func stampClosingChunk(ctx context.Context) {
	StampTokenUsage(ctx, wantPrompt, wantCompletion, wantCacheCreation, wantCacheRead)
}

// AIQG-47: the regression itself. Before the fix this recorded
// completion_tokens: 0 for every streamed response, because message_start
// latched the whole usage block before any output existed.
func TestStampTokenUsage_StreamedOutputSurvivesMessageStart(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	stampMessageStart(ctx)
	stampClosingChunk(ctx)

	s := r.Snapshot()
	if s.CompletionTokens != wantCompletion {
		t.Errorf("CompletionTokens = %d, want %d — the closing chunk's authoritative output count was discarded",
			s.CompletionTokens, wantCompletion)
	}
	if s.PromptTokens != wantPrompt || s.CacheCreationTokens != wantCacheCreation || s.CacheReadTokens != wantCacheRead {
		t.Errorf("input side changed: %#v", s)
	}
}

// The inverse hazard, and the reason the fix is per-field rather than a
// wholesale last-write-wins: a closing chunk that omits the input side must not
// erase it. On this traffic the cache counts are the dominant cost term, so
// dropping them would have been a worse bug than the one being fixed.
func TestStampTokenUsage_CacheCountsSurviveAnOutputOnlyClosingChunk(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	stampMessageStart(ctx)
	StampTokenUsage(ctx, 0, wantCompletion, 0, 0) // output only

	s := r.Snapshot()
	if s.CompletionTokens != wantCompletion {
		t.Errorf("CompletionTokens = %d, want %d", s.CompletionTokens, wantCompletion)
	}
	if s.PromptTokens != wantPrompt {
		t.Errorf("PromptTokens = %d, want %d (erased by a chunk that omitted it)", s.PromptTokens, wantPrompt)
	}
	if s.CacheCreationTokens != wantCacheCreation || s.CacheReadTokens != wantCacheRead {
		t.Errorf("cache counts erased: creation=%d read=%d", s.CacheCreationTokens, s.CacheReadTokens)
	}
}

// Raising rather than overwriting makes the stamper order-independent. This is
// the sharpest statement of the old bug: the forward order lost the output
// count while the reverse order kept it, so the result depended on which chunk
// happened to arrive first.
func TestStampTokenUsage_OrderIndependent(t *testing.T) {
	forward := NewRouting()
	fctx := WithRouting(context.Background(), forward)
	stampMessageStart(fctx)
	stampClosingChunk(fctx)

	reverse := NewRouting()
	rctx := WithRouting(context.Background(), reverse)
	stampClosingChunk(rctx)
	stampMessageStart(rctx)

	f, rv := forward.Snapshot(), reverse.Snapshot()
	if f.PromptTokens != rv.PromptTokens || f.CompletionTokens != rv.CompletionTokens ||
		f.CacheCreationTokens != rv.CacheCreationTokens || f.CacheReadTokens != rv.CacheReadTokens {
		t.Errorf("stamp order changed the result:\n forward=%#v\n reverse=%#v", f, rv)
	}
}

// A later stamp may only raise. Guards the new semantic against being
// "simplified" into last-write-wins, which would reintroduce the cache-erasing
// failure above.
func TestStampTokenUsage_NeverLowers(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	stampClosingChunk(ctx)
	StampTokenUsage(ctx, 1, 1, 1, 1)

	s := r.Snapshot()
	if s.CompletionTokens != wantCompletion || s.PromptTokens != wantPrompt ||
		s.CacheCreationTokens != wantCacheCreation || s.CacheReadTokens != wantCacheRead {
		t.Errorf("a lower stamp moved a count: %#v", s)
	}
}

// The usageSet distinction this stamper exists to preserve: an all-zero first
// stamp means "the vendor returned 0", which must stay distinguishable from
// "the vendor never returned a usage block" — and a later real stamp must then
// still be able to raise it.
func TestStampTokenUsage_ZeroStampStillUpgradeable(t *testing.T) {
	r := NewRouting()
	ctx := WithRouting(context.Background(), r)

	StampTokenUsage(ctx, 0, 0, 0, 0)
	if s := r.Snapshot(); !s.UsageSet {
		t.Fatalf("UsageSet=false after an all-zero stamp")
	}
	stampClosingChunk(ctx)
	if s := r.Snapshot(); s.CompletionTokens != wantCompletion {
		t.Errorf("CompletionTokens = %d after upgrading a zero stamp, want %d", s.CompletionTokens, wantCompletion)
	}
}
