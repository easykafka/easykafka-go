package sharedhelpers

import (
	"context"
	"errors"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
)

// partitionLeadersTimeout bounds PartitionLeaders' retries.
const partitionLeadersTimeout = 20 * time.Second

// describeAttemptTimeout bounds one request, so that one sent to a broker
// that has just died is given up and retried in time.
const describeAttemptTimeout = 3 * time.Second

// PartitionLeaders returns the current leader of each partition of topic, as
// partition → broker id. A partition without a leader maps to -1. For example:
//
//	map[int32]int32{0: 1, 1: 2, 2: 3}
//	// partition 0 → broker 1, partition 1 → broker 2, partition 2 → broker 3
//
// Two failures are retried for a few seconds before they fail the test. A
// topic just created may be unknown for a moment to the broker that answers:
// each broker learns of it on its own, from the controller. And just after a
// broker is killed, a request can still go to it, and time out.
func (c *ThreeBrokerCluster) PartitionLeaders(tb testing.TB, topic string) map[int32]int32 {
	tb.Helper()

	deadline := time.Now().Add(partitionLeadersTimeout)
	for {
		// A new client each attempt: a request stuck on a dead broker is not
		// sent there again, since newAdmin leaves out the brokers that are down.
		admin := c.newAdmin(tb)
		leaders, err := describeLeaders(admin, topic)
		admin.Close()
		if err == nil {
			return leaders
		}
		var kafkaError kfk.Error
		unknownTopic := errors.As(err, &kafkaError) && kafkaError.Code() == kfk.ErrUnknownTopicOrPart
		timedOut := errors.Is(err, context.DeadlineExceeded)
		if (!unknownTopic && !timedOut) || time.Now().After(deadline) {
			tb.Fatalf("describing topic %s: %v", topic, err)
		}
		// Give the cluster a moment before asking again, rather than flooding
		// it with requests.
		time.Sleep(200 * time.Millisecond)
	}
}

// describeLeaders asks for topic's partitions once.
func describeLeaders(admin *kfk.AdminClient, topic string) (map[int32]int32, error) {
	ctx, cancel := context.WithTimeout(context.Background(), describeAttemptTimeout)
	defer cancel()
	result, err := admin.DescribeTopics(ctx, kfk.NewTopicCollectionOfTopicNames([]string{topic}))
	if err != nil {
		return nil, err
	}

	leaders := map[int32]int32{}
	for _, description := range result.TopicDescriptions {
		if description.Error.Code() != kfk.ErrNoError {
			return nil, description.Error
		}
		for _, partition := range description.Partitions {
			leader := int32(-1)
			if partition.Leader != nil {
				leader = int32(partition.Leader.ID)
			}
			leaders[int32(partition.Partition)] = leader
		}
	}
	return leaders, nil
}
