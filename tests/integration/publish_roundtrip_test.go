package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishRoundTrip verifies that what the publisher writes is what a
// consumer reads: the key, the JSON value byte for byte as encoding/json
// writes it, the topic's headers before the call's in order with a repeated
// key kept, and the partition and offset its Result reported. An empty string
// key arrives as an empty key, not a null one, and Delete writes a tombstone.
func TestPublishRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)
	topicName := helpers.UniqueTopicName(t, "publish-roundtrip")
	cluster.CreateTopic(ctx, t, topicName, 3)

	publisher := helpers.NewPublisher(t, cluster.Brokers)
	writer := publisher.Bind(helpers.PublishInvoiceTopic(topicName))

	invoice := helpers.NewPublishInvoice("INV-1")
	delivery, err := writer.Send("player-1", invoice,
		publish.Header{Key: "trace-id", Value: []byte("abc")},
		publish.Header{Key: "__TypeId__", Value: []byte("Override")})
	require.NoError(t, err)
	result, err := delivery.Wait(ctx)
	require.NoError(t, err)
	assert.Equal(t, topicName, result.Topic)

	require.NoError(t, writer.Publish(ctx, "", helpers.NewPublishInvoice("INV-2")))
	require.NoError(t, writer.Delete(ctx, "player-1", publish.Header{Key: "reason", Value: []byte("closed")}))

	messages := cluster.ConsumeMessages(ctx, t, topicName, fmt.Sprintf("roundtrip-%d", time.Now().UnixNano()),
		3, 30*time.Second)
	require.Len(t, messages, 3)

	written := helpers.FindMessage(t, messages, result.Partition, result.Offset)
	assert.Equal(t, []byte("player-1"), written.Key)
	expectedValue, err := json.Marshal(invoice)
	require.NoError(t, err)
	assert.Equal(t, expectedValue, written.Value, "the value must be encoding/json's bytes")
	assert.Equal(t, []kfk.Header{
		{Key: "__TypeId__", Value: []byte("Invoice")},
		{Key: "trace-id", Value: []byte("abc")},
		{Key: "__TypeId__", Value: []byte("Override")},
	}, written.Headers)

	var emptyKeyRecords, tombstones int
	for _, message := range messages {
		switch {
		case message.Value == nil:
			tombstones++
			assert.Equal(t, []byte("player-1"), message.Key)
			assert.Equal(t, result.Partition, message.TopicPartition.Partition,
				"a tombstone for a key must land on that key's partition")
			assert.Equal(t, []kfk.Header{
				{Key: "__TypeId__", Value: []byte("Invoice")},
				{Key: "reason", Value: []byte("closed")},
			}, message.Headers)
		case len(message.Key) == 0:
			// The record published with key "". Its length is 0 whether the key
			// came back empty or null, since len of a nil slice is 0 too; the
			// NotNil below tells them apart. Kafka keeps an empty key ([]byte{})
			// apart from a null one (nil), and the default partitioner hashes an
			// empty key but places a null one at random.
			emptyKeyRecords++
			assert.NotNil(t, message.Key, `StringKey("") must write an empty key, not a null one`)
		}
	}
	assert.Equal(t, 1, tombstones, "Delete must write one record with a nil value")
	assert.Equal(t, 1, emptyKeyRecords)
}
