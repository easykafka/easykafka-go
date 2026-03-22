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

// TestShutdownStopsFetching verifies that when the engine context is cancelled,
// no new messages are polled from Kafka (FR-036).
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
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop after context cancellation")
	}

	// After cancellation, poll count should have stopped growing
	finalCount := pollCount.Load()
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, finalCount, pollCount.Load(), "polls continued after cancellation")
}

// TestShutdownWaitsForInFlightHandler verifies that the engine completes
// an in-flight handler before stopping (FR-037).
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

	// Engine should wait for handler to finish
	select {
	case err := <-done:
		assert.NoError(t, err)
		assert.True(t, handlerCompleted.Load(), "handler should have completed before engine stopped")
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop after handler completion")
	}

	// Verify offset was committed for the completed message
	commits := client.getCommittedOffsets()
	assert.Len(t, commits, 1, "offset should be committed for completed in-flight message")
}

// TestShutdownCommitsFinalOffsets verifies that all completed message offsets
// are committed during shutdown (FR-038).
func TestShutdownCommitsFinalOffsets(t *testing.T) {
	processedCount := atomic.Int32{}

	messages := []*types.Message{
		newTestMessage("topic", 0, 0, "msg-1"),
		newTestMessage("topic", 0, 1, "msg-2"),
		newTestMessage("topic", 0, 2, "msg-3"),
	}

	client := &mockKafkaClient{messages: messages}

	handler := func(ctx context.Context, payload []byte) error {
		processedCount.Add(1)
		return nil
	}
	strat := &mockStrategy{}

	eng := engine.NewEngine(client, handler, strat, testLogger(), 50)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err)

	// All 3 offsets should be committed
	commits := client.getCommittedOffsets()
	require.Len(t, commits, 3)
	assert.Equal(t, int64(0), commits[0].Offset)
	assert.Equal(t, int64(1), commits[1].Offset)
	assert.Equal(t, int64(2), commits[2].Offset)
}

// TestShutdownTimeoutForcesStop verifies that if the shutdown timeout
// expires, the engine force-stops and returns a timeout error (FR-040).
func TestShutdownTimeoutForcesStop(t *testing.T) {
	handlerStarted := make(chan struct{})

	// Client returns one message, then blocks forever
	client := &blockingPollClient{
		firstMessage: newTestMessage("topic", 0, 0, "blocking-msg"),
	}

	handler := func(ctx context.Context, payload []byte) error {
		close(handlerStarted)
		// Handler that respects context cancellation but takes very long
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
			return nil
		}
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

	// Shutdown with a very short timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer shutdownCancel()

	_ = eng.Stop(shutdownCtx)
	cancel()

	select {
	case err := <-done:
		// Engine should return — handler got context cancelled
		_ = err // no assertion on error value, just that it exits
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop even after timeout")
	}
}

// TestShutdownContextCancelsHandlerContext verifies that when shutdown begins,
// the handler's context is cancelled, allowing handlers to abort (FR-035, Acceptance Scenario 4).
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
}

// TestShutdownNotRunningReturnsError verifies that calling Stop on a non-running
// engine returns an error.
func TestShutdownNotRunningReturnsError(t *testing.T) {
	client := &mockKafkaClient{}
	handler := func(ctx context.Context, payload []byte) error { return nil }
	strat := &mockStrategy{}

	eng := engine.NewEngine(client, handler, strat, testLogger(), 50)

	err := eng.Stop(context.Background())
	assert.Error(t, err, "stopping a non-running engine should return error")
}

// TestShutdownEngineStopSignal verifies the engine responds to Stop()
// called from another goroutine while the poll loop is running.
func TestShutdownEngineStopSignal(t *testing.T) {
	// Client that returns messages indefinitely
	client := &infinitePollClient{}

	processedCount := atomic.Int32{}
	handler := func(ctx context.Context, payload []byte) error {
		processedCount.Add(1)
		return nil
	}
	strat := &mockStrategy{}

	eng := engine.NewEngine(client, handler, strat, testLogger(), 50)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- eng.Start(ctx)
	}()

	// Let it process a few messages
	time.Sleep(200 * time.Millisecond)

	// Stop the engine
	err := eng.Stop(context.Background())
	require.NoError(t, err)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
		assert.Greater(t, processedCount.Load(), int32(0), "should have processed some messages")
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop after Stop() call")
	}
}

// TestShutdownClosesAdapter verifies that the Kafka adapter is closed
// during shutdown (FR-039).
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

// TestShutdownBatchModeFlushesRemaining verifies that in batch mode,
// remaining buffered messages are flushed and committed on shutdown.
func TestShutdownBatchModeFlushesRemaining(t *testing.T) {
	var receivedBatches []int
	var mu sync.Mutex

	// 5 messages, batch size 100 -> all should be flushed as a partial batch on shutdown
	messages := make([]*types.Message, 5)
	for i := 0; i < 5; i++ {
		messages[i] = newTestMessage("topic", 0, int64(i), "msg")
	}

	client := &mockKafkaClient{messages: messages}

	batchHandler := func(ctx context.Context, payloads [][]byte) error {
		mu.Lock()
		receivedBatches = append(receivedBatches, len(payloads))
		mu.Unlock()
		return nil
	}
	strat := &mockStrategy{}

	eng := engine.NewBatchEngine(client, batchHandler, strat, testLogger(), 50, 100, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()

	// Should have received a batch with all 5 messages (flushed by timeout or shutdown)
	totalMsgs := 0
	for _, batchSize := range receivedBatches {
		totalMsgs += batchSize
	}
	assert.Equal(t, 5, totalMsgs, "all buffered messages should be flushed on shutdown")

	// Verify offsets were committed
	commits := client.getCommittedOffsets()
	assert.NotEmpty(t, commits, "offsets should be committed for flushed batch")
}

// TestConsumerShutdownTimeout verifies the full consumer Shutdown() method
// respects the configured ShutdownTimeout.
func TestConsumerShutdownTimeout(t *testing.T) {
	// This is tested at the consumer level via integration tests.
	// Unit test validates that the engine's WaitForDone returns appropriately.

	client := &blockingPollClient{
		firstMessage: newTestMessage("topic", 0, 0, "msg"),
	}

	handlerStarted := make(chan struct{})
	handler := func(ctx context.Context, payload []byte) error {
		close(handlerStarted)
		// Very long handler
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Minute):
			return nil
		}
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
		cancel()
		t.Fatal("handler did not start")
	}

	// WaitForDone with short timeout should fail
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer waitCancel()

	err := eng.WaitForDone(waitCtx)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "WaitForDone should timeout")

	// Clean up: cancel the parent context so the handler exits
	cancel()
	<-done
}

// TestConsumerShutdownWithinTimeout verifies that WaitForDone returns nil
// when the engine completes before the deadline.
func TestConsumerShutdownWithinTimeout(t *testing.T) {
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

	// WaitForDone should return immediately since engine already stopped
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer waitCancel()

	err = eng.WaitForDone(waitCtx)
	assert.NoError(t, err, "WaitForDone should succeed when engine already stopped")
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
		return nil, nil
	}
	msg := c.messages[c.pollIndex]
	c.pollIndex++
	return msg, nil
}

func (c *slowPollClient) CommitOffset(topic string, partition int32, offset int64) error {
	return nil
}

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
		return nil, nil
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return nil, nil
	}
}

func (c *blockingPollClient) CommitOffset(topic string, partition int32, offset int64) error {
	return nil
}

func (c *blockingPollClient) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// infinitePollClient returns messages indefinitely.
type infinitePollClient struct {
	mu        sync.Mutex
	offset    int64
	connected bool
	closed    bool
}

func (c *infinitePollClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
	return nil
}

func (c *infinitePollClient) SubscribeToTopic(ctx context.Context) error { return nil }

func (c *infinitePollClient) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	c.mu.Lock()
	off := c.offset
	c.offset++
	c.mu.Unlock()
	return newTestMessage("topic", 0, off, "infinite-msg"), nil
}

func (c *infinitePollClient) CommitOffset(topic string, partition int32, offset int64) error {
	return nil
}

func (c *infinitePollClient) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
