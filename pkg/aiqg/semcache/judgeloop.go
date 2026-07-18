package semcache

import (
	"context"
	"sync"
)

// JudgedPair is one graded example: a LabeledPair (the S2 calibration corpus
// format) enriched with the scope, similarity, and the judge's confidence. The
// Loop writes these to a PairSink; that sink IS the labeled pair set §9.2 step 1
// asks for, mined from our own traffic.
type JudgedPair struct {
	LabeledPair
	Scope      Scope   `json:"scope"`
	Similarity float64 `json:"similarity"`
	Confidence float64 `json:"confidence"`
	Observed   string  `json:"observed"`      // cascade state that produced the sample
	Reason     string  `json:"reason"`        // judge's rationale
	AtUnix     int64   `json:"at_unix"`
}

// PairSink persists judged pairs (a Redis list, a log stream, a file). The Loop
// never blocks on it beyond the worker goroutine; a sink error is counted and
// dropped, never surfaced to a request.
type PairSink interface {
	Record(ctx context.Context, p JudgedPair) error
}

// PairSinkFunc adapts a function to PairSink.
type PairSinkFunc func(ctx context.Context, p JudgedPair) error

// Record implements PairSink.
func (f PairSinkFunc) Record(ctx context.Context, p JudgedPair) error { return f(ctx, p) }

// JudgeStats is a snapshot of the loop's counters (feeds §14 metrics).
type JudgeStats struct {
	Enqueued  int // samples accepted onto the queue
	Dropped   int // samples rejected because the queue was full (off-path, never blocks)
	Graded    int // samples the judge returned a verdict for
	Errors    int // judge or sink errors
	WouldServe int // graded samples where L2 passed (the FPR denominator)
	FalseHits int // of WouldServe, judged incorrect (the FPR numerator, §14 ground truth)
	L2Rejected int // graded samples where L2 rejected the candidate
	L2Correct  int // of L2Rejected, judge agreed it would have been wrong (L2 earned its keep)
}

// SampledFPR is the sampled false-hit rate over would-serve samples — §14's "our
// only ground truth". Returns 0 when nothing would-serve has been graded yet.
func (s JudgeStats) SampledFPR() float64 {
	if s.WouldServe == 0 {
		return 0
	}
	return float64(s.FalseHits) / float64(s.WouldServe)
}

// L2Precision is the fraction of L2 rejections the judge agreed were genuinely
// wrong — evidence L2 is rejecting real near-misses, not over-tight (§14: "if it
// never fires, L1 is over-tight"; if it fires but is usually wrong to, it's too
// strict). Returns 0 when no L2 rejection has been graded.
func (s JudgeStats) L2Precision() float64 {
	if s.L2Rejected == 0 {
		return 0
	}
	return float64(s.L2Correct) / float64(s.L2Rejected)
}

// Loop is the L3 async judge, operationalized (§14.1). A single worker drains a
// bounded queue: it grades each sample, records the labeled pair, trains the
// per-entry threshold calibrator on would-serve verdicts, and tallies the sampled
// FPR. Enqueue is non-blocking and lossy by design — this runs off the request
// path, so a full queue drops the sample rather than ever slowing a request.
type Loop struct {
	grader Grader
	sink   PairSink
	cal    *SimCalibrator
	queue  chan Sample

	mu    sync.Mutex
	stats JudgeStats
}

// NewLoop builds the judge loop. queueSize bounds the backlog (excess samples are
// dropped and counted). A nil sink or grader yields a loop whose Enqueue is a
// no-op drop — so a partially-configured deployment fails safe, not panicky.
func NewLoop(grader Grader, sink PairSink, cal *SimCalibrator, queueSize int) *Loop {
	if queueSize <= 0 {
		queueSize = 256
	}
	return &Loop{
		grader: grader,
		sink:   sink,
		cal:    cal,
		queue:  make(chan Sample, queueSize),
	}
}

func (l *Loop) ready() bool { return l != nil && l.grader != nil && l.sink != nil }

// Enqueue offers a sample to the judge queue. It NEVER blocks: if the loop isn't
// ready or the queue is full it drops the sample and returns false. Safe to call
// from the request path (the drop is the whole point of being off it).
func (l *Loop) Enqueue(s Sample) bool {
	if !l.ready() {
		return false
	}
	select {
	case l.queue <- s:
		l.bump(func(st *JudgeStats) { st.Enqueued++ })
		return true
	default:
		l.bump(func(st *JudgeStats) { st.Dropped++ })
		return false
	}
}

// Run drains the queue until ctx is cancelled. Call once in a goroutine. Each
// sample is graded, recorded, and folded into the stats + calibrator.
func (l *Loop) Run(ctx context.Context) {
	if !l.ready() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-l.queue:
			l.process(ctx, s)
		}
	}
}

func (l *Loop) process(ctx context.Context, s Sample) {
	v, err := l.grader.Grade(ctx, s)
	if err != nil {
		l.bump(func(st *JudgeStats) { st.Errors++ })
		return
	}
	// Tally: a would-serve sample judged incorrect is a false hit (the ground
	// truth); an L2-rejected sample judged incorrect confirms L2 was right to
	// reject. Only would-serve verdicts train the threshold — an L2 rejection is
	// not a decision the L1 threshold governs.
	l.bump(func(st *JudgeStats) {
		st.Graded++
		switch {
		case s.wouldServe():
			st.WouldServe++
			if !v.Correct {
				st.FalseHits++
			}
		case s.Observed == StateMiss:
			st.L2Rejected++
			if !v.Correct {
				st.L2Correct++
			}
		}
	})
	if s.wouldServe() {
		l.cal.Observe(s.Similarity, v.Correct)
	}

	pair := JudgedPair{
		LabeledPair: LabeledPair{Query: s.Query, Candidate: s.CachedPrompt, Match: v.Correct},
		Scope:       s.Scope,
		Similarity:  s.Similarity,
		Confidence:  v.Confidence,
		Observed:    s.Observed,
		Reason:      v.Reason,
		AtUnix:      timeNow().Unix(),
	}
	if err := l.sink.Record(ctx, pair); err != nil {
		l.bump(func(st *JudgeStats) { st.Errors++ })
	}
}

// Stats returns a snapshot of the counters.
func (l *Loop) Stats() JudgeStats {
	if l == nil {
		return JudgeStats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stats
}

// RecommendThreshold surfaces the calibrator's suggested L1 threshold for a
// false-hit budget (ok=false until it has enough graded signal — see
// SimCalibrator.RecommendThreshold).
func (l *Loop) RecommendThreshold(fprBudget float64) (float64, bool) {
	if l == nil {
		return 0, false
	}
	return l.cal.RecommendThreshold(fprBudget)
}

func (l *Loop) bump(fn func(*JudgeStats)) {
	l.mu.Lock()
	fn(&l.stats)
	l.mu.Unlock()
}
