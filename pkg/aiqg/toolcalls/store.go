// Package toolcalls emits the gateway's com.tas.aiqg.tool_call.v1 events
// (Plan #22 T.5, design §9.5).
//
// The gateway sees a tool call twice: when a response carries it
// (`tool_calls`, or Anthropic `tool_use`) and when a later request echoes its
// result (`role: tool` / `tool_result` with the same tool_call_id). The
// writer (tas-spark-jobs tool_calls.py) treats two gateway sightings of one
// (tenant_id, tool_call_id) as a REUSED id and refuses to merge them, so the
// gateway must emit each call exactly once, with its final outcome.
//
// Hence a pending-call store: a call is OPENED when the response that made it
// completes, and CLOSED exactly once, by whichever comes first:
//
//   - its result is echoed            -> outcome ok, round_trip_ms set;
//   - a sibling's result is echoed but its own is not
//     (the next turn of the flow came back without it) -> no_result;
//   - the flow closes: the call has been pending for ResultTimeout (the
//     linkage index's lifetime, so a result that arrives later could not be
//     linked to its flow anyway) -> no_result.
//
// "Close" is an atomic remove in Redis (ZREM on the due set returns 1 to one
// caller only), so with several gateway replicas, a sweeper and an echo
// racing, exactly one of them emits the call.
//
// One race is handled explicitly: the echoing request can reach another
// replica before this replica's worker has opened the call (the open runs
// asynchronously, off the request path). An echo that finds nothing pending
// leaves a short-lived tombstone; the open that follows finds it and closes
// the call as ok immediately.
package toolcalls

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Identity is who made the call: the identity fields of the response event
// whose response carried it (contract §2.2).
type Identity struct {
	TenantID  string `json:"tenant_id"`
	TokenID   string `json:"token_id,omitempty"`
	OwnerSub  string `json:"owner_sub,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	SourceApp string `json:"source_app,omitempty"`
	FlowID    string `json:"flow_id,omitempty"`
	StepID    string `json:"step_id,omitempty"`
}

// Pending is an open call: made by a response, result not yet seen.
type Pending struct {
	Identity
	ToolCallID string    `json:"id"`
	ToolName   string    `json:"name"`
	IssuedAt   time.Time `json:"at"`
	// Siblings are the other ids the same response served (parallel calls).
	Siblings []string `json:"sib,omitempty"`
}

// OpenResult reports, per opened call, what the open found.
type OpenResult struct {
	// EchoedAt is set when an echo of this id arrived BEFORE the open (the
	// cross-replica race). The call is then already closed: emit it as ok.
	EchoedAt *time.Time
	// Displaced is a previous pending call with the same (tenant, id) that
	// this open replaced. Its result never came back before the id was
	// minted again: emit it as no_result.
	Displaced *Pending
}

// Store holds pending calls. Every method is safe for concurrent use and
// every close is exactly-once across all callers sharing the store.
type Store interface {
	// Open records calls as pending until deadline.
	Open(ctx context.Context, tenantID string, calls []Pending, deadline time.Time) ([]OpenResult, error)
	// Echo closes the pending calls whose ids this request echoed and returns
	// them (nil where nothing was pending). For an id with nothing pending it
	// leaves a tombstone at echoAt, for an Open that has not happened yet.
	Echo(ctx context.Context, tenantID string, ids []string, echoAt time.Time) ([]*Pending, error)
	// Claim closes pending calls without leaving tombstones (siblings that
	// were not echoed). Nil where nothing was pending.
	Claim(ctx context.Context, tenantID string, ids []string) ([]*Pending, error)
	// Expired closes up to limit calls whose deadline is before now.
	Expired(ctx context.Context, now time.Time, limit int) ([]Pending, error)
}

// TombstoneTTL bounds how long an unmatched echo waits for its Open. The
// open is normally milliseconds behind; a minute is generous.
const TombstoneTTL = time.Minute

// All keys share one hash tag so the scripts' keys land in one Redis Cluster
// slot. The due set is global (one sweeper scan finds every tenant's
// expired calls); members are "<tenant>|<tool_call_id>".
const (
	dueKey     = "{aiqg:tc}:due"
	pendPrefix = "{aiqg:tc}:p:"
	tombPrefix = "{aiqg:tc}:t:"
)

func member(tenantID, id string) string { return tenantID + "|" + id }

func splitMember(m string) (tenantID, id string, ok bool) {
	i := strings.IndexByte(m, '|')
	if i < 0 {
		return "", "", false
	}
	return m[:i], m[i+1:], true
}

// openScript: KEYS = [due, p1..pn, t1..tn]; ARGV = [n, score, ttlMs, m1..mn, v1..vn].
// Per call: a tombstone means the echo came first -> consume it and do not
// open ("E" + echo ms). Otherwise displace any pending call with the same id
// ("O" + old value) and open the new one.
var openScript = redis.NewScript(`
local n = tonumber(ARGV[1])
local out = {}
for i = 1, n do
  local p = KEYS[1 + i]
  local t = KEYS[1 + n + i]
  local m = ARGV[3 + i]
  local v = ARGV[3 + n + i]
  local tomb = redis.call('GET', t)
  if tomb then
    redis.call('DEL', t)
    out[i] = 'E' .. tomb
  else
    local old = ''
    if redis.call('ZREM', KEYS[1], m) == 1 then
      old = redis.call('GET', p) or ''
    end
    redis.call('SET', p, v, 'PX', ARGV[3])
    redis.call('ZADD', KEYS[1], ARGV[2], m)
    if old ~= '' then out[i] = 'O' .. old else out[i] = '' end
  end
end
return out
`)

// closeScript: KEYS = [due, p1..pn, t1..tn]; ARGV = [n, tomb (0|1), echoMs, tombTtlMs, m1..mn].
// Per id: remove from the due set; the caller whose ZREM returned 1 owns the
// close and gets the value. With tomb=1 an id with nothing pending gets a
// tombstone (SET NX: the first echo's time wins).
var closeScript = redis.NewScript(`
local n = tonumber(ARGV[1])
local out = {}
for i = 1, n do
  local p = KEYS[1 + i]
  local t = KEYS[1 + n + i]
  local m = ARGV[4 + i]
  if redis.call('ZREM', KEYS[1], m) == 1 then
    out[i] = 'C' .. (redis.call('GET', p) or '')
    redis.call('DEL', p)
  else
    if ARGV[2] == '1' then
      redis.call('SET', t, ARGV[3], 'NX', 'PX', ARGV[4])
    end
    out[i] = ''
  end
end
return out
`)

// RedisStore is the production Store, shared by every gateway replica.
type RedisStore struct {
	R redis.UniversalClient
}

// NewRedisStore wraps a go-redis client.
func NewRedisStore(r redis.UniversalClient) *RedisStore { return &RedisStore{R: r} }

func (s *RedisStore) keys(tenantID string, ids []string) []string {
	k := make([]string, 0, 1+2*len(ids))
	k = append(k, dueKey)
	for _, id := range ids {
		k = append(k, pendPrefix+member(tenantID, id))
	}
	for _, id := range ids {
		k = append(k, tombPrefix+member(tenantID, id))
	}
	return k
}

func (s *RedisStore) Open(ctx context.Context, tenantID string, calls []Pending, deadline time.Time) ([]OpenResult, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	ids := make([]string, len(calls))
	members := make([]interface{}, len(calls))
	vals := make([]interface{}, len(calls))
	for i, c := range calls {
		ids[i] = c.ToolCallID
		members[i] = member(tenantID, c.ToolCallID)
		b, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		vals[i] = string(b)
	}
	// The value outlives the deadline so the sweeper can still read it.
	ttl := time.Until(deadline) + TombstoneTTL
	if ttl < TombstoneTTL {
		ttl = TombstoneTTL
	}
	argv := []interface{}{len(calls), deadline.UnixMilli(), ttl.Milliseconds()}
	argv = append(argv, members...)
	argv = append(argv, vals...)
	raw, err := openScript.Run(ctx, s.R, s.keys(tenantID, ids), argv...).StringSlice()
	if err != nil {
		return nil, err
	}
	out := make([]OpenResult, len(calls))
	for i := range calls {
		if i >= len(raw) || raw[i] == "" {
			continue
		}
		switch raw[i][0] {
		case 'E':
			if ms, err := strconv.ParseInt(raw[i][1:], 10, 64); err == nil {
				t := time.UnixMilli(ms)
				out[i].EchoedAt = &t
			}
		case 'O':
			var p Pending
			if json.Unmarshal([]byte(raw[i][1:]), &p) == nil {
				out[i].Displaced = &p
			}
		}
	}
	return out, nil
}

func (s *RedisStore) close(ctx context.Context, tenantID string, ids []string, tomb bool, echoAt time.Time) ([]*Pending, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	t := "0"
	if tomb {
		t = "1"
	}
	argv := []interface{}{len(ids), t, echoAt.UnixMilli(), TombstoneTTL.Milliseconds()}
	for _, id := range ids {
		argv = append(argv, member(tenantID, id))
	}
	raw, err := closeScript.Run(ctx, s.R, s.keys(tenantID, ids), argv...).StringSlice()
	if err != nil {
		return nil, err
	}
	out := make([]*Pending, len(ids))
	for i := range ids {
		if i >= len(raw) || len(raw[i]) < 2 || raw[i][0] != 'C' {
			continue // nothing pending, or closed but the value had expired
		}
		var p Pending
		if json.Unmarshal([]byte(raw[i][1:]), &p) == nil {
			out[i] = &p
		}
	}
	return out, nil
}

func (s *RedisStore) Echo(ctx context.Context, tenantID string, ids []string, echoAt time.Time) ([]*Pending, error) {
	return s.close(ctx, tenantID, ids, true, echoAt)
}

func (s *RedisStore) Claim(ctx context.Context, tenantID string, ids []string) ([]*Pending, error) {
	return s.close(ctx, tenantID, ids, false, time.Time{})
}

func (s *RedisStore) Expired(ctx context.Context, now time.Time, limit int) ([]Pending, error) {
	ms, err := s.R.ZRangeByScore(ctx, dueKey, &redis.ZRangeBy{
		Min: "-inf", Max: strconv.FormatInt(now.UnixMilli(), 10), Count: int64(limit),
	}).Result()
	if err != nil || len(ms) == 0 {
		return nil, err
	}
	// Group by tenant: close() takes one tenant per call.
	byTenant := map[string][]string{}
	var order []string
	for _, m := range ms {
		tenantID, id, ok := splitMember(m)
		if !ok {
			s.R.ZRem(ctx, dueKey, m)
			continue
		}
		if _, seen := byTenant[tenantID]; !seen {
			order = append(order, tenantID)
		}
		byTenant[tenantID] = append(byTenant[tenantID], id)
	}
	var out []Pending
	for _, tenantID := range order {
		got, err := s.close(ctx, tenantID, byTenant[tenantID], false, time.Time{})
		if err != nil {
			return out, err
		}
		for _, p := range got {
			if p != nil {
				out = append(out, *p)
			}
		}
	}
	return out, nil
}

// MemoryStore is an in-process Store for tests. It is NOT used in
// production: with more than one replica, an echo landing on another pod
// would never find its call, and the call would be swept as no_result.
type MemoryStore struct {
	mu   sync.Mutex
	pend map[string]memEntry
	tomb map[string]memTomb
	now  func() time.Time
}

type memEntry struct {
	p        Pending
	deadline time.Time
}

type memTomb struct {
	at, expires time.Time
}

// NewMemoryStore returns an empty in-process store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{pend: map[string]memEntry{}, tomb: map[string]memTomb{}, now: time.Now}
}

func (s *MemoryStore) Open(_ context.Context, tenantID string, calls []Pending, deadline time.Time) ([]OpenResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]OpenResult, len(calls))
	for i, c := range calls {
		k := member(tenantID, c.ToolCallID)
		if t, ok := s.tomb[k]; ok {
			delete(s.tomb, k)
			if s.now().Before(t.expires) {
				at := t.at
				out[i].EchoedAt = &at
				continue
			}
		}
		if old, ok := s.pend[k]; ok {
			o := old.p
			out[i].Displaced = &o
		}
		s.pend[k] = memEntry{p: c, deadline: deadline}
	}
	return out, nil
}

func (s *MemoryStore) close(tenantID string, ids []string, tomb bool, echoAt time.Time) []*Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Pending, len(ids))
	for i, id := range ids {
		k := member(tenantID, id)
		if e, ok := s.pend[k]; ok {
			delete(s.pend, k)
			p := e.p
			out[i] = &p
			continue
		}
		if tomb {
			if _, exists := s.tomb[k]; !exists {
				s.tomb[k] = memTomb{at: echoAt, expires: s.now().Add(TombstoneTTL)}
			}
		}
	}
	return out
}

func (s *MemoryStore) Echo(_ context.Context, tenantID string, ids []string, echoAt time.Time) ([]*Pending, error) {
	return s.close(tenantID, ids, true, echoAt), nil
}

func (s *MemoryStore) Claim(_ context.Context, tenantID string, ids []string) ([]*Pending, error) {
	return s.close(tenantID, ids, false, time.Time{}), nil
}

func (s *MemoryStore) Expired(_ context.Context, now time.Time, limit int) ([]Pending, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Pending
	for k, e := range s.pend {
		if len(out) >= limit {
			break
		}
		if e.deadline.Before(now) {
			delete(s.pend, k)
			out = append(out, e.p)
		}
	}
	return out, nil
}

// Pending reports how many calls are open (tests).
func (s *MemoryStore) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pend)
}

var (
	_ Store = (*RedisStore)(nil)
	_ Store = (*MemoryStore)(nil)
)
