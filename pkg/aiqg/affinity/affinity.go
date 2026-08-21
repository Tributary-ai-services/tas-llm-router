// Package affinity keeps a conversation on the provider whose vendor prompt
// cache is currently warm — routing-decision.md §5.5, cache-keys-and-sessions.md
// §4, step 4.
//
// # Why this is not "session stickiness"
//
// "Stickiness" bundles three unrelated needs, and conflating them is how
// affinity implementations go wrong:
//
//	prompt-cache affinity   keyed on the prefix hash; breaking it costs a cold
//	                        cache rebuild — the thing this package protects
//	assignment affinity     keyed on conversation/user/flow; breaking it
//	                        corrupts an experiment bucket — already solved by
//	                        the experiment runner
//	conversation coherence  keyed on conversation; breaking it changes the
//	                        assistant's voice mid-thread — a product concern
//
// This package addresses only the first. It borrows the experiment runner's
// identity keys rather than inventing a second vocabulary with the same
// fallbacks and the same edge cases, because two vocabularies for one idea
// drift.
//
// # The epoch, and why topic drift is irrelevant
//
// A conversation id is stable for hours; a vendor prompt cache lives five
// minutes. Pinning a provider for the life of a conversation holds an affinity
// long after the thing it protects has expired — costing routing freedom and
// buying nothing.
//
// The tempting fix is to release affinity when the topic changes. That is
// wrong, and the reason is the crux: THE VENDOR CACHE IS KEYED ON PREFIX BYTES,
// NOT MEANING. A three-hour session covering eight topics keeps its cache the
// whole time, provided the system prompt and tool set are unchanged and
// requests stay inside the TTL. Topic drift neither warms nor cools it.
//
// So the epoch is built from the two signals that actually govern cache
// warmth:
//
//	stable_prefix_hash  system messages + tool schemas. A change means the
//	                    cache is cold regardless of routing, so affinity to the
//	                    old provider is worthless.
//	idle_bucket         advances when the gap since the previous request
//	                    exceeds the TTL. The cache has expired, so affinity
//	                    costs nothing to abandon.
//
// No embeddings, no thresholds, no inference — both signals already exist.
package affinity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// DefaultTTL matches Anthropic's default cache lifetime. Affinity should expire
// when the thing it protects expires; holding it longer trades routing freedom
// for nothing.
const DefaultTTL = 5 * time.Minute

// OnBreak selects what happens when affinity cannot be honoured.
type OnBreak string

const (
	// PreferSame tries the affine provider first and falls through silently
	// when it is unavailable. Correct for almost all traffic.
	PreferSame OnBreak = "prefer_same"
	// AllowSwitch treats affinity as a hint that selection may override for a
	// large enough improvement. For cost-sensitive batch work.
	AllowSwitch OnBreak = "allow_switch"
	// Fail refuses the request rather than switching provider.
	//
	// Deliberately narrow: it exists for a workload that must not silently
	// change model mid-conversation, where a visible error beats a subtly
	// different answer. It converts a provider blip into a failed request, so
	// it should be rare and the UI should say so.
	Fail OnBreak = "fail"
)

// Config is a rule's affinity settings. The zero value means no affinity.
type Config struct {
	// KeySource names the identity to stick on: conversation, user, flow,
	// principal, ip. Empty disables affinity.
	KeySource string
	// Scope is "vendor" or "vendor+model". vendor+model is the default and the
	// honest one — vendor prompt caches are MODEL-scoped, so affinity to a
	// vendor alone can land on a different model and rebuild the cache anyway.
	Scope string
	// TTL bounds both the stored affinity and the idle bucket. Zero uses
	// DefaultTTL.
	TTL time.Duration
	// OnBreak selects the behaviour when the affine target is unusable.
	OnBreak OnBreak
}

// Enabled reports whether affinity is configured.
func (c Config) Enabled() bool { return strings.TrimSpace(c.KeySource) != "" }

// ttl returns the effective TTL.
func (c Config) ttl() time.Duration {
	if c.TTL <= 0 {
		return DefaultTTL
	}
	return c.TTL
}

// onBreak returns the effective on-break behaviour.
func (c Config) onBreak() OnBreak {
	if c.OnBreak == "" {
		return PreferSame
	}
	return c.OnBreak
}

// Target is an affine provider/model pair.
type Target struct {
	Provider      string    `json:"provider"`
	Model         string    `json:"model,omitempty"`
	EstablishedAt time.Time `json:"established_at"`
}

// Epoch identifies a span over which the vendor cache can plausibly stay warm.
//
// Two conversations with the same id but different epochs are, for caching
// purposes, unrelated — which is why the epoch is part of the KEY rather than a
// field to compare after reading. A new epoch is a new key, not a mutation, so
// there is no window where a stale target can be read.
type Epoch struct {
	PrefixHash string
	IdleBucket int64
}

// String renders the epoch compactly for use in a Redis key.
func (e Epoch) String() string {
	h := e.PrefixHash
	if len(h) > 12 {
		// The full hash adds key length without adding discrimination: 12 hex
		// chars is 48 bits, and a collision would only mean two different
		// prefixes sharing an affinity slot within one conversation.
		h = h[:12]
	}
	if h == "" {
		h = "none"
	}
	return h + ":" + strconv.FormatInt(e.IdleBucket, 10)
}

// ComputeEpoch derives the epoch for a request.
//
// idleBucket advances only when the gap since the previous request exceeds the
// TTL — it is NOT floor(now/ttl). A wall-clock bucket would expire affinity at
// an arbitrary boundary regardless of traffic: a conversation with a request
// every 30 seconds would lose its affinity every 5 minutes despite the cache
// being continuously warm. What matters is the GAP, not the wall clock.
func ComputeEpoch(prefixHash string, lastSeen time.Time, prevBucket int64, now time.Time, ttl time.Duration) Epoch {
	bucket := prevBucket
	if lastSeen.IsZero() || now.Sub(lastSeen) > ttl {
		bucket = prevBucket + 1
	}
	return Epoch{PrefixHash: prefixHash, IdleBucket: bucket}
}

// HashPrefix hashes the stable span — system text and tool schemas — that the
// vendor cache is actually keyed on.
//
// Callers normally pass the value the gateway already computes; this exists so
// the package is usable standalone and in tests.
func HashPrefix(system string, toolNames []string) string {
	h := sha256.New()
	h.Write([]byte(system))
	for _, t := range toolNames {
		h.Write([]byte{0})
		h.Write([]byte(t))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Store persists affinity across gateway replicas.
//
// Redis rather than process memory because routing decisions must be consistent
// across replicas: per-replica affinity would send the same conversation to a
// different provider depending on which pod answered, rebuilding the cache each
// time — the exact cost this package exists to avoid.
//
// Every method FAILS OPEN. Losing affinity costs a cold cache; failing a
// request costs the request.
type Store interface {
	// Get returns the affine target for this epoch, if any.
	Get(ctx context.Context, tenant, key string, e Epoch) (Target, bool)
	// Put records the chosen target for this epoch.
	Put(ctx context.Context, tenant, key string, e Epoch, t Target, ttl time.Duration)
	// Touch records that this conversation was seen, returning the previous
	// last-seen time and idle bucket so the caller can compute the epoch.
	Touch(ctx context.Context, tenant, key string, now time.Time, ttl time.Duration) (lastSeen time.Time, bucket int64)
}

// Decision is the outcome of consulting affinity, for the event.
type Decision struct {
	// Held is true when a stored target was found and is usable.
	Held bool
	// Target is the affine provider/model when Held.
	Target Target
	// Epoch is the epoch this request belongs to.
	Epoch Epoch
	// EpochAdvanced is true when this request started a new epoch, and Reason
	// says which signal moved it. Recorded because "why did affinity break"
	// is the question an operator asks when cache hit rates fall, and the two
	// causes — an edited system prompt versus a genuine idle gap — call for
	// completely different responses.
	EpochAdvanced bool
	Reason        string
}

// Manager consults and records affinity.
type Manager struct {
	store Store
	cfg   Config
}

// New returns a Manager. A nil store yields one that never holds affinity, so
// callers need no nil checks.
func New(store Store, cfg Config) *Manager { return &Manager{store: store, cfg: cfg} }

// Enabled reports whether affinity can operate.
func (m *Manager) Enabled() bool {
	return m != nil && m.store != nil && m.cfg.Enabled()
}

// Config returns the effective configuration.
func (m *Manager) Config() Config { return m.cfg }

// Resolve computes this request's epoch and returns any affine target.
//
// usable is consulted before a stored target is offered, so affinity can never
// override health or constraints. That ordering is not a detail: a warm cache
// on an ejected provider is worthless, and a warm cache on a DENIED provider is
// a compliance breach. Economics never outranks either.
func (m *Manager) Resolve(ctx context.Context, tenant, key, prefixHash string, now time.Time, usable func(provider string) bool) Decision {
	if !m.Enabled() || strings.TrimSpace(key) == "" {
		// No conversation identifier is the common case for single-shot API
		// traffic. Guessing an identity would be worse than having none.
		return Decision{}
	}
	ttl := m.cfg.ttl()
	lastSeen, prevBucket := m.store.Touch(ctx, tenant, key, now, ttl)
	e := ComputeEpoch(prefixHash, lastSeen, prevBucket, now, ttl)

	d := Decision{Epoch: e}
	if e.IdleBucket != prevBucket {
		d.EpochAdvanced, d.Reason = true, "idle beyond ttl; vendor cache has expired"
	}

	t, ok := m.store.Get(ctx, tenant, key, e)
	if !ok {
		if !d.EpochAdvanced {
			// A miss without an idle gap means the prefix changed — the system
			// prompt or tool set was edited — which is worth distinguishing:
			// it points at a deploy, not at traffic patterns.
			d.EpochAdvanced, d.Reason = true, "stable prefix changed; system prompt or tool set was edited"
		}
		return d
	}
	if usable != nil && !usable(t.Provider) {
		// Health and constraints outrank affinity. Recorded rather than
		// silent, because "affinity was held but not honoured" and "no
		// affinity existed" look identical downstream otherwise.
		d.Reason = "affine provider " + t.Provider + " is unavailable or denied"
		return d
	}
	d.Held, d.Target = true, t
	return d
}

// Record stores the provider a request actually used, so the next turn in the
// same epoch can stick to it.
func (m *Manager) Record(ctx context.Context, tenant, key string, e Epoch, provider, model string, now time.Time) {
	if !m.Enabled() || strings.TrimSpace(key) == "" || provider == "" {
		return
	}
	t := Target{Provider: provider, EstablishedAt: now}
	// Vendor prompt caches are MODEL-scoped, so recording the vendor alone
	// would let the next turn land on a different model and rebuild the cache
	// anyway — affinity that appears to hold while buying nothing.
	if m.cfg.Scope != "vendor" {
		t.Model = model
	}
	m.store.Put(ctx, tenant, key, e, t, m.cfg.ttl())
}

// ShouldFail reports whether an unhonoured affinity must fail the request
// rather than silently switching provider.
func (m *Manager) ShouldFail(d Decision) bool {
	if !m.Enabled() || d.Held {
		return false
	}
	// Only when a target existed and could not be used — never merely because
	// none was established yet, which would fail the first request of every
	// conversation.
	return m.cfg.onBreak() == Fail && strings.Contains(d.Reason, "unavailable or denied")
}
