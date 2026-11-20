package gatekeeper

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Runs only against a real Redis: SCANONCE_REDIS_URL=redis://localhost:6379/15
func TestRedisScanOnceStore(t *testing.T) {
	url := os.Getenv("SCANONCE_REDIS_URL")
	if url == "" {
		t.Skip("SCANONCE_REDIS_URL not set")
	}
	store, err := NewRedisScanOnceStore(url)
	require.NoError(t, err)
	ctx := context.Background()
	k1, k2, k3 := scanOnceKeyPrefix+"test:1", scanOnceKeyPrefix+"test:2", scanOnceKeyPrefix+"test:missing"

	require.NoError(t, store.SetMany(ctx, map[string]ScanRecord{k1: {}, k2: {Patterns: []string{"pii-credit-card"}}}, nil, 2*time.Second))
	recs, err := store.GetMany(ctx, []string{k1, k2, k3})
	require.NoError(t, err)
	require.Len(t, recs, 3)
	require.NotNil(t, recs[0])
	assert.True(t, recs[0].Clean())
	require.NotNil(t, recs[1])
	assert.Equal(t, []string{"pii-credit-card"}, recs[1].Patterns)
	assert.Nil(t, recs[2])

	// touch refreshes the TTL
	time.Sleep(1200 * time.Millisecond)
	require.NoError(t, store.SetMany(ctx, nil, []string{k1}, 2*time.Second))
	time.Sleep(1200 * time.Millisecond)
	recs, err = store.GetMany(ctx, []string{k1, k2})
	require.NoError(t, err)
	assert.NotNil(t, recs[0], "touched key survives past its original TTL")
	assert.Nil(t, recs[1], "untouched key expired")

	// End to end through the client.
	c := clientWithStore(t, store)
	meta := ScanMeta{TenantID: "tenant-redis-" + time.Now().Format("150405.000")}
	counted := 0
	for turn := 1; turn <= 5; turn++ {
		s, err := c.ScanInbound(ctx, conversation(turn), meta)
		require.NoError(t, err)
		assert.True(t, s.StoreUsed)
		counted += countCards(s)
	}
	assert.Equal(t, 1, counted)
}
