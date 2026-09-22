package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeliveryErrorCallbackFiresOnRejectedWrite pins the mapping in
// DeliveryErrorFor against a real librdkafka delivery report rather than a
// fabricated one. The unit tests cover the field mapping; this covers the fact
// that a report with these fields populated is what actually arrives.
//
// The rejection is arranged by creating the retry topic with a tiny
// max.message.bytes. The record then passes librdkafka's own client-side check
// — which would fail the Produce call synchronously and never reach a delivery
// report — and is rejected by the broker instead, which is the path under test.
//
// Note what this test also demonstrates: the consumer sees no error at all. The
// handler failed, the retry write was rejected, the source offset advanced
// anyway, and Start returns nil. That is finding 6, and this callback is the
// only thing that makes it visible.
func TestDeliveryErrorCallbackFiresOnRejectedWrite(t *testing.T) {
	t.Log("TestDeliveryErrorCallbackFiresOnRejectedWrite started")
	defer t.Log("TestDeliveryErrorCallbackFiresOnRejectedWrite finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()

	cluster := helpers.SharedCluster(t)

	sourceTopic := helpers.UniqueTopicName(t, "delivery-error-source")
	retryTopic := helpers.UniqueTopicName(t, "delivery-error-retry")
	dlqTopic := helpers.UniqueTopicName(t, "delivery-error-dlq")
	consumerGroup := fmt.Sprintf("delivery-error-group-%d", time.Now().UnixNano())

	cluster.CreateTopic(ctx, t, sourceTopic, 1)
	cluster.CreateTopic(ctx, t, dlqTopic, 1)
	createTopicRejectingEverything(ctx, t, cluster, retryTopic)

	const payload = "a message far larger than the retry topic will accept"
	cluster.ProduceMessages(ctx, t, sourceTopic, []string{payload})

	var mu sync.Mutex
	var deliveryErrors []easykafka.DeliveryError

	onDeliveryError := func(de easykafka.DeliveryError) {
		mu.Lock()
		defer mu.Unlock()
		deliveryErrors = append(deliveryErrors, de)
	}

	retryStrategy, err := easykafka.NewRetryStrategy(
		easykafka.WithRetryTopic(retryTopic),
		easykafka.WithDLQTopic(dlqTopic),
		easykafka.WithMaxAttempts(3),
		easykafka.WithInitialDelay(1*time.Second),
		easykafka.WithDeliveryErrorFunc(onDeliveryError),
	)
	require.NoError(t, err)

	handler := func(_ context.Context, _ []byte) error {
		return fmt.Errorf("simulated failure")
	}

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
		got := len(deliveryErrors)
		mu.Unlock()

		if got > 0 {
			break
		}

		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("timed out waiting for the delivery error callback")
		case <-time.After(100 * time.Millisecond):
		}
	}

	cancel()
	consumerErr := <-done

	// The consumer is none the wiser: the retry write was rejected and it
	// stopped cleanly regardless. Asserted rather than merely observed, because
	// it is the whole reason the callback has to exist.
	require.NoError(t, consumerErr)

	mu.Lock()
	defer mu.Unlock()

	require.NotEmpty(t, deliveryErrors)
	de := deliveryErrors[0]

	assert.Equal(t, retryTopic, de.Topic, "the write was aimed at the retry topic")
	require.Error(t, de.Err)
	assert.Equal(t, kfk.ErrMsgSizeTooLarge.String(), de.Code,
		"the broker rejected the record for size")
	assert.Equal(t, payload, string(de.Value),
		"the record body is carried through, since this is the last copy of it")
	assert.Equal(t, sourceTopic, de.Headers[metadata.HeaderOriginalTopic],
		"retry headers are carried through")

	t.Logf("delivery error: topic=%s code=%s err=%v", de.Topic, de.Code, de.Err)
}

// createTopicRejectingEverything creates a topic whose max.message.bytes is too
// small for any real record, so the broker rejects the produce request and
// reports it through a delivery report.
func createTopicRejectingEverything(
	ctx context.Context,
	t *testing.T,
	cluster *helpers.KafkaTestCluster,
	topic string,
) {

	t.Helper()

	admin, err := kfk.NewAdminClient(&kfk.ConfigMap{
		"bootstrap.servers": cluster.Brokers[0],
	})
	require.NoError(t, err)
	defer admin.Close()

	adminCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	results, err := admin.CreateTopics(adminCtx, []kfk.TopicSpecification{{
		Topic:             topic,
		NumPartitions:     1,
		ReplicationFactor: 1,
		Config: map[string]string{
			// Smaller than any record the retry strategy writes, but large
			// enough that the topic is valid.
			"max.message.bytes": "1",
		},
	}})
	require.NoError(t, err)

	for _, r := range results {
		require.Equal(t, kfk.ErrNoError, r.Error.Code(), "creating topic %s: %v", r.Topic, r.Error)
	}

	t.Logf("created topic %s with max.message.bytes=1", topic)
}
