package unit

import (
	"context"
	"errors"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/internal/engine"
	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHeaderKeysMatchInternal pins the exported header constants to the internal
// ones the library actually writes.
//
// The exported constants spell their values out rather than aliasing
// metadata.Header*, so that a caller reading the generated documentation sees
// "easykafka.retry.attempt" instead of a reference into a package they cannot
// open. That readability costs a second copy of each string, and this test is
// what stops the two drifting: a rename on either side fails here.
func TestHeaderKeysMatchInternal(t *testing.T) {
	cases := []struct {
		name     string
		exported string
		internal string
	}{
		{"retry attempt", easykafka.HeaderRetryAttempt, metadata.HeaderRetryAttempt},
		{"retry time", easykafka.HeaderRetryTime, metadata.HeaderRetryTime},
		{"retry step", easykafka.HeaderRetryStep, metadata.HeaderRetryStep},
		{"error code", easykafka.HeaderErrorCode, metadata.HeaderErrorCode},
		{"error message", easykafka.HeaderErrorMessage, metadata.HeaderErrorMessage},
		{"original topic", easykafka.HeaderOriginalTopic, metadata.HeaderOriginalTopic},
		{"original partition", easykafka.HeaderOriginalPartition, metadata.HeaderOriginalPartition},
		{"original offset", easykafka.HeaderOriginalOffset, metadata.HeaderOriginalOffset},
		{"failed at", easykafka.HeaderFailedAt, metadata.HeaderFailedAt},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.internal, tc.exported,
				"exported header key has drifted from the one the library writes")
		})
	}
}

// TestPublicAccessorsReadRetryHeaders verifies the exported accessors read the
// headers the library itself builds, rather than a hand-written map that could
// agree with the accessors while both disagree with the wire format.
func TestPublicAccessorsReadRetryHeaders(t *testing.T) {
	retryTime := time.Date(2026, 2, 9, 10, 35, 0, 0, time.UTC)

	original := &types.Message{
		Topic:     "orders",
		Partition: 3,
		Offset:    12345,
		Payload:   []byte("body"),
	}

	// Built by the library, read back through the public API.
	republished := &easykafka.Message{
		Topic:   "orders.retry",
		Headers: metadata.BuildRetryHeaders(original, 2, retryTime, errors.New("db connection failed")),
	}

	assert.Equal(t, 2, easykafka.GetRetryAttempt(republished))
	assert.Equal(t, retryTime, easykafka.GetRetryTime(republished).UTC())
	assert.Equal(t, "orders", easykafka.GetOriginalTopic(republished))
	assert.Equal(t, int32(0), easykafka.GetRetryStep(republished))
}

// TestPublicAccessorsOnFirstDelivery pins the zero values a handler branches on
// to tell an original record from a republished one.
func TestPublicAccessorsOnFirstDelivery(t *testing.T) {
	msg := &easykafka.Message{Topic: "orders", Partition: 0, Offset: 1}

	assert.Equal(t, 0, easykafka.GetRetryAttempt(msg), "a first delivery has no attempts behind it")
	assert.True(t, easykafka.GetRetryTime(msg).IsZero())
	assert.Empty(t, easykafka.GetOriginalTopic(msg))

	// Nil is tolerated rather than panicking: MessageFromContext returns
	// (nil, false) in batch mode, and a caller may not check ok.
	assert.Equal(t, 0, easykafka.GetRetryAttempt(nil))
	assert.True(t, easykafka.GetRetryTime(nil).IsZero())
	assert.Empty(t, easykafka.GetOriginalTopic(nil))
}

// TestMessageFromContextIsEmptyInBatchMode pins a documented limitation rather
// than a bug: a batch handler is given many messages at once and its context
// carries none of them, so the accessor reports false.
//
// This is deliberate. Attaching one message of the batch would be worse than
// attaching none — the accessor would report true with metadata describing a
// single arbitrary element. Real per-message metadata for batches means changing
// BatchHandler's signature away from [][]byte, which is separate work. If that
// ever lands, this test should be replaced, not deleted quietly.
func TestMessageFromContextIsEmptyInBatchMode(t *testing.T) {
	var (
		sawOK       bool
		sawMsg      *easykafka.Message
		payloadSeen int
	)

	batchHandler := func(ctx context.Context, payloads [][]byte) error {
		sawMsg, sawOK = easykafka.MessageFromContext(ctx)
		payloadSeen = len(payloads)
		return nil
	}

	messages := []*types.Message{
		newTestMessage("topic", 0, 0, "a"),
		newTestMessage("topic", 0, 1, "b"),
	}

	client := &mockKafkaClient{messages: messages}
	eng := engine.NewBatchEngine(client, batchHandler, &mockStrategy{}, testLogger(), 100, 2, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.NoError(t, eng.Start(ctx))

	require.Equal(t, 2, payloadSeen, "the batch handler should have run")
	assert.False(t, sawOK, "batch contexts carry no message")
	assert.Nil(t, sawMsg)
}

// TestMessageFromContextEmptyOnBareContext verifies the accessor is safe on a
// context the library never touched, which is what a caller gets if they invoke
// a handler directly from their own tests.
func TestMessageFromContextEmptyOnBareContext(t *testing.T) {
	msg, ok := easykafka.MessageFromContext(context.Background())

	assert.False(t, ok)
	assert.Nil(t, msg)
}
