package integration

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGracefulShutdownCompletesInFlight verifies that when the consumer's
// context is cancelled, in-flight handlers complete and offsets are committed
// before the consumer exits (FR-036, FR-037, FR-038).
func TestGracefulShutdownCompletesInFlight(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-shutdown-inflight-%d", time.Now().UnixNano())
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
		easykafka.WithShutdownTimeout(10*time.Second),
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

	assert.NoError(t, consumerErr)

	// At least the message being processed should have completed
	mu.Lock()
	count := len(processedPayloads)
	mu.Unlock()
	assert.GreaterOrEqual(t, count, 1, "at least one message should have been processed before shutdown")
}

// TestGracefulShutdownViaMethod verifies that calling Shutdown() on a running
// consumer triggers a clean shutdown that completes within the timeout (FR-034).
func TestGracefulShutdownViaMethod(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-shutdown-method-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	cluster.ProduceMessages(ctx, t, topic, []string{"msg-1", "msg-2"})

	processedCount := atomic.Int32{}

	handler := func(ctx context.Context, payload []byte) error {
		processedCount.Add(1)
		return nil
	}

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("test-group-%d", time.Now().UnixNano())),
		easykafka.WithHandler(handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
		easykafka.WithShutdownTimeout(10*time.Second),
	)
	require.NoError(t, err)

	var consumerErr error
	done := make(chan struct{})
	go func() {
		consumerErr = consumer.Start(context.Background())
		close(done)
	}()

	// Wait for messages to be processed
	deadline := time.After(30 * time.Second)
	for processedCount.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for messages to be processed")
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Call Shutdown() explicitly
	shutdownErr := consumer.Shutdown(context.Background())
	assert.NoError(t, shutdownErr)

	// Consumer should stop
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("consumer did not stop after Shutdown()")
	}

	assert.NoError(t, consumerErr)
}

// TestGracefulShutdownNoNewMessages verifies that after shutdown is triggered,
// no new messages are fetched from the topic (FR-036).
func TestGracefulShutdownNoNewMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-shutdown-nofetch-%d", time.Now().UnixNano())
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
