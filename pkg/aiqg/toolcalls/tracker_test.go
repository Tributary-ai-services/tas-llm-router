package toolcalls

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
)

var t0 = time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

func ident(step string) Identity {
	return Identity{
		TenantID: "a689c0b2-02ca-46d1-9916-f9a30c00222a", TokenID: "8f29197c-818b-4b69-bf59-200f5c3228f5",
		OwnerSub: "kc-sub-123", UserID: "u-1", AgentID: "agent-1", SourceApp: "claude-code",
		FlowID: "flow-1", StepID: step,
	}
}

// stores runs fn against the memory store and, when TOOLCALLS_REDIS_URL is
// set, against a real Redis (the production store, Lua scripts included).
func stores(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Run("memory", func(t *testing.T) { fn(t, NewMemoryStore()) })
	url := os.Getenv("TOOLCALLS_REDIS_URL")
	if url == "" {
		t.Log("TOOLCALLS_REDIS_URL not set; Redis store not exercised")
		return
	}
	t.Run("redis", func(t *testing.T) {
		opt, err := redis.ParseURL(url)
		require.NoError(t, err)
		rc := redis.NewClient(opt)
		require.NoError(t, rc.FlushDB(context.Background()).Err())
		t.Cleanup(func() { rc.FlushDB(context.Background()); rc.Close() })
		fn(t, NewRedisStore(rc))
	})
}

// newTestTracker returns a tracker with no background goroutines doing work
// that races the test (sweep interval far away); tests call Process/Sweep.
func newTestTracker(s Store) (*Tracker, *events.MemoryEmitter) {
	em := &events.MemoryEmitter{}
	tr := New(s, em, logrus.New(), Config{ResultTimeout: time.Hour, SweepInterval: time.Hour}, Hooks{})
	return tr, em
}

func outcomes(em *events.MemoryEmitter) map[string]string {
	out := map[string]string{}
	for _, e := range em.ToolCalls() {
		out[e.Data.ToolCallID] = e.Data.Outcome
	}
	return out
}

func TestTracker_OKWithRoundTrip_EmittedOnce(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, em := newTestTracker(s)
		defer tr.Close(context.Background())
		ctx := context.Background()

		tr.Process(ctx, Observation{Identity: ident("step-1"), IssuedAt: t0, EchoAt: t0.Add(-time.Second),
			Served: []Call{{ID: "toolu_01", Name: "draw"}}})
		require.Empty(t, em.ToolCalls(), "a call is not emitted until it closes")

		// Turn 2 echoes the result; turns 3 and 4 re-send it as history. Only
		// the first sighting closes the call.
		for turn := 2; turn <= 4; turn++ {
			echoed := []Result{{ID: "toolu_01", Bytes: 2048}}
			tr.Process(ctx, Observation{Identity: ident("step-x"), EchoAt: t0.Add(1500 * time.Millisecond), IssuedAt: t0.Add(2 * time.Second), Echoed: echoed})
		}
		got := em.ToolCalls()
		require.Len(t, got, 1, "one event per call")
		d := got[0].Data
		assert.Equal(t, "ok", d.Outcome)
		assert.Equal(t, "gateway", d.ObservedBy)
		require.NotNil(t, d.RoundTripMs)
		assert.EqualValues(t, 1500, *d.RoundTripMs)
		assert.Equal(t, "step-1", d.StepID, "identity is the calling response's")
		assert.Equal(t, "draw", d.ToolName)
		assert.EqualValues(t, 2048, *d.ResultBytes)
		assert.EqualValues(t, 512, *d.ResultTokensEst)
		assert.Equal(t, t0, d.OccurredAt)
	})
}

func TestTracker_NoResult_WhenFlowCloses(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, em := newTestTracker(s)
		defer tr.Close(context.Background())
		ctx := context.Background()
		issued := time.Now().Add(-2 * time.Hour) // pending past the 1h timeout
		tr.Process(ctx, Observation{Identity: ident("step-1"), IssuedAt: issued, EchoAt: issued,
			Served: []Call{{ID: "call_A", Name: "search"}}})
		tr.Sweep(ctx)
		require.Equal(t, map[string]string{"call_A": "no_result"}, outcomes(em))
		assert.Nil(t, em.ToolCalls()[0].Data.RoundTripMs)

		// A late echo must not emit the same call again as ok.
		tr.Process(ctx, Observation{Identity: ident("step-2"), EchoAt: time.Now(), IssuedAt: time.Now(),
			Echoed: []Result{{ID: "call_A"}}})
		tr.Sweep(ctx)
		assert.Len(t, em.ToolCalls(), 1, "a call that closed as no_result is never also emitted as ok")
	})
}

func TestTracker_SweepLeavesLiveCalls(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, em := newTestTracker(s)
		defer tr.Close(context.Background())
		now := time.Now()
		tr.Process(context.Background(), Observation{Identity: ident("s"), IssuedAt: now, EchoAt: now, Served: []Call{{ID: "c1", Name: "x"}}})
		tr.Sweep(context.Background())
		assert.Empty(t, em.ToolCalls())
	})
}

func TestTracker_SiblingWithoutResultIsNoResult(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, em := newTestTracker(s)
		defer tr.Close(context.Background())
		ctx := context.Background()
		tr.Process(ctx, Observation{Identity: ident("step-1"), IssuedAt: t0, EchoAt: t0,
			Served: []Call{{ID: "c1", Name: "a"}, {ID: "c2", Name: "b"}, {ID: "c3", Name: "c"}}})
		// The next turn of the flow carries c1 and c3, not c2.
		tr.Process(ctx, Observation{Identity: ident("step-2"), EchoAt: t0.Add(time.Second), IssuedAt: t0.Add(2 * time.Second),
			Echoed: []Result{{ID: "c1"}, {ID: "c3"}}})
		assert.Equal(t, map[string]string{"c1": "ok", "c2": "no_result", "c3": "ok"}, outcomes(em))
		assert.Len(t, em.ToolCalls(), 3)
		tr.Sweep(ctx)
		assert.Len(t, em.ToolCalls(), 3, "nothing left pending to sweep")
	})
}

// The echo can reach another replica before this one's worker opens the
// call. The tombstone closes it as ok; it is never swept as no_result.
func TestTracker_EchoBeforeOpen(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, em := newTestTracker(s)
		defer tr.Close(context.Background())
		ctx := context.Background()
		issued := time.Now()
		tr.Process(ctx, Observation{Identity: ident("step-2"), EchoAt: issued.Add(800 * time.Millisecond), IssuedAt: issued.Add(time.Second),
			Echoed: []Result{{ID: "c1"}}})
		require.Empty(t, em.ToolCalls())
		tr.Process(ctx, Observation{Identity: ident("step-1"), IssuedAt: issued, EchoAt: issued,
			Served: []Call{{ID: "c1", Name: "a"}, {ID: "c2", Name: "b"}}})
		assert.Equal(t, map[string]string{"c1": "ok", "c2": "no_result"}, outcomes(em))
		for _, e := range em.ToolCalls() {
			if e.Data.ToolCallID == "c1" {
				require.NotNil(t, e.Data.RoundTripMs)
				assert.InDelta(t, 800, *e.Data.RoundTripMs, 1)
				assert.Equal(t, "step-1", e.Data.StepID)
			}
		}
	})
}

// A client that reuses ids ("call_0" every turn) answers call_0 and mints a
// new call_0 in the same request/response. The answer closes the old call;
// the new one stays open and is not mistaken for answered.
func TestTracker_ReusedIDAcrossTurns(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, em := newTestTracker(s)
		defer tr.Close(context.Background())
		ctx := context.Background()
		tr.Process(ctx, Observation{Identity: ident("step-1"), IssuedAt: t0, EchoAt: t0, Served: []Call{{ID: "call_0", Name: "a"}}})
		tr.Process(ctx, Observation{Identity: ident("step-2"), EchoAt: t0.Add(time.Second), IssuedAt: t0.Add(2 * time.Second),
			Echoed: []Result{{ID: "call_0"}}, Served: []Call{{ID: "call_0", Name: "b"}}})
		got := em.ToolCalls()
		require.Len(t, got, 1)
		assert.Equal(t, "ok", got[0].Data.Outcome)
		assert.Equal(t, "step-1", got[0].Data.StepID)
		tr.Process(ctx, Observation{Identity: ident("step-3"), EchoAt: t0.Add(3 * time.Second), IssuedAt: t0.Add(4 * time.Second),
			Echoed: []Result{{ID: "call_0"}}})
		got = em.ToolCalls()
		require.Len(t, got, 2)
		assert.Equal(t, "step-2", got[1].Data.StepID)
		assert.NotEqual(t, got[0].ID, got[1].ID, "distinct calls get distinct event ids")
	})
}

// An id minted again while still pending: the first call's result never
// came back, so it closes as no_result.
func TestTracker_DisplacedPendingIsNoResult(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, em := newTestTracker(s)
		defer tr.Close(context.Background())
		ctx := context.Background()
		tr.Process(ctx, Observation{Identity: ident("step-1"), IssuedAt: t0, EchoAt: t0, Served: []Call{{ID: "call_0", Name: "a"}}})
		tr.Process(ctx, Observation{Identity: ident("step-2"), IssuedAt: t0.Add(time.Second), EchoAt: t0, Served: []Call{{ID: "call_0", Name: "a"}}})
		got := em.ToolCalls()
		require.Len(t, got, 1)
		assert.Equal(t, "no_result", got[0].Data.Outcome)
		assert.Equal(t, "step-1", got[0].Data.StepID)
	})
}

func TestTracker_FindingsCreditedToTheCall(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, em := newTestTracker(s)
		defer tr.Close(context.Background())
		ctx := context.Background()
		tr.Process(ctx, Observation{Identity: ident("s1"), IssuedAt: t0, EchoAt: t0,
			Served: []Call{{ID: "c1", Name: "crm_lookup"}, {ID: "c2", Name: "weather"}, {ID: "c3", Name: "x"}}})
		tr.Process(ctx, Observation{Identity: ident("s2"), EchoAt: t0.Add(time.Second), IssuedAt: t0.Add(time.Second), Echoed: []Result{
			{ID: "c1", FindingsKnown: true, Findings: map[string]int{"pii-email": 1}},
			{ID: "c2", FindingsKnown: true, Findings: map[string]int{}, ScanSkipped: true},
			{ID: "c3"}, // not attributable (scan-once off)
		}})
		by := map[string]events.ToolCallEvent{}
		for _, e := range em.ToolCalls() {
			by[e.Data.ToolCallID] = e.Data
		}
		assert.Equal(t, map[string]int{"pii-email": 1}, by["c1"].Findings)
		assert.False(t, *by["c1"].ScanSkipped)
		assert.Equal(t, map[string]int{}, by["c2"].Findings)
		assert.True(t, *by["c2"].ScanSkipped)
		assert.Nil(t, by["c3"].Findings)
		assert.Nil(t, by["c3"].ScanSkipped)
	})
}

func TestTracker_TenantsDoNotMix(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, em := newTestTracker(s)
		defer tr.Close(context.Background())
		ctx := context.Background()
		tr.Process(ctx, Observation{Identity: ident("s1"), IssuedAt: t0, EchoAt: t0, Served: []Call{{ID: "c1", Name: "a"}}})
		other := ident("s2")
		other.TenantID = "other-tenant"
		tr.Process(ctx, Observation{Identity: other, EchoAt: t0, IssuedAt: t0, Echoed: []Result{{ID: "c1"}}})
		assert.Empty(t, em.ToolCalls())
	})
}

func TestTracker_UnnamedCallStillEmitted(t *testing.T) {
	tr, em := newTestTracker(NewMemoryStore())
	defer tr.Close(context.Background())
	ctx := context.Background()
	tr.Process(ctx, Observation{Identity: ident("s1"), IssuedAt: t0, EchoAt: t0, Served: []Call{{ID: "c1"}}})
	tr.Process(ctx, Observation{Identity: ident("s2"), IssuedAt: t0, EchoAt: t0, Echoed: []Result{{ID: "c1"}}})
	require.Len(t, em.ToolCalls(), 1)
	assert.Equal(t, UnknownToolName, em.ToolCalls()[0].Data.ToolName)
}

// blockingStore blocks every call until released, to fill the queue.
type blockingStore struct {
	*MemoryStore
	release chan struct{}
}

func (b *blockingStore) Open(ctx context.Context, tenant string, c []Pending, d time.Time) ([]OpenResult, error) {
	<-b.release
	return b.MemoryStore.Open(ctx, tenant, c, d)
}

func TestTracker_ObserveNeverBlocks(t *testing.T) {
	bs := &blockingStore{MemoryStore: NewMemoryStore(), release: make(chan struct{})}
	var dropped atomic.Int64
	tr := New(bs, &events.MemoryEmitter{}, logrus.New(), Config{QueueSize: 2, Workers: 1, SweepInterval: time.Hour},
		Hooks{Dropped: func(r string, n int) {
			if r == "queue_full" {
				dropped.Add(int64(n))
			}
		}})
	start := time.Now()
	for i := 0; i < 50; i++ {
		tr.Observe(Observation{Identity: ident("s"), IssuedAt: t0, Served: []Call{{ID: "c", Name: "a"}}})
	}
	assert.Less(t, time.Since(start), 100*time.Millisecond, "Observe must not wait on the store")
	assert.GreaterOrEqual(t, dropped.Load(), int64(40))
	close(bs.release)
	tr.Close(context.Background())
}

func TestTracker_ObserveIsAsyncAndDrains(t *testing.T) {
	tr := New(NewMemoryStore(), &events.MemoryEmitter{}, logrus.New(), Config{SweepInterval: time.Hour}, Hooks{})
	em := tr.pub.(*events.MemoryEmitter)
	tr.Observe(Observation{Identity: ident("s1"), IssuedAt: t0, EchoAt: t0, Served: []Call{{ID: "c1", Name: "a"}}})
	time.Sleep(50 * time.Millisecond) // the open must land before the echo for an ok
	tr.Observe(Observation{Identity: ident("s2"), IssuedAt: t0, EchoAt: t0.Add(time.Second), Echoed: []Result{{ID: "c1"}}})
	tr.Close(context.Background())
	require.Len(t, em.ToolCalls(), 1)
	assert.Equal(t, "ok", em.ToolCalls()[0].Data.Outcome)
}

func TestCatalogueDue_OncePerToolsetPerHour(t *testing.T) {
	tr := New(NewMemoryStore(), nil, nil, Config{SweepInterval: time.Hour}, Hooks{})
	defer tr.Close(context.Background())
	tools := []events.OfferedTool{{Name: "draw", SchemaBytes: 10}, {Name: "erase", SchemaBytes: 12}}
	assert.True(t, tr.CatalogueDue("t1", tools, t0))
	assert.False(t, tr.CatalogueDue("t1", tools, t0.Add(10*time.Minute)))
	assert.True(t, tr.CatalogueDue("t2", tools, t0), "per tenant")
	assert.True(t, tr.CatalogueDue("t1", tools[:1], t0), "a different toolset")
	assert.True(t, tr.CatalogueDue("t1", tools, t0.Add(time.Hour)), "next hour")
	var nilT *Tracker
	assert.False(t, nilT.CatalogueDue("t1", tools, t0))
}

func TestSplitMember(t *testing.T) {
	ten, id, ok := splitMember("tenant|call|with|pipes")
	assert.True(t, ok)
	assert.Equal(t, "tenant", ten)
	assert.Equal(t, "call|with|pipes", id)
	_, _, ok = splitMember("nopipe")
	assert.False(t, ok)
	assert.True(t, strings.HasPrefix(pendPrefix, "{aiqg:tc}"), "one hash tag for every key")
}
