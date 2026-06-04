package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/IBM/sarama"
)

// KafkaEmitter publishes the paired AIQG envelopes to a Kafka topic.
// Replaces (or runs alongside, via MultiEmitter) the LogEmitter when
// downstream consumers need a durable, partitioned event stream
// instead of unstructured Loki log lines.
//
// Two messages are produced per Emit call:
//   - the request envelope keyed by request_event_id
//   - the response envelope keyed by the SAME request_event_id, so
//     the pair lands on the same Kafka partition and consumers can
//     process them in order
//
// Both messages carry CloudEvents 1.0 Kafka-binding headers (ce-type,
// ce-id, ce-source) so consumers using a CE-aware SDK get structured
// access without parsing the JSON envelope first.
type KafkaEmitter struct {
	Producer sarama.SyncProducer
	Topic    string
}

// NewKafkaEmitter builds a SyncProducer against brokers and returns
// the emitter wrapping it. Configures idempotent + acks=all + a small
// retry budget so a transient broker hiccup doesn't drop events.
//
// Caller owns the returned emitter's lifecycle and must Close() it
// during shutdown so in-flight produces are flushed cleanly.
func NewKafkaEmitter(brokers []string, topic string) (*KafkaEmitter, error) {
	if len(brokers) == 0 {
		return nil, errors.New("events.NewKafkaEmitter: at least one broker required")
	}
	if topic == "" {
		return nil, errors.New("events.NewKafkaEmitter: topic required")
	}

	cfg := sarama.NewConfig()
	cfg.Version = sarama.V3_6_0_0
	cfg.Producer.RequiredAcks = sarama.WaitForAll
	cfg.Producer.Retry.Max = 5
	cfg.Producer.Retry.Backoff = 100 * time.Millisecond
	cfg.Producer.Idempotent = true
	cfg.Producer.Return.Successes = true
	// Idempotent requires MaxOpenRequests=1.
	cfg.Net.MaxOpenRequests = 1
	// Reasonable defaults for low-volume AIQG event traffic.
	cfg.Producer.Compression = sarama.CompressionSnappy
	cfg.Producer.Flush.Frequency = 100 * time.Millisecond

	producer, err := sarama.NewSyncProducer(brokers, cfg)
	if err != nil {
		return nil, fmt.Errorf("events.NewKafkaEmitter: sarama.NewSyncProducer: %w", err)
	}
	return &KafkaEmitter{Producer: producer, Topic: topic}, nil
}

// Emit publishes both envelopes to Kafka. Either message's failure
// returns an error (and the partial state — if the request landed but
// response failed — is logged by the wrapping middleware via the
// shared metrics path; at-least-once semantics from acks=all +
// idempotent producer mean retries can re-deliver without dup-keying).
func (e *KafkaEmitter) Emit(_ context.Context, req RequestEnvelope, resp ResponseEnvelope) (err error) {
	defer recordEmit("kafka", time.Now(), &err, resp)
	if e == nil || e.Producer == nil {
		err = errors.New("events.KafkaEmitter: nil producer")
		return err
	}

	reqMsg, mErr := buildKafkaMessage(e.Topic, req.Type, req.ID, req.Data.RequestEventID, req)
	if mErr != nil {
		err = mErr
		return err
	}
	respMsg, mErr := buildKafkaMessage(e.Topic, resp.Type, resp.ID, resp.Data.RequestEventID, resp)
	if mErr != nil {
		err = mErr
		return err
	}

	if _, _, err = e.Producer.SendMessage(reqMsg); err != nil {
		err = fmt.Errorf("kafka send req event: %w", err)
		return err
	}
	if _, _, err = e.Producer.SendMessage(respMsg); err != nil {
		err = fmt.Errorf("kafka send resp event: %w", err)
		return err
	}
	return nil
}

// Close flushes pending produces and tears down the underlying
// SyncProducer. Safe to call multiple times; subsequent calls no-op.
func (e *KafkaEmitter) Close() error {
	if e == nil || e.Producer == nil {
		return nil
	}
	p := e.Producer
	e.Producer = nil
	return p.Close()
}

// buildKafkaMessage marshals an envelope to JSON and wraps it in a
// ProducerMessage with CloudEvents 1.0 Kafka-binding headers + a
// partition key (request_event_id) so paired (req, resp) messages
// hit the same partition.
func buildKafkaMessage(topic, ceType, ceID, partitionKey string, payload any) (*sarama.ProducerMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return &sarama.ProducerMessage{
		Topic: topic,
		Key:   sarama.StringEncoder(partitionKey),
		Value: sarama.ByteEncoder(body),
		Headers: []sarama.RecordHeader{
			{Key: []byte("ce-specversion"), Value: []byte(SpecVersion)},
			{Key: []byte("ce-type"), Value: []byte(ceType)},
			{Key: []byte("ce-id"), Value: []byte(ceID)},
			{Key: []byte("ce-source"), Value: []byte(Source)},
			{Key: []byte("content-type"), Value: []byte(DataContentType)},
		},
	}, nil
}

// MultiEmitter fans out an Emit call to every wrapped emitter in
// order. Useful during the LogEmitter→KafkaEmitter migration so
// events keep landing in Loki for visibility while consumers wire up
// against Kafka. Returns the first error encountered, but continues
// firing the remaining emitters so a Kafka outage doesn't break log
// observability (or vice-versa).
type MultiEmitter struct {
	Emitters []Emitter
}

// Emit fans out to every configured emitter. First error wins for
// the return value, but every emitter is invoked.
func (m *MultiEmitter) Emit(ctx context.Context, req RequestEnvelope, resp ResponseEnvelope) error {
	var firstErr error
	for _, e := range m.Emitters {
		if e == nil {
			continue
		}
		if err := e.Emit(ctx, req, resp); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
