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

// TestKafkaConfigPassthrough verifies that custom KafkaConfig values are passed
// through to confluent-kafka-go and affect consumer behavior.
func TestKafkaConfigPassthrough(t *testing.T) {
	t.Log("TestKafkaConfigPassthrough started")
	defer t.Log("TestKafkaConfigPassthrough finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-passthrough-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	preMessages := []string{"pre-1", "pre-2", "pre-3"}
	cluster.ProduceMessages(ctx, t, topic, preMessages)

	time.Sleep(500 * time.Millisecond)

	var mu sync.Mutex
	var receivedPayloads []string

	handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		receivedPayloads = append(receivedPayloads, string(payload))
		t.Logf("handler received message: %s", string(payload))
		mu.Unlock()
		return nil
	}

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("test-passthrough-group-%d", time.Now().UnixNano())),
		easykafka.WithHandler(handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
		easykafka.WithKafkaConfig(map[string]any{
			"auto.offset.reset": "latest",
		}),
	)
	require.NoError(t, err)

	consumerCtx, cancel := context.WithCancel(ctx)

	var consumerErr error
	done := make(chan struct{})

	go func() {
		consumerErr = consumer.Start(consumerCtx)
		close(done)
	}()

	time.Sleep(3 * time.Second)

	postMessages := []string{"post-1", "post-2"}
	cluster.ProduceMessages(ctx, t, topic, postMessages)

	deadline := time.After(30 * time.Second)
	for {
		mu.Lock()
		count := len(receivedPayloads)
		mu.Unlock()

		if count >= len(postMessages) {
			break
		}

		select {
		case <-deadline:
			cancel()
			<-done
			mu.Lock()
			t.Fatalf("timed out waiting for post-messages, received: %v", receivedPayloads)
			mu.Unlock()
		case <-time.After(100 * time.Millisecond):
		}
	}

	cancel()
	<-done
	assert.NoError(t, consumerErr)

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, postMessages, receivedPayloads,
		"consumer with auto.offset.reset=latest should only receive messages produced after subscription")
}

// TestKafkaConfigSessionTimeout verifies that session.timeout.ms is properly
// passed through to confluent-kafka-go.
func TestKafkaConfigSessionTimeout(t *testing.T) {
	t.Log("TestKafkaConfigSessionTimeout started")
	defer t.Log("TestKafkaConfigSessionTimeout finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-session-timeout-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	var mu sync.Mutex
	var receivedPayloads []string

	handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		receivedPayloads = append(receivedPayloads, string(payload))
		mu.Unlock()
		return nil
	}

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("test-session-group-%d", time.Now().UnixNano())),
		easykafka.WithHandler(handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
		easykafka.WithKafkaConfig(map[string]any{
			"session.timeout.ms":    10000, // consider consumer 10 after 10secs without hearbeat
			"heartbeat.interval.ms": 4500,  // send heartbeat every 4.5 which differs fromd efault val (3sec)
		}),
	)
	require.NoError(t, err)

	cluster.ProduceMessages(ctx, t, topic, []string{"hello"})

	consumerCtx, cancel := context.WithCancel(ctx)

	var consumerErr error
	done := make(chan struct{})

	go func() {
		consumerErr = consumer.Start(consumerCtx)
		close(done)
	}()

	deadline := time.After(30 * time.Second)
	for {
		mu.Lock()
		count := len(receivedPayloads)
		mu.Unlock()

		if count >= 1 {
			break
		}

		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("timed out waiting for message with custom session timeout")
		case <-time.After(100 * time.Millisecond):
		}
	}

	cancel()
	<-done
	assert.NoError(t, consumerErr)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"hello"}, receivedPayloads)
}

// TestKafkaConfigManagedKeyRejection verifies that managed keys are rejected
// when passed via WithKafkaConfig.
func TestKafkaConfigManagedKeyRejection(t *testing.T) {
	t.Log("TestKafkaConfigManagedKeyRejection started")
	defer t.Log("TestKafkaConfigManagedKeyRejection finished")

	tests := []struct {
		name   string
		key    string
		val    any
		errMsg string
	}{
		{"bootstrap.servers", "bootstrap.servers", "other:9092", "kafka config key \"bootstrap.servers\" is managed by the library (managed by WithBrokers) and cannot be overridden"},
		{"group.id", "group.id", "other-group", "kafka config key \"group.id\" is managed by the library (managed by WithConsumerGroup) and cannot be overridden"},
		{"enable.auto.commit", "enable.auto.commit", true, "kafka config key \"enable.auto.commit\" is managed by the library (managed by the library for explicit offset control) and cannot be overridden"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := easykafka.New(
				easykafka.WithTopic("test-topic"),
				easykafka.WithBrokers("localhost:9092"),
				easykafka.WithConsumerGroup("test-group"),
				easykafka.WithHandler(func(ctx context.Context, payload []byte) error {
					return nil
				}),
				easykafka.WithKafkaConfig(map[string]any{
					tc.key: tc.val,
				}),
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errMsg)
		})
	}
}
