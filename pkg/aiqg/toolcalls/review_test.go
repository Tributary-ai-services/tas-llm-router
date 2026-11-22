package toolcalls

// Regression tests for the PR #284 review findings.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
)

// dropCounter records Dropped hook calls.
type dropCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func (d *dropCounter) hook(reason string, n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.n == nil {
		d.n = map[string]int{}
	}
	d.n[reason] += n
}

func (d *dropCounter) get(reason string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n[reason]
}

func trackerWithDrops(s Store, pub events.ToolCallPublisher) (*Tracker, *dropCounter) {
	dc := &dropCounter{}
	return New(s, pub, logrus.New(), Config{ResultTimeout: time.Hour, SweepInterval: time.Hour}, Hooks{Dropped: dc.hook}), dc
}

// recordingStore records the size of every Echo/Claim/Open batch.
type recordingStore struct {
	Store
	mu      sync.Mutex
	batches []int
	echoErr error
}

func (r *recordingStore) note(n int) {
	r.mu.Lock()
	r.batches = append(r.batches, n)
	r.mu.Unlock()
}

func (r *recordingStore) Echo(ctx context.Context, t string, ids []string, at time.Time) ([]*Pending, error) {
	r.note(len(ids))
	if r.echoErr != nil {
		return nil, r.echoErr
	}
	return r.Store.Echo(ctx, t, ids, at)
}

func (r *recordingStore) Claim(ctx context.Context, t string, ids []string) ([]*Pending, error) {
	r.note(len(ids))
	return r.Store.Claim(ctx, t, ids)
}

func (r *recordingStore) Open(ctx context.Context, t string, c []Pending, d time.Time) ([]OpenResult, error) {
	r.note(len(c))
	return r.Store.Open(ctx, t, c, d)
}

// Finding 1: one observation never sends more than MaxIDsPerObservation ids
// to the store, and the overflow is counted.
func TestReview1_IDsPerObservationCapped(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		rs := &recordingStore{Store: s}
		tr, dc := trackerWithDrops(rs, &events.MemoryEmitter{})
		defer tr.Close(context.Background())
		echoed := make([]Result, 20000)
		served := make([]Call, 300)
		for i := range echoed {
			echoed[i] = Result{ID: fmt.Sprintf("r%d", i)}
		}
		for i := range served {
			served[i] = Call{ID: fmt.Sprintf("c%d", i), Name: "x"}
		}
		tr.Process(context.Background(), Observation{Identity: ident("s"), IssuedAt: t0, EchoAt: t0,
			Echoed: echoed, Served: served, Overflow: 5})
		for _, n := range rs.batches {
			assert.LessOrEqual(t, n, MaxIDsPerObservation)
		}
		assert.Equal(t, (20000-MaxIDsPerObservation)+(300-MaxIDsPerObservation)+5, dc.get(DropTooManyIDs))
	})
}

func TestReview1_ObserveCapsBeforeQueueing(t *testing.T) {
	tr, dc := trackerWithDrops(NewMemoryStore(), &events.MemoryEmitter{})
	defer tr.Close(context.Background())
	echoed := make([]Result, 1000)
	for i := range echoed {
		echoed[i] = Result{ID: fmt.Sprintf("r%d", i)}
	}
	tr.Observe(Observation{Identity: ident("s"), EchoAt: t0, Echoed: echoed})
	assert.Equal(t, 1000-MaxIDsPerObservation, dc.get(DropTooManyIDs))
}

// Finding 2: every no_result carries its basis.
func TestReview2_NoResultBasis(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, em := newTestTracker(s)
		defer tr.Close(context.Background())
		ctx := context.Background()
		now := time.Now() // recent: the sweep below must not time these out
		// sibling
		tr.Process(ctx, Observation{Identity: ident("s1"), IssuedAt: now, EchoAt: now,
			Served: []Call{{ID: "a1", Name: "a"}, {ID: "a2", Name: "a"}}})
		tr.Process(ctx, Observation{Identity: ident("s2"), IssuedAt: now, EchoAt: now.Add(time.Second), Echoed: []Result{{ID: "a1"}}})
		// reminted (b1@s4 stays open)
		tr.Process(ctx, Observation{Identity: ident("s3"), IssuedAt: now, EchoAt: now, Served: []Call{{ID: "b1", Name: "b"}}})
		tr.Process(ctx, Observation{Identity: ident("s4"), IssuedAt: now.Add(time.Second), EchoAt: now, Served: []Call{{ID: "b1", Name: "b"}}})
		// timeout
		old := time.Now().Add(-2 * time.Hour)
		tr.Process(ctx, Observation{Identity: ident("s5"), IssuedAt: old, EchoAt: old, Served: []Call{{ID: "c1", Name: "c"}}})
		tr.Sweep(ctx)

		basis := map[string]string{}
		for _, e := range em.ToolCalls() {
			if e.Data.Outcome == events.ToolOutcomeNoResult {
				basis[e.Data.ToolCallID+"@"+e.Data.StepID] = e.Data.NoResultBasis
			} else {
				assert.Empty(t, e.Data.NoResultBasis)
			}
		}
		assert.Equal(t, map[string]string{"a2@s1": "sibling", "b1@s3": "reminted", "c1@s5": "timeout"}, basis)
	})
}

// Finding 2: a call whose tenant lost echo work after the call was made is
// not reported as a timeout no_result; it is counted as suppressed.
func TestReview2_TimeoutSuppressedAfterQueueFull(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, dc := trackerWithDrops(s, &events.MemoryEmitter{})
		em := tr.pub.(*events.MemoryEmitter)
		defer tr.Close(context.Background())
		ctx := context.Background()
		old := time.Now().Add(-2 * time.Hour)
		tr.Process(ctx, Observation{Identity: ident("s1"), IssuedAt: old, EchoAt: old, Served: []Call{{ID: "lost", Name: "x"}}})
		// Its echo was dropped on the request path.
		tr.lose(Observation{Identity: ident("s2"), EchoAt: old.Add(time.Second), Echoed: []Result{{ID: "lost"}}}, DropQueueFull)
		tr.Sweep(ctx)
		assert.Empty(t, em.ToolCalls(), "no false no_result")
		assert.Equal(t, 1, dc.get(DropSuppressed))
		assert.Equal(t, 1, dc.get(DropQueueFull))
	})
}

func TestReview2_TimeoutSuppressedAfterEchoStoreError(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		rs := &recordingStore{Store: s}
		tr, dc := trackerWithDrops(rs, &events.MemoryEmitter{})
		em := tr.pub.(*events.MemoryEmitter)
		defer tr.Close(context.Background())
		ctx := context.Background()
		old := time.Now().Add(-2 * time.Hour)
		tr.Process(ctx, Observation{Identity: ident("s1"), IssuedAt: old, EchoAt: old, Served: []Call{{ID: "c1", Name: "x"}}})
		rs.echoErr = errors.New("redis: i/o timeout")
		tr.Process(ctx, Observation{Identity: ident("s2"), EchoAt: old.Add(time.Second), IssuedAt: old, Echoed: []Result{{ID: "c1"}}})
		rs.echoErr = nil
		tr.Sweep(ctx)
		assert.Empty(t, em.ToolCalls())
		assert.Equal(t, 1, dc.get(DropStoreError))
		assert.Equal(t, 1, dc.get(DropSuppressed))
	})
}

// A loss BEFORE the call was made says nothing about it: still reported.
func TestReview2_EarlierLossDoesNotSuppress(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, em := newTestTracker(s)
		defer tr.Close(context.Background())
		ctx := context.Background()
		old := time.Now().Add(-2 * time.Hour)
		tr.noteLoss(ident("x").TenantID, old.Add(-time.Minute))
		tr.Process(ctx, Observation{Identity: ident("s1"), IssuedAt: old, EchoAt: old, Served: []Call{{ID: "c1", Name: "x"}}})
		tr.Sweep(ctx)
		require.Len(t, em.ToolCalls(), 1)
		assert.Equal(t, "timeout", em.ToolCalls()[0].Data.NoResultBasis)
	})
}

// Finding 3: the same id twice in one tail is one ok event, not a silent loss.
func TestReview3_DuplicateIDInTail(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, dc := trackerWithDrops(s, &events.MemoryEmitter{})
		em := tr.pub.(*events.MemoryEmitter)
		defer tr.Close(context.Background())
		ctx := context.Background()
		tr.Process(ctx, Observation{Identity: ident("s1"), IssuedAt: t0, EchoAt: t0, Served: []Call{{ID: "c1", Name: "x"}}})
		tr.Process(ctx, Observation{Identity: ident("s2"), IssuedAt: t0, EchoAt: t0.Add(time.Second),
			Echoed: []Result{{ID: "c1", Bytes: 10}, {ID: "c1", Bytes: 99}}})
		require.Len(t, em.ToolCalls(), 1)
		assert.Equal(t, "ok", em.ToolCalls()[0].Data.Outcome)
		assert.EqualValues(t, 10, *em.ToolCalls()[0].Data.ResultBytes, "first sighting wins")
		assert.Zero(t, dc.get(DropExpired))
	})
}

// Finding 4: a stale tombstone (a retry's echo of an earlier same-id call)
// does not close a NEW call as ok with round trip 0.
func TestReview4_StaleTombstoneIgnored(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, em := newTestTracker(s)
		defer tr.Close(context.Background())
		ctx := context.Background()
		now := time.Now()
		// A retried turn echoes call_0 that nothing has pending: tombstone.
		tr.Process(ctx, Observation{Identity: ident("retry"), EchoAt: now, IssuedAt: now, Echoed: []Result{{ID: "call_0"}}})
		// Five seconds later a different response mints a brand-new call_0.
		tr.Process(ctx, Observation{Identity: ident("new"), IssuedAt: now.Add(5 * time.Second), EchoAt: now.Add(4 * time.Second),
			Served: []Call{{ID: "call_0", Name: "x"}}})
		assert.Empty(t, em.ToolCalls(), "the new call is still open")
		// Its real echo closes it, with a real round trip.
		tr.Process(ctx, Observation{Identity: ident("next"), EchoAt: now.Add(8 * time.Second), IssuedAt: now.Add(9 * time.Second),
			Echoed: []Result{{ID: "call_0"}}})
		require.Len(t, em.ToolCalls(), 1)
		d := em.ToolCalls()[0].Data
		assert.Equal(t, "ok", d.Outcome)
		assert.Equal(t, "new", d.StepID)
		assert.InDelta(t, 3000, *d.RoundTripMs, 2)
	})
}

// The race the tombstone exists for still works, including a small clock
// skew where the echoing replica's clock reads slightly before IssuedAt.
func TestReview4_RaceWithinSkewStillOK(t *testing.T) {
	stores(t, func(t *testing.T, s Store) {
		tr, em := newTestTracker(s)
		defer tr.Close(context.Background())
		ctx := context.Background()
		issued := time.Now()
		tr.Process(ctx, Observation{Identity: ident("s2"), EchoAt: issued.Add(-200 * time.Millisecond), IssuedAt: issued, Echoed: []Result{{ID: "c1"}}})
		tr.Process(ctx, Observation{Identity: ident("s1"), IssuedAt: issued, EchoAt: issued, Served: []Call{{ID: "c1", Name: "x"}}})
		require.Len(t, em.ToolCalls(), 1)
		assert.Equal(t, "ok", em.ToolCalls()[0].Data.Outcome)
		assert.EqualValues(t, 0, *em.ToolCalls()[0].Data.RoundTripMs, "negative skew clamps to 0")
	})
}

// slowPub blocks its first publish until released; panics when told to,
// as sarama does on a closed producer.
type slowPub struct {
	mu       sync.Mutex
	release  chan struct{}
	first    bool
	n        int
	panicNow bool
}

func (p *slowPub) PublishToolCall(context.Context, events.ToolCallEnvelope) error {
	p.mu.Lock()
	first := !p.first
	p.first = true
	p.mu.Unlock()
	if first {
		<-p.release
	}
	p.mu.Lock()
	pn := p.panicNow
	p.mu.Unlock()
	if pn {
		panic("send on closed channel")
	}
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
	return nil
}

// Finding 5: once Close gives up waiting, nothing more is published, and a
// publish that panics (producer already closed) is recovered and counted.
func TestReview5_NoPublishAfterClose(t *testing.T) {
	pub := &slowPub{release: make(chan struct{})}
	dc := &dropCounter{}
	tr := New(NewMemoryStore(), pub, logrus.New(), Config{Workers: 1, SweepInterval: time.Hour}, Hooks{Dropped: dc.hook})
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("c%d", i)
		tr.Process(context.Background(), Observation{Identity: ident("s" + id), IssuedAt: t0, EchoAt: t0, Served: []Call{{ID: id, Name: "x"}}})
	}
	for i := 0; i < 5; i++ {
		tr.Observe(Observation{Identity: ident("e"), IssuedAt: t0, EchoAt: t0.Add(time.Second), Echoed: []Result{{ID: fmt.Sprintf("c%d", i)}}})
	}
	time.Sleep(20 * time.Millisecond) // worker is now blocked in the first publish
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	tr.Close(ctx)
	cancel()
	// The caller now closes the producer: any further publish would panic.
	pub.mu.Lock()
	pub.panicNow = true
	pub.mu.Unlock()
	close(pub.release)
	tr.wg.Wait()
	assert.Zero(t, pub.n, "nothing published after Close returned")
	assert.Equal(t, 1, dc.get(DropPanic), "the in-flight publish panicked and was recovered")
	assert.Equal(t, 4, dc.get(DropShutdown), "the rest were counted, not sent")
}

// Finding 6: a pending value that expired before its close is counted.
func TestReview6_ExpiredValueCounted(t *testing.T) {
	url := os.Getenv("TOOLCALLS_REDIS_URL")
	if url == "" {
		t.Skip("TOOLCALLS_REDIS_URL not set (needs Redis key expiry)")
	}
	opt, err := redis.ParseURL(url)
	require.NoError(t, err)
	rc := redis.NewClient(opt)
	defer rc.Close()
	ctx := context.Background()
	require.NoError(t, rc.FlushDB(ctx).Err())
	defer rc.FlushDB(ctx)

	tr, dc := trackerWithDrops(NewRedisStore(rc), &events.MemoryEmitter{})
	em := tr.pub.(*events.MemoryEmitter)
	defer tr.Close(context.Background())
	id := ident("s1")
	tr.Process(ctx, Observation{Identity: id, IssuedAt: t0, EchoAt: t0, Served: []Call{{ID: "e1", Name: "x"}, {ID: "e2", Name: "x"}}})
	// Simulate the value TTL elapsing while the due entry remains.
	require.NoError(t, rc.Del(ctx, pendPrefix+member(id.TenantID, "e1"), pendPrefix+member(id.TenantID, "e2")).Err())
	tr.Process(ctx, Observation{Identity: ident("s2"), IssuedAt: t0, EchoAt: t0.Add(time.Second), Echoed: []Result{{ID: "e1"}}})
	tr.Sweep(ctx) // e2's deadline (t0+1h) is long past
	assert.Empty(t, em.ToolCalls())
	assert.Equal(t, 2, dc.get(DropExpired))
}
