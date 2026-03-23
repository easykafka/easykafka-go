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

// TestBrokerReconnectionAfterRestart verifies FR-042: automatic reconnection
// on broker unavailability. The test:
// 1. Starts a consumer and verifies it processes messages
// 2. Stops the Kafka container (simulating broker failure)
// 3. Restarts the container on the same port
// 4. Produces new messages and verifies the consumer reconnects and resumes processing
func TestBrokerReconnectionAfterRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	// Start Kafka cluster
	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-reconnect-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	// Produce initial messages (before broker failure)
	preFailureMessages := []string{"before-1", "before-2", "before-3"}
	cluster.ProduceMessages(ctx, t, topic, preFailureMessages)

	// Track received messages
	var mu sync.Mutex
	var receivedPayloads []string

	handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		defer mu.Unlock()
		receivedPayloads = append(receivedPayloads, string(payload))
		t.Logf("Handler received: %s (total: %d)", string(payload), len(receivedPayloads))
		return nil
	}

	// Create consumer with reconnection-friendly settings
	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("reconnect-group-%d", time.Now().UnixNano())),
		easykafka.WithHandler(handler),
		easykafka.WithPollTimeout(200*time.Millisecond),
		easykafka.WithKafkaConfig(map[string]any{
			// Fast reconnection for testing
			"reconnect.backoff.ms":     100,
			"reconnect.backoff.max.ms": 1000,
			/*
				The session.timeout.ms: 10000 (10s) isn't strictly needed — it just speeds up the test.
				The default is 45s, meaning after the broker goes down, the consumer group coordinator would
				wait 45s before considering the consumer dead and triggering a session timeout/rejoin.
				With 10s, that cycle completes faster after the broker comes back, so the consumer re-joins
				the group and resumes consuming sooner.
			*/
			"session.timeout.ms": 10000,
		}),
	)
	require.NoError(t, err)

	// Start consumer in background
	consumerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var consumerErr error
	done := make(chan struct{})
	go func() {
		consumerErr = consumer.Start(consumerCtx)
		close(done)
	}()

	// Phase 1: Wait for pre-failure messages to be consumed
	t.Log("Phase 1: Waiting for pre-failure messages...")
	waitForMessages(t, &mu, &receivedPayloads, len(preFailureMessages), 30*time.Second)

	mu.Lock()
	preCount := len(receivedPayloads)
	mu.Unlock()
	t.Logf("Phase 1: Received %d messages before broker failure", preCount)
	require.GreaterOrEqual(t, preCount, len(preFailureMessages),
		"should have received all pre-failure messages")

	// Phase 2: Stop the Kafka container (simulate broker failure)
	t.Log("Phase 2: Stopping Kafka container (simulating broker failure)...")
	cluster.StopBroker(ctx, t)

	// Wait a bit for the consumer to notice the broker is down
	t.Log("Phase 2: Waiting for consumer to detect broker unavailability...")
	time.Sleep(3 * time.Second)

	// Phase 3: Restart the Kafka container (same port preserved by creating new container)
	t.Log("Phase 3: Restarting Kafka container...")
	cluster.StartBroker(ctx, t)

	// Wait for Kafka to be fully ready after restart
	t.Log("Phase 3: Waiting for Kafka to be ready after restart...")
	cluster.WaitForBrokerReady(ctx, t, 60*time.Second)

	// Re-create the topic on the new container (data was lost with old container)
	//cluster.CreateTopic(ctx, t, topic, 1) // disabled, what AI says above is not true, the topic will be there after restart

	// Phase 4: Produce new messages after recovery
	t.Log("Phase 4: Producing post-recovery messages...")
	postRecoveryMessages := []string{"after-1", "after-2", "after-3"}
	cluster.ProduceMessages(ctx, t, topic, postRecoveryMessages)

	// Phase 5: Wait for post-recovery messages to be consumed
	t.Log("Phase 5: Waiting for post-recovery messages...")
	totalExpected := len(preFailureMessages) + len(postRecoveryMessages)
	waitForMessages(t, &mu, &receivedPayloads, totalExpected, 60*time.Second)

	// Stop consumer
	cancel()
	<-done

	assert.NoError(t, consumerErr)

	// Verify we received all messages (at-least-once: may have duplicates)
	mu.Lock()
	defer mu.Unlock()

	t.Logf("Total messages received: %d (expected at least %d)", len(receivedPayloads), totalExpected)
	assert.GreaterOrEqual(t, len(receivedPayloads), totalExpected,
		"should have received all messages including post-recovery")

	// Verify post-recovery messages are present (proves reconnection worked)
	for _, expected := range postRecoveryMessages {
		assert.Contains(t, receivedPayloads, expected,
			"post-recovery message %q should have been received", expected)
	}
}

// TestConsumerSurvivesTransientErrors verifies that transient Kafka errors
// (non-fatal broker errors) do not crash the consumer.
func TestConsumerSurvivesTransientErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-transient-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	messages := []string{"msg-1", "msg-2", "msg-3"}
	cluster.ProduceMessages(ctx, t, topic, messages)

	var mu sync.Mutex
	var received []string

	handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		received = append(received, string(payload))
		mu.Unlock()
		return nil
	}

	// Include an unreachable broker alongside the real one. librdkafka will
	// continuously emit non-fatal error events (ERR__TRANSPORT) for the bad
	// broker while still consuming successfully from the good one. This proves
	// the consumer doesn't crash on transient/non-fatal Kafka errors.
	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers[0], "localhost:19092"),
		easykafka.WithConsumerGroup(fmt.Sprintf("test-transient-group-%d", time.Now().UnixNano())),
		easykafka.WithHandler(handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
		easykafka.WithKafkaConfig(map[string]any{
			// Fast reconnect attempts to the bad broker so errors appear quickly
			"reconnect.backoff.ms":     50,
			"reconnect.backoff.max.ms": 200,
		}),
	)
	require.NoError(t, err)

	consumerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var consumerErr error
	done := make(chan struct{})
	go func() {
		consumerErr = consumer.Start(consumerCtx)
		close(done)
	}()

	// Wait for messages
	deadline := time.After(30 * time.Second)
	for {
		mu.Lock()
		count := len(received)
		mu.Unlock()
		if count >= len(messages) {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out, received %d of %d", count, len(messages))
		case <-time.After(100 * time.Millisecond):
		}
	}

	cancel()
	<-done

	assert.NoError(t, consumerErr)

	mu.Lock()
	defer mu.Unlock()
	for _, msg := range messages {
		assert.Contains(t, received, msg)
	}
}

// waitForMessages polls until at least `count` payloads are received or timeout occurs.
func waitForMessages(t *testing.T, mu *sync.Mutex, payloads *[]string, count int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		mu.Lock()
		n := len(*payloads)
		mu.Unlock()
		if n >= count {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d messages, only received %d", count, n)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
