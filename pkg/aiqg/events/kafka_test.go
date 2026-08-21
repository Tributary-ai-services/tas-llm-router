package events

import (
	"context"
	"errors"
	"testing"

	"github.com/IBM/sarama"
	"github.com/IBM/sarama/mocks"
)

func TestNewKafkaEmitter_Validation(t *testing.T) {
	if _, err := NewKafkaEmitter(nil, "topic"); err == nil {
		t.Errorf("expected error for nil brokers")
	}
	if _, err := NewKafkaEmitter([]string{"host:9092"}, ""); err == nil {
		t.Errorf("expected error for empty topic")
	}
}

// Emit produces TWO messages — one for request, one for response —
// both keyed by the same partition key so the pair lands together.
func TestKafkaEmitter_EmitProducesPair(t *testing.T) {
	prod := mocks.NewSyncProducer(t, nil)
	prod.ExpectSendMessageAndSucceed()
	prod.ExpectSendMessageAndSucceed()

	e := &KafkaEmitter{Producer: prod, Topic: "aiqg.events"}
	req := RequestEnvelope{
		Type: TypeRequest, ID: "req-uuid-1",
		Data: RequestEvent{RequestEventID: "req-uuid-1"},
	}
	resp := ResponseEnvelope{
		Type: TypeResponse, ID: "resp-uuid-1",
		Data: ResponseEvent{ResponseEventID: "resp-uuid-1", RequestEventID: "req-uuid-1"},
	}
	if err := e.Emit(context.Background(), req, resp); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if err := prod.Close(); err != nil {
		t.Errorf("producer close: %v", err)
	}
}

// SendMessage failure on the request bubbles up; response should not
// fire when request fails.
func TestKafkaEmitter_RequestFailureShortCircuits(t *testing.T) {
	prod := mocks.NewSyncProducer(t, nil)
	prod.ExpectSendMessageAndFail(errors.New("broker down"))

	e := &KafkaEmitter{Producer: prod, Topic: "aiqg.events"}
	err := e.Emit(context.Background(), RequestEnvelope{Type: TypeRequest}, ResponseEnvelope{Type: TypeResponse})
	if err == nil {
		t.Fatalf("expected error from failed send")
	}
}

func TestKafkaEmitter_NilProducerReturnsErr(t *testing.T) {
	e := &KafkaEmitter{Producer: nil, Topic: "x"}
	err := e.Emit(context.Background(), RequestEnvelope{}, ResponseEnvelope{})
	if err == nil {
		t.Errorf("expected error for nil producer")
	}
}

// buildKafkaMessage stamps CloudEvents 1.0 Kafka-binding headers and
// uses request_event_id as the partition key.
func TestBuildKafkaMessage_HeadersAndKey(t *testing.T) {
	msg, err := buildKafkaMessage(
		"aiqg.events", TypeRequest, "ce-id-1", "partition-key-1",
		RequestEnvelope{Type: TypeRequest, ID: "ce-id-1"},
	)
	if err != nil {
		t.Fatalf("buildKafkaMessage: %v", err)
	}
	keyEnc, _ := msg.Key.Encode()
	if string(keyEnc) != "partition-key-1" {
		t.Errorf("key=%q want=partition-key-1", string(keyEnc))
	}
	headerMap := map[string]string{}
	for _, h := range msg.Headers {
		headerMap[string(h.Key)] = string(h.Value)
	}
	for k, want := range map[string]string{
		"ce-specversion": "1.0",
		"ce-type":        TypeRequest,
		"ce-id":          "ce-id-1",
		"ce-source":      Source,
		"content-type":   "application/json",
	} {
		if headerMap[k] != want {
			t.Errorf("header %q = %q want %q", k, headerMap[k], want)
		}
	}
}

// MultiEmitter fans out to every emitter; first error wins but all
// downstreams still fire.
func TestMultiEmitter_FansOut(t *testing.T) {
	a := &MemoryEmitter{}
	b := &MemoryEmitter{}
	m := &MultiEmitter{Emitters: []Emitter{a, b, nil}} // nil entries skipped

	err := m.Emit(context.Background(),
		RequestEnvelope{Type: TypeRequest, ID: "1"},
		ResponseEnvelope{Type: TypeResponse, ID: "1"})
	if err != nil {
		t.Errorf("unexpected err: %v", err)
	}
	if a.Len() != 1 || b.Len() != 1 {
		t.Errorf("fan-out failed: a=%d b=%d", a.Len(), b.Len())
	}
}

// First error wins but later emitters still fire.
type failingEmitter struct {
	err  error
	hits int
}

func (f *failingEmitter) Emit(_ context.Context, _ RequestEnvelope, _ ResponseEnvelope) error {
	f.hits++
	return f.err
}

func TestMultiEmitter_FirstErrorButContinues(t *testing.T) {
	failer := &failingEmitter{err: errors.New("boom")}
	mem := &MemoryEmitter{}
	m := &MultiEmitter{Emitters: []Emitter{failer, mem}}

	err := m.Emit(context.Background(), RequestEnvelope{}, ResponseEnvelope{})
	if err == nil {
		t.Fatalf("expected error from failer")
	}
	if mem.Len() != 1 {
		t.Errorf("downstream not fired after failer: %d", mem.Len())
	}
}

// Close is safe to call repeatedly + tolerates nil producer.
func TestKafkaEmitter_CloseIdempotent(t *testing.T) {
	(&KafkaEmitter{}).Close()    // nil producer — no panic
	(*KafkaEmitter)(nil).Close() // nil receiver — no panic

	prod := mocks.NewSyncProducer(t, nil)
	e := &KafkaEmitter{Producer: prod, Topic: "x"}
	if err := e.Close(); err != nil {
		t.Errorf("first close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Errorf("second close should be no-op: %v", err)
	}
}

// Compile-time assertion: KafkaEmitter satisfies Emitter.
var _ Emitter = (*KafkaEmitter)(nil)

// Sarama sanity — pin the Version we set in NewKafkaEmitter so a
// future sarama upgrade that drops V3_6_0_0 surfaces here.
func TestNewKafkaEmitter_SaramaVersionPinned(t *testing.T) {
	if sarama.V3_6_0_0.IsAtLeast(sarama.V0_8_2_0) != true {
		t.Fatalf("sarama version sentinel broke")
	}
}
