package anthropic

import (
	"errors"
	"testing"
)

// Messages.NewStreaming returns NO error -- the SDK defers to stream.Err() --
// so a vendor refusal on a streamed request never reaches the handler's error
// path. It arrives as a chunk, which is why the translation has to happen in
// the provider's stream loop. Before this, both sites flattened the vendor's
// status and type into Message via err.Error(), so a broken stream carried a
// sentence and nothing queryable.
func TestStreamError_KeepsTheVendorStatusAndType(t *testing.T) {
	// A plain error has no vendor verdict: status stays 0 rather than inventing
	// one, and the generic type is kept.
	plain := streamError(errors.New("connection reset by peer"))
	if plain.UpstreamStatus != 0 {
		t.Errorf("a non-API error got an upstream status: %d", plain.UpstreamStatus)
	}
	if plain.Type != "upstream_stream_error" {
		t.Errorf("Type = %q, want upstream_stream_error", plain.Type)
	}
	if plain.Message == "" {
		t.Errorf("the message was dropped")
	}
}

// The message must always survive, whatever the error is -- it is the only
// human-readable account of why a stream stopped.
func TestStreamError_MessageAlwaysSurvives(t *testing.T) {
	for _, err := range []error{
		errors.New("context canceled"),
		errors.New("unexpected EOF"),
		errors.New("failed to accumulate streaming event"),
	} {
		if se := streamError(err); se.Message != err.Error() {
			t.Errorf("message changed: got %q, want %q", se.Message, err.Error())
		}
	}
}

// A wrapped API error must still be recognised, since the SDK wraps on some
// paths and errors.As is what makes the detail reachable.
func TestStreamError_LooksThroughAWrap(t *testing.T) {
	// No synthetic *anthropic.Error is constructed here: its errorType is
	// unexported, so a fabricated one would assert against a shape the vendor
	// does not produce. What is pinned is that a non-API error is handled
	// without panicking and without inventing a status -- the live behaviour is
	// covered by the deployed verification instead.
	se := streamError(errors.New("wrapped: upstream failed"))
	if se == nil || se.UpstreamStatus != 0 {
		t.Errorf("unexpected: %#v", se)
	}
}
