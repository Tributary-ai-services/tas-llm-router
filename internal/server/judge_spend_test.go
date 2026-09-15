package server

import (
	"math"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
)

// These tests pin the accounting added for tas-llm-router#184: gateway-initiated
// evaluation calls (judge scoring, shadow replays) are real billed spend that
// previously appeared in no metric or cost total.
//
// The counters are process-global, so every assertion is a DELTA around the
// call rather than an absolute — otherwise the tests would depend on whether
// another test in this package happened to run first.

const epsilon = 1e-9

// haiku is in the pricing table (pkg/clear/cost.go) at $0.00080/1k input and
// $0.00400/1k output. Using a real priced pair rather than a fixture keeps the
// test honest about the arithmetic the invoice will use.
const (
	pricedVendor = "anthropic"
	pricedModel  = "claude-haiku-4-5-20251001"
)

func TestCountJudge_RecordsTokensAndDollarSpend(t *testing.T) {
	inBefore := testutil.ToFloat64(metrics.JudgeTokensTotal.WithLabelValues("input"))
	outBefore := testutil.ToFloat64(metrics.JudgeTokensTotal.WithLabelValues("output"))
	spendBefore := testutil.ToFloat64(metrics.UnbilledSpendUSDTotal.WithLabelValues(metrics.SpendPathJudge))
	callsBefore := testutil.ToFloat64(metrics.JudgeCallsTotal.WithLabelValues(metrics.JudgeCompleted))

	countJudge(pricedVendor, pricedModel, &types.Usage{PromptTokens: 1000, CompletionTokens: 500})

	if got := testutil.ToFloat64(metrics.JudgeTokensTotal.WithLabelValues("input")); got != inBefore+1000 {
		t.Errorf("input tokens: got delta %v, want 1000", got-inBefore)
	}
	if got := testutil.ToFloat64(metrics.JudgeTokensTotal.WithLabelValues("output")); got != outBefore+500 {
		t.Errorf("output tokens: got delta %v, want 500", got-outBefore)
	}
	if got := testutil.ToFloat64(metrics.JudgeCallsTotal.WithLabelValues(metrics.JudgeCompleted)); got != callsBefore+1 {
		t.Errorf("completed calls: got delta %v, want 1", got-callsBefore)
	}

	// 1000 input @ $0.00080/1k + 500 output @ $0.00400/1k = $0.0028.
	// Asserting the dollar figure, not just that *a* number moved: the whole
	// point of this metric is that tokens alone can't be summed into money.
	const wantCost = 0.0028
	if got := testutil.ToFloat64(metrics.UnbilledSpendUSDTotal.WithLabelValues(metrics.SpendPathJudge)); math.Abs((got-spendBefore)-wantCost) > epsilon {
		t.Errorf("judge spend: got delta %v, want %v", got-spendBefore, wantCost)
	}
}

func TestCountJudge_NilUsageIsADistinctOutcome(t *testing.T) {
	inBefore := testutil.ToFloat64(metrics.JudgeTokensTotal.WithLabelValues("input"))
	noUsageBefore := testutil.ToFloat64(metrics.JudgeCallsTotal.WithLabelValues(metrics.JudgeCompletedNoUsage))
	completedBefore := testutil.ToFloat64(metrics.JudgeCallsTotal.WithLabelValues(metrics.JudgeCompleted))

	countJudge(pricedVendor, pricedModel, nil)

	if got := testutil.ToFloat64(metrics.JudgeCallsTotal.WithLabelValues(metrics.JudgeCompletedNoUsage)); got != noUsageBefore+1 {
		t.Errorf("completed_no_usage: got delta %v, want 1", got-noUsageBefore)
	}
	// A provider that stops reporting usage must not look like a drop in
	// spend, nor be silently folded into the healthy `completed` bucket.
	if got := testutil.ToFloat64(metrics.JudgeCallsTotal.WithLabelValues(metrics.JudgeCompleted)); got != completedBefore {
		t.Errorf("completed must not move on nil usage: got delta %v", got-completedBefore)
	}
	if got := testutil.ToFloat64(metrics.JudgeTokensTotal.WithLabelValues("input")); got != inBefore {
		t.Errorf("tokens must not move on nil usage: got delta %v", got-inBefore)
	}
}

func TestCountSpend_UnpricedModelIsCountedNotDropped(t *testing.T) {
	spendBefore := testutil.ToFloat64(metrics.UnbilledSpendUSDTotal.WithLabelValues(metrics.SpendPathShadowReplay))
	unpricedBefore := testutil.ToFloat64(metrics.UnpricedCallsTotal.WithLabelValues(metrics.SpendPathShadowReplay))

	countSpend(metrics.SpendPathShadowReplay, "anthropic", "not-a-real-model", &types.Usage{PromptTokens: 10, CompletionTokens: 10})

	// The spend total must stay flat — we refuse to fabricate a cost for a
	// model we have no rate for...
	if got := testutil.ToFloat64(metrics.UnbilledSpendUSDTotal.WithLabelValues(metrics.SpendPathShadowReplay)); math.Abs(got-spendBefore) > epsilon {
		t.Errorf("unpriced model must not contribute spend: got delta %v", got-spendBefore)
	}
	// ...but the omission has to be visible, or a missing pricing row makes
	// evaluation look free rather than unmeasured.
	if got := testutil.ToFloat64(metrics.UnpricedCallsTotal.WithLabelValues(metrics.SpendPathShadowReplay)); got != unpricedBefore+1 {
		t.Errorf("unpriced calls: got delta %v, want 1", got-unpricedBefore)
	}
}

func TestEffectiveModel_PrefersRegistryResolution(t *testing.T) {
	// An aliased or fallen-back model bills under the name that actually ran,
	// so pricing keyed on the requested name would mis-price the call.
	if got := effectiveModel(&types.RouterMetadata{ResolvedModel: "claude-haiku-4-5-20251001"}, "claude-latest"); got != "claude-haiku-4-5-20251001" {
		t.Errorf("resolved model should win, got %q", got)
	}
	if got := effectiveModel(&types.RouterMetadata{}, "gpt-4o-mini"); got != "gpt-4o-mini" {
		t.Errorf("empty resolution should fall back to requested, got %q", got)
	}
	if got := effectiveModel(nil, "gpt-4o-mini"); got != "gpt-4o-mini" {
		t.Errorf("nil metadata should fall back to requested, got %q", got)
	}
}
