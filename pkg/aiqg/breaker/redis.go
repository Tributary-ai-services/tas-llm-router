package breaker

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore keeps breaker state fleet-wide so every replica shares one view
// of a target and a recovering provider receives one probe in total.
//
// # Key layout
//
// All keys are prefixed aiqg:brk:{target}: and ALL of them carry a TTL. That
// is not incidental tidiness: redis-shared runs maxmemory=0 with noeviction
// and is shared with other services, so a key without a TTL is a permanent
// leak in someone else's datastore.
//
//	:open     presence means ejected. TTL IS the ejection duration, so the
//	          ejection expires by itself with no sweeper and no clock skew
//	          between replicas.
//	:pending  outlives :open and marks "this target was ejected and has not
//	          yet passed a probe". Its presence after :open has expired is
//	          exactly the half-open condition.
//	:probe    SET NX. Whoever wins it is the single fleet-wide probe. Short
//	          TTL so a replica that crashes mid-probe releases it rather than
//	          wedging the target open forever.
//	:cons     consecutive server errors.
//	:tot/:err fixed-window counters (see below).
//	:retry    retries consumed in the current window.
//	:reason   human text for the last transition, for the health panel.
//	:since    unix seconds of the last transition.
//
// # Fixed window, not sliding
//
// :tot and :err are counters whose TTL is the window, so the window resets
// wholesale rather than sliding. A sliding window would need a sorted set per
// target with per-request members — far more Redis traffic and memory on the
// hot path, for a threshold decision that is inherently approximate. The cost
// is that error-rate detection can be delayed by up to one window when errors
// straddle a reset. Consecutive-error detection has no such delay, which is
// why both rules exist rather than just the rate one.
//
// # Failing open
//
// Every method returns a permissive result when Redis errors. See the package
// doc: losing outlier detection is a far smaller failure than losing routing.
type RedisStore struct {
	rdb    redis.UniversalClient
	prefix string
}

// NewRedisStore returns a Store backed by rdb.
func NewRedisStore(rdb redis.UniversalClient) *RedisStore {
	return &RedisStore{rdb: rdb, prefix: "aiqg:brk:"}
}

const (
	// probeTTL bounds how long a single half-open probe may hold the slot.
	// Longer than any plausible completion, short enough that a crashed
	// replica does not strand the target.
	probeTTL = 60 * time.Second
	// targetsKey indexes known targets for the health panel.
	targetsKey = "aiqg:brk:targets"
	// targetsTTL keeps the index from outliving interest in it.
	targetsTTL = 24 * time.Hour
)

func (r *RedisStore) k(target, suffix string) string { return r.prefix + target + ":" + suffix }

// pendingTTL bounds how long a target may sit half-open awaiting a probe
// result. Generous relative to the ejection, but finite: a target nobody sends
// traffic to should eventually return to closed rather than stay marked
// forever.
func pendingTTL(cfg Config) time.Duration {
	d := cfg.EjectFor * 4
	if d < 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func (r *RedisStore) Admit(ctx context.Context, target string, cfg Config) (bool, State, error) {
	pipe := r.rdb.Pipeline()
	openC := pipe.Exists(ctx, r.k(target, "open"))
	pendC := pipe.Exists(ctx, r.k(target, "pending"))
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return true, Closed, nil // fail open
	}
	if openC.Val() > 0 {
		return false, Open, nil
	}
	if pendC.Val() == 0 {
		return true, Closed, nil
	}
	// Ejection has elapsed but no probe has succeeded yet. Exactly one caller
	// fleet-wide wins the probe slot; the rest keep waiting rather than all
	// rushing a provider that may still be broken.
	won, err := r.rdb.SetNX(ctx, r.k(target, "probe"), "1", probeTTL).Result()
	if err != nil {
		return true, Closed, nil // fail open
	}
	if !won {
		return false, Open, nil
	}
	r.note(ctx, target, "ejection elapsed; probing with a single request", cfg)
	return true, HalfOpen, nil
}

func (r *RedisStore) Record(ctx context.Context, target string, outcome Outcome, cfg Config) error {
	r.touchTarget(ctx, target)

	// Half-open resolution takes precedence: a probe result is decisive on
	// its own and must not be diluted by the window counters.
	pending, err := r.rdb.Exists(ctx, r.k(target, "pending")).Result()
	if err == nil && pending > 0 {
		open, _ := r.rdb.Exists(ctx, r.k(target, "open")).Result()
		if open == 0 {
			// We are past the ejection window, so this outcome IS the probe.
			if outcome.countsTowardEjection() {
				r.trip(ctx, target, "half-open probe failed; ejected again", cfg)
			} else if outcome == Success {
				r.restore(ctx, target)
			}
			// A 429 or client error during half-open resolves nothing: it is
			// not evidence either way, so release the slot and let the next
			// request be the real probe.
			if outcome == RateLimited || outcome == ClientError {
				r.rdb.Del(ctx, r.k(target, "probe"))
			}
			return nil
		}
	}

	isErr := outcome.countsTowardEjection()
	if outcome != Success && !isErr {
		// Neither evidence for nor against the provider — see MemoryStore.
		return nil
	}

	pipe := r.rdb.Pipeline()
	totC := pipe.Incr(ctx, r.k(target, "tot"))
	pipe.Expire(ctx, r.k(target, "tot"), cfg.Window)
	var errC, consC *redis.IntCmd
	if isErr {
		errC = pipe.Incr(ctx, r.k(target, "err"))
		pipe.Expire(ctx, r.k(target, "err"), cfg.Window)
		consC = pipe.Incr(ctx, r.k(target, "cons"))
		pipe.Expire(ctx, r.k(target, "cons"), cfg.Window*2)
	} else {
		pipe.Del(ctx, r.k(target, "cons"))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil // fail open
	}

	if !isErr {
		return nil
	}
	consecutive := int(consC.Val())
	errs := int(errC.Val())
	total := int(totC.Val())
	if reason, trip := shouldTrip(consecutive, errs, total, cfg); trip {
		r.trip(ctx, target, reason, cfg)
	}
	return nil
}

// trip ejects the target. Setting :open and :pending together is what makes
// the later expiry of :open mean half-open rather than closed.
func (r *RedisStore) trip(ctx context.Context, target, reason string, cfg Config) {
	pipe := r.rdb.Pipeline()
	pipe.Set(ctx, r.k(target, "open"), reason, cfg.EjectFor)
	pipe.Set(ctx, r.k(target, "pending"), "1", pendingTTL(cfg))
	pipe.Set(ctx, r.k(target, "reason"), reason, pendingTTL(cfg))
	pipe.Set(ctx, r.k(target, "since"), strconv.FormatInt(time.Now().Unix(), 10), pendingTTL(cfg))
	pipe.Del(ctx, r.k(target, "probe"), r.k(target, "cons"), r.k(target, "err"), r.k(target, "tot"))
	_, _ = pipe.Exec(ctx)
}

// restore returns the target to service and clears the evidence that ejected
// it, so a stale error count cannot immediately re-trip it.
func (r *RedisStore) restore(ctx context.Context, target string) {
	pipe := r.rdb.Pipeline()
	pipe.Del(ctx,
		r.k(target, "open"), r.k(target, "pending"), r.k(target, "probe"),
		r.k(target, "cons"), r.k(target, "err"), r.k(target, "tot"))
	pipe.Set(ctx, r.k(target, "reason"), "half-open probe succeeded; restored", targetsTTL)
	pipe.Set(ctx, r.k(target, "since"), strconv.FormatInt(time.Now().Unix(), 10), targetsTTL)
	_, _ = pipe.Exec(ctx)
}

func (r *RedisStore) note(ctx context.Context, target, reason string, cfg Config) {
	pipe := r.rdb.Pipeline()
	pipe.Set(ctx, r.k(target, "reason"), reason, pendingTTL(cfg))
	pipe.Set(ctx, r.k(target, "since"), strconv.FormatInt(time.Now().Unix(), 10), pendingTTL(cfg))
	_, _ = pipe.Exec(ctx)
}

func (r *RedisStore) AllowRetry(ctx context.Context, target string, cfg Config) (bool, error) {
	total, err := r.rdb.Get(ctx, r.k(target, "tot")).Int()
	if err != nil && err != redis.Nil {
		return true, nil // fail open
	}
	budget := int(float64(total) * cfg.RetryRatio)
	if budget < cfg.MinRetries {
		budget = cfg.MinRetries
	}
	// Increment first, then compare, so concurrent replicas cannot both read
	// "under budget" and both proceed. Overshoot is bounded by the number of
	// replicas racing at the exact threshold, which is a far better failure
	// than the check-then-act version where every replica gets a full budget.
	used, err := r.rdb.Incr(ctx, r.k(target, "retry")).Result()
	if err != nil {
		return true, nil // fail open
	}
	r.rdb.Expire(ctx, r.k(target, "retry"), cfg.Window)
	if int(used) > budget {
		// Give the slot back so a rejected retry does not consume budget it
		// never used; otherwise a burst of rejections would suppress retries
		// long after the burst.
		r.rdb.Decr(ctx, r.k(target, "retry"))
		return false, nil
	}
	return true, nil
}

func (r *RedisStore) Status(ctx context.Context, target string, cfg Config) (Status, error) {
	pipe := r.rdb.Pipeline()
	openC := pipe.Get(ctx, r.k(target, "open"))
	openTTL := pipe.TTL(ctx, r.k(target, "open"))
	pendC := pipe.Exists(ctx, r.k(target, "pending"))
	reasonC := pipe.Get(ctx, r.k(target, "reason"))
	sinceC := pipe.Get(ctx, r.k(target, "since"))
	totC := pipe.Get(ctx, r.k(target, "tot"))
	errC := pipe.Get(ctx, r.k(target, "err"))
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return Status{Target: target, State: Closed}, nil
	}

	st := Status{Target: target, State: Closed}
	switch {
	case openC.Err() == nil:
		st.State = Open
		if d := openTTL.Val(); d > 0 {
			st.EjectedUntil = time.Now().Add(d)
		}
	case pendC.Val() > 0:
		st.State = HalfOpen
	}
	st.Reason, _ = reasonC.Result()
	if s, err := sinceC.Int64(); err == nil {
		st.Since = time.Unix(s, 0)
	}
	st.Total, _ = totC.Int()
	st.Errors, _ = errC.Int()
	return st, nil
}

// touchTarget records that a target exists, for the health panel. A sorted set
// scored by last-seen lets stale entries be pruned on read, which a plain set
// could not do — set members cannot carry individual TTLs.
func (r *RedisStore) touchTarget(ctx context.Context, target string) {
	now := time.Now()
	pipe := r.rdb.Pipeline()
	pipe.ZAdd(ctx, targetsKey, redis.Z{Score: float64(now.Unix()), Member: target})
	pipe.ZRemRangeByScore(ctx, targetsKey, "-inf", strconv.FormatInt(now.Add(-targetsTTL).Unix(), 10))
	pipe.Expire(ctx, targetsKey, targetsTTL)
	_, _ = pipe.Exec(ctx)
}

func (r *RedisStore) Targets(ctx context.Context) ([]string, error) {
	out, err := r.rdb.ZRange(ctx, targetsKey, 0, -1).Result()
	if err != nil && err != redis.Nil {
		return nil, nil // fail open: an empty panel beats a failed request
	}
	return out, nil
}
