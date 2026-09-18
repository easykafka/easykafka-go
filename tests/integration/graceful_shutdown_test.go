package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGracefulShutdownCompletesInFlight verifies that when the consumer's
// context is cancelled, the in-flight dispatch returns before Start does.
//
// "Completes" means the dispatch returned, not that the work succeeded. The
// handler here ignores its context, so it does finish; one that watched the
// context would see it cancelled and return an error, and that error would go
// to the error strategy like any other.
func TestGracefulShutdownCompletesInFlight(t *testing.T) {
	t.Log("TestGracefulShutdownCompletesInFlight started")
	defer t.Log("TestGracefulShutdownCompletesInFlight finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()

	cluster := helpers.SharedCluster(t)

	topic := helpers.UniqueTopicName(t, "test-shutdown-inflight")
	cluster.CreateTopic(ctx, t, topic, 1)

	// Produce messages
	cluster.ProduceMessages(ctx, t, topic, []string{"msg-1", "msg-2", "msg-3"})

	var mu sync.Mutex
	var processedPayloads []string
	handlerStarted := make(chan struct{}, 3)

	handler := func(ctx context.Context, payload []byte) error {
		handlerStarted <- struct{}{}
		// Simulate some processing time
		time.Sleep(200 * time.Millisecond)

		mu.Lock()
		processedPayloads = append(processedPayloads, string(payload))
		mu.Unlock()
		return nil
	}

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("test-group-%d", time.Now().UnixNano())),
		easykafka.WithHandler(handler),
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

	// Wait for at least one handler to start processing
	select {
	case <-handlerStarted:
	case <-time.After(30 * time.Second):
		cancel()
		<-done
		t.Fatal("timed out waiting for handler to start")
	}

	// Cancel context to trigger graceful shutdown
	cancel()

	// Consumer should stop
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("consumer did not stop after context cancellation")
	}

	require.NoError(t, consumerErr)

	// At least the message being processed should have completed
	mu.Lock()
	count := len(processedPayloads)
	mu.Unlock()
	assert.GreaterOrEqual(t, count, 1, "at least one message should have been processed before shutdown")
}

// TestGracefulShutdownNoNewMessages verifies that after shutdown is triggered,
// no new messages are fetched from the topic.
func TestGracefulShutdownNoNewMessages(t *testing.T) {
	t.Log("TestGracefulShutdownNoNewMessages started")
	defer t.Log("TestGracefulShutdownNoNewMessages finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()

	cluster := helpers.SharedCluster(t)

	topic := helpers.UniqueTopicName(t, "test-shutdown-nofetch")
	cluster.CreateTopic(ctx, t, topic, 1)

	// Produce initial messages
	cluster.ProduceMessages(ctx, t, topic, []string{"before-1", "before-2"})

	var mu sync.Mutex
	var received []string
	beforeDone := make(chan struct{})

	handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		received = append(received, string(payload))
		count := len(received)
		mu.Unlock()
		if count == 2 {
			// Signal that initial messages are done
			select {
			case beforeDone <- struct{}{}:
			default:
			}
		}
		return nil
	}

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("test-group-%d", time.Now().UnixNano())),
		easykafka.WithHandler(handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	consumerCtx, cancel := context.WithCancel(ctx)

	done := make(chan struct{})
	go func() {
		_ = consumer.Start(consumerCtx)
		close(done)
	}()

	// Wait for initial messages to be processed
	select {
	case <-beforeDone:
	case <-time.After(30 * time.Second):
		cancel()
		<-done
		t.Fatal("timed out waiting for initial messages")
	}

	// Produce more messages AFTER we trigger shutdown
	cancel()

	// Wait a moment then produce
	time.Sleep(200 * time.Millisecond)
	cluster.ProduceMessages(ctx, t, topic, []string{"after-1", "after-2"})

	// Wait for consumer to stop
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("consumer did not stop")
	}

	mu.Lock()
	defer mu.Unlock()

	// Should NOT contain "after-*" messages
	for _, msg := range received {
		assert.NotContains(t, msg, "after-", "messages produced after shutdown should not be consumed")
	}
}

// TestGracefulShutdownBatchBufferIsRedelivered verifies that a batch buffer the
// consumer never dispatched survives shutdown in the only way that matters: it
// is dropped rather than handed to the handler, no offset advances, and a fresh
// consumer in the same group reads every message.
//
// The batch size is above the message count and the batch timeout longer than
// the test, so the buffer fills and the only thing that could flush it is the
// shutdown — which no longer does.
func TestGracefulShutdownBatchBufferIsRedelivered(t *testing.T) {
	t.Log("TestGracefulShutdownBatchBufferIsRedelivered started")
	defer t.Log("TestGracefulShutdownBatchBufferIsRedelivered finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()

	cluster := helpers.SharedCluster(t)

	topic := helpers.UniqueTopicName(t, "test-shutdown-batch-drop")
	cluster.CreateTopic(ctx, t, topic, 1)

	payloads := []string{"batch-1", "batch-2", "batch-3"}
	cluster.ProduceMessages(ctx, t, topic, payloads)

	group := fmt.Sprintf("test-group-%d", time.Now().UnixNano())

	var firstDispatched int
	var mu sync.Mutex

	first, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(group),
		easykafka.WithBatchHandler(func(ctx context.Context, batch [][]byte) error {
			mu.Lock()
			firstDispatched += len(batch)
			mu.Unlock()
			return nil
		}),
		easykafka.WithBatchSize(len(payloads)+1),
		easykafka.WithBatchTimeout(10*time.Minute),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	firstCtx, cancelFirst := context.WithCancel(ctx)
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Start(firstCtx) }()

	// Let the consumer join the group, take the partition, and buffer every
	// message. There is nothing to wait on: the handler is never called, which
	// is the whole point.
	time.Sleep(10 * time.Second)

	cancelFirst()
	select {
	case err := <-firstDone:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("consumer did not stop after context cancellation")
	}

	mu.Lock()
	assert.Zero(t, firstDispatched, "a buffered batch must not be dispatched on shutdown")
	mu.Unlock()

	assert.Equal(t, kfk.OffsetInvalid, cluster.CommittedOffset(ctx, t, group, topic, 0),
		"a dropped buffer must not advance the group's committed offset")

	// A fresh consumer in the same group must see every message.
	var mu2 sync.Mutex
	var received []string
	allReceived := make(chan struct{})

	second, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(group),
		easykafka.WithBatchHandler(func(ctx context.Context, batch [][]byte) error {
			mu2.Lock()
			defer mu2.Unlock()
			for _, p := range batch {
				received = append(received, string(p))
			}
			if len(received) == len(payloads) {
				close(allReceived)
			}
			return nil
		}),
		easykafka.WithBatchSize(len(payloads)),
		easykafka.WithBatchTimeout(time.Second),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	secondCtx, cancelSecond := context.WithCancel(ctx)
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.Start(secondCtx) }()

	select {
	case <-allReceived:
	case <-time.After(60 * time.Second):
		cancelSecond()
		<-secondDone
		mu2.Lock()
		got := append([]string(nil), received...)
		mu2.Unlock()
		t.Fatalf("fresh consumer did not receive the dropped messages, got %v", got)
	}

	cancelSecond()
	require.NoError(t, <-secondDone)

	mu2.Lock()
	defer mu2.Unlock()
	assert.ElementsMatch(t, payloads, received)
}
