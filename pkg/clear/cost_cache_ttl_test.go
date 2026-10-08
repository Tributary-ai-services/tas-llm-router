package clear

import "testing"

// A 1-hour cache WRITE costs 2x input against 1.25x for 5m. Before the SDK
// carried a TTL field the gateway could not request 1h, so the premium was
// deliberately unmodeled; now that it can, not modeling it would under-report
// every 1h creation by 1.6x.
func TestCacheWriteMultiplierFor(t *testing.T) {
	cases := map[string]float64{
		"1h": CacheWrite1hMultiplier,
		"1H": CacheWrite1hMultiplier, // the stamp is not normalised for us
		"5m": CacheWriteMultiplier,
		"":   CacheWriteMultiplier, // no preference means the vendor default, 5m
		"7d": CacheWriteMultiplier, // unrecognised: never guess a premium
	}
	for ttl, want := range cases {
		if got := CacheWriteMultiplierFor(ttl); got != want {
			t.Errorf("CacheWriteMultiplierFor(%q) = %v, want %v", ttl, got, want)
		}
	}
}

// The ratio is the thing that matters, so assert it rather than a dollar figure
// that would need updating whenever the pricing table moves.
func TestCacheAwareCostTTL_1hCreationCosts1_6xOf5m(t *testing.T) {
	const (
		vendor   = "anthropic"
		model    = "claude-haiku-4-5"
		creation = 10000
	)
	five := CacheAwareCostTTL(vendor, model, 0, creation, 0, 0, true, "5m")
	hour := CacheAwareCostTTL(vendor, model, 0, creation, 0, 0, true, "1h")
	if !five.Priced || !hour.Priced {
		t.Fatalf("%s:%s is not in the pricing table", vendor, model)
	}
	ratio := hour.CacheCreationUSD / five.CacheCreationUSD
	if want := CacheWrite1hMultiplier / CacheWriteMultiplier; ratio < want-1e-9 || ratio > want+1e-9 {
		t.Errorf("1h/5m creation cost ratio = %v, want %v", ratio, want)
	}
	if hour.TotalUSD <= five.TotalUSD {
		t.Errorf("1h total %v is not dearer than 5m total %v", hour.TotalUSD, five.TotalUSD)
	}
}

// A cache READ costs the same at either TTL -- only the write rate differs.
func TestCacheAwareCostTTL_ReadsAreTTLIndependent(t *testing.T) {
	five := CacheAwareCostTTL("anthropic", "claude-haiku-4-5", 0, 0, 10000, 0, true, "5m")
	hour := CacheAwareCostTTL("anthropic", "claude-haiku-4-5", 0, 0, 10000, 0, true, "1h")
	if five.CacheReadUSD != hour.CacheReadUSD {
		t.Errorf("read cost moved with TTL: 5m=%v 1h=%v", five.CacheReadUSD, hour.CacheReadUSD)
	}
}

// The old signature must keep its old answer, so existing callers are provably
// unaffected by the new parameter.
func TestCacheAwareCost_UnchangedForExistingCallers(t *testing.T) {
	old := CacheAwareCost("anthropic", "claude-haiku-4-5", 100, 10000, 500, 50, true)
	new5m := CacheAwareCostTTL("anthropic", "claude-haiku-4-5", 100, 10000, 500, 50, true, "5m")
	if old.TotalUSD != new5m.TotalUSD {
		t.Errorf("CacheAwareCost changed: %v vs %v", old.TotalUSD, new5m.TotalUSD)
	}
}
