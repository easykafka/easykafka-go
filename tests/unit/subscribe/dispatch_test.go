package subscribe_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/subscribe/types"
	"github.com/easykafka/easykafka-go/subscribe"
	"github.com/easykafka/easykafka-go/tests/unit/subscribe/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSubscriberDispatchSuccess verifies the handler is invoked and offset committed on success.
func TestSubscriberDispatchSuccess(t *testing.T) {
	var receivedPayloads []string
	var mu sync.Mutex

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		mu.Lock()
		defer mu.Unlock()
		receivedPayloads = append(receivedPayloads, string(payload))
		return nil
	}

	messages := []*types.Message{
		helpers.NewTestMessage("test-topic", 0, 0, "msg-1"),
		helpers.NewTestMessage("test-topic", 0, 1, "msg-2"),
		helpers.NewTestMessage("test-topic", 0, 2, "msg-3"),
	}

	client := &helpers.FakeConsumer{Messages: messages}
	strat := &helpers.FakeStrategy{}

	subscriber := helpers.NewSubscriberOnFake(t, client,
		subscribe.WithHandler(handler),
		subscribe.WithErrorStrategy(strat),
		subscribe.WithPollTimeout(100*time.Millisecond),
	)

	// Cancel after all messages are consumed (the fake returns nil after messages)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := subscriber.Start(ctx)
	require.NoError(t, err)

	// Verify all messages were dispatched to handler
	mu.Lock()
	assert.Equal(t, []string{"msg-1", "msg-2", "msg-3"}, receivedPayloads)
	mu.Unlock()

	// Verify all offsets were committed
	commits := client.StoredOffsets()
	require.Len(t, commits, 3)
	assert.Equal(t, int64(0), commits[0].Offset)
	assert.Equal(t, int64(1), commits[1].Offset)
	assert.Equal(t, int64(2), commits[2].Offset)

	// Verify strategy was never called (no errors)
	assert.Empty(t, strat.HandleCalls())

	// Verify driver lifecycle
	assert.True(t, client.Connected())
	assert.True(t, client.Subscribed())
	assert.True(t, client.Closed())
}

// TestSubscriberDispatchHandlerError verifies the error strategy is called on handler failure
// and offset is committed even for failed message.
func TestSubscriberDispatchHandlerError(t *testing.T) {
	handlerErr := errors.New("processing failed")

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		if string(payload) == "bad-msg" {
			return &types.Failure{Err: handlerErr}
		}
		return nil
	}

	messages := []*types.Message{
		helpers.NewTestMessage("test-topic", 0, 0, "good-msg"),
		helpers.NewTestMessage("test-topic", 0, 1, "bad-msg"),
		helpers.NewTestMessage("test-topic", 0, 2, "good-msg-2"),
	}

	client := &helpers.FakeConsumer{Messages: messages}
	strat := &helpers.FakeStrategy{} // Returns nil = continue consumption

	subscriber := helpers.NewSubscriberOnFake(t, client,
		subscribe.WithHandler(handler),
		subscribe.WithErrorStrategy(strat),
		subscribe.WithPollTimeout(100*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := subscriber.Start(ctx)
	require.NoError(t, err)

	// Verify only good messages had their offsets committed
	commits := client.StoredOffsets()
	require.Len(t, commits, 3)
	assert.Equal(t, int64(0), commits[0].Offset) // good-msg
	assert.Equal(t, int64(1), commits[1].Offset) // bad-msg
	assert.Equal(t, int64(2), commits[2].Offset) // good-msg-2

	// Verify strategy was called once with the bad message
	calls := strat.HandleCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, handlerErr, calls[0].HandlerErr)
	require.Len(t, calls[0].Msgs, 1)
	assert.Equal(t, int64(1), calls[0].Msgs[0].Offset)
}

// TestSubscriberDispatchStrategyFatal verifies the subscriber stops when strategy returns an error.
func TestSubscriberDispatchStrategyFatal(t *testing.T) {
	strategyErr := errors.New("fatal: must stop")

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		return &types.Failure{Err: errors.New("handler error")}
	}

	messages := []*types.Message{
		helpers.NewTestMessage("test-topic", 0, 0, "msg-1"),
		helpers.NewTestMessage("test-topic", 0, 1, "msg-2"),
	}

	client := &helpers.FakeConsumer{Messages: messages}
	strat := &helpers.FakeStrategy{ReturnErr: strategyErr}

	subscriber := helpers.NewSubscriberOnFake(t, client,
		subscribe.WithHandler(handler),
		subscribe.WithErrorStrategy(strat),
		subscribe.WithPollTimeout(100*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := subscriber.Start(ctx)

	// Subscriber should return error from strategy
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error strategy")

	// Strategy should have been called only once (subscriber stops after first fatal)
	calls := strat.HandleCalls()
	assert.Len(t, calls, 1)

	// No offsets should be committed
	assert.Empty(t, client.StoredOffsets())
}

// TestSubscriberDispatchPanicRecovery verifies that handler panics are recovered
// and treated as errors.
func TestSubscriberDispatchPanicRecovery(t *testing.T) {
	handler := func(ctx context.Context, payload []byte) *types.Failure {
		if string(payload) == "panic-msg" {
			panic("unexpected crash!")
		}
		return nil
	}

	messages := []*types.Message{
		helpers.NewTestMessage("test-topic", 0, 0, "good-msg"),
		helpers.NewTestMessage("test-topic", 0, 1, "panic-msg"),
		helpers.NewTestMessage("test-topic", 0, 2, "good-msg-2"),
	}

	client := &helpers.FakeConsumer{Messages: messages}
	strat := &helpers.FakeStrategy{} // Returns nil = continue

	subscriber := helpers.NewSubscriberOnFake(t, client,
		subscribe.WithHandler(handler),
		subscribe.WithErrorStrategy(strat),
		subscribe.WithPollTimeout(100*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := subscriber.Start(ctx)
	require.NoError(t, err)

	// Verify strategy was called with the panic error
	calls := strat.HandleCalls()
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].HandlerErr.Error(), "handler panic")
	assert.Contains(t, calls[0].HandlerErr.Error(), "unexpected crash!")

	// Verify all messages were committed (even failed ones since error strategy does not commit by itself!)
	commits := client.StoredOffsets()
	require.Len(t, commits, 3)
	assert.Equal(t, int64(0), commits[0].Offset)
	assert.Equal(t, int64(1), commits[1].Offset)
	assert.Equal(t, int64(2), commits[2].Offset)
}

// TestSubscriberContextCancellation verifies the subscriber exits cleanly on context cancellation.
func TestSubscriberContextCancellation(t *testing.T) {
	callCount := 0
	var mu sync.Mutex

	ctx, cancel := context.WithCancel(context.Background())

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		mu.Lock()
		callCount++
		count := callCount
		mu.Unlock()

		if count >= 2 {
			cancel() // Cancel after processing 2 messages
		}
		return nil
	}

	// Provide many messages but expect only ~2 to be processed
	messages := make([]*types.Message, 10)
	for i := range messages {
		messages[i] = helpers.NewTestMessage("test-topic", 0, int64(i), fmt.Sprintf("msg-%d", i))
	}

	client := &helpers.FakeConsumer{Messages: messages}
	strat := &helpers.FakeStrategy{}

	subscriber := helpers.NewSubscriberOnFake(t, client,
		subscribe.WithHandler(handler),
		subscribe.WithErrorStrategy(strat),
		subscribe.WithPollTimeout(100*time.Millisecond),
	)

	err := subscriber.Start(ctx)
	require.NoError(t, err)

	// Should have processed at least 2 messages and then stopped
	mu.Lock()
	assert.GreaterOrEqual(t, callCount, 2)
	mu.Unlock()

	assert.True(t, client.Closed())
}

// TestSubscriberConnectError verifies error handling when Kafka connection fails.
func TestSubscriberConnectError(t *testing.T) {
	client := &helpers.FakeConsumer{ConnectErr: errors.New("connection refused")}
	strat := &helpers.FakeStrategy{}

	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

	subscriber := helpers.NewSubscriberOnFake(t, client,
		subscribe.WithHandler(handler),
		subscribe.WithErrorStrategy(strat),
		subscribe.WithPollTimeout(100*time.Millisecond),
	)

	err := subscriber.Start(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to connect")
}

// TestSubscriberSubscribeError verifies error handling when subscription fails.
func TestSubscriberSubscribeError(t *testing.T) {
	client := &helpers.FakeConsumer{SubscribeErr: errors.New("subscription failed")}
	strat := &helpers.FakeStrategy{}

	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

	subscriber := helpers.NewSubscriberOnFake(t, client,
		subscribe.WithHandler(handler),
		subscribe.WithErrorStrategy(strat),
		subscribe.WithPollTimeout(100*time.Millisecond),
	)

	err := subscriber.Start(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to subscribe")
	assert.True(t, client.Closed())
}

// TestSubscriberPollError verifies the subscriber stops on fatal poll errors.
func TestSubscriberPollError(t *testing.T) {
	strat := &helpers.FakeStrategy{}

	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

	// Use a client that returns an error on second poll
	fatalClient := &helpers.FakeFatalPollConsumer{
		Messages:  []*types.Message{helpers.NewTestMessage("test-topic", 0, 0, "msg-1")},
		PollError: errors.New("kafka connection lost"),
	}

	subscriber := helpers.NewSubscriberOnFake(t, fatalClient,
		subscribe.WithHandler(handler),
		subscribe.WithErrorStrategy(strat),
		subscribe.WithPollTimeout(100*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := subscriber.Start(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "polling error")
}

// TestSubscriberMessageContext verifies that message metadata is accessible via context.
func TestSubscriberMessageContext(t *testing.T) {
	var capturedMsg *types.Message

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		// Deliberately the public accessor, not internal/subscribe/metadata: this test is
		// the guard that the re-export in handler.go stays.
		msg, ok := subscribe.MessageFromContext(ctx)
		if ok {
			capturedMsg = msg
		}
		return nil
	}

	messages := []*types.Message{
		{
			Topic:     "ctx-topic",
			Partition: 3,
			Offset:    42,
			Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Headers:   map[string]string{"key": "value"},
			Payload:   []byte("test-payload"),
		},
	}

	client := &helpers.FakeConsumer{Messages: messages}
	strat := &helpers.FakeStrategy{}

	subscriber := helpers.NewSubscriberOnFake(t, client,
		subscribe.WithHandler(handler),
		subscribe.WithErrorStrategy(strat),
		subscribe.WithPollTimeout(100*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := subscriber.Start(ctx)
	require.NoError(t, err)

	require.NotNil(t, capturedMsg)
	assert.Equal(t, "ctx-topic", capturedMsg.Topic)
	assert.Equal(t, int32(3), capturedMsg.Partition)
	assert.Equal(t, int64(42), capturedMsg.Offset)
	assert.Equal(t, "value", capturedMsg.Headers["key"])
}

// TestSubscriberHandlerCannotMoveStoredOffset verifies that changing the message a
// handler reads from its context does not change the offset the subscriber stores,
// nor the message the error strategy receives.
func TestSubscriberHandlerCannotMoveStoredOffset(t *testing.T) {
	handler := func(ctx context.Context, payload []byte) *types.Failure {
		msg, ok := subscribe.MessageFromContext(ctx)
		require.True(t, ok)
		msg.Topic, msg.Partition, msg.Offset = "elsewhere", 9, 1000
		return &types.Failure{Err: errors.New("fail")}
	}

	client := &helpers.FakeConsumer{
		Messages: []*types.Message{helpers.NewTestMessage("topic", 0, 5, "msg")},
	}
	strat := &helpers.FakeStrategy{}
	subscriber := helpers.NewSubscriberOnFake(t, client,
		subscribe.WithHandler(handler),
		subscribe.WithErrorStrategy(strat),
		subscribe.WithPollTimeout(100*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	require.NoError(t, subscriber.Start(ctx))

	assert.Equal(t, []helpers.StoreRecord{{Topic: "topic", Partition: 0, Offset: 5}},
		client.StoredOffsets(), "the stored offset must come from the polled message")

	calls := strat.HandleCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(5), calls[0].Msgs[0].Offset, "the strategy must see the polled message")
}

// TestSubscriberDoubleStartError verifies subscriber prevents double-start.
func TestSubscriberDoubleStartError(t *testing.T) {
	client := &helpers.FakeConsumer{}
	strat := &helpers.FakeStrategy{}

	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

	subscriber := helpers.NewSubscriberOnFake(t, client,
		subscribe.WithHandler(handler),
		subscribe.WithErrorStrategy(strat),
		subscribe.WithPollTimeout(100*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// First start
	_ = subscriber.Start(ctx)

	// Second start should fail
	err := subscriber.Start(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already started")
}
