// Package breaker implements passive outlier detection and retry budgets for
// routing targets — step 2 of the routing-decision plan.
//
// # Why passive, when the router already health-checks
//
// The router runs an ACTIVE probe: every 30s it calls provider.HealthCheck(),
// which for OpenAI is ListModels(). That answers "is the vendor's control
// plane reachable from this pod", which is not the question routing needs. A
// vendor can serve ListModels perfectly while returning 500s to every chat
// completion; it can be degraded for one model and fine for another; it can
// fail for our API key and nobody else's. Between probes the router keeps
// sending traffic into a provider it has no current evidence about, and a
// provider that has never been probed is treated as healthy ("unknown" counts
// as healthy, deliberately, so a cold start doesn't eject everything).
//
// Passive detection uses the traffic itself as the probe. Every real request
// is evidence, so detection time is bounded by request rate rather than by a
// timer, and the evidence is about the operation we actually care about.
//
// The two are complements, not substitutes: the active probe can mark a
// provider down with no traffic at all, and the breaker can mark it down while
// the probe still passes. A target must satisfy BOTH to receive traffic.
//
// # Why the state lives in Redis
//
// The gateway runs multiple replicas. Per-replica breaker state means each
// replica must independently learn a provider is bad — N times the damage
// before detection, and N inconsistent views of the same provider. Worse, the
// half-open probe would be per-replica: a recovering provider gets N
// simultaneous probes rather than one, which is exactly the thundering herd
// ejection exists to prevent.
//
// Redis makes the state fleet-wide. It also means Redis being down must not
// take routing down: every operation here FAILS OPEN. An unreachable Redis
// yields "allowed, closed" and a recorded outcome is dropped. Degrading to
// "no outlier detection" is strictly better than degrading to "no traffic".
//
// # Not every failure is the provider's fault
//
// Only failures the provider is ACCOUNTABLE for count toward ejection:
//
//	ServerError  — 5xx, connection refused, timeout. The provider's fault.
//	RateLimited  — 429. The provider is healthy and telling us to slow down;
//	               ejecting it would convert a throttle into an outage.
//	ClientError  — 4xx. OUR request was wrong. Counting these would let one
//	               badly-configured route rule eject a provider for every
//	               tenant on the gateway — see tas-llm-router#151, where a rule
//	               pinning a provider that doesn't serve the requested model
//	               produces an endless 404 stream. That must never be read as
//	               "the provider is down".
//
// This asymmetry is the difference between a circuit breaker that protects
// traffic and one that amplifies a config error into an outage.
package breaker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"
)

// Outcome classifies a completed attempt against a target. See the package
// doc: only ServerError counts toward ejection.
type Outcome int

const (
	// Success is any response the provider produced as asked.
	Success Outcome = iota
	// ServerError is a failure the provider is accountable for: 5xx,
	// connection failure, or timeout.
	ServerError
	// RateLimited is 429. Never ejects; drives backoff instead.
	RateLimited
	// ClientError is a non-429 4xx: our request was malformed or asked for
	// something this provider does not have. Never ejects.
	ClientError
)

func (o Outcome) String() string {
	switch o {
	case Success:
		return "success"
	case ServerError:
		return "server_error"
	case RateLimited:
		return "rate_limited"
	case ClientError:
		return "client_error"
	default:
		return "unknown"
	}
}

// countsTowardEjection reports whether this outcome is evidence against the
// provider. Kept as a method so the rule lives with the type rather than being
// re-derived at each call site.
func (o Outcome) countsTowardEjection() bool { return o == ServerError }

// State is a target's admission state.
type State string

const (
	// Closed is the normal state: traffic flows and outcomes are counted.
	Closed State = "closed"
	// Open means the target is ejected and receives no traffic.
	Open State = "open"
	// HalfOpen means the ejection has elapsed and exactly one probe request
	// fleet-wide is admitted to test recovery.
	HalfOpen State = "half_open"
)

// Config tunes ejection. Zero values are replaced by the defaults below, so a
// zero Config is usable and conservative.
type Config struct {
	// ConsecutiveErrors trips the breaker after this many server errors in a
	// row with no intervening success. Catches a hard outage fast.
	ConsecutiveErrors int
	// ErrorRatePercent trips the breaker when the error rate over Window
	// reaches this percentage. Catches partial degradation that consecutive
	// counting misses — a provider failing 40% of requests never accumulates
	// a long error streak but is badly broken.
	ErrorRatePercent int
	// MinRequests is the minimum sample in Window before ErrorRatePercent is
	// evaluated. Without it, the first request failing reads as 100% and
	// ejects a healthy provider on a sample of one.
	MinRequests int
	// Window is the rolling period for rate accounting.
	Window time.Duration
	// EjectFor is how long a tripped target stays Open before a probe is
	// admitted. This is the single most consequential setting: too short
	// re-hammers a struggling provider, too long extends an outage past its
	// cause.
	EjectFor time.Duration
	// RetryRatio caps retries as a fraction of live requests (0.1 = retries
	// may not exceed 10% of request volume). A per-request attempt cap alone
	// cannot prevent a retry storm: when a provider degrades, EVERY request
	// retries at once and the retries themselves become the load that keeps
	// it down. Envoy calls this a retry budget.
	RetryRatio float64
	// MinRetries is a floor of retries always permitted regardless of ratio,
	// so low-traffic tenants can still retry at all — at 2 rps, 10% of live
	// requests rounds to zero and retries would be disabled entirely.
	MinRetries int
}

// Defaults chosen to be slow to eject and quick to restore, because a false
// ejection removes capacity from a working system while a missed one merely
// leaves us where we already are.
const (
	DefaultConsecutiveErrors = 5
	DefaultErrorRatePercent  = 50
	DefaultMinRequests       = 20
	DefaultWindow            = 30 * time.Second
	DefaultEjectFor          = 30 * time.Second
	DefaultRetryRatio        = 0.1
	DefaultMinRetries        = 3
)

func (c Config) withDefaults() Config {
	if c.ConsecutiveErrors <= 0 {
		c.ConsecutiveErrors = DefaultConsecutiveErrors
	}
	if c.ErrorRatePercent <= 0 {
		c.ErrorRatePercent = DefaultErrorRatePercent
	}
	if c.MinRequests <= 0 {
		c.MinRequests = DefaultMinRequests
	}
	if c.Window <= 0 {
		c.Window = DefaultWindow
	}
	if c.EjectFor <= 0 {
		c.EjectFor = DefaultEjectFor
	}
	if c.RetryRatio <= 0 {
		c.RetryRatio = DefaultRetryRatio
	}
	if c.MinRetries <= 0 {
		c.MinRetries = DefaultMinRetries
	}
	return c
}

// Validate reports configuration that would behave surprisingly. Used by the
// control plane at write time so an operator learns immediately rather than
// discovering it during an incident.
func (c Config) Validate() error {
	var errs []string
	if c.ConsecutiveErrors < 0 {
		errs = append(errs, "consecutive_errors must not be negative")
	}
	if c.ErrorRatePercent < 0 || c.ErrorRatePercent > 100 {
		errs = append(errs, "error_rate_percent must be between 0 and 100")
	}
	if c.MinRequests < 0 {
		errs = append(errs, "min_requests must not be negative")
	}
	if c.Window < 0 {
		errs = append(errs, "window must not be negative")
	}
	if c.EjectFor < 0 {
		errs = append(errs, "eject_for must not be negative")
	}
	if c.RetryRatio < 0 || c.RetryRatio > 1 {
		errs = append(errs, "retry_ratio must be between 0 and 1")
	}
	if c.MinRetries < 0 {
		errs = append(errs, "min_retries must not be negative")
	}
	// A rate threshold that can never be reached is almost certainly a typo
	// rather than an intentional "never eject on rate".
	if c.ErrorRatePercent == 100 && c.MinRequests == 0 {
		errs = append(errs, "error_rate_percent=100 with min_requests=0 can only trip when every request in the window fails")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Status is a target's observable state, for the control plane and the
// operator-facing health panel. An ejection nobody can see is
// indistinguishable from an outage, so Reason and Since are not optional
// extras — they are the point.
type Status struct {
	Target string `json:"target"`
	State  State  `json:"state"`
	// Reason explains the last transition in plain language.
	Reason string `json:"reason,omitempty"`
	// Since is when the current state began. Zero when unknown (the state is
	// reconstructed from Redis TTLs, which survive a replica restart but not
	// a Redis flush).
	Since time.Time `json:"since,omitzero"`
	// EjectedUntil is set when State is Open.
	EjectedUntil time.Time `json:"ejected_until,omitzero"`
	// Errors and Total describe the current window, for display.
	Errors int `json:"errors"`
	Total  int `json:"total"`
}

// Store is the persistence the breaker needs. Redis is the production
// implementation; the in-memory one exists for tests and for running without
// Redis configured.
//
// Every method must FAIL OPEN — return the permissive answer and a nil error
// rather than propagating a backend failure into a routing decision.
type Store interface {
	// Admit reports whether a request may be sent to target, and the state
	// that decision came from. When the ejection has elapsed, exactly one
	// caller fleet-wide receives (true, HalfOpen); all others receive
	// (false, Open) until that probe reports back.
	Admit(ctx context.Context, target string, cfg Config) (bool, State, error)
	// Record accounts a completed attempt and trips or restores the breaker.
	Record(ctx context.Context, target string, outcome Outcome, cfg Config) error
	// AllowRetry reports whether a retry fits within the budget, and consumes
	// budget when it does.
	AllowRetry(ctx context.Context, target string, cfg Config) (bool, error)
	// Status reports current state for display.
	Status(ctx context.Context, target string, cfg Config) (Status, error)
	// Targets lists targets with known state, for the health panel.
	Targets(ctx context.Context) ([]string, error)
}

// Breaker wraps a Store with configuration and target naming.
type Breaker struct {
	store Store
	cfg   Config
}

// New returns a Breaker over store. A nil store yields a Breaker that admits
// everything and records nothing, so callers never need a nil check —
// "breaker not configured" and "breaker says yes" are the same code path.
func New(store Store, cfg Config) *Breaker {
	return &Breaker{store: store, cfg: cfg.withDefaults()}
}

// Config returns the effective configuration, defaults applied.
func (b *Breaker) Config() Config { return b.cfg }

// Enabled reports whether a store is attached.
func (b *Breaker) Enabled() bool { return b != nil && b.store != nil }

// Target names the unit of ejection: provider, or provider+model when a model
// is given.
//
// Model granularity matters because vendors degrade per model far more often
// than wholesale — a new or heavily-loaded model can be failing while the rest
// of the vendor's fleet is fine. Ejecting the whole vendor for one bad model
// discards working capacity; never ejecting per model leaves the bad one in
// rotation. The caller decides which granularity a decision used.
func Target(provider, model string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return provider
	}
	return provider + "/" + model
}

// Admit reports whether target may receive a request.
func (b *Breaker) Admit(ctx context.Context, target string) (bool, State) {
	if !b.Enabled() {
		return true, Closed
	}
	ok, st, err := b.store.Admit(ctx, target, b.cfg)
	if err != nil {
		return true, Closed // fail open
	}
	return ok, st
}

// Record accounts an attempt's outcome.
func (b *Breaker) Record(ctx context.Context, target string, outcome Outcome) {
	if !b.Enabled() {
		return
	}
	_ = b.store.Record(ctx, target, outcome, b.cfg) // fail open
}

// AllowRetry reports whether a retry fits the budget.
func (b *Breaker) AllowRetry(ctx context.Context, target string) bool {
	if !b.Enabled() {
		return true
	}
	ok, err := b.store.AllowRetry(ctx, target, b.cfg)
	if err != nil {
		return true // fail open
	}
	return ok
}

// Status reports a target's state.
func (b *Breaker) Status(ctx context.Context, target string) (Status, error) {
	if !b.Enabled() {
		return Status{Target: target, State: Closed}, nil
	}
	return b.store.Status(ctx, target, b.cfg)
}

// AllStatus reports every known target's state, for the health panel.
func (b *Breaker) AllStatus(ctx context.Context) ([]Status, error) {
	if !b.Enabled() {
		return nil, nil
	}
	targets, err := b.store.Targets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(targets))
	for _, t := range targets {
		st, err := b.store.Status(ctx, t, b.cfg)
		if err != nil {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}

// ClassifyStatus maps an HTTP status code to an Outcome.
func ClassifyStatus(code int) Outcome {
	switch {
	case code == 429:
		return RateLimited
	case code >= 500:
		return ServerError
	case code >= 400:
		return ClientError
	default:
		return Success
	}
}

// ClassifyError maps a transport-level error to an Outcome. A nil error is
// Success. Errors carrying a recognisable vendor status are classified by it,
// so a 404 surfaced as an error string is still correctly read as our fault
// rather than the provider's.
func ClassifyError(err error) Outcome {
	if err == nil {
		return Success
	}
	s := strings.ToLower(err.Error())
	// Vendor SDKs and our own wrappers stringify the status; check the
	// non-ejecting classes first so they win over the generic error path.
	switch {
	case strings.Contains(s, "429") || strings.Contains(s, "rate limit") || strings.Contains(s, "too many requests"):
		return RateLimited
	case containsAny(s, "400 ", "401 ", "403 ", "404 ", "422 ",
		"bad request", "unauthorized", "not_found_error", "not found", "invalid_request"):
		return ClientError
	}
	// Everything else — 5xx, connection refused, timeout, context deadline —
	// is the provider's account.
	return ServerError
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// RetryAfter extracts a Retry-After duration from a header value. Supports
// both forms in RFC 9110: delay-seconds and an HTTP-date. Returns ok=false
// when absent or unparseable, so the caller falls back to its own backoff
// rather than treating a malformed header as "retry immediately".
func RetryAfter(header string, now time.Time) (time.Duration, bool) {
	h := strings.TrimSpace(header)
	if h == "" {
		return 0, false
	}
	if secs, err := parseUint(h); err == nil {
		return time.Duration(secs) * time.Second, true
	}
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC} {
		if t, err := time.Parse(layout, h); err == nil {
			d := t.Sub(now)
			if d < 0 {
				// A past date means "retry now"; a negative sleep would be a
				// bug at the call site.
				return 0, true
			}
			return d, true
		}
	}
	return 0, false
}

func parseUint(s string) (uint64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	var n uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + uint64(c-'0')
		if n > 1<<32 {
			return 0, fmt.Errorf("out of range")
		}
	}
	return n, nil
}

// Override returns a Breaker sharing this one's store but using cfg overrides
// merged over the base configuration. Only NON-ZERO override fields apply, so
// a route rule that sets eject_for alone keeps every other threshold — a rule
// carrying one setting must not silently reset the rest to zero.
//
// The store is shared deliberately. Overrides change the THRESHOLDS a decision
// is judged against, never which counters it reads: two rules disagreeing about
// eject_for must still observe the same provider, or each would see only its
// own slice of the traffic and neither would detect an outage.
//
// Note which direction that works in. The trip rule is evaluated by whichever
// config RECORDS an outcome, so a stricter rule ejects on its own traffic and
// the resulting ejection is visible to everyone — rather than a lenient rule's
// past records being retroactively re-judged against a stricter threshold. The
// alternative would make a target's state depend on which rule happened to ask
// about it, which is not a state at all.
func (b *Breaker) Override(h *resilience.Health, bu *resilience.Budgets) *Breaker {
	if !b.Enabled() || (h == nil && bu == nil) {
		return b
	}
	cfg := b.cfg
	if h != nil {
		if h.ConsecutiveErrors > 0 {
			cfg.ConsecutiveErrors = h.ConsecutiveErrors
		}
		if h.ErrorRatePercent > 0 {
			cfg.ErrorRatePercent = h.ErrorRatePercent
		}
		if h.MinRequests > 0 {
			cfg.MinRequests = h.MinRequests
		}
		if h.WindowSeconds > 0 {
			cfg.Window = time.Duration(h.WindowSeconds) * time.Second
		}
		if h.EjectForSeconds > 0 {
			cfg.EjectFor = time.Duration(h.EjectForSeconds) * time.Second
		}
	}
	if bu != nil {
		if bu.RetryRatio > 0 {
			cfg.RetryRatio = bu.RetryRatio
		}
		if bu.MinRetries > 0 {
			cfg.MinRetries = bu.MinRetries
		}
	}
	return &Breaker{store: b.store, cfg: cfg}
}

// ---------------------------------------------------------------------------
// Fallback classification.
//
// Deliberately in the same file as ClassifyError so the two-axes distinction
// is visible at a glance rather than discovered later: whether a failure
// EJECTS a provider and whether it ADVANCES a fallback chain are different
// questions with different answers, and reading one function without the other
// is how they get conflated.
// ---------------------------------------------------------------------------

// ClassifyFailure maps an error to the fallback taxonomy
// (go-aiqg-resilience.FailureClass), reporting ok=false when the failure is
// not one a chain should ever act on.
//
// The ok=false case matters as much as the classes. A malformed request, a bad
// API key, or a request for a model that does not exist will be rejected
// identically by every provider — so advancing the chain only multiplies one
// client error across every vendor, spending the whole chain and the latency
// budget to arrive at the same answer, while making the real cause harder to
// see.
func ClassifyFailure(err error) (resilience.FailureClass, bool) {
	if err == nil {
		return "", false
	}
	s := strings.ToLower(err.Error())

	// Context overflow first: it is a 4xx and would otherwise be swallowed by
	// the client-error branch below, yet it is precisely the case a chain
	// exists to solve — a larger-window model serves the same request
	// unchanged.
	if containsAny(s,
		"context length", "context_length_exceeded", "maximum context",
		"context window", "too many tokens", "prompt is too long", "input is too long") {
		return resilience.FailureContextOverflow, true
	}
	if containsAny(s, "429", "rate limit", "too many requests", "overloaded") {
		return resilience.FailureRateLimited, true
	}
	if containsAny(s, "content_filter", "content policy", "refused", "safety") {
		return resilience.FailureContentFilter, true
	}
	if containsAny(s, "timeout", "timed out", "deadline exceeded", "context canceled") {
		return resilience.FailureTimeout, true
	}
	// Our own error: every provider will say the same thing.
	if containsAny(s, "400 ", "401 ", "403 ", "404 ", "422 ",
		"bad request", "unauthorized", "not_found_error", "not found", "invalid_request") {
		return "", false
	}
	return resilience.FailureVendorError, true
}
