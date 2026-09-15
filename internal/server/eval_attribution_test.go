package server

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
)

// captureEmitter records what was emitted instead of shipping it anywhere.
type captureEmitter struct {
	reqs  []events.RequestEnvelope
	resps []events.ResponseEnvelope
	err   error
}

func (c *captureEmitter) Emit(_ context.Context, req events.RequestEnvelope, resp events.ResponseEnvelope) error {
	c.reqs = append(c.reqs, req)
	c.resps = append(c.resps, resp)
	return c.err
}

func testAttr() *evalAttribution {
	return &evalAttribution{
		TenantID:          "tenant-1",
		AIQGAccountID:     "acct-1",
		TokenID:           "tok-1",
		SourceApp:         "checkout",
		ParentEventID:     "resp-evt-123",
		ExperimentID:      "exp-1",
		ExperimentVariant: "control",
		Workflow:          "single_turn_qa",
		Path:              "judge",
	}
}

func TestEvalAttribution_ContextRoundTrip(t *testing.T) {
	a := testAttr()
	got := evalAttributionFrom(withEvalAttribution(context.Background(), a))
	if got == nil || got.TenantID != "tenant-1" || got.ParentEventID != "resp-evt-123" {
		t.Fatalf("attribution did not survive the context: %+v", got)
	}
	// A bare context must yield nil rather than a zero-valued struct — an
	// empty tenant would emit an event into a blank attribution bucket.
	if evalAttributionFrom(context.Background()) != nil {
		t.Error("bare context should carry no attribution")
	}
	//nolint:staticcheck // deliberately passing a nil context to prove it is safe
	if evalAttributionFrom(nil) != nil {
		t.Error("nil context should carry no attribution")
	}
	if withEvalAttribution(context.Background(), nil) == nil {
		t.Error("withEvalAttribution(nil) should return a usable context")
	}
}

func TestEmitEvalEvent_CarriesTenantAndParentReference(t *testing.T) {
	cap := &captureEmitter{}
	jr := &judgeRunner{emitter: cap, region: "us-east-1", log: logrus.New()}

	jr.emitEvalEvent(context.Background(), testAttr(), "anthropic", "claude-haiku-4-5-20251001",
		&types.Usage{PromptTokens: 1000, CompletionTokens: 500}, "stop")

	if len(cap.resps) != 1 {
		t.Fatalf("expected one emitted event, got %d", len(cap.resps))
	}
	d := cap.resps[0].Data

	// The whole point of the event path: attribution metrics cannot carry.
	if d.TenantID != "tenant-1" {
		t.Errorf("tenant_id = %q, want tenant-1", d.TenantID)
	}
	if d.ExperimentID != "exp-1" || d.ExperimentVariant != "control" {
		t.Errorf("experiment attribution lost: %q/%q", d.ExperimentID, d.ExperimentVariant)
	}
	if d.Vendor != "anthropic" || d.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("vendor/model = %q/%q", d.Vendor, d.Model)
	}

	// Marked synthetic, with a reason that says which kind of non-customer
	// traffic it is. "declared" would claim a customer header said so.
	if !d.Synthetic {
		t.Error("evaluation spend must be marked synthetic or it inflates customer usage")
	}
	if d.SyntheticReason != events.SyntheticGatewayEval {
		t.Errorf("synthetic_reason = %q, want %q", d.SyntheticReason, events.SyntheticGatewayEval)
	}

	// The parent reference is what lets eval spend be joined back to the
	// request that caused it.
	if d.AgentContext == nil {
		t.Fatal("no agent context, so no parent reference")
	}
	if d.AgentContext.ParentStepID != "resp-evt-123" {
		t.Errorf("parent_step_id = %q, want resp-evt-123", d.AgentContext.ParentStepID)
	}
	// Linked means evidence matched (an echoed tool_call_id). This call proved
	// no such thing, so identity must not be promoted to the linked tier.
	if d.AgentContext.IdentitySource == "linked" {
		t.Error("identity_source must not claim `linked` for a gateway eval call")
	}
}

func TestEmitEvalEvent_SkipsWhenUnattributable(t *testing.T) {
	cases := []struct {
		name string
		jr   *judgeRunner
		attr *evalAttribution
	}{
		{"no emitter", &judgeRunner{log: logrus.New()}, testAttr()},
		{"no attribution", &judgeRunner{emitter: &captureEmitter{}, log: logrus.New()}, nil},
		{"empty tenant", &judgeRunner{emitter: &captureEmitter{}, log: logrus.New()}, &evalAttribution{Path: "judge"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Must not panic, and must not emit an event with a blank tenant —
			// that would quietly pollute per-tenant totals.
			tc.jr.emitEvalEvent(context.Background(), tc.attr, "anthropic", "m", &types.Usage{}, "stop")
			if ce, ok := tc.jr.emitter.(*captureEmitter); ok && len(ce.resps) != 0 {
				t.Errorf("emitted %d events, want 0", len(ce.resps))
			}
		})
	}
}

// A nil runner is the judging-disabled path and must stay a no-op.
func TestEmitEvalEvent_NilRunnerIsSafe(t *testing.T) {
	var jr *judgeRunner
	jr.emitEvalEvent(context.Background(), testAttr(), "anthropic", "m", &types.Usage{}, "stop")
}
