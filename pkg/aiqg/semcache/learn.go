package semcache

import "math"

// The similarity signal we care about lives in a narrow band near 1.0 (genuine
// near-dupes cluster ~0.98, near-misses ~0.90). Fitting a logistic on raw
// similarity there is ill-conditioned — the weight and bias are near-collinear
// and convergence crawls. Center and scale the feature into O(1) so the fit is
// well-behaved: feature(sim) = (sim - simCenter) * simScale.
const (
	simCenter = 0.90
	simScale  = 10.0
)

func simFeature(sim float64) float64 { return (sim - simCenter) * simScale }

// SimCalibrator is the adaptive-threshold mechanism (docs/AIQG-SEMANTIC-CACHING.md
// §9.3, S4). It fits Pr(correct | similarity) online as a logistic curve —
// sigmoid(a·sim + b) — over (similarity, correctness) pairs delivered by the L3
// judge, and turns that fit into a recommended L1 threshold for a chosen
// false-hit budget.
//
// This is vCache's mechanism, and only its mechanism. Its Pr ≥ 1−δ guarantee
// assumes i.i.d. prompts (false for real traffic — topic drift, bursts, deploys)
// and a truly sigmoid correctness curve; we take the online-BCE fit and the
// FPR-budget framing and do NOT quote the bound. The recommendation is a
// data-backed suggestion for a human to accept, not an auto-applied number.
//
// Not safe for concurrent use; the judge Loop owns one and calls it from its
// single worker goroutine.
type SimCalibrator struct {
	a, b   float64 // logit weights: P(correct) = sigmoid(a·sim + b)
	lr     float64 // learning rate
	pos    int     // observed correct (would-serve, judged correct)
	neg    int     // observed incorrect (would-serve, judged a FALSE HIT)
	warmup int     // min of each class before RecommendThreshold will fire
}

// NewSimCalibrator returns a calibrator. lr defaults to 0.5, warmup to 20 — a
// recommendation before ~20 graded examples of each class is noise, so it
// abstains. a starts positive (similarity should raise P(correct)); b starts so
// the initial crossover sits near the conservative 0.97 default (§9.3).
func NewSimCalibrator(lr float64, warmup int) *SimCalibrator {
	if lr <= 0 {
		lr = 0.5
	}
	if warmup <= 0 {
		warmup = 20
	}
	// Seed the crossover near the §9.3 prior (P≈0.5 at sim 0.97): a·feature(0.97)
	// + b = 0 with feature(0.97)=0.7 ⇒ b = -0.7a. This only biases the first few
	// updates; data dominates fast.
	return &SimCalibrator{a: 1, b: -0.7, lr: lr, warmup: warmup}
}

// Observe applies one online BCE gradient step for a graded would-serve sample.
// Only would-serve samples (L2 passed) belong here: an L2-rejected near-miss is
// not a cache decision the threshold governs, so it must not train the curve.
func (c *SimCalibrator) Observe(sim float64, correct bool) {
	if c == nil {
		return
	}
	y := 0.0
	if correct {
		y = 1.0
		c.pos++
	} else {
		c.neg++
	}
	f := simFeature(sim)
	p := c.Prob(sim)
	// dL/dz = (p - y) for BCE with a sigmoid; z = a·feature + b.
	grad := p - y
	c.a -= c.lr * grad * f
	c.b -= c.lr * grad
}

// Prob returns the fitted P(correct | sim) in (0,1).
func (c *SimCalibrator) Prob(sim float64) float64 {
	if c == nil {
		return 0
	}
	return 1 / (1 + math.Exp(-(c.a*simFeature(sim) + c.b)))
}

// RecommendThreshold returns the smallest similarity whose fitted P(correct) is
// at least 1−fprBudget — i.e. the loosest threshold that keeps the predicted
// false-hit rate within budget (§9.2 step 4: fix the FPR budget, maximize hits
// under it). ok is false until warmup is met in BOTH classes, or when the fitted
// curve isn't monotonically increasing in similarity (a ≤ 0) — meaning the
// embedding can't separate this task and the fix is the model/L2/scope, not a
// number (§9.2 step 5).
func (c *SimCalibrator) RecommendThreshold(fprBudget float64) (float64, bool) {
	if c == nil || c.pos < c.warmup || c.neg < c.warmup {
		return 0, false
	}
	if c.a <= 0 {
		return 0, false
	}
	if fprBudget <= 0 {
		fprBudget = 1e-6
	}
	target := 1 - fprBudget // required P(correct)
	// Solve sigmoid(a·feature(s) + b) = target ⇒ feature(s) = (logit(target) − b)/a,
	// then invert feature: s = simCenter + feature(s)/simScale.
	f := (math.Log(target/(1-target)) - c.b) / c.a
	s := simCenter + f/simScale
	if s < 0 {
		s = 0
	}
	if s > 1 {
		return 1, true // even sim 1.0 doesn't clear the budget — effectively don't serve
	}
	return round3(s), true
}

// Counts returns the graded would-serve tallies (correct, false-hit).
func (c *SimCalibrator) Counts() (correct, falseHit int) {
	if c == nil {
		return 0, 0
	}
	return c.pos, c.neg
}
