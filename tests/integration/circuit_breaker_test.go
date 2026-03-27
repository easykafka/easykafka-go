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

// TestCircuitBreakerStopsConsumerAfterThreshold verifies that the circuit breaker
// opens after consecutive handler failures reach the threshold, causing the
// consumer to stop with a circuit breaker error.
func TestCircuitBreakerStopsConsumerAfterThreshold(t *testing.T) {
	t.Log("TestCircuitBreakerStopsConsumerAfterThreshold started")
	defer t.Log("TestCircuitBreakerStopsConsumerAfterThreshold finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	topic := "cb-test-" + suffix
	consumerGroup := "cb-group-" + suffix

	cluster.CreateTopic(ctx, t, topic, 1)

	messages := []string{"msg-1", "msg-2", "msg-3", "msg-4", "msg-5"}
	cluster.ProduceMessages(ctx, t, topic, messages)

	var mu sync.Mutex
	var handlerCalls int

	handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		handlerCalls++
		call := handlerCalls
		mu.Unlock()
		t.Logf("Handler called (%d): %s", call, string(payload))
		return fmt.Errorf("downstream service unavailable")
	}

	cbStrategy, err := easykafka.NewCircuitBreakerStrategy(
		easykafka.WithFailureThreshold(3),
		easykafka.WithCooldownPeriod(60*time.Second),
	)
	require.NoError(t, err)

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(consumerGroup),
		easykafka.WithHandler(handler),
		easykafka.WithErrorStrategy(cbStrategy),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	consumerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	consumerErr := consumer.Start(consumerCtx)

	require.Error(t, consumerErr, "consumer should stop when circuit breaker opens")
	assert.Contains(t, consumerErr.Error(), "circuit breaker", "error should mention circuit breaker")

	mu.Lock()
	finalCalls := handlerCalls
	mu.Unlock()
	assert.Equal(t, 3, finalCalls, "handler should be called exactly threshold times")

	t.Logf("Consumer stopped with error: %v (after %d handler calls)", consumerErr, finalCalls)
}

// TestCircuitBreakerAllowsCleanShutdownBeforeThreshold verifies that a consumer
// with a circuit breaker strategy can be cleanly shut down before the threshold is reached.
func TestCircuitBreakerAllowsCleanShutdownBeforeThreshold(t *testing.T) {
	t.Log("TestCircuitBreakerAllowsCleanShutdownBeforeThreshold started")
	defer t.Log("TestCircuitBreakerAllowsCleanShutdownBeforeThreshold finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	topic := "cb-clean-" + suffix
	consumerGroup := "cb-clean-group-" + suffix

	cluster.CreateTopic(ctx, t, topic, 1)

	cluster.ProduceMessages(ctx, t, topic, []string{"single-msg"})

	var called sync.WaitGroup
	called.Add(1)

	handler := func(ctx context.Context, payload []byte) error {
		called.Done()
		return fmt.Errorf("one failure")
	}

	cbStrategy, err := easykafka.NewCircuitBreakerStrategy(
		easykafka.WithFailureThreshold(100),
	)
	require.NoError(t, err)

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(consumerGroup),
		easykafka.WithHandler(handler),
		easykafka.WithErrorStrategy(cbStrategy),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	consumerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- consumer.Start(consumerCtx)
	}()

	called.Wait()

	cancel()
	consumerErr := <-done

	assert.NoError(t, consumerErr, "consumer should shut down cleanly before threshold")
}
