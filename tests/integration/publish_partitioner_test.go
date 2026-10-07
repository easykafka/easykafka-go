package integration

import (
	"context"
	"testing"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// partitionerTestPartitions is the partition count of the partitioner tests'
// topics: enough that two hashes rarely agree by chance.
const partitionerTestPartitions = 12

// TestJavaCompatiblePartitionerMatchesJavaHash verifies that
// PartitionerJavaCompatible places every key where the Java client's default
// partitioner does, toPositive(murmur2(key)) % partitions, so a Go and a Java
// writer of one topic agree on each key's partition.
//
// The expected partition comes from a Go port of the Java client's murmur2,
// itself checked first against Kafka's own reference vectors, so the test does
// not compare librdkafka against a copy of librdkafka.
func TestJavaCompatiblePartitionerMatchesJavaHash(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	for _, vector := range helpers.Murmur2ReferenceVectors() {
		require.Equal(t, vector.Hash, helpers.JavaMurmur2([]byte(vector.Key)),
			"the murmur2 port disagrees with the Java client on %q", vector.Key)
	}

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)
	topicName := helpers.UniqueTopicName(t, "publish-java-partitioner")
	cluster.CreateTopic(ctx, t, topicName, partitionerTestPartitions)

	publisher := helpers.NewPublisher(t, cluster.Brokers, publish.WithPartitioner(publish.PartitionerJavaCompatible))
	writer := publisher.Bind(helpers.PublishInvoiceTopic(topicName))

	for _, key := range helpers.PartitionerTestKeys() {
		delivery, err := writer.Send(key, helpers.NewPublishInvoice(key))
		require.NoError(t, err)
		result, err := delivery.Wait(ctx)
		require.NoError(t, err)
		assert.Equal(t, helpers.JavaPartition([]byte(key), partitionerTestPartitions), result.Partition,
			"key %q", key)
	}
}

// TestDefaultPartitionerIsNotJavaCompatible is the control for the test above:
// the same keys through the default partitioner do not all land where the
// Java client puts them, which shows that test can fail.
func TestDefaultPartitionerIsNotJavaCompatible(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)
	topicName := helpers.UniqueTopicName(t, "publish-default-partitioner")
	cluster.CreateTopic(ctx, t, topicName, partitionerTestPartitions)

	publisher := helpers.NewPublisher(t, cluster.Brokers)
	writer := publisher.Bind(helpers.PublishInvoiceTopic(topicName))

	elsewhere := 0
	for _, key := range helpers.PartitionerTestKeys() {
		delivery, err := writer.Send(key, helpers.NewPublishInvoice(key))
		require.NoError(t, err)
		result, err := delivery.Wait(ctx)
		require.NoError(t, err)
		if result.Partition != helpers.JavaPartition([]byte(key), partitionerTestPartitions) {
			elsewhere++
		}
	}
	t.Logf("%d of %d keys placed elsewhere than the Java client would", elsewhere, len(helpers.PartitionerTestKeys()))
	assert.Positive(t, elsewhere)
}

// TestJavaCompatibleNullKeyIsSpread verifies the one difference between the
// two murmur2 partitioners: PartitionerJavaCompatible spreads null-key records
// over partitions, where PartitionerMurmur2 hashes a null key as empty input
// and sends every such record to one partition.
//
// Sticky partitioning is turned off, so each null-key record is placed on its
// own rather than the whole run sticking to one partition for a while.
func TestJavaCompatibleNullKeyIsSpread(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	const records = 24

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)

	partitionsUsed := func(partitioner publish.Partitioner) map[int32]bool {
		topicName := helpers.UniqueTopicName(t, "publish-null-key-"+string(partitioner))
		cluster.CreateTopic(ctx, t, topicName, partitionerTestPartitions)
		//nolint:contextcheck // NewPublisher closes the publisher at cleanup, after ctx's test is over
		publisher := helpers.NewPublisher(t, cluster.Brokers,
			publish.WithPartitioner(partitioner),
			publish.WithKafkaConfig(map[string]any{"sticky.partitioning.linger.ms": 0}))
		writer := publisher.Bind(publish.Topic[[]byte, []byte]{
			Name:        topicName,
			EncodeKey:   publish.BytesKey,
			EncodeValue: publish.RawValue,
		})

		used := map[int32]bool{}
		for range records {
			delivery, err := writer.Send(nil, []byte("no key"))
			require.NoError(t, err)
			result, err := delivery.Wait(ctx)
			require.NoError(t, err)
			used[result.Partition] = true
		}
		return used
	}

	assert.Greater(t, len(partitionsUsed(publish.PartitionerJavaCompatible)), 1,
		"null-key records must be spread over partitions")
	assert.Len(t, partitionsUsed(publish.PartitionerMurmur2), 1,
		"PartitionerMurmur2 must send every null-key record to one partition")
}
