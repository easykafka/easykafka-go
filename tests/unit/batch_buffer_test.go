package unit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/engine"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// BatchBuffer Unit Tests
// ============================================================================

func TestBatchBuffer_AccumulatesMessages(t *testing.T) {
	buf := engine.NewBatchBuffer(5, 10*time.Second)

	msg1 := newTestMessage("topic", 0, 0, "msg-1")
	msg2 := newTestMessage("topic", 0, 1, "msg-2")

	buf.Add(msg1)
	buf.Add(msg2)

	assert.Equal(t, 2, buf.Len())
	assert.False(t, buf.Ready(), "should not be ready with 2 of 5 messages")
}

func TestBatchBuffer_ReadyWhenFull(t *testing.T) {
	buf := engine.NewBatchBuffer(3, 10*time.Second)

	buf.Add(newTestMessage("topic", 0, 0, "a"))
	buf.Add(newTestMessage("topic", 0, 1, "b"))
	assert.False(t, buf.Ready())

	buf.Add(newTestMessage("topic", 0, 2, "c"))
	assert.True(t, buf.Ready(), "should be ready when batch size reached")
}

func TestBatchBuffer_FlushReturnsMessagesAndResets(t *testing.T) {
	buf := engine.NewBatchBuffer(5, 10*time.Second)

	buf.Add(newTestMessage("topic", 0, 0, "a"))
	buf.Add(newTestMessage("topic", 0, 1, "b"))
	buf.Add(newTestMessage("topic", 0, 2, "c"))

	flushed := buf.Flush()
	require.Len(t, flushed, 3)
	assert.Equal(t, "a", string(flushed[0].Payload))
	assert.Equal(t, "b", string(flushed[1].Payload))
	assert.Equal(t, "c", string(flushed[2].Payload))

	// After flush, buffer should be empty and not ready
	assert.Equal(t, 0, buf.Len())
	assert.False(t, buf.Ready())
}

func TestBatchBuffer_FlushReturnsNilWhenEmpty(t *testing.T) {
	buf := engine.NewBatchBuffer(5, 10*time.Second)
	flushed := buf.Flush()
	assert.Nil(t, flushed)
}

func TestBatchBuffer_MaintainsOrder(t *testing.T) {
	buf := engine.NewBatchBuffer(10, 10*time.Second)

	for i := 0; i < 5; i++ {
		buf.Add(newTestMessage("topic", 0, int64(i), "msg-"+string(rune('a'+i))))
	}

	flushed := buf.Flush()
	require.Len(t, flushed, 5)
	for i, msg := range flushed {
		assert.Equal(t, int64(i), msg.Offset, "messages should maintain Kafka ordering")
	}
}

func TestBatchBuffer_TimedOutAfterTimeout(t *testing.T) {
	// Very short timeout for test
	buf := engine.NewBatchBuffer(100, 50*time.Millisecond)

	buf.Add(newTestMessage("topic", 0, 0, "msg"))

	// Should not be timed out immediately
	assert.False(t, buf.TimedOut())

	// Wait for timeout
	time.Sleep(60 * time.Millisecond)
	assert.True(t, buf.TimedOut(), "should be timed out after duration expires")
}

func TestBatchBuffer_TimedOutNotTriggeredWhenEmpty(t *testing.T) {
	buf := engine.NewBatchBuffer(100, 50*time.Millisecond)

	// Empty buffer should never time out
	time.Sleep(60 * time.Millisecond)
	assert.False(t, buf.TimedOut(), "empty buffer should not time out")
}

func TestBatchBuffer_TimeoutResetsAfterFlush(t *testing.T) {
	buf := engine.NewBatchBuffer(100, 50*time.Millisecond)

	buf.Add(newTestMessage("topic", 0, 0, "msg"))
	time.Sleep(60 * time.Millisecond)
	assert.True(t, buf.TimedOut())

	// Flush resets the timer
	buf.Flush()
	assert.False(t, buf.TimedOut(), "timeout should reset after flush")

	// Add new message - timer resets
	buf.Add(newTestMessage("topic", 0, 1, "msg2"))
	assert.False(t, buf.TimedOut(), "should not be timed out right after adding new message")
}

// ============================================================================
// Batch Engine Dispatch Tests
// ============================================================================

func TestBatchEngine_DispatchesBatchWhenFull(t *testing.T) {
	var mu sync.Mutex
	var receivedBatches [][]string

	batchHandler := func(ctx context.Context, payloads [][]byte) error {
		mu.Lock()
		defer mu.Unlock()
		batch := make([]string, len(payloads))
		for i, p := range payloads {
			batch[i] = string(p)
		}
		receivedBatches = append(receivedBatches, batch)
		return nil
	}

	// 6 messages with batch size 3 => should produce 2 batches
	messages := []*types.Message{
		newTestMessage("topic", 0, 0, "a"),
		newTestMessage("topic", 0, 1, "b"),
		newTestMessage("topic", 0, 2, "c"),
		newTestMessage("topic", 0, 3, "d"),
		newTestMessage("topic", 0, 4, "e"),
		newTestMessage("topic", 0, 5, "f"),
	}

	client := &mockKafkaClient{messages: messages}
	strat := &mockStrategy{}

	eng := engine.NewBatchEngine(client, batchHandler, strat, testLogger(), 100, 3, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, receivedBatches, 2)
	assert.Equal(t, []string{"a", "b", "c"}, receivedBatches[0])
	assert.Equal(t, []string{"d", "e", "f"}, receivedBatches[1])

	// All 6 offsets should be committed (committed after each batch)
	commits := client.getCommittedOffsets()
	// Each batch commit commits the last offset in the batch
	require.Len(t, commits, 2)
	assert.Equal(t, int64(2), commits[0].Offset) // last offset in batch 1
	assert.Equal(t, int64(5), commits[1].Offset) // last offset in batch 2
}

func TestBatchEngine_DispatchesPartialBatchOnTimeout(t *testing.T) {
	var mu sync.Mutex
	var receivedBatches [][]string

	batchHandler := func(ctx context.Context, payloads [][]byte) error {
		mu.Lock()
		defer mu.Unlock()
		batch := make([]string, len(payloads))
		for i, p := range payloads {
			batch[i] = string(p)
		}
		receivedBatches = append(receivedBatches, batch)
		return nil
	}

	// 2 messages with batch size 10 => partial batch should be flushed on timeout
	messages := []*types.Message{
		newTestMessage("topic", 0, 0, "x"),
		newTestMessage("topic", 0, 1, "y"),
	}

	client := &mockKafkaClient{messages: messages}
	strat := &mockStrategy{}

	eng := engine.NewBatchEngine(client, batchHandler, strat, testLogger(), 100, 10, 200*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(receivedBatches), 1, "partial batch should be delivered on timeout")
	assert.Equal(t, []string{"x", "y"}, receivedBatches[0])
}

func TestBatchEngine_AtomicCommitOnSuccess(t *testing.T) {
	batchHandler := func(ctx context.Context, payloads [][]byte) error {
		return nil
	}

	messages := []*types.Message{
		newTestMessage("topic", 0, 0, "a"),
		newTestMessage("topic", 0, 1, "b"),
		newTestMessage("topic", 0, 2, "c"),
	}

	client := &mockKafkaClient{messages: messages}
	strat := &mockStrategy{}

	eng := engine.NewBatchEngine(client, batchHandler, strat, testLogger(), 100, 3, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err)

	// Atomic commit: only the highest offset in the batch should be committed
	commits := client.getCommittedOffsets()
	require.Len(t, commits, 1)
	assert.Equal(t, int64(2), commits[0].Offset)
}

func TestBatchEngine_ErrorStrategyOnBatchFailure(t *testing.T) {
	batchErr := errors.New("batch processing failed")

	batchHandler := func(ctx context.Context, payloads [][]byte) error {
		return batchErr
	}

	messages := []*types.Message{
		newTestMessage("topic", 0, 0, "a"),
		newTestMessage("topic", 0, 1, "b"),
		newTestMessage("topic", 0, 2, "c"),
	}

	client := &mockKafkaClient{messages: messages}
	strat := &mockStrategy{} // returns nil => continue

	eng := engine.NewBatchEngine(client, batchHandler, strat, testLogger(), 100, 3, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err)

	// Error strategy should receive all messages in the batch
	calls := strat.getHandleCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, batchErr, calls[0].HandlerErr)
	require.Len(t, calls[0].Msgs, 3, "strategy should receive entire batch")
	assert.Equal(t, int64(0), calls[0].Msgs[0].Offset)
	assert.Equal(t, int64(1), calls[0].Msgs[1].Offset)
	assert.Equal(t, int64(2), calls[0].Msgs[2].Offset)
}

func TestBatchEngine_FatalStrategyStopsEngine(t *testing.T) {
	batchHandler := func(ctx context.Context, payloads [][]byte) error {
		return errors.New("fail")
	}

	messages := []*types.Message{
		newTestMessage("topic", 0, 0, "a"),
		newTestMessage("topic", 0, 1, "b"),
		newTestMessage("topic", 0, 2, "c"),
	}

	client := &mockKafkaClient{messages: messages}
	strat := &mockStrategy{returnErr: errors.New("fatal: stop consumer")}

	eng := engine.NewBatchEngine(client, batchHandler, strat, testLogger(), 100, 3, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := eng.Start(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error strategy")
}

func TestBatchEngine_PanicRecoveryInBatchHandler(t *testing.T) {
	batchHandler := func(ctx context.Context, payloads [][]byte) error {
		panic("batch handler exploded")
	}

	messages := []*types.Message{
		newTestMessage("topic", 0, 0, "a"),
		newTestMessage("topic", 0, 1, "b"),
	}

	client := &mockKafkaClient{messages: messages}
	strat := &mockStrategy{}

	eng := engine.NewBatchEngine(client, batchHandler, strat, testLogger(), 100, 2, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err) // strategy returned nil => engine continues

	// Panic should be recovered and treated as handler error
	calls := strat.getHandleCalls()
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].HandlerErr.Error(), "handler panic")
}

func TestBatchEngine_CommitsAfterStrategySuccess(t *testing.T) {
	// When error strategy returns nil (continue), offsets should still be committed
	batchHandler := func(ctx context.Context, payloads [][]byte) error {
		return errors.New("temporary error")
	}

	messages := []*types.Message{
		newTestMessage("topic", 0, 0, "a"),
		newTestMessage("topic", 0, 1, "b"),
		newTestMessage("topic", 0, 2, "c"),
	}

	client := &mockKafkaClient{messages: messages}
	strat := &mockStrategy{} // returns nil => continue

	eng := engine.NewBatchEngine(client, batchHandler, strat, testLogger(), 100, 3, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err)

	// Even though handler failed, strategy said continue => commit offset
	commits := client.getCommittedOffsets()
	require.Len(t, commits, 1)
	assert.Equal(t, int64(2), commits[0].Offset)
}
