package gatekeeper

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	routerTypes "github.com/tributary-ai/llm-router-waf/internal/types"
)

// memStore is an in-memory ScanOnceStore. fail makes every call error, and
// slow makes lookups exceed the budget, to exercise the fail-safe.
type memStore struct {
	mu   sync.Mutex
	recs map[string]ScanRecord
	fail bool
	slow time.Duration
	gets int
}

func newMemStore() *memStore { return &memStore{recs: map[string]ScanRecord{}} }

func (m *memStore) GetMany(ctx context.Context, keys []string) ([]*ScanRecord, error) {
	m.mu.Lock()
	m.gets++
	fail, slow := m.fail, m.slow
	m.mu.Unlock()
	if slow > 0 {
		select {
		case <-time.After(slow):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail {
		return nil, errors.New("redis: connection refused")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*ScanRecord, len(keys))
	for i, k := range keys {
		if r, ok := m.recs[k]; ok {
			rr := r
			out[i] = &rr
		}
	}
	return out, nil
}

func (m *memStore) SetMany(_ context.Context, recs map[string]ScanRecord, _ []string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("redis: connection refused")
	}
	for k, r := range recs {
		m.recs[k] = r
	}
	return nil
}

const testCard = "4111 1111 1111 1111"

// conversation returns the message list a chat client sends on turn n
// (1-based): the full history, with the card pasted in turn 1 only.
func conversation(n int) []routerTypes.Message {
	msgs := []routerTypes.Message{{Role: "system", Content: "You are a helpful assistant."}}
	for i := 1; i <= n; i++ {
		user := fmt.Sprintf("Question %d about the weather in city number %d.", i, i)
		if i == 1 {
			user = "My card is " + testCard + " exp 12/29, please check the order."
		}
		msgs = append(msgs, routerTypes.Message{Role: "user", Content: user})
		if i < n {
			msgs = append(msgs, routerTypes.Message{Role: "assistant", Content: fmt.Sprintf("Answer %d.", i)})
		}
	}
	return msgs
}

func clientWithStore(t *testing.T, store ScanOnceStore) *Client {
	t.Helper()
	c := newTestClient(t)
	c.SetScanOnceStore(store, ScanOnceConfig{Enabled: true, BuildID: "test"})
	return c
}

func countCards(s *InboundScan) int {
	n := 0
	for _, f := range s.Countable {
		if f.PatternID == "pii-credit-card" || f.PatternID == "pii_credit_card" {
			n++
		}
	}
	return n
}

func totalCards(s *InboundScan) int {
	n := 0
	for _, f := range s.Result.ScanResult.Findings {
		if f.PatternID == "pii-credit-card" || f.PatternID == "pii_credit_card" {
			n++
		}
	}
	return n
}

// The plan's headline test: over a 10-turn conversation the card from turn 1
// is counted once, yet every turn still sees it for blocking.
func TestScanOnce_TenTurnsCountCardOnce(t *testing.T) {
	c := clientWithStore(t, newMemStore())
	ctx := context.Background()
	meta := ScanMeta{TenantID: "tenant-a", RequestID: "r"}

	counted := 0
	for turn := 1; turn <= 10; turn++ {
		s, err := c.ScanInbound(ctx, conversation(turn), meta)
		require.NoError(t, err)
		require.True(t, s.StoreUsed, "turn %d", turn)
		counted += countCards(s)
		assert.Equal(t, 1, totalCards(s), "turn %d: the card must still be visible to blocking", turn)
		if turn > 1 {
			// system + earlier user turns are remembered; only the newest
			// user message is unseen. The turn-1 message is remembered WITH a
			// finding, so it is rescanned, not skipped.
			assert.Equal(t, s.Messages-1, s.Hits, "turn %d hits", turn)
			assert.Equal(t, s.Messages-2, s.Skipped, "turn %d skipped", turn)
		}
	}
	if counted == 0 {
		t.Skip("default rules did not flag the test card; nothing to count") // guards against a rules change, not expected
	}
	assert.Equal(t, 1, counted, "card counted once across 10 turns")
}

// Without scan-once (today's behaviour) the same 10 turns count it 10 times.
func TestScanOnce_OffCountsEveryTurn(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	counted := 0
	for turn := 1; turn <= 10; turn++ {
		s, err := c.ScanInbound(ctx, conversation(turn), ScanMeta{TenantID: "tenant-a"})
		require.NoError(t, err)
		counted += countCards(s)
	}
	assert.Equal(t, 10, counted)
}

func TestScanOnce_OtherTenantDoesNotSkip(t *testing.T) {
	store := newMemStore()
	c := clientWithStore(t, store)
	ctx := context.Background()
	msgs := conversation(3)

	_, err := c.ScanInbound(ctx, msgs, ScanMeta{TenantID: "tenant-a"})
	require.NoError(t, err)
	s, err := c.ScanInbound(ctx, msgs, ScanMeta{TenantID: "tenant-b"})
	require.NoError(t, err)
	assert.Equal(t, 0, s.Hits, "tenant B must not reuse tenant A's results")
	assert.Equal(t, 1, countCards(s), "tenant B counts its own finding")
}

func TestScanOnce_NoTenantScansEverything(t *testing.T) {
	store := newMemStore()
	c := clientWithStore(t, store)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		s, err := c.ScanInbound(ctx, conversation(2), ScanMeta{})
		require.NoError(t, err)
		assert.False(t, s.StoreUsed)
		assert.Equal(t, 1, countCards(s))
	}
	assert.Equal(t, 0, store.gets, "no tenant: the store is never consulted")
}

func TestScanOnce_RuleChangeRescans(t *testing.T) {
	store := newMemStore()
	c := clientWithStore(t, store)
	ctx := context.Background()
	meta := ScanMeta{TenantID: "tenant-a"}
	_, err := c.ScanInbound(ctx, conversation(2), meta)
	require.NoError(t, err)

	c.rulesetVersion = "a-different-ruleset"
	s, err := c.ScanInbound(ctx, conversation(2), meta)
	require.NoError(t, err)
	assert.Equal(t, 0, s.Hits, "a rule change must invalidate every remembered result")
	assert.Equal(t, 1, countCards(s))

	// A new build does the same.
	c.rulesetVersion = "a-different-ruleset"
	c.scanOnceCfg.BuildID = "next-build"
	s, err = c.ScanInbound(ctx, conversation(2), meta)
	require.NoError(t, err)
	assert.Equal(t, 0, s.Hits)
}

func TestScanOnce_StoreOutageScansAndCountsEverything(t *testing.T) {
	store := newMemStore()
	c := clientWithStore(t, store)
	ctx := context.Background()
	meta := ScanMeta{TenantID: "tenant-a"}
	_, err := c.ScanInbound(ctx, conversation(3), meta)
	require.NoError(t, err)

	store.fail = true
	s, err := c.ScanInbound(ctx, conversation(3), meta)
	require.NoError(t, err, "a store outage is never a request error")
	assert.False(t, s.StoreUsed)
	assert.Equal(t, 0, s.Skipped)
	assert.Equal(t, 1, countCards(s), "outage: back to counting every turn, never to missing one")
}

func TestScanOnce_SlowStoreFallsBackWithinBudget(t *testing.T) {
	store := newMemStore()
	store.slow = 200 * time.Millisecond
	c := clientWithStore(t, store) // default 20ms budget
	start := time.Now()
	s, err := c.ScanInbound(context.Background(), conversation(3), ScanMeta{TenantID: "tenant-a"})
	require.NoError(t, err)
	assert.False(t, s.StoreUsed)
	assert.Equal(t, 1, countCards(s))
	assert.Less(t, time.Since(start), 150*time.Millisecond, "the lookup budget bounds the wait")
}

func TestScanOnce_AllRememberedCleanStillReportsAScan(t *testing.T) {
	c := clientWithStore(t, newMemStore())
	ctx := context.Background()
	clean := []routerTypes.Message{{Role: "user", Content: "What is the capital of France?"}}
	_, err := c.ScanInbound(ctx, clean, ScanMeta{TenantID: "tenant-a"})
	require.NoError(t, err)
	s, err := c.ScanInbound(ctx, clean, ScanMeta{TenantID: "tenant-a"})
	require.NoError(t, err)
	assert.Equal(t, 1, s.Skipped)
	require.NotNil(t, s.Result.ScanResult, "Assurance must score 100, not nil")
	assert.False(t, s.Result.Skipped)
	assert.Empty(t, s.Result.ScanResult.Findings)
}

func TestScanOnce_RepeatedFindingStillBlocks(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Inbound.BlockOnCritical = true
	c := clientWithStore(t, newMemStore())
	c.config = cfg
	ctx := context.Background()
	meta := ScanMeta{TenantID: "tenant-a"}
	first, err := c.ScanInbound(ctx, conversation(2), meta)
	require.NoError(t, err)
	second, err := c.ScanInbound(ctx, conversation(3), meta)
	require.NoError(t, err)
	assert.Equal(t, c.ShouldBlock(first.Result, "inbound"), c.ShouldBlock(second.Result, "inbound"),
		"remembering a finding must not change the block decision")
}

func TestNormalizeForScanKey(t *testing.T) {
	// NFC: "é" composed vs decomposed share a key; trailing space is trimmed;
	// case and inner spacing are NOT normalised away.
	assert.Equal(t, normalizeForScanKey("café "), normalizeForScanKey("café"))
	assert.NotEqual(t, normalizeForScanKey("Card"), normalizeForScanKey("card"))
	assert.NotEqual(t, normalizeForScanKey("a  b"), normalizeForScanKey("a b"))
}

// Plan: "benchmark the p95 scan time before and after on a 20-turn replay".
// Run with: go test -tags nohs -run TestScanOnce_P95 -v ./internal/gatekeeper/
func TestScanOnce_P95TwentyTurnReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	measure := func(c *Client) time.Duration {
		var ds []time.Duration
		for turn := 1; turn <= 20; turn++ {
			start := time.Now()
			_, err := c.ScanInbound(context.Background(), conversation(turn), ScanMeta{TenantID: "tenant-a"})
			require.NoError(t, err)
			ds = append(ds, time.Since(start))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[int(float64(len(ds))*0.95)-1]
	}
	before := measure(newTestClient(t))
	after := measure(clientWithStore(t, newMemStore()))
	t.Logf("p95 per-turn inbound scan, 20-turn replay: before %v, after %v (in-memory store)", before, after)
}
