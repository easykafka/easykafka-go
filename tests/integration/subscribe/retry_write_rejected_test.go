package subscribe_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/easykafka/easykafka-go/internal/subscribe/metadata"
	"github.com/easykafka/easykafka-go/subscribe"
	"github.com/easykafka/easykafka-go/tests/integration/sharedhelpers"
	"github.com/easykafka/easykafka-go/tests/integration/subscribe/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRejectedDLQWriteStopsTheConsumerAndKeepsTheMessage is the regression test
// for a retry or DLQ write the broker refuses. Before the retry strategy wrote
// through a publisher, such a write was never confirmed: the handler failed,
// the DLQ write was rejected, the source offset advanced anyway, and Start
// returned nil. The message was lost, and the delivery-error callback was the
// only trace of it.
//
// Now HandleError waits for the broker's answer, so the rejection fails it: the
// consumer stops with an error, the source offset is not committed, and a
// fresh consumer in the same group receives the message again.
//
// The rejection is arranged by creating the DLQ topic with a tiny
// max.message.bytes. The record then passes librdkafka's own client-side check
// — which would fail the send synchronously and never reach a delivery report
// — and is rejected by the broker instead, which is the path under test.
func TestRejectedDLQWriteStopsTheConsumerAndKeepsTheMessage(t *testing.T) {
	t.Log("TestRejectedDLQWriteStopsTheConsumerAndKeepsTheMessage started")
	defer t.Log("TestRejectedDLQWriteStopsTheConsumerAndKeepsTheMessage finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()

	cluster := sharedhelpers.SharedCluster(t)

	sourceTopic := sharedhelpers.UniqueTopicName(t, "delivery-error-source")
	retryTopic := sharedhelpers.UniqueTopicName(t, "delivery-error-retry")
	dlqTopic := sharedhelpers.UniqueTopicName(t, "delivery-error-dlq")
	consumerGroup := fmt.Sprintf("delivery-error-group-%d", time.Now().UnixNano())

	cluster.CreateTopic(ctx, t, sourceTopic, 1)
	cluster.CreateTopic(ctx, t, retryTopic, 1)
	cluster.CreateTopicRejectingEverything(ctx, t, dlqTopic)

	const payload = "a message far larger than the DLQ topic will accept"
	cluster.ProduceMessages(ctx, t, sourceTopic, []string{payload})

	recorder := &sharedhelpers.PublishDeliveryErrorRecorder{}
	retryStrategy, err := subscribe.NewRetryStrategy(
		subscribe.WithRetryTopic(retryTopic),
		subscribe.WithDLQTopic(dlqTopic),
		subscribe.WithMaxAttempts(1), // straight to the DLQ
		subscribe.WithDeliveryErrorFunc(recorder.CallbackFunc),
	)
	require.NoError(t, err)

	failing := func(_ context.Context, _ []byte) *subscribe.Failure {
		return &subscribe.Failure{Err: fmt.Errorf("simulated failure")}
	}
	consumer, err := subscribe.New(
		subscribe.WithTopic(sourceTopic),
		subscribe.WithBrokers(cluster.Brokers...),
		subscribe.WithConsumerGroup(consumerGroup),
		subscribe.WithHandler(failing),
		subscribe.WithErrorStrategy(retryStrategy),
		subscribe.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	// The consumer stops on its own: nothing cancels this context.
	startCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	consumerErr := consumer.Start(startCtx)

	require.Error(t, consumerErr, "a rejected DLQ write must stop the consumer")
	require.NoError(t, startCtx.Err(), "the consumer must stop because of the write, not the test's timeout")
	assert.Contains(t, consumerErr.Error(), "not acknowledged")

	assert.Equal(t, kfk.OffsetInvalid, cluster.CommittedOffset(ctx, t, consumerGroup, sourceTopic, 0),
		"the source offset must not be committed past a message whose DLQ write failed")

	delivered := recorder.Errors()
	require.Len(t, delivered, 1, "the callback hears of the rejected record once")
	deliveryError := delivered[0]
	assert.Equal(t, dlqTopic, deliveryError.Topic, "the write was aimed at the DLQ")
	require.Error(t, deliveryError.Err)
	assert.Equal(t, kfk.ErrMsgSizeTooLarge.String(), deliveryError.Code, "the broker rejected the record for size")
	assert.Equal(t, payload, string(deliveryError.Value), "the record body is carried through")
	headers := map[string]string{}
	for _, header := range deliveryError.Headers {
		headers[header.Key] = string(header.Value)
	}
	assert.Equal(t, sourceTopic, headers[metadata.HeaderOriginalTopic], "the failure headers are carried through")

	// A fresh consumer in the same group receives the message again.
	received := make(chan string, 1)
	succeeding := func(_ context.Context, body []byte) *subscribe.Failure {
		select {
		case received <- string(body):
		default:
		}
		return nil
	}
	again, err := subscribe.New(
		subscribe.WithTopic(sourceTopic),
		subscribe.WithBrokers(cluster.Brokers...),
		subscribe.WithConsumerGroup(consumerGroup),
		subscribe.WithHandler(succeeding),
		subscribe.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)
	stop := helpers.RunUntil(ctx, t, again)

	select {
	case body := <-received:
		assert.Equal(t, payload, body)
	case <-time.After(30 * time.Second):
		_ = stop()
		require.FailNow(t, "the message was not consumed again after the consumer stopped")
	}
	require.NoError(t, stop())
}
