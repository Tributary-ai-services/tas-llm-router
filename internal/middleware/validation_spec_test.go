package middleware

import (
	"testing"

	"github.com/sirupsen/logrus"
)

// TestValidationMiddlewareUsesEmbeddedSpec covers startup in the container,
// where docs/openapi.yaml is not on disk: the middleware must still initialize
// from the embedded spec rather than failing the server boot.
func TestValidationMiddlewareUsesEmbeddedSpec(t *testing.T) {
	t.Chdir(t.TempDir())

	logger := logrus.New()
	logger.SetOutput(newTestWriter(t))

	for name, specPath := range map[string]string{
		"configured path missing": "docs/openapi.yaml",
		"no path configured":      "",
	} {
		t.Run(name, func(t *testing.T) {
			vm, err := NewValidationMiddleware(&ValidationConfig{
				Enabled:  true,
				SpecPath: specPath,
			}, logger)
			if err != nil {
				t.Fatalf("NewValidationMiddleware() error = %v, want nil", err)
			}
			if vm.router == nil {
				t.Fatal("router is nil, embedded spec was not loaded")
			}
		})
	}
}

type testWriter struct{ t *testing.T }

func newTestWriter(t *testing.T) *testWriter { return &testWriter{t: t} }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
