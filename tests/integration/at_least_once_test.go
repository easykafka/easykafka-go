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

// TestAtLeastOnceDelivery verifies FR-043: at-least-once delivery semantics.
// All produced messages must be received by the handler. Duplicates are
// acceptable (at-least-once), but no message may be lost.
func TestAtLeastOnceDelivery(t *testing.T) {
	t.Log("TestAtLeastOnceDelivery started")
	defer t.Log("TestAtLeastOnceDelivery finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-alo-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 3) // multiple partitions for realistic coverage

	// Produce a batch of messages
	const messageCount = 50
	var produced []string
	for i := 0; i < messageCount; i++ {
		produced = append(produced, fmt.Sprintf("alo-msg-%d", i))
	}
	cluster.ProduceMessages(ctx, t, topic, produced)

	// Track received messages (may contain duplicates)
	var mu sync.Mutex
	var received []string

	handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		received = append(received, string(payload))
		mu.Unlock()
		return nil
	}

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("alo-group-%d", time.Now().UnixNano())),
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

	// Wait until we have received at least all produced messages
	waitForMessages(t, &mu, &received, messageCount, 30*time.Second)

	cancel()
	<-done

	assert.NoError(t, consumerErr)

	mu.Lock()
	defer mu.Unlock()

	// Build a set of unique received messages
	receivedSet := make(map[string]int, len(received))
	for _, msg := range received {
		receivedSet[msg]++
	}

	// At-least-once: every produced message must be present
	for _, msg := range produced {
		assert.GreaterOrEqual(t, receivedSet[msg], 1,
			"message %q was produced but never received (violates at-least-once)", msg)
	}

	// Duplicates are acceptable: log them but don't fail
	for msg, count := range receivedSet {
		if count > 1 {
			t.Logf("duplicate delivery (acceptable): %q received %d times", msg, count)
		}
	}

	t.Logf("at-least-once validation: %d produced, %d received (unique: %d)",
		len(produced), len(received), len(receivedSet))
}

// TestAtLeastOnceAfterBrokerRestart verifies at-least-once semantics survive
// broker failures. Messages produced before and after a broker restart must
// all be delivered to the handler. Duplicates caused by the restart are
// acceptable.
func TestAtLeastOnceAfterBrokerRestart(t *testing.T) {
	t.Log("TestAtLeastOnceAfterBrokerRestart started")
	defer t.Log("TestAtLeastOnceAfterBrokerRestart finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-alo-restart-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	// Phase 1: produce pre-restart messages
	preMessages := []string{"pre-1", "pre-2", "pre-3", "pre-4", "pre-5"}
	cluster.ProduceMessages(ctx, t, topic, preMessages)

	var mu sync.Mutex
	var received []string

	handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		received = append(received, string(payload))
		mu.Unlock()
		return nil
	}

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("alo-restart-group-%d", time.Now().UnixNano())),
		easykafka.WithHandler(handler),
		easykafka.WithPollTimeout(200*time.Millisecond),
		easykafka.WithKafkaConfig(map[string]any{
			"reconnect.backoff.ms":     100,
			"reconnect.backoff.max.ms": 1000,
			"session.timeout.ms":       10000,
		}),
	)
	require.NoError(t, err)

	consumerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var consumerErr error
	done := make(chan struct{})
	go func() {
		consumerErr = consumer.Start(consumerCtx)
		close(done)
	}()

	// Wait for pre-restart messages
	waitForMessages(t, &mu, &received, len(preMessages), 30*time.Second)
	t.Log("Phase 1 complete: pre-restart messages received")

	// Phase 2: broker restart
	cluster.StopBroker(ctx, t)
	time.Sleep(3 * time.Second)
	cluster.StartBroker(ctx, t)
	cluster.WaitForBrokerReady(ctx, t, 60*time.Second)
	t.Log("Phase 2 complete: broker restarted")

	// Phase 3: produce post-restart messages
	postMessages := []string{"post-1", "post-2", "post-3", "post-4", "post-5"}
	cluster.ProduceMessages(ctx, t, topic, postMessages)

	// Wait for all messages (pre + post), allowing duplicates
	totalExpected := len(preMessages) + len(postMessages)
	waitForMessages(t, &mu, &received, totalExpected, 60*time.Second)

	cancel()
	<-done

	assert.NoError(t, consumerErr)

	mu.Lock()
	defer mu.Unlock()

	receivedSet := make(map[string]int, len(received))
	for _, msg := range received {
		receivedSet[msg]++
	}

	// Every produced message (pre and post restart) must appear at least once
	allProduced := append(preMessages, postMessages...)
	for _, msg := range allProduced {
		assert.GreaterOrEqual(t, receivedSet[msg], 1,
			"message %q was never delivered after broker restart (violates at-least-once)", msg)
	}

	t.Logf("at-least-once after restart: %d produced, %d received (unique: %d)",
		len(allProduced), len(received), len(receivedSet))
}

// TestAtLeastOnceWithHandlerErrors verifies that messages causing handler
// errors are still accounted for - they go through the error strategy (skip
// by default) and the consumer continues processing remaining messages.
// No message is silently dropped.
func TestAtLeastOnceWithHandlerErrors(t *testing.T) {
	t.Log("TestAtLeastOnceWithHandlerErrors started")
	defer t.Log("TestAtLeastOnceWithHandlerErrors finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-alo-errors-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	messages := []string{"ok-1", "fail-msg", "ok-2", "ok-3"}
	cluster.ProduceMessages(ctx, t, topic, messages)

	var mu sync.Mutex
	var successPayloads []string
	var errorPayloads []string

	handler := func(ctx context.Context, payload []byte) error {
		msg := string(payload)
		if msg == "fail-msg" {
			mu.Lock()
			errorPayloads = append(errorPayloads, msg)
			mu.Unlock()
			return fmt.Errorf("simulated failure for %s", msg)
		}
		mu.Lock()
		successPayloads = append(successPayloads, msg)
		mu.Unlock()
		return nil
	}

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("alo-errors-group-%d", time.Now().UnixNano())),
		easykafka.WithHandler(handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
		// Default skip strategy: errors are logged and consumption continues
	)
	require.NoError(t, err)

	consumerCtx, cancel := context.WithCancel(ctx)

	var consumerErr error
	done := make(chan struct{})
	go func() {
		consumerErr = consumer.Start(consumerCtx)
		close(done)
	}()

	// Wait until all messages were dispatched (3 success + 1 error = 4 total)
	deadline := time.After(30 * time.Second)
	for {
		mu.Lock()
		total := len(successPayloads) + len(errorPayloads)
		mu.Unlock()
		if total >= len(messages) {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			mu.Lock()
			t.Fatalf("timed out, received %d success + %d errors of %d total",
				len(successPayloads), len(errorPayloads), len(messages))
			mu.Unlock()
		case <-time.After(100 * time.Millisecond):
		}
	}

	cancel()
	<-done

	assert.NoError(t, consumerErr)

	mu.Lock()
	defer mu.Unlock()

	// Verify all successful messages were received
	assert.ElementsMatch(t, []string{"ok-1", "ok-2", "ok-3"}, successPayloads)

	// Verify the failing message was still dispatched (at-least-once: handler
	// was invoked even though it returned an error)
	assert.Contains(t, errorPayloads, "fail-msg",
		"failing message must still be dispatched to the handler")

	t.Logf("at-least-once with errors: %d success, %d errors, %d total dispatched",
		len(successPayloads), len(errorPayloads), len(successPayloads)+len(errorPayloads))
}
