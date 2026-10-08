package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"strconv"
)

// gather renders a registry's series as text, without depending on a testutil
// signature that has moved between client_golang versions.
func gather(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			b.WriteString(mf.GetName())
			b.WriteString("{")
			for _, l := range m.GetLabel() {
				b.WriteString(l.GetName() + "=\"" + l.GetValue() + "\",")
			}
			b.WriteString("} ")
			var v float64
			if g := m.GetGauge(); g != nil {
				v = g.GetValue()
			}
			b.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// OPS-55: llm_router_provider_health is a bare 1/0, so an alert built on it can
// fire but cannot say WHY. On 2026-10-05 every Claude path in TAS was down
// because our own prepaid account had no credit, and the only available signal
// said "anthropic unhealthy" -- which reads as the vendor being down.
func TestProviderUnhealthyReason_ExposesTheReason(t *testing.T) {
	reasons := map[string]string{"anthropic": "credit_exhausted"}
	c := &reasonCollector{
		desc: prometheus.NewDesc(
			"llm_router_provider_unhealthy_reason", "test",
			[]string{"provider", "reason"}, nil,
		),
		reasons: func() map[string]string { return reasons },
	}

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	s := gather(t, reg)
	if !strings.Contains(s, `provider="anthropic"`) || !strings.Contains(s, `reason="credit_exhausted"`) {
		t.Errorf("the reason is not on the series:\n%s", s)
	}

	// A healthy provider must emit NO series, so the metric is ABSENT rather
	// than a zero an alert has to equality-test -- and absent() still
	// distinguishes "nothing wrong" from "nothing scraping".
	reasons = map[string]string{}
	if s2 := gather(t, reg); strings.Contains(s2, "llm_router_provider_unhealthy_reason{") {
		t.Errorf("a healthy provider emitted a series:\n%s", s2)
	}
}

// A nil source must not panic: the collector is scraped on a schedule the
// router does not control.
func TestProviderUnhealthyReason_NilSourceIsSafe(t *testing.T) {
	c := &reasonCollector{
		desc:    prometheus.NewDesc("llm_router_provider_unhealthy_reason", "test", []string{"provider", "reason"}, nil),
		reasons: nil,
	}
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	_ = gather(t, reg) // must not panic

}
