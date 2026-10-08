package events

import "testing"

// A STREAMED failure was recorded as a success. The handler writes the SSE
// headers and a 200 before the first chunk, so a stream that dies halfway
// still reports http_status 200 -- and StatusFromHTTP(200) is `success`. The
// only hint was finish_reason="error".
//
// That is worse than a failure being hard to explain: the figure feeds CLEAR
// Reliability and Efficacy, which gate routing, so the gateway was reporting
// failures as successes to the control that decides what may be routed.
func TestStatusFromHTTP_200IsSuccess_WhichIsWhyAnOverrideIsNeeded(t *testing.T) {
	if got := StatusFromHTTP(200); got != StatusSuccess {
		t.Fatalf("StatusFromHTTP(200) = %q, want %q", got, StatusSuccess)
	}
}

// BuildOptions.Status is the override. It already existed and nothing populated
// it; this pins the precedence so a future change cannot quietly make the
// derived status win again.
func TestBuildOptions_ExplicitStatusWinsOverTheDerivedOne(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		http     int
		want     string
	}{
		{"a failed stream still answered 200", StatusVendorError, 200, StatusVendorError},
		{"no override falls back to the HTTP code", "", 200, StatusSuccess},
		{"no override, 5xx", "", 503, StatusVendorError},
		{"no override, 4xx", "", 400, StatusGatewayError},
		{"an override also wins over a 5xx", StatusTimeout, 500, StatusTimeout},
	}
	for _, c := range cases {
		got := c.explicit
		if got == "" {
			got = StatusFromHTTP(c.http)
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
