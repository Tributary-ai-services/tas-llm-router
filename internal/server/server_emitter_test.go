package server

import (
	"io"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
	aiqgmetrics "github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
)

func quietLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

// #176: a Kafka broker that is unreachable at startup must NOT be fatal — the
// gateway degrades to the log emitter and keeps serving, raising
// aiqg_emitter_degraded rather than crash-looping.
func TestBuildAIQGEmitter_KafkaUnavailableDegrades(t *testing.T) {
	aiqgmetrics.EmitterDegraded.Set(0)
	cfg := &AIQGServerConfig{
		EmitterType: "kafka",
		// 127.0.0.1:1 refuses immediately — no long dial timeout.
		Kafka: AIQGKafkaConfig{Brokers: []string{"127.0.0.1:1"}, Topic: "t"},
	}

	em, err := buildAIQGEmitter(cfg, quietLogger())
	if err != nil {
		t.Fatalf("Kafka unavailable must degrade, not error: %v", err)
	}
	if _, ok := em.(*events.LogEmitter); !ok {
		t.Fatalf("expected LogEmitter fallback, got %T", em)
	}
	if v := testutil.ToFloat64(aiqgmetrics.EmitterDegraded); v != 1 {
		t.Errorf("aiqg_emitter_degraded=%v, want 1", v)
	}
}

// "both" degrades the same way — to the log emitter, not a MultiEmitter with a
// dead Kafka leg.
func TestBuildAIQGEmitter_BothDegradesToLog(t *testing.T) {
	aiqgmetrics.EmitterDegraded.Set(0)
	cfg := &AIQGServerConfig{
		EmitterType: "both",
		Kafka:       AIQGKafkaConfig{Brokers: []string{"127.0.0.1:1"}, Topic: "t"},
	}
	em, err := buildAIQGEmitter(cfg, quietLogger())
	if err != nil {
		t.Fatalf("must degrade, not error: %v", err)
	}
	if _, ok := em.(*events.LogEmitter); !ok {
		t.Fatalf("expected LogEmitter fallback, got %T", em)
	}
}

// The log-only path is unaffected and never touches Kafka.
func TestBuildAIQGEmitter_LogUnaffected(t *testing.T) {
	em, err := buildAIQGEmitter(&AIQGServerConfig{EmitterType: "log"}, quietLogger())
	if err != nil {
		t.Fatalf("log emitter must build: %v", err)
	}
	if _, ok := em.(*events.LogEmitter); !ok {
		t.Fatalf("expected LogEmitter, got %T", em)
	}
}
