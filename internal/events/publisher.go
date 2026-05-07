// Package events publishes CloudEvents 1.0 activity events to Kafka for
// consumption by the TAS Live Streams pipeline (Spark + aether-be).
package events

import (
	"context"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/sirupsen/logrus"

	tasevents "github.com/Tributary-ai-services/aether-shared/go-events"
	"github.com/Tributary-ai-services/aether-shared/go-events/kafkabind"
	"github.com/Tributary-ai-services/aether-shared/go-events/payloads"
	"github.com/Tributary-ai-services/aether-shared/go-events/topics"
)

const ceSource = "urn:tas:service:llm-router"

type kafkaWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// Publisher fire-and-forget publishes LLM request/response activity events.
// A nil Publisher is safe to call: every method becomes a no-op. This means
// the server can be constructed without Kafka and the publisher simply
// disappears from the hot path.
type Publisher struct {
	writer  kafkaWriter
	logger  *logrus.Logger
	timeout time.Duration
}

// Config configures the Publisher. Brokers is required; everything else
// has sensible defaults.
type Config struct {
	Brokers      []string
	Topic        string
	Logger       *logrus.Logger
	WriteTimeout time.Duration
}

// New returns a Publisher backed by a kafka-go Writer. Returns nil if
// brokers is empty — callers can pass the result directly to the server
// and skip a nil check.
func New(cfg Config) *Publisher {
	if len(cfg.Brokers) == 0 {
		return nil
	}
	topic := cfg.Topic
	if topic == "" {
		topic = topics.ActivityLLM
	}
	logger := cfg.Logger
	if logger == nil {
		logger = logrus.New()
	}
	timeout := cfg.WriteTimeout
	if timeout == 0 {
		timeout = 2 * time.Second
	}

	writer := &kafka.Writer{
		Addr:         kafka.TCP(cfg.Brokers...),
		Topic:        topic,
		Balancer:     &kafka.Hash{},
		BatchTimeout: 10 * time.Millisecond,
		WriteTimeout: timeout,
		RequiredAcks: kafka.RequireOne,
	}

	return &Publisher{writer: writer, logger: logger, timeout: timeout}
}

// NewWithWriter is for tests — inject a fake writer.
func NewWithWriter(w kafkaWriter, logger *logrus.Logger) *Publisher {
	if logger == nil {
		logger = logrus.New()
	}
	return &Publisher{writer: w, logger: logger, timeout: 2 * time.Second}
}

// PublishRequest emits com.tas.activity.llm.request when a chat request
// enters the router. subject is the request ID (so the request and its
// response correlate via Kafka key on the consumer side).
func (p *Publisher) PublishRequest(ctx context.Context, tenantID, userID, requestID string, payload payloads.LLMRequest) {
	p.publish(ctx, payloads.TypeLLMRequest, tenantID, userID, requestID, requestID, payload)
}

// PublishResponse emits com.tas.activity.llm.response when the router
// completes a chat response (success or error). subject is the request ID.
func (p *Publisher) PublishResponse(ctx context.Context, tenantID, userID, requestID string, payload payloads.LLMResponse) {
	p.publish(ctx, payloads.TypeLLMResponse, tenantID, userID, requestID, requestID, payload)
}

func (p *Publisher) publish(ctx context.Context, eventType, tenantID, userID, requestID, subject string, data any) {
	if p == nil || p.writer == nil {
		return
	}

	ce := tasevents.New(eventType, ceSource,
		tasevents.WithTenant(tenantID, ""),
		tasevents.WithUser(userID),
		tasevents.WithRequest(requestID),
		tasevents.WithSubject(subject),
		tasevents.WithSeverity(tasevents.SeverityInfo),
		tasevents.WithData(data),
	)

	value, headers, err := kafkabind.Encode(ce)
	if err != nil {
		p.logger.WithError(err).WithField("type", eventType).Warn("llm-router events: encode failed")
		return
	}

	writeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	msg := kafka.Message{
		Key:     kafkabind.MessageKey(ce),
		Value:   value,
		Headers: headers,
		Time:    ce.Time,
	}
	if err := p.writer.WriteMessages(writeCtx, msg); err != nil {
		p.logger.WithError(err).WithField("type", eventType).Warn("llm-router events: publish failed")
	}
}

// Close flushes pending messages.
func (p *Publisher) Close() error {
	if p == nil || p.writer == nil {
		return nil
	}
	return p.writer.Close()
}
