package helpers

import (
	"context"
	"fmt"
	"testing"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/testcontainers/testcontainers-go/modules/kafka"
)

// KafkaTestCluster manages a Kafka container for integration tests.
type KafkaTestCluster struct {
	Container *kafka.KafkaContainer
	Brokers   []string
}

// StartKafkaCluster starts a Kafka container using confluentinc/cp-kafka.
func StartKafkaCluster(ctx context.Context, t *testing.T) *KafkaTestCluster {
	t.Helper()

	container, err := kafka.Run(ctx, "confluentinc/cp-kafka:7.5.0",
		kafka.WithClusterID("test-cluster"),
	)
	if err != nil {
		t.Fatalf("failed to start kafka container: %v", err)
	}

	brokers, err := container.Brokers(ctx)
	if err != nil {
		t.Fatalf("failed to get broker addresses: %v", err)
	}

	t.Logf("Kafka container started, brokers: %v", brokers)

	return &KafkaTestCluster{
		Container: container,
		Brokers:   brokers,
	}
}

// Stop terminates the Kafka container.
func (k *KafkaTestCluster) Stop(ctx context.Context, t *testing.T) {
	t.Helper()
	if err := k.Container.Terminate(ctx); err != nil {
		t.Logf("warning: failed to terminate kafka container: %v", err)
	}
}

// CreateTopic creates a topic with the given name and partitions using an admin client.
func (k *KafkaTestCluster) CreateTopic(ctx context.Context, t *testing.T, topic string, partitions int) {
	t.Helper()

	admin, err := kfk.NewAdminClient(&kfk.ConfigMap{
		"bootstrap.servers": k.Brokers[0],
	})
	if err != nil {
		t.Fatalf("failed to create admin client: %v", err)
	}
	defer admin.Close()

	results, err := admin.CreateTopics(ctx, []kfk.TopicSpecification{
		{
			Topic:             topic,
			NumPartitions:     partitions,
			ReplicationFactor: 1,
		},
	})
	if err != nil {
		t.Fatalf("failed to create topic: %v", err)
	}

	for _, result := range results {
		if result.Error.Code() != kfk.ErrNoError {
			t.Fatalf("failed to create topic %s: %v", result.Topic, result.Error)
		}
	}

	t.Logf("Created topic %s with %d partitions", topic, partitions)
}

// ProduceMessages produces messages to a topic and waits for delivery.
func (k *KafkaTestCluster) ProduceMessages(ctx context.Context, t *testing.T, topic string, messages []string) {
	t.Helper()

	producer, err := kfk.NewProducer(&kfk.ConfigMap{
		"bootstrap.servers": k.Brokers[0],
	})
	if err != nil {
		t.Fatalf("failed to create producer: %v", err)
	}
	defer producer.Close()

	deliveryChan := make(chan kfk.Event, len(messages))

	for i, msg := range messages {
		err := producer.Produce(&kfk.Message{
			TopicPartition: kfk.TopicPartition{
				Topic:     &topic,
				Partition: kfk.PartitionAny,
			},
			Value: []byte(msg),
		}, deliveryChan)
		if err != nil {
			t.Fatalf("failed to produce message %d: %v", i, err)
		}
	}

	// Wait for deliveries
	for i := 0; i < len(messages); i++ {
		ev := <-deliveryChan
		m := ev.(*kfk.Message)
		if m.TopicPartition.Error != nil {
			t.Fatalf("delivery failed for message %d: %v", i, m.TopicPartition.Error)
		}
	}

	t.Logf("Produced %d messages to topic %s", len(messages), topic)
}

// UniqueTopicName generates a unique topic name for a test.
func UniqueTopicName(t *testing.T, prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, t.Name())
}
