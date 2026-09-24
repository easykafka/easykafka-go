package helpers

import (
	"context"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/stretchr/testify/require"
)

// CreateTopicRejectingEverything creates a topic whose max.message.bytes is too
// small for any real record, so the broker rejects the produce request and
// reports it through a delivery report.
func (k *KafkaTestCluster) CreateTopicRejectingEverything(ctx context.Context, t *testing.T, topic string) {
	t.Helper()

	admin, err := kfk.NewAdminClient(&kfk.ConfigMap{
		"bootstrap.servers": k.Brokers[0],
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
