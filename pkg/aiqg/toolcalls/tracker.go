package toolcalls

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
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
}

// Hooks observe the tracker for metrics. Any field may be nil.
type Hooks struct {
	Emitted func(outcome string)
	Dropped func(reason string)
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

	catMu   sync.Mutex
	catHour int64
	catSeen map[string]struct{}
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
	if t == nil || o.Identity.TenantID == "" || (len(o.Echoed) == 0 && len(o.Served) == 0) {
		return
	}
	select {
	case <-t.stop:
		return
	default:
	}
	select {
	case t.queue <- o:
	default:
		t.drop("queue_full")
	}
}

// Close stops the sweeper and drains the queue, waiting at most until ctx
// is done.
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
}

func (t *Tracker) work() {
	defer t.wg.Done()
	for {
		select {
		case o := <-t.queue:
			t.Process(context.Background(), o)
		case <-t.stop:
			// Drain what is already queued, then exit.
			for {
				select {
				case o := <-t.queue:
					t.Process(context.Background(), o)
				default:
					return
				}
			}
		}
	}
}

func (t *Tracker) sweepLoop() {
	defer t.wg.Done()
	tk := time.NewTicker(t.cfg.SweepInterval)
	defer tk.Stop()
	for {
		select {
		case <-tk.C:
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
	tenant := o.Identity.TenantID

	if len(o.Echoed) > 0 {
		ids := make([]string, len(o.Echoed))
		echoed := make(map[string]bool, len(o.Echoed))
		for i, r := range o.Echoed {
			ids[i] = r.ID
			echoed[r.ID] = true
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
			t.drop("store_error")
		}
		var missing []string
		for i, p := range closed {
			if p == nil {
				continue
			}
			r := o.Echoed[i]
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
				t.drop("store_error")
			}
			for _, p := range got {
				if p != nil {
					t.emit(ctx, noResultEvent(*p))
				}
			}
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
			t.drop("store_error")
			return
		}
		anyEchoed := false
		closedHere := map[string]bool{}
		for i, r := range res {
			if r.Displaced != nil {
				t.emit(ctx, noResultEvent(*r.Displaced))
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
				t.drop("store_error")
			}
			for _, p := range got {
				if p != nil {
					t.emit(ctx, noResultEvent(*p))
				}
			}
		}
	}
}

// closeEchoes closes ids (tombstoning tombIDs, not plainIDs) and returns
// the results aligned with ids.
func (t *Tracker) closeEchoes(ctx context.Context, tenant string, ids, tombIDs, plainIDs []string, echoAt time.Time) ([]*Pending, error) {
	byID := map[string]*Pending{}
	var firstErr error
	if len(tombIDs) > 0 {
		got, err := t.store.Echo(ctx, tenant, tombIDs, echoAt)
		if err != nil {
			firstErr = err
		}
		for i, p := range got {
			byID[tombIDs[i]] = p
		}
	}
	if len(plainIDs) > 0 {
		got, err := t.store.Claim(ctx, tenant, plainIDs)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		for i, p := range got {
			byID[plainIDs[i]] = p
		}
	}
	out := make([]*Pending, len(ids))
	for i, id := range ids {
		out[i] = byID[id]
	}
	return out, firstErr
}

// Sweep emits no_result for every call whose flow has closed.
func (t *Tracker) Sweep(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for i := 0; i < 20; i++ { // bounded: at most 2,000 calls per tick
		got, err := t.store.Expired(ctx, t.now(), 100)
		if err != nil {
			t.log.WithError(err).Warn("aiqg tool_call: sweep failed")
			return
		}
		for _, p := range got {
			t.emit(ctx, noResultEvent(p))
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

func (t *Tracker) emit(ctx context.Context, ev events.ToolCallEvent) {
	if t.pub == nil {
		return
	}
	if err := t.pub.PublishToolCall(ctx, events.NewToolCallEnvelope(ev, t.now())); err != nil {
		t.log.WithError(err).Warn("aiqg tool_call: publish failed")
		t.drop("publish_error")
		return
	}
	if t.hooks.Emitted != nil {
		t.hooks.Emitted(ev.Outcome)
	}
}

func (t *Tracker) drop(reason string) {
	if t.hooks.Dropped != nil {
		t.hooks.Dropped(reason)
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

func noResultEvent(p Pending) events.ToolCallEvent {
	ev := baseEvent(p)
	ev.Outcome = events.ToolOutcomeNoResult
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
