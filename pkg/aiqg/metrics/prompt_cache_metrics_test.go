package metrics

import (
	"math"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
	"github.com/tributary-ai/llm-router-waf/pkg/clear"
)

// #100 (savings metric): RecordEvent must translate an event's cache-token
// accounting into prompt-cache volume + savings series, keyed by vendor and
// applied mode, so a dashboard can show the win and a zero-hit alert can catch
// the silent failure (auto-mode requests with no reads).

func TestRecordPromptCache_ReadCreationSavingsAndRequest(t *testing.T) {
	vendor := "anthropic_pctest"
	readBefore := testutil.ToFloat64(PromptCacheReadTokensTotal.WithLabelValues(vendor))
	createBefore := testutil.ToFloat64(PromptCacheCreationTokensTotal.WithLabelValues(vendor))
	savingsBefore := testutil.ToFloat64(PromptCacheSavingsUSDTotal.WithLabelValues(vendor))
	reqBefore := testutil.ToFloat64(PromptCacheRequestsTotal.WithLabelValues(vendor, "passthrough"))

	resp := events.ResponseEnvelope{Data: events.ResponseEvent{
		Vendor:          vendor,
		PromptCacheMode: "passthrough",
		TokenAccounting: &events.TokenAccounting{
			CacheReadTokens:     1000,
			CacheCreationTokens: 400,
			CacheReadCostUSD:    0.002,
		},
	}}
	RecordEvent(resp)

	if got := testutil.ToFloat64(PromptCacheReadTokensTotal.WithLabelValues(vendor)); got != readBefore+1000 {
		t.Errorf("read tokens: got %v want %v", got, readBefore+1000)
	}
	if got := testutil.ToFloat64(PromptCacheCreationTokensTotal.WithLabelValues(vendor)); got != createBefore+400 {
		t.Errorf("creation tokens: got %v want %v", got, createBefore+400)
	}
	// savings = read_cost × (1-mult)/mult — the avoided 90% at a 0.10× read rate.
	wantSavings := savingsBefore + 0.002*(1-clear.CacheReadMultiplier)/clear.CacheReadMultiplier
	if got := testutil.ToFloat64(PromptCacheSavingsUSDTotal.WithLabelValues(vendor)); math.Abs(got-wantSavings) > 1e-9 {
		t.Errorf("savings: got %v want %v", got, wantSavings)
	}
	if got := testutil.ToFloat64(PromptCacheRequestsTotal.WithLabelValues(vendor, "passthrough")); got != reqBefore+1 {
		t.Errorf("requests: got %v want %v", got, reqBefore+1)
	}
}

// Synthetic traffic must not inflate savings or auto-mode volume.
func TestRecordPromptCache_SyntheticExcluded(t *testing.T) {
	vendor := "synthetic_pctest"
	reqBefore := testutil.ToFloat64(PromptCacheRequestsTotal.WithLabelValues(vendor, "passthrough"))
	readBefore := testutil.ToFloat64(PromptCacheReadTokensTotal.WithLabelValues(vendor))

	resp := events.ResponseEnvelope{Data: events.ResponseEvent{
		Vendor:          vendor,
		PromptCacheMode: "passthrough",
		Synthetic:       true,
		TokenAccounting: &events.TokenAccounting{CacheReadTokens: 999, CacheReadCostUSD: 1.0},
	}}
	RecordEvent(resp)

	if got := testutil.ToFloat64(PromptCacheRequestsTotal.WithLabelValues(vendor, "passthrough")); got != reqBefore {
		t.Errorf("synthetic traffic bumped request count: before=%v after=%v", reqBefore, got)
	}
	if got := testutil.ToFloat64(PromptCacheReadTokensTotal.WithLabelValues(vendor)); got != readBefore {
		t.Errorf("synthetic traffic bumped read tokens: before=%v after=%v", readBefore, got)
	}
}

// A request with no cache tokens still counts toward per-mode volume — that is
// the denominator the zero-hit alert divides reads against.
func TestRecordPromptCache_NoTokensStillCountsRequest(t *testing.T) {
	vendor := "notok_pctest"
	reqBefore := testutil.ToFloat64(PromptCacheRequestsTotal.WithLabelValues(vendor, "off"))

	resp := events.ResponseEnvelope{Data: events.ResponseEvent{Vendor: vendor, PromptCacheMode: "off"}}
	RecordEvent(resp)

	if got := testutil.ToFloat64(PromptCacheRequestsTotal.WithLabelValues(vendor, "off")); got != reqBefore+1 {
		t.Errorf("request count not bumped without token accounting: before=%v after=%v", reqBefore, got)
	}
}

// An empty vendor/mode must not produce empty label values (which read as gaps
// in a panel); they fall back to explicit sentinels.
func TestRecordPromptCache_EmptyVendorModeSentinels(t *testing.T) {
	reqBefore := testutil.ToFloat64(PromptCacheRequestsTotal.WithLabelValues("unknown", "none"))
	RecordEvent(events.ResponseEnvelope{Data: events.ResponseEvent{}})
	if got := testutil.ToFloat64(PromptCacheRequestsTotal.WithLabelValues("unknown", "none")); got != reqBefore+1 {
		t.Errorf("empty vendor/mode did not fall back to unknown/none: before=%v after=%v", reqBefore, got)
	}
}
