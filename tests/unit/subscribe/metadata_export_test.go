package subscribe_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/subscribe/metadata"
	"github.com/easykafka/easykafka-go/internal/subscribe/types"
	"github.com/easykafka/easykafka-go/subscribe"
	"github.com/easykafka/easykafka-go/tests/unit/subscribe/helpers"
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
		{"retry attempt", subscribe.HeaderRetryAttempt, metadata.HeaderRetryAttempt},
		{"retry time", subscribe.HeaderRetryTime, metadata.HeaderRetryTime},
		{"retry step", subscribe.HeaderRetryStep, metadata.HeaderRetryStep},
		{"error code", subscribe.HeaderErrorCode, metadata.HeaderErrorCode},
		{"error message", subscribe.HeaderErrorMessage, metadata.HeaderErrorMessage},
		{"original topic", subscribe.HeaderOriginalTopic, metadata.HeaderOriginalTopic},
		{"original partition", subscribe.HeaderOriginalPartition, metadata.HeaderOriginalPartition},
		{"original offset", subscribe.HeaderOriginalOffset, metadata.HeaderOriginalOffset},
		{"failed at", subscribe.HeaderFailedAt, metadata.HeaderFailedAt},
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

	// Built by the library from what a handler reported, read back through the
	// public API — the round trip a resume point and an error code exist for.
	failure := subscribe.Failure{Err: errors.New("db connection failed"), Step: 2, Code: "write_failed"}
	republished := &subscribe.Message{
		Topic:   "orders.retry",
		Headers: metadata.BuildRetryHeaders(original, 2, retryTime, failure),
	}

	assert.Equal(t, 2, subscribe.GetRetryAttempt(republished))
	assert.Equal(t, retryTime, subscribe.GetRetryTime(republished).UTC())
	assert.Equal(t, "orders", subscribe.GetOriginalTopic(republished))
	assert.Equal(t, int32(2), subscribe.GetRetryStep(republished))
	assert.Equal(t, "write_failed", subscribe.GetErrorCode(republished))
}

// TestPublicAccessorsOnFirstDelivery pins the zero values a handler branches on
// to tell an original record from a republished one.
func TestPublicAccessorsOnFirstDelivery(t *testing.T) {
	msg := &subscribe.Message{Topic: "orders", Partition: 0, Offset: 1}

	assert.Equal(t, 0, subscribe.GetRetryAttempt(msg), "a first delivery has no attempts behind it")
	assert.True(t, subscribe.GetRetryTime(msg).IsZero())
	assert.Empty(t, subscribe.GetOriginalTopic(msg))
	assert.Zero(t, subscribe.GetRetryStep(msg))
	assert.Empty(t, subscribe.GetErrorCode(msg))

	// Nil is tolerated rather than panicking: MessageFromContext returns
	// (nil, false) in batch mode, and a caller may not check ok.
	assert.Equal(t, 0, subscribe.GetRetryAttempt(nil))
	assert.True(t, subscribe.GetRetryTime(nil).IsZero())
	assert.Empty(t, subscribe.GetOriginalTopic(nil))
	assert.Empty(t, subscribe.GetErrorCode(nil))
}

// TestMessageFromContextIsEmptyInBatchMode pins a documented limitation rather
// than a bug: a batch handler is given many messages at once and its context
// carries none of them, so the accessor reports false.
//
// This is deliberate. Attaching one message of the batch would be worse than
// attaching none — the accessor would report true with metadata describing a
// single arbitrary element. Nor is anything missing: a batch handler reads each
// message's metadata from item.Message(), which a shared context could not do
// better.
func TestMessageFromContextIsEmptyInBatchMode(t *testing.T) {
	var (
		sawOK       bool
		sawMsg      *subscribe.Message
		payloadSeen int
	)

	batchHandler := func(ctx context.Context, batch *types.Batch) *types.Failure {
		sawMsg, sawOK = subscribe.MessageFromContext(ctx)
		payloadSeen = batch.Len()
		return nil
	}

	messages := []*types.Message{
		helpers.NewTestMessage("topic", 0, 0, "a"),
		helpers.NewTestMessage("topic", 0, 1, "b"),
	}

	client := &helpers.FakeConsumer{Messages: messages}
	subscriber := helpers.NewSubscriberOnFake(t, client,
		subscribe.WithBatchHandler(batchHandler),
		subscribe.WithErrorStrategy(&helpers.FakeStrategy{}),
		subscribe.WithPollTimeout(100*time.Millisecond),
		subscribe.WithBatchSize(2),
		subscribe.WithBatchTimeout(5*time.Second),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.NoError(t, subscriber.Start(ctx))

	require.Equal(t, 2, payloadSeen, "the batch handler should have run")
	assert.False(t, sawOK, "batch contexts carry no message")
	assert.Nil(t, sawMsg)
}

// TestMessageFromContextEmptyOnBareContext verifies the accessor is safe on a
// context the library never touched, which is what a caller gets if they invoke
// a handler directly from their own tests.
func TestMessageFromContextEmptyOnBareContext(t *testing.T) {
	msg, ok := subscribe.MessageFromContext(context.Background())

	assert.False(t, ok)
	assert.Nil(t, msg)
}
