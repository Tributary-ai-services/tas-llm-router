package anthropic

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// #2: a probe must distinguish a definitive "model does not exist" (HTTP 404 →
// unavailable) from a transient failure (which must NOT downgrade the model).
func TestProbeErrorMeansUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"404 not found", &anthropic.Error{StatusCode: http.StatusNotFound}, true},
		{"wrapped 404", fmt.Errorf("probe: %w", &anthropic.Error{StatusCode: http.StatusNotFound}), true},
		{"500 server error (transient)", &anthropic.Error{StatusCode: http.StatusInternalServerError}, false},
		{"429 rate limit (transient)", &anthropic.Error{StatusCode: http.StatusTooManyRequests}, false},
		{"529 overloaded (transient)", &anthropic.Error{StatusCode: 529}, false},
		{"plain network error (transient)", errors.New("connection refused"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := probeErrorMeansUnavailable(c.err); got != c.want {
				t.Errorf("probeErrorMeansUnavailable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
