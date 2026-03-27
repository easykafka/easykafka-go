package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBasicConsumption verifies that a consumer with a simple handler can:
// 1. Connect to a real Kafka cluster
// 2. Receive messages produced to a topic
// 3. Commit offsets after successful processing
// 4. Access message metadata via context
func TestBasicConsumption(t *testing.T) {
	t.Log("TestBasicConsumption started")
	defer t.Log("TestBasicConsumption finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	// Start a Kafka cluster
	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-basic-%d", time.Now().UnixNano())

	// Create topic with 1 partition for deterministic ordering
	cluster.CreateTopic(ctx, t, topic, 1)

	// Produce test messages
	expectedMessages := []string{"hello", "world", "easykafka"}
	cluster.ProduceMessages(ctx, t, topic, expectedMessages)

	// Track received messages
	var mu sync.Mutex
	var receivedPayloads []string
	var receivedOffsets []int64

	// Set up handler
	handler := func(ctx context.Context, payload []byte) error {
		msg, ok := metadata.MessageFromContext(ctx)

		mu.Lock()
		defer mu.Unlock()
		receivedPayloads = append(receivedPayloads, string(payload))
		if ok && msg != nil {
			receivedOffsets = append(receivedOffsets, msg.Offset)
		}
		return nil
	}

	// Create and start consumer
	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("test-group-%d", time.Now().UnixNano())),
		easykafka.WithHandler(handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	// Run consumer with timeout — stop after processing all messages
	consumerCtx, cancel := context.WithCancel(ctx)

	var consumerErr error
	done := make(chan struct{})

	go func() {
		consumerErr = consumer.Start(consumerCtx)
		close(done)
	}()

	// Wait for all messages to be received (with timeout)
	deadline := time.After(30 * time.Second)
	for {
		mu.Lock()
		count := len(receivedPayloads)
		mu.Unlock()

		if count >= len(expectedMessages) {
			break
		}

		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out waiting for messages, received %d of %d", count, len(expectedMessages))
		case <-time.After(100 * time.Millisecond):
			// Continue polling
		}
	}

	// Stop the consumer
	cancel()
	<-done

	// consumerErr is expected to be nil (context cancellation is clean)
	assert.NoError(t, consumerErr)

	// Verify all messages were received
	mu.Lock()
	defer mu.Unlock()

	require.Len(t, receivedPayloads, len(expectedMessages))
	assert.Equal(t, expectedMessages, receivedPayloads)

	// Verify offsets are sequential (0, 1, 2)
	require.Len(t, receivedOffsets, len(expectedMessages))
	for i, offset := range receivedOffsets {
		assert.Equal(t, int64(i), offset, "unexpected offset at position %d", i)
	}
}

// TestBasicConsumptionWithErrorStrategy verifies that when a handler returns an error,
// the error strategy is invoked and the consumer continues (skip strategy).
func TestBasicConsumptionWithErrorStrategy(t *testing.T) {
	t.Log("TestBasicConsumptionWithErrorStrategy started")
	defer t.Log("TestBasicConsumptionWithErrorStrategy finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-error-strategy-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	// Produce messages — one will fail
	messages := []string{"good-1", "bad-msg", "good-2"}
	cluster.ProduceMessages(ctx, t, topic, messages)

	var mu sync.Mutex
	var processedPayloads []string

	handler := func(ctx context.Context, payload []byte) error {
		msg := string(payload)
		if msg == "bad-msg" {
			return fmt.Errorf("simulated processing failure")
		}
		mu.Lock()
		processedPayloads = append(processedPayloads, msg)
		mu.Unlock()
		return nil
	}

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("test-group-%d", time.Now().UnixNano())),
		easykafka.WithHandler(handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
		// Default strategy is skip — will continue on error
	)
	require.NoError(t, err)

	consumerCtx, cancel := context.WithCancel(ctx)

	var consumerErr error
	done := make(chan struct{})

	go func() {
		consumerErr = consumer.Start(consumerCtx)
		close(done)
	}()

	// Wait for 2 successful messages (the bad one is skipped)
	deadline := time.After(30 * time.Second)
	for {
		mu.Lock()
		count := len(processedPayloads)
		mu.Unlock()

		if count >= 2 {
			break
		}

		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out waiting for messages, only received %d", count)
		case <-time.After(100 * time.Millisecond):
		}
	}

	cancel()
	<-done

	assert.NoError(t, consumerErr)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, processedPayloads, 2)
	assert.Equal(t, []string{"good-1", "good-2"}, processedPayloads)
}

// TestBasicConsumptionPanicRecovery verifies that handler panics are recovered
// and treated as errors via the error strategy.
func TestBasicConsumptionPanicRecovery(t *testing.T) {
	t.Log("TestBasicConsumptionPanicRecovery started")
	defer t.Log("TestBasicConsumptionPanicRecovery finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-panic-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	messages := []string{"normal", "panic-me", "after-panic"}
	cluster.ProduceMessages(ctx, t, topic, messages)

	var mu sync.Mutex
	var receivedPayloads []string

	handler := func(ctx context.Context, payload []byte) error {
		if string(payload) == "panic-me" {
			panic("test panic!")
		}
		mu.Lock()
		receivedPayloads = append(receivedPayloads, string(payload))
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

	// Wait for 2 non-panic messages
	deadline := time.After(30 * time.Second)
	for {
		mu.Lock()
		count := len(receivedPayloads)
		mu.Unlock()

		if count >= 2 {
			break
		}

		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out waiting for messages, only received %d", count)
		case <-time.After(100 * time.Millisecond):
		}
	}

	cancel()
	<-done

	assert.NoError(t, consumerErr)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"normal", "after-panic"}, receivedPayloads)
}
