package routing

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/tributary-ai/llm-router-waf/internal/providers"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// stubProvider is a minimal LLMProvider whose HealthCheck outcome is
// controllable, used to verify the background health prober.
type stubProvider struct {
	name       string
	healthErr  error
	checkCount int32
}

func (s *stubProvider) GetCapabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (s *stubProvider) GetProviderName() string { return s.name }
func (s *stubProvider) ChatCompletion(ctx context.Context, req *types.ChatRequest) (*types.ChatResponse, error) {
	return nil, nil
}
func (s *stubProvider) StreamCompletion(ctx context.Context, req *types.ChatRequest) (<-chan *types.ChatChunk, error) {
	return nil, nil
}
func (s *stubProvider) EstimateCost(req *types.ChatRequest) (*types.CostEstimate, error) {
	return &types.CostEstimate{}, nil
}
func (s *stubProvider) HealthCheck(ctx context.Context) error {
	atomic.AddInt32(&s.checkCount, 1)
	return s.healthErr
}

var _ providers.LLMProvider = (*stubProvider)(nil)

// TestStartHealthChecks_WarmsProvidersWithoutTraffic is the regression test for
// the idle-replica 503: the background prober must mark providers healthy at
// boot without any request ever being routed.
func TestStartHealthChecks_WarmsProvidersWithoutTraffic(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.FatalLevel)
	router := NewRouter(logger)

	healthy := &stubProvider{name: "healthy"}
	broken := &stubProvider{name: "broken", healthErr: errors.New("boom")}
	router.RegisterProvider("healthy", healthy)
	router.RegisterProvider("broken", broken)

	// Before probing: both "unknown".
	for name, st := range router.GetHealthStatus() {
		if st.Status != "unknown" {
			t.Fatalf("provider %s: expected unknown before probing, got %s", name, st.Status)
		}
	}

	router.SetHealthCheckInterval(20 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router.StartHealthChecks(ctx)

	// The immediate probe should populate status without any Route() call.
	deadline := time.Now().Add(2 * time.Second)
	for {
		status := router.GetHealthStatus()
		if status["healthy"].Status == "healthy" && status["broken"].Status == "unhealthy" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("providers not probed in time: healthy=%s broken=%s",
				status["healthy"].Status, status["broken"].Status)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// last_checked must be populated (non-zero) — the original bug left it 0.
	if router.GetHealthStatus()["healthy"].LastChecked == 0 {
		t.Error("expected LastChecked to be set after background probe")
	}

	// The ticker must keep probing on its interval (more than the one boot probe).
	time.Sleep(80 * time.Millisecond)
	if c := atomic.LoadInt32(&healthy.checkCount); c < 2 {
		t.Errorf("expected recurring probes (>=2), got %d", c)
	}
}

// TestSetHealthCheckInterval_IgnoresNonPositive guards the interval setter.
func TestSetHealthCheckInterval_IgnoresNonPositive(t *testing.T) {
	r := NewRouter(logrus.New())
	r.SetHealthCheckInterval(45 * time.Second)
	if r.healthCheckInterval != 45*time.Second {
		t.Fatalf("expected 45s, got %s", r.healthCheckInterval)
	}
	r.SetHealthCheckInterval(0)
	r.SetHealthCheckInterval(-1 * time.Second)
	if r.healthCheckInterval != 45*time.Second {
		t.Fatalf("non-positive interval must be ignored, got %s", r.healthCheckInterval)
	}
}
