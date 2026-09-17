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

// These tests cover the store/commit split. A store or commit failure cannot be
// provoked against a real broker, so they run against a fake adapter rather than
// in the integration suite.
//
// The rule they pin down: a store failure on a partition we still hold must stop
// the consumer, because carrying on lets the next message store a higher offset
// whose commit silently declares the failed one done. A revoked partition is the
// one exception, and a commit failure is harmless.

// TestStoreFailureStopsConsumer verifies that a store failure which is *not* a
// revoked partition stops the engine instead of continuing past it.
func TestStoreFailureStopsConsumer(t *testing.T) {
	storeErr := errors.New("offsets store failed")

	var handled int
	var mu sync.Mutex
	handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		handled++
		mu.Unlock()
		return nil
	}

	messages := []*types.Message{
		newTestMessage("test-topic", 0, 0, "msg-1"),
		newTestMessage("test-topic", 0, 1, "msg-2"),
		newTestMessage("test-topic", 0, 2, "msg-3"),
	}

	client := &mockKafkaClient{messages: messages, storeErr: storeErr}
	eng := engine.NewEngine(client, handler, &mockStrategy{}, testLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)

	require.Error(t, err, "a non-revoked store failure must stop the consumer")
	require.ErrorIs(t, err, storeErr, "the underlying store error should be wrapped, not swallowed")

	// Only the first message should have been handled: the engine stops rather
	// than processing message 2, whose store would have committed past message 1.
	mu.Lock()
	assert.Equal(t, 1, handled, "engine should stop after the first store failure")
	mu.Unlock()

	assert.Empty(t, client.getStoredOffsets(), "nothing should have been stored")
}

// TestRevokedPartitionDoesNotStopConsumer is the counterpart to the above: the
// one store failure that must be tolerated. Without this, an implementation that
// stops on every store error would pass the test above while dying on every
// rebalance.
func TestRevokedPartitionDoesNotStopConsumer(t *testing.T) {
	var handled int
	var mu sync.Mutex
	handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		handled++
		mu.Unlock()
		return nil
	}

	messages := []*types.Message{
		newTestMessage("test-topic", 0, 0, "msg-1"),
		newTestMessage("test-topic", 0, 1, "msg-2"),
		newTestMessage("test-topic", 0, 2, "msg-3"),
	}

	client := &mockKafkaClient{
		messages: messages,
		storeErr: types.ErrPartitionRevoked,
	}
	eng := engine.NewEngine(client, handler, &mockStrategy{}, testLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err, "a revoked partition is an ordinary rebalance race, not a failure")

	// All three were handled: the engine kept going. Their offsets are simply not
	// recorded, so whoever owns the partition now will redeliver them.
	mu.Lock()
	assert.Equal(t, 3, handled, "engine should continue consuming after a revoked partition")
	mu.Unlock()

	assert.Empty(t, client.getStoredOffsets())
}

// TestCommitFailureDoesNotStopConsumer verifies the asymmetry: unlike a store
// failure, a failed commit loses nothing, because the offset stays in the store
// and the next commit covers it.
func TestCommitFailureDoesNotStopConsumer(t *testing.T) {
	handler := func(ctx context.Context, payload []byte) error { return nil }

	messages := []*types.Message{
		newTestMessage("test-topic", 0, 0, "msg-1"),
		newTestMessage("test-topic", 0, 1, "msg-2"),
		newTestMessage("test-topic", 0, 2, "msg-3"),
	}

	client := &mockKafkaClient{
		messages:  messages,
		commitErr: errors.New("coordinator unavailable"),
	}
	eng := engine.NewEngine(client, handler, &mockStrategy{}, testLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err, "a commit failure is recoverable and must not stop the consumer")

	// Every message was still recorded as processed; only publishing failed.
	stored := client.getStoredOffsets()
	require.Len(t, stored, 3)
	assert.Equal(t, int64(2), stored[2].Offset)
}

// TestBatchStoreFailureStoresRemainingPartitions verifies that a failure on one
// partition does not abandon the others: the loop finishes, the offsets that did
// store are committed, and only then does the engine stop.
func TestBatchStoreFailureStoresRemainingPartitions(t *testing.T) {
	storeErr := errors.New("offsets store failed")

	batchHandler := func(ctx context.Context, payloads [][]byte) error { return nil }

	// One batch spanning three partitions: 0 fails outright, 1 was revoked,
	// 2 is still ours and must still be stored.
	messages := []*types.Message{
		newTestMessage("test-topic", 0, 10, "p0"),
		newTestMessage("test-topic", 1, 20, "p1"),
		newTestMessage("test-topic", 2, 30, "p2"),
	}

	client := &mockKafkaClient{
		messages: messages,
		storeErrByPartition: map[int32]error{
			0: storeErr,
			1: types.ErrPartitionRevoked,
		},
	}

	eng := engine.NewBatchEngine(
		client, batchHandler, &mockStrategy{}, testLogger(),
		100, len(messages), time.Second,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)

	require.Error(t, err, "the real store failure must stop the consumer")
	require.ErrorIs(t, err, storeErr)

	// Partition 2 was still ours, so it must have been stored despite partition 0
	// failing earlier in the same loop.
	stored := client.getStoredOffsets()
	require.Len(t, stored, 1, "the surviving partition should still be stored")
	assert.Equal(t, int32(2), stored[0].Partition)
	assert.Equal(t, int64(30), stored[0].Offset)

	// And committed — on the fatal path too, so the restart replays less.
	assert.Positive(t, client.getCommitCount(), "stored offsets should be committed before stopping")
}

// TestBatchRevokedPartitionOnlyDoesNotStopConsumer verifies that a batch whose
// only store failure is a revoked partition keeps the consumer running.
func TestBatchRevokedPartitionOnlyDoesNotStopConsumer(t *testing.T) {
	batchHandler := func(ctx context.Context, payloads [][]byte) error { return nil }

	messages := []*types.Message{
		newTestMessage("test-topic", 0, 10, "p0"),
		newTestMessage("test-topic", 1, 20, "p1"),
	}

	client := &mockKafkaClient{
		messages: messages,
		storeErrByPartition: map[int32]error{
			0: types.ErrPartitionRevoked,
		},
	}

	eng := engine.NewBatchEngine(
		client, batchHandler, &mockStrategy{}, testLogger(),
		100, len(messages), time.Second,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err, "a revoked partition alone must not stop the consumer")

	stored := client.getStoredOffsets()
	require.Len(t, stored, 1)
	assert.Equal(t, int32(1), stored[0].Partition)
}

// TestRevokeDropsBatchBuffer verifies that buffered-but-undispatched messages are
// discarded when partitions are revoked, rather than handed to the handler for
// partitions another consumer now owns.
func TestRevokeDropsBatchBuffer(t *testing.T) {
	var dispatched int
	var mu sync.Mutex
	batchHandler := func(ctx context.Context, payloads [][]byte) error {
		mu.Lock()
		dispatched += len(payloads)
		mu.Unlock()
		return nil
	}

	messages := []*types.Message{
		newTestMessage("test-topic", 0, 0, "msg-1"),
		newTestMessage("test-topic", 0, 1, "msg-2"),
	}

	// Revoke once both messages are buffered. The mock fires the hook from inside
	// Poll, which is where librdkafka runs the rebalance callback — so the buffer
	// is only ever touched by the polling goroutine, exactly as in production.
	client := &mockKafkaClient{messages: messages, revokeAtPoll: len(messages)}

	// Batch size above the message count and a long timeout, so the buffer fills
	// but never dispatches on its own.
	eng := engine.NewBatchEngine(
		client, batchHandler, &mockStrategy{}, testLogger(),
		10, 100, 10*time.Second,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Start(ctx) }()

	require.Eventually(t, client.didRevoke, time.Second, 5*time.Millisecond,
		"the revoke hook should have fired")

	cancel()
	require.NoError(t, <-done)

	// The ctx.Done() path flushes the buffer before exiting, so if the drop had
	// not happened these messages would have been dispatched for partitions we no
	// longer own.
	mu.Lock()
	assert.Zero(t, dispatched, "buffered messages should be dropped on revoke, not dispatched")
	mu.Unlock()

	assert.Empty(t, client.getStoredOffsets(), "dropped messages must not advance any offset")
}
