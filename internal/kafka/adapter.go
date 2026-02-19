package kafka

import (
	"context"
	"fmt"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/rs/zerolog"

	"github.com/easykafka/easykafka-go/internal/types"
)

// Adapter wraps confluent-kafka-go for the consumer.
type Adapter struct {
	consumer *kfk.Consumer
	config   *kfk.ConfigMap
	brokers  []string
	topic    string
	groupID  string
	logger   zerolog.Logger
}

// NewAdapter creates a new Kafka adapter from configuration.
func NewAdapter(brokers []string, topic string, groupID string, kafkaConfig map[string]any, logger zerolog.Logger) (*Adapter, error) {
	if len(brokers) == 0 {
		return nil, fmt.Errorf("at least one broker is required")
	}
	if topic == "" {
		return nil, fmt.Errorf("topic cannot be empty")
	}
	if groupID == "" {
		return nil, fmt.Errorf("group ID cannot be empty")
	}

	// Build confluent-kafka-go config
	config := &kfk.ConfigMap{}
	config.SetKey("bootstrap.servers", brokers)
	config.SetKey("group.id", groupID)
	config.SetKey("auto.offset.reset", "earliest")

	// Apply any additional Kafka configuration
	if kafkaConfig != nil {
		for key, value := range kafkaConfig {
			if err := config.SetKey(key, value); err != nil {
				return nil, fmt.Errorf("setting kafka config %s: %w", key, err)
			}
		}
	}

	return &Adapter{
		config:  config,
		brokers: brokers,
		topic:   topic,
		groupID: groupID,
		logger:  logger,
	}, nil
}

// Connect establishes a connection to Kafka.
func (a *Adapter) Connect(ctx context.Context) error {
	if a.consumer != nil {
		return fmt.Errorf("consumer already connected")
	}

	consumer, err := kfk.NewConsumer(a.config)
	if err != nil {
		return fmt.Errorf("failed to create kafka consumer: %w", err)
	}

	a.consumer = consumer

	a.logger.Info().Strs("brokers", a.brokers).Str("topic", a.topic).Str("group", a.groupID).Msg("kafka consumer connected")

	return nil
}

// SubscribeToTopic subscribes the consumer to the topic.
func (a *Adapter) SubscribeToTopic(ctx context.Context) error {
	if a.consumer == nil {
		return fmt.Errorf("consumer not connected")
	}

	err := a.consumer.SubscribeTopics([]string{a.topic}, nil)
	if err != nil {
		return fmt.Errorf("failed to subscribe to topic %s: %w", a.topic, err)
	}

	a.logger.Info().Str("topic", a.topic).Msg("subscribed to topic")

	return nil
}

// Poll retrieves messages from Kafka with timeout in milliseconds.
func (a *Adapter) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	if a.consumer == nil {
		return nil, fmt.Errorf("consumer not connected")
	}

	// Create a timeout from milliseconds
	timeout := time.Duration(timeoutMs) * time.Millisecond

	msg, err := a.consumer.ReadMessage(timeout)
	if err != nil {
		// Check for timeout
		if kfkErr, ok := err.(kfk.Error); ok && kfkErr.Code() == kfk.ErrTimedOut {
			return nil, nil
		}
		return nil, fmt.Errorf("error reading message: %w", err)
	}

	// Convert confluent message to our Message type
	headers := make(map[string]string)
	for _, h := range msg.Headers {
		headers[h.Key] = string(h.Value)
	}

	return &types.Message{
		Topic:     *msg.TopicPartition.Topic,
		Partition: msg.TopicPartition.Partition,
		Offset:    int64(msg.TopicPartition.Offset),
		Timestamp: msg.Timestamp,
		Headers:   headers,
		Payload:   msg.Value,
	}, nil
}

// CommitOffset commits an offset for a partition.
func (a *Adapter) CommitOffset(topic string, partition int32, offset int64) error {
	if a.consumer == nil {
		return fmt.Errorf("consumer not connected")
	}

	topicStr := topic
	tp := kfk.TopicPartition{
		Topic:     &topicStr,
		Partition: partition,
		Offset:    kfk.Offset(offset + 1), // Offset to commit is the next one to consume
	}

	_, err := a.consumer.CommitOffsets([]kfk.TopicPartition{tp})
	if err != nil {
		return fmt.Errorf("failed to commit offset: %w", err)
	}

	return nil
}

// Close gracefully closes the Kafka connection.
func (a *Adapter) Close(ctx context.Context) error {
	if a.consumer == nil {
		return nil
	}

	err := a.consumer.Close()
	a.consumer = nil

	if err != nil {
		return fmt.Errorf("failed to close kafka consumer: %w", err)
	}

	a.logger.Info().Msg("kafka consumer closed")

	return nil
}
