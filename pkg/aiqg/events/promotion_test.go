package events

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

// Promotion is what makes a field queryable. A field can be populated on the
// envelope for months and answer nothing, which is the shape of defect AIQG-41
// closed for cache cost and this closes for attribution.
//
// Measured 2026-10-06 over 96h of code_generation traffic: 88 priced response
// events, NONE attributable to a calling app, because source_app landed on the
// REQUEST event and cost on the RESPONSE event — joinable only on
// request_event_id, which no dashboard query does.
func emitAndParse(t *testing.T, resp ResponseEnvelope) map[string]interface{} {
	t.Helper()
	var buf bytes.Buffer
	lg := logrus.New()
	lg.SetOutput(&buf)
	lg.SetFormatter(&logrus.JSONFormatter{})
	lg.SetLevel(logrus.InfoLevel)

	e := &LogEmitter{Logger: lg}
	if err := e.Emit(context.Background(), RequestEnvelope{}, resp); err != nil {
		t.Fatalf("emit: %v", err)
	}
	// Selected on the log message rather than the CloudEvents type, because
	// the type lives in `ce_type` and an envelope under test need not set it.
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]interface{}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if msg, _ := m["msg"].(string); strings.Contains(msg, "response event") {
			return m
		}
	}
	t.Fatalf("no response line emitted; got:\n%s", buf.String())
	return nil
}

func TestEmitter_PromotesSourceAppOntoTheResponseStream(t *testing.T) {
	resp := ResponseEnvelope{}
	resp.Data.SourceApp = "claude-code"
	resp.Data.Model = "claude-sonnet-5-5"
	resp.Data.Workflow = "code_generation"
	resp.Data.TokenAccounting = &TokenAccounting{
		PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120,
		TotalCostUSD:        0.001,
		ModelPricingVersion: "pricing-v2026-10-06.1",
	}
	m := emitAndParse(t, resp)

	if got, _ := m["source_app"].(string); got != "claude-code" {
		t.Errorf("source_app not promoted (got %v) — cost stays unattributable to an app", m["source_app"])
	}
	// Needed because two pricing tables existed on one date, so "what rates
	// priced this row" has to be answerable per row.
	if got, _ := m["model_pricing_version"].(string); got != "pricing-v2026-10-06.1" {
		t.Errorf("model_pricing_version not promoted (got %v)", m["model_pricing_version"])
	}
}

// Absent stays absent: an empty source_app must not emit a key, or every
// unattributed row grows a misleading empty dimension.
func TestEmitter_OmitsSourceAppWhenAbsent(t *testing.T) {
	resp := ResponseEnvelope{}
	resp.Data.Model = "claude-sonnet-5-5"
	resp.Data.TokenAccounting = &TokenAccounting{PromptTokens: 1, CompletionTokens: 1, TotalCostUSD: 0.00001}
	m := emitAndParse(t, resp)
	if _, present := m["source_app"]; present {
		t.Errorf("source_app emitted for traffic that declared none: %v", m["source_app"])
	}
	if _, present := m["model_pricing_version"]; present {
		t.Errorf("model_pricing_version emitted when unset: %v", m["model_pricing_version"])
	}
}
