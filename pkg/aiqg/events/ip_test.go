package events

import "testing"

func TestTruncateIP(t *testing.T) {
	cases := map[string]string{
		"192.168.68.74":        "192.168.68.0/24",
		"10.0.0.5":             "10.0.0.0/24",
		"203.0.113.42":         "203.0.113.0/24",
		"":                     "",
		"not-an-ip":            "",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1::/48",
	}
	for in, want := range cases {
		if got := truncateIP(in); got != want {
			t.Errorf("truncateIP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApplyIPMode(t *testing.T) {
	const raw = "192.168.68.74"
	if got := applyIPMode(raw, "off"); got != "" {
		t.Errorf("off = %q, want empty", got)
	}
	if got := applyIPMode(raw, "full"); got != raw {
		t.Errorf("full = %q, want raw", got)
	}
	if got := applyIPMode(raw, "minimized"); got != "192.168.68.0/24" {
		t.Errorf("minimized = %q, want /24", got)
	}
	if got := applyIPMode(raw, ""); got != "192.168.68.0/24" {
		t.Errorf("default (empty mode) = %q, want /24 minimized", got)
	}
}
