package breaker

import (
	"context"
	"errors"
	"testing"
	"time"
)

// clock is a manual time source. Every window here is tens of seconds, so a
// test that slept would be unusably slow and flaky.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBreaker(cfg Config) (*Breaker, *MemoryStore, *clock) {
	c := &clock{t: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	st := NewMemoryStore()
	st.SetClock(c.now)
	return New(st, cfg), st, c
}

func record(t *testing.T, b *Breaker, target string, o Outcome, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		b.Record(context.Background(), target, o)
	}
}

func admit(t *testing.T, b *Breaker, target string) (bool, State) {
	t.Helper()
	return b.Admit(context.Background(), target)
}

// ---------------------------------------------------------------------------
// The central asymmetry: only the provider's own failures eject it.
// ---------------------------------------------------------------------------

func TestServerErrorsEject(t *testing.T) {
	b, _, _ := newTestBreaker(Config{ConsecutiveErrors: 3})
	record(t, b, "openai", ServerError, 3)
	ok, state := admit(t, b, "openai")
	if ok || state != Open {
		t.Fatalf("after 3 consecutive server errors: admitted=%v state=%v, want denied/open", ok, state)
	}
}

// A 429 means the provider is up and asking us to slow down. Ejecting it would
// convert a throttle into an outage.
func TestRateLimitNeverEjects(t *testing.T) {
	b, _, _ := newTestBreaker(Config{ConsecutiveErrors: 3})
	record(t, b, "openai", RateLimited, 50)
	if ok, state := admit(t, b, "openai"); !ok || state != Closed {
		t.Fatalf("429s ejected the provider: admitted=%v state=%v", ok, state)
	}
}

// The tas-llm-router#151 shape: a route rule pins a provider that does not
// serve the requested model, producing an endless 404 stream. If client errors
// counted, one bad rule would eject a healthy provider for EVERY tenant.
func TestClientErrorsNeverEject(t *testing.T) {
	b, _, _ := newTestBreaker(Config{ConsecutiveErrors: 3})
	record(t, b, "anthropic", ClientError, 100)
	if ok, state := admit(t, b, "anthropic"); !ok || state != Closed {
		t.Fatalf("client errors ejected the provider: admitted=%v state=%v", ok, state)
	}
}

// Non-evidence outcomes must not dilute the error rate either — otherwise a
// flood of 429s would mask a genuine failure rate.
func TestNonEvidenceOutcomesAreNotCountedInWindow(t *testing.T) {
	b, st, _ := newTestBreaker(Config{})
	record(t, b, "openai", RateLimited, 10)
	record(t, b, "openai", ClientError, 10)
	s, _ := st.Status(context.Background(), "openai", b.Config())
	if s.Total != 0 {
		t.Fatalf("window total = %d, want 0 — 429s and 4xx are not evidence either way", s.Total)
	}
}

func TestSuccessResetsConsecutiveCount(t *testing.T) {
	b, _, _ := newTestBreaker(Config{ConsecutiveErrors: 3})
	record(t, b, "openai", ServerError, 2)
	record(t, b, "openai", Success, 1)
	record(t, b, "openai", ServerError, 2)
	if ok, _ := admit(t, b, "openai"); !ok {
		t.Fatal("ejected on 2+2 errors split by a success — the streak must reset")
	}
}

// ---------------------------------------------------------------------------
// Error rate catches partial degradation that a streak never would.
// ---------------------------------------------------------------------------

func TestErrorRateEjectsWithoutAStreak(t *testing.T) {
	b, _, _ := newTestBreaker(Config{ConsecutiveErrors: 100, ErrorRatePercent: 50, MinRequests: 10})
	// Alternating: never two errors in a row, but half of all traffic fails.
	for i := 0; i < 12; i++ {
		record(t, b, "openai", ServerError, 1)
		record(t, b, "openai", Success, 1)
	}
	if ok, _ := admit(t, b, "openai"); ok {
		t.Fatal("50% error rate with no streak was not ejected — rate detection is the point")
	}
}

// Without a minimum sample the first failure reads as 100% and ejects a
// perfectly healthy provider on a sample of one.
func TestMinRequestsPreventsEjectionOnTinySample(t *testing.T) {
	b, _, _ := newTestBreaker(Config{ConsecutiveErrors: 100, ErrorRatePercent: 50, MinRequests: 20})
	record(t, b, "openai", ServerError, 3)
	if ok, _ := admit(t, b, "openai"); !ok {
		t.Fatal("ejected on a 3-request sample; min_requests must gate rate evaluation")
	}
}

func TestWindowExpiryDropsOldErrors(t *testing.T) {
	b, _, clk := newTestBreaker(Config{ConsecutiveErrors: 100, ErrorRatePercent: 50, MinRequests: 10, Window: 30 * time.Second})
	record(t, b, "openai", ServerError, 9)
	clk.advance(31 * time.Second)
	record(t, b, "openai", ServerError, 1)
	if ok, _ := admit(t, b, "openai"); !ok {
		t.Fatal("stale errors outside the window still ejected — accounting must be rolling")
	}
}

// ---------------------------------------------------------------------------
// Half-open: one probe, fleet-wide, and it is decisive.
// ---------------------------------------------------------------------------

func TestHalfOpenAdmitsExactlyOneProbe(t *testing.T) {
	b, _, clk := newTestBreaker(Config{ConsecutiveErrors: 2, EjectFor: 30 * time.Second})
	record(t, b, "openai", ServerError, 2)
	clk.advance(31 * time.Second)

	ok1, st1 := admit(t, b, "openai")
	if !ok1 || st1 != HalfOpen {
		t.Fatalf("first caller after ejection: admitted=%v state=%v, want admitted/half_open", ok1, st1)
	}
	// This is the multi-replica case: a second caller must NOT also probe, or
	// a recovering provider gets a stampede instead of a single request.
	if ok2, _ := admit(t, b, "openai"); ok2 {
		t.Fatal("a second probe was admitted while the first was in flight")
	}
}

func TestHalfOpenSuccessRestores(t *testing.T) {
	b, _, clk := newTestBreaker(Config{ConsecutiveErrors: 2, EjectFor: 30 * time.Second})
	record(t, b, "openai", ServerError, 2)
	clk.advance(31 * time.Second)
	admit(t, b, "openai")
	record(t, b, "openai", Success, 1)
	if ok, state := admit(t, b, "openai"); !ok || state != Closed {
		t.Fatalf("probe succeeded but state=%v admitted=%v, want closed/admitted", state, ok)
	}
}

func TestHalfOpenFailureReEjects(t *testing.T) {
	b, _, clk := newTestBreaker(Config{ConsecutiveErrors: 2, EjectFor: 30 * time.Second})
	record(t, b, "openai", ServerError, 2)
	clk.advance(31 * time.Second)
	admit(t, b, "openai")
	record(t, b, "openai", ServerError, 1)
	if ok, state := admit(t, b, "openai"); ok || state != Open {
		t.Fatalf("probe failed but state=%v admitted=%v, want open/denied", state, ok)
	}
}

// A single probe success must clear the evidence that caused the ejection,
// otherwise the stale error count re-trips the breaker on the next request.
func TestRestoreClearsPriorErrors(t *testing.T) {
	b, _, clk := newTestBreaker(Config{ConsecutiveErrors: 2, EjectFor: 30 * time.Second})
	record(t, b, "openai", ServerError, 2)
	clk.advance(31 * time.Second)
	admit(t, b, "openai")
	record(t, b, "openai", Success, 1)
	record(t, b, "openai", ServerError, 1) // 1 error, not 3
	if ok, _ := admit(t, b, "openai"); !ok {
		t.Fatal("re-ejected after one error post-restore; the old streak was not cleared")
	}
}

// ---------------------------------------------------------------------------
// Retry budget: the storm control a per-request attempt cap cannot provide.
// ---------------------------------------------------------------------------

func TestRetryBudgetCapsRetriesAsAShareOfTraffic(t *testing.T) {
	b, _, _ := newTestBreaker(Config{RetryRatio: 0.1, MinRetries: 0, Window: time.Minute})
	record(t, b, "openai", Success, 100) // budget = 10
	allowed := 0
	for i := 0; i < 50; i++ {
		if b.AllowRetry(context.Background(), "openai") {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("allowed %d retries against a 10%% budget on 100 requests, want 10", allowed)
	}
}

// At low volume a pure ratio rounds to zero and would disable retries
// entirely, which is worse than the storm it guards against.
func TestMinRetriesFloorAppliesAtLowVolume(t *testing.T) {
	b, _, _ := newTestBreaker(Config{RetryRatio: 0.1, MinRetries: 3, Window: time.Minute})
	record(t, b, "openai", Success, 2) // ratio budget = 0
	allowed := 0
	for i := 0; i < 10; i++ {
		if b.AllowRetry(context.Background(), "openai") {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("allowed %d retries at low volume, want the min_retries floor of 3", allowed)
	}
}

// ---------------------------------------------------------------------------
// A nil store must behave like "no opinion", not panic — callers should never
// need a nil check.
// ---------------------------------------------------------------------------

func TestNilStoreAdmitsEverything(t *testing.T) {
	b := New(nil, Config{})
	if ok, state := admit(t, b, "openai"); !ok || state != Closed {
		t.Fatalf("nil store: admitted=%v state=%v, want admitted/closed", ok, state)
	}
	b.Record(context.Background(), "openai", ServerError)
	if !b.AllowRetry(context.Background(), "openai") {
		t.Fatal("nil store denied a retry")
	}
	if b.Enabled() {
		t.Fatal("nil store reported Enabled")
	}
}

// ---------------------------------------------------------------------------
// Target naming and classification.
// ---------------------------------------------------------------------------

func TestTargetGranularity(t *testing.T) {
	if got := Target("OpenAI", ""); got != "openai" {
		t.Fatalf("Target(provider) = %q, want lowercased provider", got)
	}
	if got := Target("OpenAI", "GPT-4o"); got != "openai/gpt-4o" {
		t.Fatalf("Target(provider, model) = %q", got)
	}
	// Provider-level and model-level ejection must be distinct, or ejecting
	// one model would take the whole vendor out.
	if Target("openai", "") == Target("openai", "gpt-4o") {
		t.Fatal("provider and provider/model must be different targets")
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := map[int]Outcome{200: Success, 201: Success, 400: ClientError, 404: ClientError, 429: RateLimited, 500: ServerError, 503: ServerError}
	for code, want := range cases {
		if got := ClassifyStatus(code); got != want {
			t.Errorf("ClassifyStatus(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		err  error
		want Outcome
	}{
		{nil, Success},
		{errors.New("connection refused"), ServerError},
		{errors.New("context deadline exceeded"), ServerError},
		{errors.New("429 Too Many Requests"), RateLimited},
		{errors.New("rate limit reached for gpt-4o"), RateLimited},
		// The #151 error verbatim: it must read as OUR fault, not the
		// provider's, or a bad pin ejects a healthy vendor.
		{errors.New(`anthropic api call failed: POST "https://api.anthropic.com/v1/messages": 404 Not Found {"type":"not_found_error","message":"model: gpt-4o-mini"}`), ClientError},
		{errors.New("401 Unauthorized"), ClientError},
	}
	for _, c := range cases {
		if got := ClassifyError(c.err); got != c.want {
			t.Errorf("ClassifyError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	if d, ok := RetryAfter("30", now); !ok || d != 30*time.Second {
		t.Fatalf("delay-seconds: got %v ok=%v", d, ok)
	}
	if d, ok := RetryAfter("Thu, 20 Aug 2026 12:00:30 UTC", now); !ok || d != 30*time.Second {
		t.Fatalf("http-date: got %v ok=%v", d, ok)
	}
	// A date in the past means retry now, not sleep backwards.
	if d, ok := RetryAfter("Thu, 20 Aug 2026 11:59:00 UTC", now); !ok || d != 0 {
		t.Fatalf("past http-date: got %v ok=%v, want 0/true", d, ok)
	}
	// Unparseable must fall back to the caller's own backoff rather than
	// being read as "retry immediately".
	for _, bad := range []string{"", "soon", "-5", "12.5"} {
		if _, ok := RetryAfter(bad, now); ok {
			t.Errorf("RetryAfter(%q) reported ok; want fallback to caller backoff", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Config validation — an operator should learn at write time, not mid-incident.
// ---------------------------------------------------------------------------

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"zero is valid", Config{}, false},
		{"sane", Config{ConsecutiveErrors: 5, ErrorRatePercent: 50, MinRequests: 20, RetryRatio: 0.1}, false},
		{"rate above 100", Config{ErrorRatePercent: 101}, true},
		{"negative rate", Config{ErrorRatePercent: -1}, true},
		{"ratio above 1", Config{RetryRatio: 1.5}, true},
		{"negative eject", Config{EjectFor: -time.Second}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.cfg.Validate(); (err != nil) != c.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

func TestDefaultsAppliedToZeroConfig(t *testing.T) {
	got := New(NewMemoryStore(), Config{}).Config()
	if got.ConsecutiveErrors != DefaultConsecutiveErrors || got.Window != DefaultWindow || got.EjectFor != DefaultEjectFor {
		t.Fatalf("zero config did not pick up defaults: %+v", got)
	}
}

func TestStatusReportsReasonAndWindow(t *testing.T) {
	b, _, _ := newTestBreaker(Config{ConsecutiveErrors: 3})
	record(t, b, "openai", ServerError, 3)
	st, err := b.Status(context.Background(), "openai")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != Open {
		t.Fatalf("state = %v, want open", st.State)
	}
	// An ejection nobody can explain is indistinguishable from an outage.
	if st.Reason == "" {
		t.Fatal("Status carries no reason for the ejection")
	}
	if st.EjectedUntil.IsZero() {
		t.Fatal("Status carries no ejected_until for an open breaker")
	}
}
