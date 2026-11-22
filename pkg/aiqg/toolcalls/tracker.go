package toolcalls

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
)

// MaxIDsPerObservation caps the tool results and the tool calls one request
// contributes. The pending-call store runs each batch as one Lua script on
// the shared AIQG Redis (linkage, C1 cache, breaker, affinity), and a 10 MB
// body holds ~150k tool messages: unbounded, one request could stall that
// Redis for hundreds of milliseconds. Real turns carry a handful of parallel
// calls. Overflow is counted as dropped, reason "too_many_ids".
const MaxIDsPerObservation = 128

// Drop reasons, the values of aiqg_tool_call_events_dropped_total{reason}.
const (
	DropQueueFull    = "queue_full"
	DropStoreError   = "store_error"
	DropPublishError = "publish_error"
	DropTooManyIDs   = "too_many_ids"
	DropTooManyTools = "too_many_tools"
	DropExpired      = "expired"
	DropShutdown     = "shutdown"
	DropPanic        = "panic"
	// DropSuppressed: a call reached its result timeout after echo work for
	// its tenant had been lost. Its result may have been the lost one, so it
	// is not reported as no_result.
	DropSuppressed = "suppressed_after_loss"
)

// Call is one tool call a response made.
type Call struct {
	ID   string
	Name string
}

// Result is one tool result a request carried, as first seen: the tool
// messages after the request's last assistant message. Older tool messages
// are history the client re-sends every turn; their calls were closed on the
// turn that first carried them.
type Result struct {
	ID    string
	Bytes int
	// FindingsKnown is true when the inbound scan attributed findings to this
	// message (per-message scan-once path). Findings are the countable ones.
	FindingsKnown bool
	Findings      map[string]int
	// ScanSkipped is set only when FindingsKnown: scan-once remembered the
	// content as clean and did not scan it.
	ScanSkipped bool
}

// Observation is everything one served request tells the tracker.
type Observation struct {
	// Identity of the response event: it made Served, so a call's identity
	// is its maker's, which is what the response event carries.
	Identity Identity
	// EchoAt is when this request arrived (round_trip_ms ends here).
	EchoAt time.Time
	// IssuedAt is when this response completed (round_trip_ms starts here).
	IssuedAt time.Time
	Echoed   []Result
	Served   []Call
	// Overflow is how many tool results or calls the extractor did not
	// collect because the request was over MaxIDsPerObservation.
	Overflow int
}

// Hooks observe the tracker for metrics. Any field may be nil.
type Hooks struct {
	Emitted func(outcome string)
	Dropped func(reason string, n int)
}

// Config configures a Tracker.
type Config struct {
	// ResultTimeout is how long a call may wait for its result before the
	// flow counts as closed and the call is emitted as no_result.
	ResultTimeout time.Duration
	// SweepInterval is how often expired calls are swept. Default 30s.
	SweepInterval time.Duration
	// QueueSize bounds the observation queue. A full queue drops, never
	// blocks the request. Default 4096.
	QueueSize int
	// Workers drain the queue. Default 2.
	Workers int
}

// DefaultResultTimeout matches the linkage index's lifetime
// (pkg/aiqg/linkage.DefaultTTL): past it, the echo of an id no longer links
// its request into the flow, so the flow has closed for the gateway.
const DefaultResultTimeout = time.Hour

// lossFlushInterval is how often recorded echo losses reach the shared store.
const lossFlushInterval = time.Second

// Tracker turns observations into exactly one tool_call event per call. All
// store and publish work runs on its own goroutines: Observe never blocks.
type Tracker struct {
	store Store
	pub   events.ToolCallPublisher
	log   *logrus.Logger
	cfg   Config
	hooks Hooks
	now   func() time.Time

	queue chan Observation
	stop  chan struct{}
	wg    sync.WaitGroup
	once  sync.Once
	// closed is set when Close gives up waiting. From then on nothing is
	// published: the caller is about to close the producer underneath us.
	closed atomic.Bool

	catMu   sync.Mutex
	catHour int64
	catSeen map[string]struct{}

	// lost holds, per tenant, the latest time echo work was lost and not yet
	// written to the store. Kept in memory so recording a loss never does
	// I/O on the request path (queue_full happens there).
	lossMu sync.Mutex
	lost   map[string]time.Time
}

// New returns a started tracker. Close it on shutdown.
func New(store Store, pub events.ToolCallPublisher, log *logrus.Logger, cfg Config, hooks Hooks) *Tracker {
	if cfg.ResultTimeout <= 0 {
		cfg.ResultTimeout = DefaultResultTimeout
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = 30 * time.Second
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 4096
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if log == nil {
		log = logrus.New()
	}
	t := &Tracker{
		store: store, pub: pub, log: log, cfg: cfg, hooks: hooks, now: time.Now,
		queue:   make(chan Observation, cfg.QueueSize),
		stop:    make(chan struct{}),
		catSeen: map[string]struct{}{},
		lost:    map[string]time.Time{},
	}
	for i := 0; i < cfg.Workers; i++ {
		t.wg.Add(1)
		go t.work()
	}
	t.wg.Add(1)
	go t.sweepLoop()
	return t
}

// Observe queues one request's tool activity. Never blocks: a full queue
// drops the observation and counts it.
func (t *Tracker) Observe(o Observation) {
	if t == nil || o.Identity.TenantID == "" {
		return
	}
	o = capObservation(t, o)
	if len(o.Echoed) == 0 && len(o.Served) == 0 {
		return
	}
	select {
	case <-t.stop:
		t.lose(o, DropShutdown)
		return
	default:
	}
	select {
	case t.queue <- o:
	default:
		t.lose(o, DropQueueFull)
	}
}

// capObservation enforces MaxIDsPerObservation and counts the overflow.
func capObservation(t *Tracker, o Observation) Observation {
	over := o.Overflow
	o.Overflow = 0
	if n := len(o.Echoed); n > MaxIDsPerObservation {
		over += n - MaxIDsPerObservation
		o.Echoed = o.Echoed[:MaxIDsPerObservation]
	}
	if n := len(o.Served); n > MaxIDsPerObservation {
		over += n - MaxIDsPerObservation
		o.Served = o.Served[:MaxIDsPerObservation]
	}
	if over > 0 {
		t.dropN(DropTooManyIDs, over)
	}
	return o
}

// lose counts a dropped observation and, when it carried echoes, records
// the loss so the sweeper does not report their calls as no_result.
func (t *Tracker) lose(o Observation, reason string) {
	t.drop(reason)
	if len(o.Echoed) > 0 {
		t.noteLoss(o.Identity.TenantID, o.EchoAt)
	}
}

func (t *Tracker) noteLoss(tenantID string, at time.Time) {
	if at.IsZero() {
		at = t.now()
	}
	t.lossMu.Lock()
	defer t.lossMu.Unlock()
	if len(t.lost) >= 10000 {
		return // bounded; the flusher empties it every second
	}
	if at.After(t.lost[tenantID]) {
		t.lost[tenantID] = at
	}
}

// flushLosses writes recorded losses to the store. A failed write is kept
// for the next flush (a store outage is when losses happen).
func (t *Tracker) flushLosses(ctx context.Context) {
	t.lossMu.Lock()
	pending := t.lost
	t.lost = map[string]time.Time{}
	t.lossMu.Unlock()
	ttl := t.cfg.ResultTimeout + time.Hour
	for tenant, at := range pending {
		if err := t.store.MarkLoss(ctx, tenant, at, ttl); err != nil {
			t.noteLoss(tenant, at)
		}
	}
}

// Close stops the sweeper and drains the queue, waiting at most until ctx
// is done. After it returns nothing more is published, so the caller may
// close the producer.
func (t *Tracker) Close(ctx context.Context) {
	if t == nil {
		return
	}
	t.once.Do(func() { close(t.stop) })
	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	t.closed.Store(true)
	fctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.flushLosses(fctx)
	cancel()
}

func (t *Tracker) work() {
	defer t.wg.Done()
	for {
		select {
		case o := <-t.queue:
			t.safeProcess(o)
		case <-t.stop:
			// Drain what is already queued, then exit. Once Close has given
			// up, what is left is counted, not processed.
			for {
				select {
				case o := <-t.queue:
					if t.closed.Load() {
						t.lose(o, DropShutdown)
						continue
					}
					t.safeProcess(o)
				default:
					return
				}
			}
		}
	}
}

// safeProcess keeps a bad observation from killing a worker.
func (t *Tracker) safeProcess(o Observation) {
	defer func() {
		if r := recover(); r != nil {
			t.log.WithField("panic", r).Error("aiqg tool_call: worker recovered")
			t.lose(o, DropPanic)
		}
	}()
	t.Process(context.Background(), o)
}

func (t *Tracker) sweepLoop() {
	defer t.wg.Done()
	sweep := time.NewTicker(t.cfg.SweepInterval)
	defer sweep.Stop()
	flush := time.NewTicker(lossFlushInterval)
	defer flush.Stop()
	for {
		select {
		case <-flush.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			t.flushLosses(ctx)
			cancel()
		case <-sweep.C:
			t.Sweep(context.Background())
		case <-t.stop:
			return
		}
	}
}

// Process applies one observation synchronously. Exported for tests; the
// request path uses Observe.
//
// Order matters: echoes close calls BEFORE this response's calls open, so a
// client that re-mints an id it just answered ("call_0" every turn) opens a
// fresh call instead of displacing the one it answered.
func (t *Tracker) Process(ctx context.Context, o Observation) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	o = capObservation(t, o)
	tenant := o.Identity.TenantID

	if len(o.Echoed) > 0 {
		// One id twice in one tail is one result: dedupe, first wins.
		results := make([]Result, 0, len(o.Echoed))
		ids := make([]string, 0, len(o.Echoed))
		echoed := make(map[string]bool, len(o.Echoed))
		for _, r := range o.Echoed {
			if r.ID == "" || echoed[r.ID] {
				continue
			}
			echoed[r.ID] = true
			results = append(results, r)
			ids = append(ids, r.ID)
		}
		// An id this same response mints again must not leave a tombstone,
		// or the open below would close the NEW call as already answered.
		reminted := map[string]bool{}
		for _, c := range o.Served {
			reminted[c.ID] = true
		}
		var tombIDs, plainIDs []string
		for _, id := range ids {
			if reminted[id] {
				plainIDs = append(plainIDs, id)
			} else {
				tombIDs = append(tombIDs, id)
			}
		}
		closed, err := t.closeEchoes(ctx, tenant, ids, tombIDs, plainIDs, o.EchoAt)
		if err != nil {
			t.log.WithError(err).Warn("aiqg tool_call: echo close failed")
			t.drop(DropStoreError)
			t.noteLoss(tenant, o.EchoAt)
		}
		var missing []string
		for i, p := range closed {
			if p == nil {
				continue
			}
			if p.Expired {
				t.drop(DropExpired)
				continue
			}
			r := results[i]
			t.emit(ctx, okEvent(*p, o.EchoAt, &r))
			for _, sib := range p.Siblings {
				if !echoed[sib] {
					missing = append(missing, sib)
					echoed[sib] = true // claim each sibling once
				}
			}
		}
		// The next turn of this flow came back without these results.
		if len(missing) > 0 {
			got, err := t.store.Claim(ctx, tenant, missing)
			if err != nil {
				t.log.WithError(err).Warn("aiqg tool_call: sibling close failed")
				t.drop(DropStoreError)
			}
			t.emitNoResults(ctx, got, events.NoResultSibling)
		}
	}

	if len(o.Served) > 0 {
		calls := make([]Pending, 0, len(o.Served))
		ids := make([]string, 0, len(o.Served))
		seen := map[string]bool{}
		for _, c := range o.Served {
			if c.ID == "" || seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			ids = append(ids, c.ID)
		}
		for _, c := range o.Served {
			if c.ID == "" || !seen[c.ID] {
				continue
			}
			delete(seen, c.ID)
			p := Pending{Identity: o.Identity, ToolCallID: c.ID, ToolName: c.Name, IssuedAt: o.IssuedAt}
			for _, id := range ids {
				if id != c.ID {
					p.Siblings = append(p.Siblings, id)
				}
			}
			calls = append(calls, p)
		}
		res, err := t.store.Open(ctx, tenant, calls, o.IssuedAt.Add(t.cfg.ResultTimeout))
		if err != nil {
			t.log.WithError(err).Warn("aiqg tool_call: open failed")
			t.drop(DropStoreError)
			return
		}
		anyEchoed := false
		closedHere := map[string]bool{}
		for i, r := range res {
			if r.Displaced != nil {
				t.emit(ctx, noResultEvent(*r.Displaced, events.NoResultReminted))
			}
			if r.EchoedAt != nil {
				anyEchoed = true
				closedHere[calls[i].ToolCallID] = true
				t.emit(ctx, okEvent(calls[i], *r.EchoedAt, nil))
			}
		}
		// The echo beat the open and carried some results but not all: the
		// rest did not come back with their siblings.
		if anyEchoed {
			var rest []string
			for _, c := range calls {
				if !closedHere[c.ToolCallID] {
					rest = append(rest, c.ToolCallID)
				}
			}
			got, err := t.store.Claim(ctx, tenant, rest)
			if err != nil {
				t.drop(DropStoreError)
			}
			t.emitNoResults(ctx, got, events.NoResultSibling)
		}
	}
}

func (t *Tracker) emitNoResults(ctx context.Context, got []*Pending, basis string) {
	for _, p := range got {
		switch {
		case p == nil:
		case p.Expired:
			t.drop(DropExpired)
		default:
			t.emit(ctx, noResultEvent(*p, basis))
		}
	}
}

// closeEchoes closes ids (tombstoning tombIDs, not plainIDs) and returns
// the results aligned with ids. ids must be distinct.
func (t *Tracker) closeEchoes(ctx context.Context, tenant string, ids, tombIDs, plainIDs []string, echoAt time.Time) ([]*Pending, error) {
	byID := map[string]*Pending{}
	var firstErr error
	if len(tombIDs) > 0 {
		got, err := t.store.Echo(ctx, tenant, tombIDs, echoAt)
		if err != nil {
			firstErr = err
		}
		for i, p := range got {
			if p != nil {
				byID[tombIDs[i]] = p
			}
		}
	}
	if len(plainIDs) > 0 {
		got, err := t.store.Claim(ctx, tenant, plainIDs)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		for i, p := range got {
			if p != nil {
				byID[plainIDs[i]] = p
			}
		}
	}
	out := make([]*Pending, len(ids))
	for i, id := range ids {
		out[i] = byID[id]
	}
	return out, firstErr
}

// Sweep emits no_result for every call whose flow has closed, except calls
// whose tenant lost echo work after the call was made: their result may be
// the one that was lost, and a false no_result is worse than a missing one.
func (t *Tracker) Sweep(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	t.flushLosses(ctx) // this replica's own losses count too
	lastLoss := map[string]*time.Time{}
	lossFor := func(tenant string) *time.Time {
		if v, ok := lastLoss[tenant]; ok {
			return v
		}
		var v *time.Time
		if at, ok, err := t.store.LastLoss(ctx, tenant); err == nil && ok {
			v = &at
		} else if err != nil {
			// Cannot tell: suppress rather than risk a false no_result.
			now := t.now()
			v = &now
		}
		lastLoss[tenant] = v
		return v
	}
	for i := 0; i < 20; i++ { // bounded: at most 2,000 calls per tick
		got, err := t.store.Expired(ctx, t.now(), 100)
		if err != nil {
			t.log.WithError(err).Warn("aiqg tool_call: sweep failed")
			return
		}
		for _, p := range got {
			switch {
			case p.Expired:
				t.drop(DropExpired)
			case func() bool { l := lossFor(p.TenantID); return l != nil && !l.Before(p.IssuedAt) }():
				t.drop(DropSuppressed)
			default:
				t.emit(ctx, noResultEvent(p, events.NoResultTimeout))
			}
		}
		if len(got) < 100 {
			return
		}
	}
}

// CatalogueDue reports whether this tenant's toolset has not yet been
// reported this hour by this replica, and marks it reported. The catalogue
// rows are snapshots: once per toolset per hour is enough, and the
// writer's unique key absorbs the per-replica repeats.
func (t *Tracker) CatalogueDue(tenantID string, tools []events.OfferedTool, now time.Time) bool {
	if t == nil || tenantID == "" || len(tools) == 0 {
		return false
	}
	h := sha256.New()
	h.Write([]byte(tenantID))
	for _, tl := range tools {
		h.Write([]byte{0})
		h.Write([]byte(tl.Name))
	}
	key := hex.EncodeToString(h.Sum(nil)[:12])
	hour := now.Unix() / 3600
	t.catMu.Lock()
	defer t.catMu.Unlock()
	if hour != t.catHour || len(t.catSeen) > 50000 {
		t.catHour = hour
		t.catSeen = map[string]struct{}{}
	}
	if _, ok := t.catSeen[key]; ok {
		return false
	}
	t.catSeen[key] = struct{}{}
	return true
}

// DropOverflow counts collection overflow reported by a caller that capped
// its own extraction (e.g. a toolset over the offered-tools cap).
func (t *Tracker) DropOverflow(reason string, n int) {
	if t == nil || n <= 0 {
		return
	}
	t.dropN(reason, n)
}

func (t *Tracker) emit(ctx context.Context, ev events.ToolCallEvent) {
	if t.pub == nil {
		return
	}
	if t.closed.Load() {
		t.drop(DropShutdown)
		return
	}
	if err := t.publish(ctx, ev); err != nil {
		t.log.WithError(err).Warn("aiqg tool_call: publish failed")
		if _, panicked := err.(errPublishPanicked); panicked {
			t.drop(DropPanic)
		} else {
			t.drop(DropPublishError)
		}
		return
	}
	if t.hooks.Emitted != nil {
		t.hooks.Emitted(ev.Outcome)
	}
}

// errPublishPanicked reports a publish that panicked: sarama panics when a
// message is sent on a producer closed during shutdown.
type errPublishPanicked struct{ v interface{} }

func (e errPublishPanicked) Error() string { return "publish panicked during shutdown" }

func (t *Tracker) publish(ctx context.Context, ev events.ToolCallEvent) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errPublishPanicked{r}
		}
	}()
	return t.pub.PublishToolCall(ctx, events.NewToolCallEnvelope(ev, t.now()))
}

func (t *Tracker) drop(reason string) { t.dropN(reason, 1) }

func (t *Tracker) dropN(reason string, n int) {
	if t.hooks.Dropped != nil {
		t.hooks.Dropped(reason, n)
	}
}

func baseEvent(p Pending) events.ToolCallEvent {
	return events.ToolCallEvent{
		TenantID:   p.TenantID,
		TokenID:    p.TokenID,
		OwnerSub:   p.OwnerSub,
		UserID:     p.UserID,
		AgentID:    p.AgentID,
		SourceApp:  p.SourceApp,
		FlowID:     p.FlowID,
		StepID:     p.StepID,
		ToolCallID: p.ToolCallID,
		ToolName:   toolName(p.ToolName),
		ObservedBy: events.ObservedByGateway,
		OccurredAt: p.IssuedAt.UTC(),
	}
}

func okEvent(p Pending, echoAt time.Time, r *Result) events.ToolCallEvent {
	ev := baseEvent(p)
	ev.Outcome = events.ToolOutcomeOK
	rt := echoAt.Sub(p.IssuedAt).Milliseconds()
	if rt < 0 { // clock skew between replicas
		rt = 0
	}
	ev.RoundTripMs = &rt
	if r != nil {
		b := int64(r.Bytes)
		tok := (b + 3) / 4 // pkg/clear's 4 bytes/token
		ev.ResultBytes, ev.ResultTokensEst = &b, &tok
		if r.FindingsKnown {
			ev.Findings = map[string]int{}
			for k, v := range r.Findings {
				ev.Findings[k] = v
			}
			skipped := r.ScanSkipped
			ev.ScanSkipped = &skipped
		}
	}
	return ev
}

func noResultEvent(p Pending, basis string) events.ToolCallEvent {
	ev := baseEvent(p)
	ev.Outcome = events.ToolOutcomeNoResult
	ev.NoResultBasis = basis
	return ev
}

// UnknownToolName stands in when a stream never named its call: the writer
// drops events without a tool_name, and a call with no name is still a call.
const UnknownToolName = "(unknown)"

func toolName(n string) string {
	if n == "" {
		return UnknownToolName
	}
	return n
}
