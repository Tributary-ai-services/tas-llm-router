// Package linkage implements the deterministic `linked` tier of the AIQG
// agent-flow attribution ladder (docs/AIQG-AGENT-FLOW-ATTRIBUTION.md §A).
//
// The protocol echoes our own output back at us: when a response we serve
// contains tool_calls, the vendor mints tool_call_ids, and the agent's next
// request MUST include role=tool messages carrying those exact ids. So we
// index every served tool_call_id → the (flow, step) that produced it; when
// an id reappears on a later request, that request is *provably* the next
// step of the same flow, and the indexed step is its parent. This
// reconstructs the ReAct loop with zero client cooperation and zero
// ambiguity — no headers, no tagging.
//
// This is the FTO-clear, deterministic tier. The probabilistic
// `fingerprinted` tier (toolset hashing, template mining, content-propagation
// graph) is held pending FTO clearance and is NOT implemented here.
package linkage

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Link is the flow + step a request links back to: the step whose served
// response carried the tool_call_id(s) the request echoes.
type Link struct {
	FlowID string `json:"flow_id"`
	StepID string `json:"step_id"`
}

// Store indexes served tool_call_ids and resolves echoed ones back to their
// originating (flow, step). Implementations are safe for concurrent use.
type Store interface {
	// IndexToolCalls records that this (flow, step) served these tool_call_ids.
	IndexToolCalls(ctx context.Context, tenantID string, toolCallIDs []string, link Link) error
	// ResolveToolCalls returns the linked (flow, step) for the first of these
	// echoed tool_call_ids that we previously served, or ok=false if none match.
	ResolveToolCalls(ctx context.Context, tenantID string, toolCallIDs []string) (link Link, ok bool, err error)
}

// DefaultTTL bounds how long a served tool_call_id stays linkable. A tool
// result echoes back in seconds-to-minutes; an hour is generous headroom
// while keeping the index small and privacy-bounded.
const DefaultTTL = time.Hour

func redisKey(tenantID, id string) string { return "aiqg:tcid:" + tenantID + ":" + id }

// RedisStore is the production Store. Keys are short-TTL so the index
// self-prunes; tenant_id is part of the key so flows never cross tenants.
type RedisStore struct {
	R   redis.UniversalClient
	TTL time.Duration
}

// NewRedisStore wraps a go-redis client; ttl<=0 uses DefaultTTL.
func NewRedisStore(r redis.UniversalClient, ttl time.Duration) *RedisStore {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &RedisStore{R: r, TTL: ttl}
}

func (s *RedisStore) IndexToolCalls(ctx context.Context, tenantID string, toolCallIDs []string, link Link) error {
	if s == nil || s.R == nil || len(toolCallIDs) == 0 {
		return nil
	}
	body, err := json.Marshal(link)
	if err != nil {
		return err
	}
	pipe := s.R.Pipeline()
	for _, id := range toolCallIDs {
		if id == "" {
			continue
		}
		pipe.Set(ctx, redisKey(tenantID, id), body, s.TTL)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *RedisStore) ResolveToolCalls(ctx context.Context, tenantID string, toolCallIDs []string) (Link, bool, error) {
	if s == nil || s.R == nil {
		return Link{}, false, nil
	}
	for _, id := range toolCallIDs {
		if id == "" {
			continue
		}
		v, err := s.R.Get(ctx, redisKey(tenantID, id)).Bytes()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			return Link{}, false, err
		}
		var l Link
		if json.Unmarshal(v, &l) == nil && (l.FlowID != "" || l.StepID != "") {
			return l, true, nil
		}
	}
	return Link{}, false, nil
}

// MemoryStore is an in-process Store for tests and as a Redis-less fallback
// (linkage degrades to single-process when Redis isn't configured). Unlike
// RedisStore it doesn't TTL-evict; callers that need bounded memory in a
// long-lived process should prefer RedisStore.
type MemoryStore struct {
	mu sync.Mutex
	m  map[string]Link
}

// NewMemoryStore returns an empty in-process store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{m: make(map[string]Link)} }

func (s *MemoryStore) IndexToolCalls(_ context.Context, tenantID string, toolCallIDs []string, link Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range toolCallIDs {
		if id != "" {
			s.m[redisKey(tenantID, id)] = link
		}
	}
	return nil
}

func (s *MemoryStore) ResolveToolCalls(_ context.Context, tenantID string, toolCallIDs []string) (Link, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range toolCallIDs {
		if id == "" {
			continue
		}
		if l, ok := s.m[redisKey(tenantID, id)]; ok {
			return l, true, nil
		}
	}
	return Link{}, false, nil
}

// Ensure both satisfy Store.
var (
	_ Store = (*RedisStore)(nil)
	_ Store = (*MemoryStore)(nil)
)
