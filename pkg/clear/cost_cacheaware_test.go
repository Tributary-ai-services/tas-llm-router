package clear

import (
	"math"
	"testing"
)

// haiku input rate is $0.00080/1k, output $0.00400/1k (see modelPricing).
const (
	haikuVendor = "anthropic"
	haikuModel  = "claude-haiku-4-5-20251001"
	haikuIn     = 0.00080
	haikuOut    = 0.00400
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// With zero cache tokens, CacheAwareCost must equal ActualCost exactly, so
// callers with no cache data are unaffected (backward-compatible).
func TestCacheAwareCost_NoCacheEqualsActual(t *testing.T) {
	prompt, completion := 3000, 500
	aware := CacheAwareCost(haikuVendor, haikuModel, prompt, 0, 0, completion, true)
	flat := ActualCost(haikuVendor, haikuModel, prompt, completion, true)
	if !aware.Priced || !flat.Priced {
		t.Fatal("expected priced=true for a known model")
	}
	if !approx(aware.TotalUSD, flat.TotalUSD) {
		t.Fatalf("no-cache total mismatch: aware=%v flat=%v", aware.TotalUSD, flat.TotalUSD)
	}
	if !approx(aware.InputUSD, flat.InputUSD) || !approx(aware.OutputUSD, flat.OutputUSD) {
		t.Fatalf("no-cache in/out mismatch: aware=%+v flat=%+v", aware, flat)
	}
	if !aware.CacheAware {
		t.Fatal("CacheAware flag should be true")
	}
}

// Cache-read (0.10x) and cache-write (1.25x) are priced at the right
// multiples of the input rate.
func TestCacheAwareCost_Multipliers(t *testing.T) {
	uncached, create, read, completion := 1000, 2000, 8000, 400
	c := CacheAwareCost(haikuVendor, haikuModel, uncached, create, read, completion, true)

	wantIn := (float64(uncached) / 1000.0) * haikuIn
	wantCreate := (float64(create) / 1000.0) * haikuIn * 1.25
	wantRead := (float64(read) / 1000.0) * haikuIn * 0.10
	wantOut := (float64(completion) / 1000.0) * haikuOut

	if !approx(c.InputUSD, wantIn) {
		t.Errorf("InputUSD = %v, want %v", c.InputUSD, wantIn)
	}
	if !approx(c.CacheCreationUSD, wantCreate) {
		t.Errorf("CacheCreationUSD = %v, want %v (1.25x input)", c.CacheCreationUSD, wantCreate)
	}
	if !approx(c.CacheReadUSD, wantRead) {
		t.Errorf("CacheReadUSD = %v, want %v (0.10x input)", c.CacheReadUSD, wantRead)
	}
	if !approx(c.OutputUSD, wantOut) {
		t.Errorf("OutputUSD = %v, want %v", c.OutputUSD, wantOut)
	}
	if !approx(c.TotalUSD, wantIn+wantCreate+wantRead+wantOut) {
		t.Errorf("TotalUSD = %v, want %v", c.TotalUSD, wantIn+wantCreate+wantRead+wantOut)
	}
}

// The whole point: naive flat-rate pricing of a cache-heavy request (pricing
// every input token at the full rate) overstates the real cache-aware cost.
// Here ~89% of input is cache-read; the flat estimate should be several times
// the cache-aware total.
func TestCacheAwareCost_FlatOverstatesOnCacheHeavy(t *testing.T) {
	uncached, create, read, completion := 1000, 0, 8000, 200
	aware := CacheAwareCost(haikuVendor, haikuModel, uncached, create, read, completion, true)

	// Flat: price ALL input (uncached + cache-read) at the full input rate.
	flat := ActualCost(haikuVendor, haikuModel, uncached+read, completion, true)

	if flat.TotalUSD <= aware.TotalUSD {
		t.Fatalf("flat (%v) should exceed cache-aware (%v)", flat.TotalUSD, aware.TotalUSD)
	}
	ratio := flat.InputUSD / (aware.InputUSD + aware.CacheReadUSD)
	if ratio < 3.0 {
		t.Errorf("expected flat input to overstate >3x on cache-heavy traffic, got %.2fx", ratio)
	}
}

func TestCacheAwareCost_UnpricedModel(t *testing.T) {
	c := CacheAwareCost("acme", "nope", 100, 100, 100, 100, true)
	if c.Priced {
		t.Fatal("unknown vendor:model must return Priced=false")
	}
}
