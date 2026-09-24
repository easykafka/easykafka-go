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

// TestRetryStrategyWritesToRetryTopic verifies that when a handler fails,
// the retry strategy writes the message to the retry topic with correct headers.
func TestRetryStrategyWritesToRetryTopic(t *testing.T) {
	t.Log("TestRetryStrategyWritesToRetryTopic started")
	defer t.Log("TestRetryStrategyWritesToRetryTopic finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()

	cluster := helpers.SharedCluster(t)

	sourceTopic := helpers.UniqueTopicName(t, "retry-source")
	retryTopic := helpers.UniqueTopicName(t, "retry-queue")
	dlqTopic := helpers.UniqueTopicName(t, "retry-dlq")
	consumerGroup := fmt.Sprintf("retry-test-group-%d", time.Now().UnixNano())

	cluster.CreateTopic(ctx, t, sourceTopic, 1)
	cluster.CreateTopic(ctx, t, retryTopic, 1)
	cluster.CreateTopic(ctx, t, dlqTopic, 1)

	cluster.ProduceMessages(ctx, t, sourceTopic, []string{"msg-1", "msg-2"})

	var mu sync.Mutex
	var handlerCalls int

	handler := func(ctx context.Context, payload []byte) *easykafka.Failure {
		mu.Lock()
		handlerCalls++
		mu.Unlock()
		return &easykafka.Failure{Err: fmt.Errorf("simulated failure for: %s", string(payload))}
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
	require.NoError(t, consumerErr)

	retryMsgs := cluster.ConsumeMessages(ctx, t, retryTopic,
		fmt.Sprintf("verify-retry-%d", time.Now().UnixNano()), 2, 15*time.Second)

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

	// The assertions above read the headers off the wire. Read them once more the
	// way an application has to — through a consumer, off the Message the library
	// delivers — because that path crosses a seam the wire check does not: the
	// adapter turning Kafka headers into Message.Headers, which is what the
	// exported accessors are handed. Writing a retry consumer is exactly this.
	var (
		gotAttempt int
		gotOrigin  string
		gotDue     time.Time
		gotMeta    bool
		readOnce   sync.Once
		gotRecord  = make(chan struct{})
	)

	retryReader, err := easykafka.New(
		easykafka.WithTopic(retryTopic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("retry-reader-%d", time.Now().UnixNano())),
		easykafka.WithHandler(func(handlerCtx context.Context, _ []byte) *easykafka.Failure {
			if msg, ok := easykafka.MessageFromContext(handlerCtx); ok {
				gotMeta = true
				gotAttempt = easykafka.GetRetryAttempt(msg)
				gotOrigin = easykafka.GetOriginalTopic(msg)
				gotDue = easykafka.GetRetryTime(msg)
			}
			readOnce.Do(func() { close(gotRecord) })
			return nil
		}),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	readerCtx, stopReader := context.WithTimeout(ctx, 30*time.Second)
	defer stopReader()
	readerDone := make(chan error, 1)
	go func() { readerDone <- retryReader.Start(readerCtx) }()

	select {
	case <-gotRecord:
	case <-readerCtx.Done():
		t.Fatal("retry consumer never received a republished record")
	}
	stopReader()
	require.NoError(t, <-readerDone)

	require.True(t, gotMeta, "MessageFromContext must report true in single-message mode")
	assert.Equal(t, 1, gotAttempt)
	assert.Equal(t, sourceTopic, gotOrigin, "the accessor must name the source, not the retry topic")
	assert.False(t, gotDue.IsZero(), "the library stamps a due time when it republishes")
}

// TestRetryStrategyWritesToDLQAfterMaxAttempts verifies that when max retry attempts
// are exhausted, the message is written to the DLQ topic as it was consumed, with
// the failure described in headers.
func TestRetryStrategyWritesToDLQAfterMaxAttempts(t *testing.T) {
	t.Log("TestRetryStrategyWritesToDLQAfterMaxAttempts started")
	defer t.Log("TestRetryStrategyWritesToDLQAfterMaxAttempts finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()

	cluster := helpers.SharedCluster(t)

	sourceTopic := helpers.UniqueTopicName(t, "dlq-source")
	retryTopic := helpers.UniqueTopicName(t, "dlq-retry")
	dlqTopic := helpers.UniqueTopicName(t, "dlq-target")
	consumerGroup := fmt.Sprintf("dlq-test-group-%d", time.Now().UnixNano())

	cluster.CreateTopic(ctx, t, sourceTopic, 1)
	cluster.CreateTopic(ctx, t, retryTopic, 1)
	cluster.CreateTopic(ctx, t, dlqTopic, 1)

	body := `{"orderId":"DLQ-001"}`
	cluster.ProduceMessages(ctx, t, sourceTopic, []string{body})

	handler := func(ctx context.Context, payload []byte) *easykafka.Failure {
		return &easykafka.Failure{Err: fmt.Errorf("permanent failure")}
	}

	retryStrategy, err := easykafka.NewRetryStrategy(
		easykafka.WithRetryTopic(retryTopic),
		easykafka.WithDLQTopic(dlqTopic),
		easykafka.WithMaxAttempts(1),
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
	require.NoError(t, consumerErr)

	time.Sleep(1 * time.Second)

	dlqMsgs := cluster.ConsumeMessages(ctx, t, dlqTopic,
		fmt.Sprintf("verify-dlq-%d", time.Now().UnixNano()), 1, 15*time.Second)

	require.Len(t, dlqMsgs, 1, "expected 1 message in DLQ")

	assert.Equal(t, body, string(dlqMsgs[0].Value), "DLQ body should be the consumed payload, byte for byte")

	assert.Equal(t, sourceTopic, helpers.GetHeader(dlqMsgs[0], metadata.HeaderOriginalTopic))
	assert.Equal(t, "0", helpers.GetHeader(dlqMsgs[0], metadata.HeaderOriginalPartition))
	assert.Equal(t, "0", helpers.GetHeader(dlqMsgs[0], metadata.HeaderOriginalOffset))
	assert.Contains(t, helpers.GetHeader(dlqMsgs[0], metadata.HeaderErrorMessage), "permanent failure")
	assert.Equal(t, "1", helpers.GetHeader(dlqMsgs[0], metadata.HeaderRetryAttempt))
	assert.NotEmpty(t, helpers.GetHeader(dlqMsgs[0], metadata.HeaderFailedAt))

	t.Logf("DLQ message: %s", string(dlqMsgs[0].Value))
}
