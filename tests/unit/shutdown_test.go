package unit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/engine"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/easykafka/easykafka-go/tests/unit/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Cancelling the context passed to Start is the only way to stop a consumer, so
// every test in this file triggers shutdown that way.

// TestShutdownStopsFetching verifies that when the engine context is cancelled,
// no new messages are polled from Kafka.
func TestShutdownStopsFetching(t *testing.T) {
	var pollCount atomic.Int32

	// Client that tracks poll calls and blocks after first message
	client := &helpers.SlowPollClient{
		Messages: []*types.Message{
			helpers.NewTestMessage("topic", 0, 0, "msg-1"),
		},
		PollCount: &pollCount,
	}

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		return nil
	}
	strat := &helpers.MockStrategy{}

	eng := engine.NewEngine(client, handler, strat, helpers.TestLogger(), 50)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- eng.Start(ctx)
	}()

	// Wait for at least one poll to complete
	deadline := time.After(2 * time.Second)
	for pollCount.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for initial poll")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Cancel context to trigger shutdown
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop after context cancellation")
	}

	// After cancellation, poll count should have stopped growing
	finalCount := pollCount.Load()
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, finalCount, pollCount.Load(), "polls continued after cancellation")
}

// TestShutdownWaitsForInFlightHandler verifies that Start does not return until
// the in-flight handler has returned. The handler here ignores its context on
// purpose: a handler that keeps working through cancellation holds Start open,
// which is what makes Start's return a usable join point.
func TestShutdownWaitsForInFlightHandler(t *testing.T) {
	handlerStarted := make(chan struct{})
	handlerCompleted := atomic.Bool{}

	// One message that takes time to process
	client := &helpers.MockKafkaClient{
		Messages: []*types.Message{
			helpers.NewTestMessage("topic", 0, 0, "slow-msg"),
		},
	}

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		close(handlerStarted)
		// Simulate slow processing
		time.Sleep(500 * time.Millisecond)
		handlerCompleted.Store(true)
		return nil
	}
	strat := &helpers.MockStrategy{}

	eng := engine.NewEngine(client, handler, strat, helpers.TestLogger(), 50)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- eng.Start(ctx)
	}()

	// Wait for handler to start
	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}

	// Cancel while handler is still processing
	cancel()

	// Start must not return before the handler does
	select {
	case err := <-done:
		require.NoError(t, err)
		assert.True(t, handlerCompleted.Load(), "Start returned before the handler finished")
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop after handler completion")
	}

	// Verify offset was committed for the completed message
	commits := client.StoredOffsets()
	assert.Len(t, commits, 1, "offset should be committed for completed in-flight message")
}

// TestShutdownReturnsAfterFinalCommitAndClose pins the guarantee the documented
// stop pattern rests on: when Start returns there is nothing left to wait for.
// The final commit and the adapter close have both already happened, and no
// further call reaches the adapter afterwards.
func TestShutdownReturnsAfterFinalCommitAndClose(t *testing.T) {
	client := &helpers.RecordingClient{
		Messages: []*types.Message{
			helpers.NewTestMessage("topic", 0, 0, "msg-1"),
		},
	}

	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }
	strat := &helpers.MockStrategy{}

	eng := engine.NewEngine(client, handler, strat, helpers.TestLogger(), 50)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Start(ctx) }()

	require.Eventually(t, func() bool { return client.Polls() > 0 }, 2*time.Second, 10*time.Millisecond,
		"engine never polled")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop after context cancellation")
	}

	calls := client.Calls()
	require.GreaterOrEqual(t, len(calls), 2)
	assert.Equal(t, "close", calls[len(calls)-1], "the adapter should be closed before Start returns")
	assert.Equal(t, "commit", calls[len(calls)-2], "the final commit should happen before the close")

	// Nothing may still be running once Start has returned.
	settled := len(calls)
	time.Sleep(200 * time.Millisecond)
	assert.Len(t, client.Calls(), settled, "the adapter was called after Start returned")
}

// TestShutdownContextCancelsHandlerContext verifies that cancelling the context
// cancels the in-flight handler's context with it. That is deliberate: the
// library does not hand handlers a context that outlives the shutdown, so an
// interrupted message is failed like any other and goes to the error strategy —
// written off under skip, republished under retry.
func TestShutdownContextCancelsHandlerContext(t *testing.T) {
	handlerCtxCancelled := atomic.Bool{}
	handlerStarted := make(chan struct{})

	client := &helpers.BlockingPollClient{
		FirstMessage: helpers.NewTestMessage("topic", 0, 0, "ctx-msg"),
	}

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		close(handlerStarted)
		// Wait for context cancellation
		<-ctx.Done()
		handlerCtxCancelled.Store(true)
		return &types.Failure{Err: ctx.Err()}
	}
	strat := &helpers.MockStrategy{}

	eng := engine.NewEngine(client, handler, strat, helpers.TestLogger(), 50)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- eng.Start(ctx)
	}()

	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}

	// Cancel the context — handler should see it
	cancel()

	select {
	case <-done:
		assert.True(t, handlerCtxCancelled.Load(), "handler context should have been cancelled")
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop")
	}

	// The abandoned message reached the error strategy: the engine cannot tell
	// "this failed" from "this was abandoned", and does not try.
	assert.Len(t, strat.HandleCalls(), 1, "the interrupted message should reach the error strategy")
}

// TestShutdownClosesAdapter verifies that the Kafka adapter is closed
// during shutdown.
func TestShutdownClosesAdapter(t *testing.T) {
	client := &helpers.MockKafkaClient{
		Messages: []*types.Message{
			helpers.NewTestMessage("topic", 0, 0, "msg-1"),
		},
	}

	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }
	strat := &helpers.MockStrategy{}

	eng := engine.NewEngine(client, handler, strat, helpers.TestLogger(), 50)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err)

	closed := client.Closed()

	assert.True(t, closed, "adapter should be closed after engine stops")
}

// TestShutdownBatchModeDropsBuffered verifies that in batch mode a buffer that
// was never dispatched is discarded on shutdown, not flushed.
//
// Dispatching it instead would run a bulk handler while the consumer is
// stopping, hand it a dead context, and let the error strategy advance offsets
// over work that never happened. Nothing is lost by dropping — the offsets were
// never stored, so the messages are re-read by whoever holds the partition next.
func TestShutdownBatchModeDropsBuffered(t *testing.T) {
	var dispatched int
	var mu sync.Mutex

	messages := make([]*types.Message, 5)
	for i := range 5 {
		messages[i] = helpers.NewTestMessage("topic", 0, int64(i), "msg")
	}

	client := &helpers.MockKafkaClient{Messages: messages}

	batchHandler := func(ctx context.Context, batch *types.Batch) *types.Failure {
		mu.Lock()
		dispatched += batch.Len()
		mu.Unlock()
		return nil
	}
	strat := &helpers.MockStrategy{}

	// Batch size well above the message count and a timeout far longer than the
	// test, so the buffer fills and shutdown is the only thing that could flush it.
	eng := engine.NewBatchEngine(client, batchHandler, strat, helpers.TestLogger(), 10, 100, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Start(ctx) }()

	require.Eventually(t, func() bool { return client.PolledCount() == len(messages) },
		2*time.Second, 10*time.Millisecond, "engine did not buffer all messages")

	cancel()
	require.NoError(t, <-done)

	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, dispatched, "buffered messages must not be dispatched on shutdown")
	assert.Empty(t, client.StoredOffsets(), "dropped messages must not advance any offset")
}
