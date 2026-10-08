package helpers

import (
	"testing"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
)

// CreateTopicLedByEachBroker creates a three-partition topic, replication
// factor 3 and min.insync.replicas=2, in which partition i is led by broker
// i+1: each broker leads exactly one partition. It waits until every
// partition is fully in sync, as CreateTopic does.
//
// A partition's first listed replica is its preferred leader, the one a new
// topic gets, so the assignment below is what pins the leaders. Without it,
// Kafka usually spreads them the same way, but does not promise to.
func (c *ThreeBrokerCluster) CreateTopicLedByEachBroker(tb testing.TB, topic string) {
	tb.Helper()

	c.createTopic(tb, kfk.TopicSpecification{
		Topic:         topic,
		NumPartitions: threeBrokerCount,
		// The outer slice has one entry per partition, its index being the
		// partition number. Each inner slice lists the broker ids holding that
		// partition's copies: the first is its leader, and if it goes away the
		// next one still in sync takes over. So partition 0 is led by broker
		// 1, then 2, then 3.
		ReplicaAssignment: [][]int32{{1, 2, 3}, {2, 3, 1}, {3, 1, 2}},
		Config:            map[string]string{"min.insync.replicas": "2"},
	})
	tb.Logf("created topic %s, partitions 0, 1 and 2 led by brokers 1, 2 and 3", topic)
}
