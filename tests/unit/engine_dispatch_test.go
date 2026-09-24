package unit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/internal/engine"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/easykafka/easykafka-go/tests/unit/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEngineDispatchSuccess verifies the handler is invoked and offset committed on success.
func TestEngineDispatchSuccess(t *testing.T) {
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

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{}

	eng := engine.NewEngine(client, handler, strat, helpers.TestLogger(), 100)

	// Cancel after all messages are consumed (mock returns nil after messages)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)
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

	// Verify adapter lifecycle
	assert.True(t, client.Connected())
	assert.True(t, client.Subscribed())
	assert.True(t, client.Closed())
}

// TestEngineDispatchHandlerError verifies the error strategy is called on handler failure
// and offset is committed even for failed message.
func TestEngineDispatchHandlerError(t *testing.T) {
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

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{} // Returns nil = continue consumption

	eng := engine.NewEngine(client, handler, strat, helpers.TestLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)
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

// TestEngineDispatchStrategyFatal verifies the engine stops when strategy returns an error.
func TestEngineDispatchStrategyFatal(t *testing.T) {
	strategyErr := errors.New("fatal: must stop")

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		return &types.Failure{Err: errors.New("handler error")}
	}

	messages := []*types.Message{
		helpers.NewTestMessage("test-topic", 0, 0, "msg-1"),
		helpers.NewTestMessage("test-topic", 0, 1, "msg-2"),
	}

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{ReturnErr: strategyErr}

	eng := engine.NewEngine(client, handler, strat, helpers.TestLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)

	// Engine should return error from strategy
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error strategy")

	// Strategy should have been called only once (engine stops after first fatal)
	calls := strat.HandleCalls()
	assert.Len(t, calls, 1)

	// No offsets should be committed
	assert.Empty(t, client.StoredOffsets())
}

// TestEngineDispatchPanicRecovery verifies that handler panics are recovered
// and treated as errors.
func TestEngineDispatchPanicRecovery(t *testing.T) {
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

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{} // Returns nil = continue

	eng := engine.NewEngine(client, handler, strat, helpers.TestLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)
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

// TestEngineContextCancellation verifies the engine exits cleanly on context cancellation.
func TestEngineContextCancellation(t *testing.T) {
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

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{}

	eng := engine.NewEngine(client, handler, strat, helpers.TestLogger(), 100)

	err := eng.Start(ctx)
	require.NoError(t, err)

	// Should have processed at least 2 messages and then stopped
	mu.Lock()
	assert.GreaterOrEqual(t, callCount, 2)
	mu.Unlock()

	assert.True(t, client.Closed())
}

// TestEngineConnectError verifies error handling when Kafka connection fails.
func TestEngineConnectError(t *testing.T) {
	client := &helpers.MockKafkaClient{ConnectErr: errors.New("connection refused")}
	strat := &helpers.MockStrategy{}

	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

	eng := engine.NewEngine(client, handler, strat, helpers.TestLogger(), 100)

	err := eng.Start(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to connect")
}

// TestEngineSubscribeError verifies error handling when subscription fails.
func TestEngineSubscribeError(t *testing.T) {
	client := &helpers.MockKafkaClient{SubscribeErr: errors.New("subscription failed")}
	strat := &helpers.MockStrategy{}

	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

	eng := engine.NewEngine(client, handler, strat, helpers.TestLogger(), 100)

	err := eng.Start(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to subscribe")
	assert.True(t, client.Closed())
}

// TestEnginePollError verifies the engine stops on fatal poll errors.
func TestEnginePollError(t *testing.T) {
	strat := &helpers.MockStrategy{}

	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

	// Use a client that returns an error on second poll
	fatalClient := &helpers.FatalPollClient{
		Messages:  []*types.Message{helpers.NewTestMessage("test-topic", 0, 0, "msg-1")},
		PollError: errors.New("kafka connection lost"),
	}

	eng := engine.NewEngine(fatalClient, handler, strat, helpers.TestLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "polling error")
}

// TestEngineMessageContext verifies that message metadata is accessible via context.
func TestEngineMessageContext(t *testing.T) {
	var capturedMsg *types.Message

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		// Deliberately the public accessor, not internal/metadata: this test is
		// the guard that the re-export in handler.go stays.
		msg, ok := easykafka.MessageFromContext(ctx)
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

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{}

	eng := engine.NewEngine(client, handler, strat, helpers.TestLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err)

	require.NotNil(t, capturedMsg)
	assert.Equal(t, "ctx-topic", capturedMsg.Topic)
	assert.Equal(t, int32(3), capturedMsg.Partition)
	assert.Equal(t, int64(42), capturedMsg.Offset)
	assert.Equal(t, "value", capturedMsg.Headers["key"])
}

// TestEngineDoubleStartError verifies engine prevents double-start.
func TestEngineDoubleStartError(t *testing.T) {
	client := &helpers.MockKafkaClient{}
	strat := &helpers.MockStrategy{}

	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

	eng := engine.NewEngine(client, handler, strat, helpers.TestLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// First start
	_ = eng.Start(ctx)

	// Second start should fail
	err := eng.Start(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already started")
}
