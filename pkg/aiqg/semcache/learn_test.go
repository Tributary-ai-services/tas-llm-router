package semcache

import (
	"math"
	"testing"
)

// TestSimCalibrator_LearnsSeparableCurve trains on data where high similarity is
// reliably correct and low similarity reliably a false hit, then checks the fit
// is monotone and the recommended threshold sits between the two clusters and
// tightens as the FPR budget shrinks.
func TestSimCalibrator_LearnsSeparableCurve(t *testing.T) {
	c := NewSimCalibrator(0.5, 20)
	// A clean, separable world: sim >= 0.97 correct, sim <= 0.90 a false hit.
	for range 200 {
		c.Observe(0.99, true)
		c.Observe(0.985, true)
		c.Observe(0.90, false)
		c.Observe(0.88, false)
	}
	// Monotone: P(correct) rises with similarity.
	if c.Prob(0.99) <= c.Prob(0.90) {
		t.Errorf("Prob should increase with similarity: P(0.99)=%.3f P(0.90)=%.3f", c.Prob(0.99), c.Prob(0.90))
	}
	// The high cluster reads as mostly-correct, the low cluster as mostly-wrong.
	if c.Prob(0.99) < 0.8 {
		t.Errorf("P(correct|0.99)=%.3f, want >=0.8", c.Prob(0.99))
	}
	if c.Prob(0.88) > 0.2 {
		t.Errorf("P(correct|0.88)=%.3f, want <=0.2", c.Prob(0.88))
	}

	// Recommendation lands between the clusters.
	th, ok := c.RecommendThreshold(0.05)
	if !ok {
		t.Fatal("expected a recommendation after warmup")
	}
	if th <= 0.90 || th >= 0.99 {
		t.Errorf("recommended threshold %.3f should sit between the clusters (0.90, 0.99)", th)
	}
	// A tighter budget must not recommend a looser threshold.
	thTight, ok := c.RecommendThreshold(0.001)
	if !ok {
		t.Fatal("expected a recommendation for the tight budget")
	}
	if thTight < th-1e-9 {
		t.Errorf("tighter FPR budget gave a looser threshold: %.3f < %.3f", thTight, th)
	}
}

func TestSimCalibrator_AbstainsBeforeWarmup(t *testing.T) {
	c := NewSimCalibrator(0.5, 20)
	for range 5 {
		c.Observe(0.99, true)
		c.Observe(0.90, false)
	}
	if _, ok := c.RecommendThreshold(0.05); ok {
		t.Error("must abstain before warmup is met in both classes")
	}
}

func TestSimCalibrator_AbstainsWhenPositivesOnly(t *testing.T) {
	// Calibrating on positives alone (no false hits observed) must not yield a
	// number — it's the §9.2 "tunes straight to threshold 0" trap.
	c := NewSimCalibrator(0.5, 20)
	for range 100 {
		c.Observe(0.99, true)
	}
	if _, ok := c.RecommendThreshold(0.05); ok {
		t.Error("must abstain with no observed false hits (only positives)")
	}
}

func TestSimCalibrator_CountsAndProbBounds(t *testing.T) {
	c := NewSimCalibrator(0.5, 1)
	c.Observe(0.99, true)
	c.Observe(0.95, true)
	c.Observe(0.90, false)
	pos, neg := c.Counts()
	if pos != 2 || neg != 1 {
		t.Errorf("Counts = %d/%d, want 2/1", pos, neg)
	}
	for _, s := range []float64{0, 0.5, 0.9, 1} {
		if p := c.Prob(s); p <= 0 || p >= 1 || math.IsNaN(p) {
			t.Errorf("Prob(%.2f)=%v out of (0,1)", s, p)
		}
	}
}

func TestSimCalibrator_NilSafe(t *testing.T) {
	var c *SimCalibrator
	c.Observe(0.9, true) // no panic
	if c.Prob(0.9) != 0 {
		t.Error("nil Prob should be 0")
	}
	if _, ok := c.RecommendThreshold(0.05); ok {
		t.Error("nil calibrator should not recommend")
	}
	if pos, neg := c.Counts(); pos != 0 || neg != 0 {
		t.Error("nil Counts should be 0/0")
	}
}
