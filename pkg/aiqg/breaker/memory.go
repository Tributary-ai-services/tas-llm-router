package breaker

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryStore is a single-process Store.
//
// It is the correct choice for tests and for a single-replica deployment, and
// the WRONG choice for a multi-replica one: each replica would learn about a
// failing provider independently, and a recovering provider would receive one
// half-open probe per replica rather than one in total. Server wiring prefers
// RedisStore whenever Redis is configured and only falls back here, logging
// the downgrade, so the weaker guarantee is never silently in force.
type MemoryStore struct {
	mu      sync.Mutex
	targets map[string]*memState
	// now is injectable so tests can advance time without sleeping. Real
	// wall-clock sleeps in a breaker test make the suite slow and flaky, and
	// the windows here are tens of seconds.
	now func() time.Time
}

type memState struct {
	consecutive  int
	events       []memEvent // rolling window, pruned on access
	retries      []time.Time
	state        State
	reason       string
	since        time.Time
	ejectedUntil time.Time
	// probeInFlight guards the half-open probe so only one is admitted.
	probeInFlight bool
}

type memEvent struct {
	at    time.Time
	isErr bool
}

// NewMemoryStore returns an in-process Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{targets: map[string]*memState{}, now: time.Now}
}

// SetClock replaces the time source. Test-only.
func (m *MemoryStore) SetClock(f func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = f
}

func (m *MemoryStore) get(target string) *memState {
	s, ok := m.targets[target]
	if !ok {
		s = &memState{state: Closed, since: m.now()}
		m.targets[target] = s
	}
	return s
}

// prune drops events outside the window so rate accounting stays rolling.
func (s *memState) prune(now time.Time, window time.Duration) {
	cut := now.Add(-window)
	keep := s.events[:0]
	for _, e := range s.events {
		if e.at.After(cut) {
			keep = append(keep, e)
		}
	}
	s.events = keep
	rkeep := s.retries[:0]
	for _, t := range s.retries {
		if t.After(cut) {
			rkeep = append(rkeep, t)
		}
	}
	s.retries = rkeep
}

func (m *MemoryStore) Admit(_ context.Context, target string, cfg Config) (bool, State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	s := m.get(target)

	switch s.state {
	case Closed:
		return true, Closed, nil
	case HalfOpen:
		// Already probing. Anyone arriving while that probe is outstanding
		// must wait — this branch is the whole point of half-open, and
		// falling through to "not Open, so allow" would admit the stampede
		// the state exists to prevent.
		if s.probeInFlight {
			return false, Open, nil
		}
		s.probeInFlight = true
		return true, HalfOpen, nil
	}

	// Open.
	if now.Before(s.ejectedUntil) {
		return false, Open, nil
	}
	// Ejection elapsed. Admit exactly one probe; everyone else keeps waiting
	// so a recovering provider sees one request, not a stampede.
	if s.probeInFlight {
		return false, Open, nil
	}
	s.probeInFlight = true
	s.state = HalfOpen
	s.reason = "ejection elapsed; probing with a single request"
	s.since = now
	return true, HalfOpen, nil
}

func (m *MemoryStore) Record(_ context.Context, target string, outcome Outcome, cfg Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	s := m.get(target)
	s.prune(now, cfg.Window)

	// A half-open probe is decisive on its own: one success restores, one
	// failure re-ejects. Counting it into the window instead would let a
	// probe success sit alongside stale errors and immediately re-trip.
	if s.state == HalfOpen {
		s.probeInFlight = false
		if outcome.countsTowardEjection() {
			s.state = Open
			s.reason = "half-open probe failed; ejected again"
			s.since = now
			s.ejectedUntil = now.Add(cfg.EjectFor)
			return nil
		}
		s.state = Closed
		s.reason = "half-open probe succeeded; restored"
		s.since = now
		s.consecutive = 0
		s.events = nil
		return nil
	}

	isErr := outcome.countsTowardEjection()
	// Rate-limited and client-error attempts are recorded as neither success
	// nor error: they are not evidence about the provider in either
	// direction, so letting them dilute the error rate would be as wrong as
	// counting them as failures.
	if outcome == Success || isErr {
		s.events = append(s.events, memEvent{at: now, isErr: isErr})
	}
	if isErr {
		s.consecutive++
	} else if outcome == Success {
		s.consecutive = 0
	}

	if s.state == Open {
		return nil
	}
	if reason, trip := shouldTrip(s.consecutive, countErrs(s.events), len(s.events), cfg); trip {
		s.state = Open
		s.reason = reason
		s.since = now
		s.ejectedUntil = now.Add(cfg.EjectFor)
	}
	return nil
}

func countErrs(evts []memEvent) int {
	n := 0
	for _, e := range evts {
		if e.isErr {
			n++
		}
	}
	return n
}

// shouldTrip holds the ejection rule for BOTH stores so the two
// implementations cannot disagree about when a target is ejected — the same
// class of drift the shared matcher module exists to prevent.
func shouldTrip(consecutive, errs, total int, cfg Config) (string, bool) {
	if cfg.ConsecutiveErrors > 0 && consecutive >= cfg.ConsecutiveErrors {
		return fmt.Sprintf("%d consecutive server errors", consecutive), true
	}
	if cfg.MinRequests > 0 && total >= cfg.MinRequests && cfg.ErrorRatePercent > 0 {
		rate := errs * 100 / total
		if rate >= cfg.ErrorRatePercent {
			return fmt.Sprintf("error rate %d%% over %d requests", rate, total), true
		}
	}
	return "", false
}

func (m *MemoryStore) AllowRetry(_ context.Context, target string, cfg Config) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	s := m.get(target)
	s.prune(now, cfg.Window)

	budget := int(float64(len(s.events)) * cfg.RetryRatio)
	if budget < cfg.MinRetries {
		budget = cfg.MinRetries
	}
	if len(s.retries) >= budget {
		return false, nil
	}
	s.retries = append(s.retries, now)
	return true, nil
}

func (m *MemoryStore) Status(_ context.Context, target string, cfg Config) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	s := m.get(target)
	s.prune(now, cfg.Window)
	st := Status{
		Target: target,
		State:  s.state,
		Reason: s.reason,
		Since:  s.since,
		Errors: countErrs(s.events),
		Total:  len(s.events),
	}
	if s.state == Open {
		st.EjectedUntil = s.ejectedUntil
	}
	return st, nil
}

func (m *MemoryStore) Targets(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.targets))
	for t := range m.targets {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}
