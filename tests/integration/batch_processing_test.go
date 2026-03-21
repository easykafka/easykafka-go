package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBatchProcessing_SizeTrigger verifies that when enough messages accumulate
// to fill the batch size, the batch handler is called with the full batch.
func TestBatchProcessing_SizeTrigger(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-batch-size-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	// Produce exactly 6 messages => expect 2 batches of size 3
	messages := []string{"m1", "m2", "m3", "m4", "m5", "m6"}
	cluster.ProduceMessages(ctx, t, topic, messages)

	var mu sync.Mutex
	var batches [][]string

	batchHandler := func(ctx context.Context, payloads [][]byte) error {
		mu.Lock()
		defer mu.Unlock()
		batch := make([]string, len(payloads))
		for i, p := range payloads {
			batch[i] = string(p)
		}
		batches = append(batches, batch)
		return nil
	}

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("test-batch-group-%d", time.Now().UnixNano())),
		easykafka.WithBatchHandler(batchHandler),
		easykafka.WithBatchSize(3),
		easykafka.WithBatchTimeout(10*time.Second), // long timeout so size triggers first
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	consumerCtx, cancel := context.WithCancel(ctx)
	var consumerErr error
	done := make(chan struct{})

	go func() {
		consumerErr = consumer.Start(consumerCtx)
		close(done)
	}()

	// Wait for both batches
	deadline := time.After(30 * time.Second)
	for {
		mu.Lock()
		totalMessages := 0
		for _, b := range batches {
			totalMessages += len(b)
		}
		mu.Unlock()

		if totalMessages >= len(messages) {
			break
		}

		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out waiting for batches, received %d messages", totalMessages)
		case <-time.After(100 * time.Millisecond):
		}
	}

	cancel()
	<-done
	assert.NoError(t, consumerErr)

	mu.Lock()
	defer mu.Unlock()

	// All 6 messages should be received in batches of <=3
	totalReceived := 0
	for _, b := range batches {
		assert.LessOrEqual(t, len(b), 3, "batch should not exceed batch size")
		totalReceived += len(b)
	}
	assert.Equal(t, len(messages), totalReceived)
}

// TestBatchProcessing_TimeoutTrigger verifies that a partial batch is delivered
// when the batch timeout expires, even if batch size is not reached.
func TestBatchProcessing_TimeoutTrigger(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-batch-timeout-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	// Produce 2 messages -- batch size is 100, so only timeout should trigger
	messages := []string{"alpha", "beta"}
	cluster.ProduceMessages(ctx, t, topic, messages)

	var mu sync.Mutex
	var batches [][]string

	batchHandler := func(ctx context.Context, payloads [][]byte) error {
		mu.Lock()
		defer mu.Unlock()
		batch := make([]string, len(payloads))
		for i, p := range payloads {
			batch[i] = string(p)
		}
		batches = append(batches, batch)
		return nil
	}

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("test-batch-timeout-group-%d", time.Now().UnixNano())),
		easykafka.WithBatchHandler(batchHandler),
		easykafka.WithBatchSize(100),                     // large batch size so it won't fill
		easykafka.WithBatchTimeout(500*time.Millisecond), // short timeout to trigger partial
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	consumerCtx, cancel := context.WithCancel(ctx)
	var consumerErr error
	done := make(chan struct{})

	go func() {
		consumerErr = consumer.Start(consumerCtx)
		close(done)
	}()

	// Wait for the partial batch to be delivered
	deadline := time.After(30 * time.Second)
	for {
		mu.Lock()
		totalMessages := 0
		for _, b := range batches {
			totalMessages += len(b)
		}
		mu.Unlock()

		if totalMessages >= len(messages) {
			break
		}

		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out waiting for partial batch")
		case <-time.After(100 * time.Millisecond):
		}
	}

	cancel()
	<-done
	assert.NoError(t, consumerErr)

	mu.Lock()
	defer mu.Unlock()

	// Should have received both messages (possibly in one or two batches)
	totalReceived := 0
	for _, b := range batches {
		totalReceived += len(b)
	}
	assert.Equal(t, len(messages), totalReceived)
}

// TestBatchProcessing_AtomicCommit verifies that offsets are committed atomically
// after the batch handler succeeds. A second consumer on the same group should
// not receive already-committed messages.
func TestBatchProcessing_AtomicCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-batch-commit-%d", time.Now().UnixNano())
	groupID := fmt.Sprintf("test-batch-commit-group-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1) // todo: with more than one partition this will probably not work!

	// Produce 3 messages
	messages := []string{"one", "two", "three"}
	cluster.ProduceMessages(ctx, t, topic, messages)

	var mu sync.Mutex
	processedCount := 0

	batchHandler := func(ctx context.Context, payloads [][]byte) error {
		mu.Lock()
		defer mu.Unlock()
		processedCount += len(payloads)
		return nil
	}

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(groupID),
		easykafka.WithBatchHandler(batchHandler),
		easykafka.WithBatchSize(3),
		easykafka.WithBatchTimeout(5*time.Second),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	consumerCtx, cancel := context.WithCancel(ctx)
	var consumerErr error
	done := make(chan struct{})

	go func() {
		consumerErr = consumer.Start(consumerCtx)
		close(done)
	}()

	// Wait for all messages to be processed
	deadline := time.After(30 * time.Second)
	for {
		mu.Lock()
		count := processedCount
		mu.Unlock()

		if count >= len(messages) {
			break
		}

		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out, processed %d of %d", count, len(messages))
		case <-time.After(100 * time.Millisecond):
		}
	}

	cancel()
	<-done
	assert.NoError(t, consumerErr)

	// Verify offsets were committed by starting a new consumer on the same group.
	// It should not receive any messages since offsets were committed.
	time.Sleep(500 * time.Millisecond) // brief delay for commit propagation

	consumer2, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(groupID),
		easykafka.WithBatchHandler(func(ctx context.Context, payloads [][]byte) error {
			mu.Lock()
			defer mu.Unlock()
			processedCount += len(payloads)
			return nil
		}),
		easykafka.WithBatchSize(10),
		easykafka.WithBatchTimeout(1*time.Second),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	consumer2Ctx, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()

	mu.Lock()
	countBefore := processedCount
	mu.Unlock()

	_ = consumer2.Start(consumer2Ctx)

	mu.Lock()
	countAfter := processedCount
	mu.Unlock()

	assert.Equal(t, countBefore, countAfter, "second consumer should not receive already-committed messages")
}
