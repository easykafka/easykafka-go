package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFailFastStopsConsumerOnFirstError verifies the engine's side of the error-strategy
// contract: a non-nil error from HandleError stops the poll loop and surfaces as Start's
// return value, leaving the remaining messages unconsumed.
//
// The strategies themselves are unit-tested, but this is the only test covering what the
// engine does with what they return. It replaces the circuit-breaker integration test,
// which asserted the same engine path via a strategy that has since been removed.
func TestFailFastStopsConsumerOnFirstError(t *testing.T) {
	t.Log("TestFailFastStopsConsumerOnFirstError started")
	defer t.Log("TestFailFastStopsConsumerOnFirstError finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()

	cluster := helpers.SharedCluster(t)

	topic := helpers.UniqueTopicName(t, "fail-fast")
	consumerGroup := fmt.Sprintf("fail-fast-group-%d", time.Now().UnixNano())

	cluster.CreateTopic(ctx, t, topic, 1)

	// More than one message, so "stopped on the first" is distinguishable from
	// "ran out of messages".
	cluster.ProduceMessages(ctx, t, topic, []string{"msg-1", "msg-2", "msg-3"})

	handlerErr := errors.New("downstream service unavailable")

	var mu sync.Mutex
	var handlerCalls int

	handler := func(_ context.Context, payload []byte) error {
		mu.Lock()
		handlerCalls++
		call := handlerCalls
		mu.Unlock()
		t.Logf("Handler called (%d): %s", call, string(payload))
		return handlerErr
	}

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(consumerGroup),
		easykafka.WithHandler(handler),
		easykafka.WithErrorStrategy(easykafka.NewFailFastStrategy()),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	// The timeout is the failure mode, not the exit mechanism: Start is expected to
	// return on its own well before it fires.
	consumerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	consumerErr := consumer.Start(consumerCtx)

	require.Error(t, consumerErr, "consumer should stop when the error strategy returns an error")
	require.ErrorIs(t, consumerErr, handlerErr, "the handler error should be wrapped all the way out")
	require.NoError(t, consumerCtx.Err(), "consumer should have stopped on its own, not on the timeout")

	mu.Lock()
	finalCalls := handlerCalls
	mu.Unlock()
	assert.Equal(t, 1, finalCalls, "handler should be called exactly once before the consumer stops")
}
