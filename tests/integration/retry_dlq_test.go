package integration

import (
	"context"
	"encoding/json"
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

// TestRetryStrategyWritesToRetryTopic verifies that when a handler fails,
// the retry strategy writes the message to the retry topic with correct headers.
func TestRetryStrategyWritesToRetryTopic(t *testing.T) {
	t.Log("TestRetryStrategyWritesToRetryTopic started")
	defer t.Log("TestRetryStrategyWritesToRetryTopic finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	sourceTopic := "retry-source-" + suffix
	retryTopic := "retry-queue-" + suffix
	dlqTopic := "retry-dlq-" + suffix
	consumerGroup := "retry-test-group-" + suffix

	cluster.CreateTopic(ctx, t, sourceTopic, 1)
	cluster.CreateTopic(ctx, t, retryTopic, 1)
	cluster.CreateTopic(ctx, t, dlqTopic, 1)

	cluster.ProduceMessages(ctx, t, sourceTopic, []string{"msg-1", "msg-2"})

	var mu sync.Mutex
	var handlerCalls int

	handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		handlerCalls++
		mu.Unlock()
		return fmt.Errorf("simulated failure for: %s", string(payload))
	}

	retryStrategy, err := easykafka.NewRetryStrategy(
		easykafka.WithRetryTopic(retryTopic),
		easykafka.WithDLQTopic(dlqTopic),
		easykafka.WithMaxAttempts(3),
		easykafka.WithInitialDelay(1*time.Second),
	)
	require.NoError(t, err)

	consumer, err := easykafka.New(
		easykafka.WithTopic(sourceTopic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(consumerGroup),
		easykafka.WithHandler(handler),
		easykafka.WithErrorStrategy(retryStrategy),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	consumerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- consumer.Start(consumerCtx)
	}()

	deadline := time.After(30 * time.Second)
	for {
		mu.Lock()
		calls := handlerCalls
		mu.Unlock()

		if calls >= 2 {
			break
		}

		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out waiting for handler calls, got %d", calls)
		case <-time.After(100 * time.Millisecond):
		}
	}

	time.Sleep(2 * time.Second)

	cancel()
	consumerErr := <-done
	assert.NoError(t, consumerErr)

	retryMsgs := cluster.ConsumeMessages(ctx, t, retryTopic,
		"verify-retry-"+suffix, 2, 15*time.Second)

	require.Len(t, retryMsgs, 2, "expected 2 messages in retry topic")

	for i, msg := range retryMsgs {
		attempt := helpers.GetHeader(msg, metadata.HeaderRetryAttempt)
		assert.Equal(t, "1", attempt, "retry attempt should be 1 for msg %d", i)

		origTopic := helpers.GetHeader(msg, metadata.HeaderOriginalTopic)
		assert.Equal(t, sourceTopic, origTopic, "original topic should be source for msg %d", i)

		retryTime := helpers.GetHeader(msg, metadata.HeaderRetryTime)
		assert.NotEmpty(t, retryTime, "retry time should be set for msg %d", i)

		errMsg := helpers.GetHeader(msg, metadata.HeaderErrorMessage)
		assert.Contains(t, errMsg, "simulated failure", "error message should be set for msg %d", i)

		t.Logf("Retry message %d: attempt=%s, origTopic=%s, retryTime=%s, payload=%s",
			i, attempt, origTopic, retryTime, string(msg.Value))
	}
}

// TestRetryStrategyWritesToDLQAfterMaxAttempts verifies that when max retry attempts
// are exhausted, the message is written to the DLQ topic with a JSON envelope.
func TestRetryStrategyWritesToDLQAfterMaxAttempts(t *testing.T) {
	t.Log("TestRetryStrategyWritesToDLQAfterMaxAttempts started")
	defer t.Log("TestRetryStrategyWritesToDLQAfterMaxAttempts finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	sourceTopic := "dlq-source-" + suffix
	retryTopic := "dlq-retry-" + suffix
	dlqTopic := "dlq-target-" + suffix
	consumerGroup := "dlq-test-group-" + suffix

	cluster.CreateTopic(ctx, t, sourceTopic, 1)
	cluster.CreateTopic(ctx, t, retryTopic, 1)
	cluster.CreateTopic(ctx, t, dlqTopic, 1)

	cluster.ProduceMessages(ctx, t, sourceTopic, []string{`{"orderId":"DLQ-001"}`})

	handler := func(ctx context.Context, payload []byte) error {
		return fmt.Errorf("permanent failure")
	}

	retryStrategy, err := easykafka.NewRetryStrategy(
		easykafka.WithRetryTopic(retryTopic),
		easykafka.WithDLQTopic(dlqTopic),
		easykafka.WithMaxAttempts(1),
		easykafka.WithFailedMessagePayloadEncoding(easykafka.PayloadEncodingJSON),
	)
	require.NoError(t, err)

	consumer, err := easykafka.New(
		easykafka.WithTopic(sourceTopic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(consumerGroup),
		easykafka.WithHandler(handler),
		easykafka.WithErrorStrategy(retryStrategy),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	consumerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- consumer.Start(consumerCtx)
	}()

	time.Sleep(5 * time.Second)

	cancel()
	consumerErr := <-done
	assert.NoError(t, consumerErr)

	time.Sleep(1 * time.Second)

	dlqMsgs := cluster.ConsumeMessages(ctx, t, dlqTopic,
		"verify-dlq-"+suffix, 1, 15*time.Second)

	require.Len(t, dlqMsgs, 1, "expected 1 message in DLQ")

	var envelope map[string]interface{}
	err = json.Unmarshal(dlqMsgs[0].Value, &envelope)
	require.NoError(t, err, "DLQ message should be valid JSON")

	assert.Equal(t, sourceTopic, envelope["originalTopic"], "originalTopic should match source")
	assert.Contains(t, envelope["error"], "permanent failure", "error should contain handler error")
	assert.Contains(t, envelope["payload"], "DLQ-001", "payload should contain original message")
	assert.Equal(t, "json", envelope["payloadEncoding"], "payloadEncoding should be json")
	assert.NotNil(t, envelope["timestamp"], "timestamp should be set")
	assert.NotNil(t, envelope["attemptCount"], "attemptCount should be set")

	origTopic := helpers.GetHeader(dlqMsgs[0], metadata.HeaderOriginalTopic)
	assert.Equal(t, sourceTopic, origTopic, "DLQ header original topic should match source")

	t.Logf("DLQ message: %s", string(dlqMsgs[0].Value))
}
