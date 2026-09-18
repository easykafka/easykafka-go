package unit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/engine"
	"github.com/easykafka/easykafka-go/internal/types"
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
	client := &slowPollClient{
		messages: []*types.Message{
			newTestMessage("topic", 0, 0, "msg-1"),
		},
		pollCount: &pollCount,
	}

	handler := func(ctx context.Context, payload []byte) error {
		return nil
	}
	strat := &mockStrategy{}

	eng := engine.NewEngine(client, handler, strat, testLogger(), 50)

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
	client := &mockKafkaClient{
		messages: []*types.Message{
			newTestMessage("topic", 0, 0, "slow-msg"),
		},
	}

	handler := func(ctx context.Context, payload []byte) error {
		close(handlerStarted)
		// Simulate slow processing
		time.Sleep(500 * time.Millisecond)
		handlerCompleted.Store(true)
		return nil
	}
	strat := &mockStrategy{}

	eng := engine.NewEngine(client, handler, strat, testLogger(), 50)

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
	commits := client.getStoredOffsets()
	assert.Len(t, commits, 1, "offset should be committed for completed in-flight message")
}

// TestShutdownReturnsAfterFinalCommitAndClose pins the guarantee the documented
// stop pattern rests on: when Start returns there is nothing left to wait for.
// The final commit and the adapter close have both already happened, and no
// further call reaches the adapter afterwards.
func TestShutdownReturnsAfterFinalCommitAndClose(t *testing.T) {
	client := &recordingClient{
		messages: []*types.Message{
			newTestMessage("topic", 0, 0, "msg-1"),
		},
	}

	handler := func(ctx context.Context, payload []byte) error { return nil }
	strat := &mockStrategy{}

	eng := engine.NewEngine(client, handler, strat, testLogger(), 50)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Start(ctx) }()

	require.Eventually(t, func() bool { return client.polls() > 0 }, 2*time.Second, 10*time.Millisecond,
		"engine never polled")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop after context cancellation")
	}

	calls := client.calls()
	require.GreaterOrEqual(t, len(calls), 2)
	assert.Equal(t, "close", calls[len(calls)-1], "the adapter should be closed before Start returns")
	assert.Equal(t, "commit", calls[len(calls)-2], "the final commit should happen before the close")

	// Nothing may still be running once Start has returned.
	settled := len(calls)
	time.Sleep(200 * time.Millisecond)
	assert.Len(t, client.calls(), settled, "the adapter was called after Start returned")
}

// TestShutdownContextCancelsHandlerContext verifies that cancelling the context
// cancels the in-flight handler's context with it. That is deliberate: the
// library does not hand handlers a context that outlives the shutdown, so an
// interrupted message is failed like any other and goes to the error strategy —
// written off under skip, republished under retry.
func TestShutdownContextCancelsHandlerContext(t *testing.T) {
	handlerCtxCancelled := atomic.Bool{}
	handlerStarted := make(chan struct{})

	client := &blockingPollClient{
		firstMessage: newTestMessage("topic", 0, 0, "ctx-msg"),
	}

	handler := func(ctx context.Context, payload []byte) error {
		close(handlerStarted)
		// Wait for context cancellation
		<-ctx.Done()
		handlerCtxCancelled.Store(true)
		return ctx.Err()
	}
	strat := &mockStrategy{}

	eng := engine.NewEngine(client, handler, strat, testLogger(), 50)

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
	assert.Len(t, strat.getHandleCalls(), 1, "the interrupted message should reach the error strategy")
}

// TestShutdownClosesAdapter verifies that the Kafka adapter is closed
// during shutdown.
func TestShutdownClosesAdapter(t *testing.T) {
	client := &mockKafkaClient{
		messages: []*types.Message{
			newTestMessage("topic", 0, 0, "msg-1"),
		},
	}

	handler := func(ctx context.Context, payload []byte) error { return nil }
	strat := &mockStrategy{}

	eng := engine.NewEngine(client, handler, strat, testLogger(), 50)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err)

	client.mu.Lock()
	closed := client.closed
	client.mu.Unlock()

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
		messages[i] = newTestMessage("topic", 0, int64(i), "msg")
	}

	client := &mockKafkaClient{messages: messages}

	batchHandler := func(ctx context.Context, payloads [][]byte) error {
		mu.Lock()
		dispatched += len(payloads)
		mu.Unlock()
		return nil
	}
	strat := &mockStrategy{}

	// Batch size well above the message count and a timeout far longer than the
	// test, so the buffer fills and shutdown is the only thing that could flush it.
	eng := engine.NewBatchEngine(client, batchHandler, strat, testLogger(), 10, 100, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Start(ctx) }()

	require.Eventually(t, func() bool { return client.getPolledCount() == len(messages) },
		2*time.Second, 10*time.Millisecond, "engine did not buffer all messages")

	cancel()
	require.NoError(t, <-done)

	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, dispatched, "buffered messages must not be dispatched on shutdown")
	assert.Empty(t, client.getStoredOffsets(), "dropped messages must not advance any offset")
}

// ============================================================================
// Test Helper Types
// ============================================================================

// slowPollClient tracks poll count and returns messages, then nil.
type slowPollClient struct {
	mu        sync.Mutex
	messages  []*types.Message
	pollIndex int
	pollCount *atomic.Int32
	connected bool
	closed    bool
}

func (c *slowPollClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
	return nil
}

func (c *slowPollClient) SubscribeToTopic(ctx context.Context) error { return nil }

func (c *slowPollClient) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	c.pollCount.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pollIndex >= len(c.messages) {
		// Simulate blocking poll behavior
		time.Sleep(time.Duration(timeoutMs) * time.Millisecond)
		return nil, nil //nolint:nilnil // nil,nil is the mock poll contract for "no message"
	}
	msg := c.messages[c.pollIndex]
	c.pollIndex++
	return msg, nil
}

func (c *slowPollClient) StoreOffset(topic string, partition int32, offset int64) error {
	return nil
}

func (c *slowPollClient) CommitStored() error {
	return nil
}

func (c *slowPollClient) SetOnRevoke(fn func()) {}

func (c *slowPollClient) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// blockingPollClient returns one message, then blocks until context is cancelled.
type blockingPollClient struct {
	mu           sync.Mutex
	firstMessage *types.Message
	returned     bool
	connected    bool
	closed       bool
}

func (c *blockingPollClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
	return nil
}

func (c *blockingPollClient) SubscribeToTopic(ctx context.Context) error { return nil }

func (c *blockingPollClient) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	c.mu.Lock()
	if !c.returned {
		c.returned = true
		msg := c.firstMessage
		c.mu.Unlock()
		return msg, nil
	}
	c.mu.Unlock()
	// Block until context is cancelled
	select {
	case <-ctx.Done():
		return nil, nil //nolint:nilnil // nil,nil is the mock poll contract for "no message"
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return nil, nil //nolint:nilnil // nil,nil is the mock poll contract for "no message"
	}
}

func (c *blockingPollClient) StoreOffset(topic string, partition int32, offset int64) error {
	return nil
}

func (c *blockingPollClient) CommitStored() error {
	return nil
}

func (c *blockingPollClient) SetOnRevoke(fn func()) {}

func (c *blockingPollClient) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// recordingClient records the order of the calls the engine makes, so a test can
// assert what has already happened by the time Start returns. Poll is counted
// rather than recorded — it fires on every loop iteration and would bury the
// sequence the tests care about.
type recordingClient struct {
	mu        sync.Mutex
	events    []string
	pollCount int
	messages  []*types.Message
	pollIndex int
}

func (c *recordingClient) Connect(ctx context.Context) error { return nil }

func (c *recordingClient) SubscribeToTopic(ctx context.Context) error { return nil }

func (c *recordingClient) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	c.mu.Lock()
	c.pollCount++
	if c.pollIndex >= len(c.messages) {
		c.mu.Unlock()
		time.Sleep(time.Duration(timeoutMs) * time.Millisecond)
		return nil, nil //nolint:nilnil // nil,nil is the mock poll contract for "no message"
	}
	msg := c.messages[c.pollIndex]
	c.pollIndex++
	c.mu.Unlock()
	return msg, nil
}

func (c *recordingClient) StoreOffset(topic string, partition int32, offset int64) error {
	c.record("store")
	return nil
}

func (c *recordingClient) CommitStored() error {
	c.record("commit")
	return nil
}

func (c *recordingClient) SetOnRevoke(fn func()) {}

func (c *recordingClient) Close(ctx context.Context) error {
	c.record("close")
	return nil
}

func (c *recordingClient) record(event string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *recordingClient) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

func (c *recordingClient) polls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pollCount
}
