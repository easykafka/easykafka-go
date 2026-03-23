package kafka

import (
	"context"
	"fmt"
	"strings"
	"sync"

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

	// Rebalance tracking
	mu                 sync.Mutex
	assignedPartitions []kfk.TopicPartition

	// Reconnection tracking (FR-042)
	brokerConnected bool
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
	if err := config.SetKey("bootstrap.servers", strings.Join(brokers, ",")); err != nil {
		return nil, fmt.Errorf("setting bootstrap.servers: %w", err)
	}
	if err := config.SetKey("group.id", groupID); err != nil {
		return nil, fmt.Errorf("setting group.id: %w", err)
	}
	if err := config.SetKey("auto.offset.reset", "earliest"); err != nil {
		return nil, fmt.Errorf("setting auto.offset.reset: %w", err)
	}

	// Disable auto-commit for explicit offset management (FR-044: offset commit protection)
	if err := config.SetKey("enable.auto.commit", false); err != nil {
		return nil, fmt.Errorf("setting enable.auto.commit: %w", err)
	}

	// FR-042: Configure reconnection backoff defaults.
	// confluent-kafka-go (librdkafka) handles automatic reconnection natively.
	// These defaults ensure reasonable backoff with exponential increase.
	reconnectDefaults := map[string]any{
		"reconnect.backoff.ms":     100,   // initial backoff
		"reconnect.backoff.max.ms": 10000, // max backoff (10s)
	}
	for key, value := range reconnectDefaults {
		if err := config.SetKey(key, value); err != nil {
			return nil, fmt.Errorf("setting %s: %w", key, err)
		}
	}

	// Apply any additional Kafka configuration (user can override defaults including reconnect settings)
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

	a.mu.Lock()
	a.brokerConnected = true
	a.mu.Unlock()

	a.logger.Info().
		Strs("brokers", a.brokers).
		Str("topic", a.topic).
		Str("group", a.groupID).
		Msg("kafka consumer connected")

	return nil
}

// SubscribeToTopic subscribes the consumer to the topic with rebalance handling.
func (a *Adapter) SubscribeToTopic(ctx context.Context) error {
	if a.consumer == nil {
		return fmt.Errorf("consumer not connected")
	}

	err := a.consumer.SubscribeTopics([]string{a.topic}, a.rebalanceCallback)
	if err != nil {
		return fmt.Errorf("failed to subscribe to topic %s: %w", a.topic, err)
	}

	a.logger.Info().Str("topic", a.topic).Msg("subscribed to topic")

	return nil
}

// rebalanceCallback handles partition assignment and revocation.
func (a *Adapter) rebalanceCallback(c *kfk.Consumer, event kfk.Event) error {
	switch ev := event.(type) {
	case kfk.AssignedPartitions:
		a.mu.Lock()
		a.assignedPartitions = ev.Partitions
		a.mu.Unlock()

		partitions := make([]string, 0, len(ev.Partitions))
		for _, tp := range ev.Partitions {
			partitions = append(partitions, fmt.Sprintf("%s[%d]", *tp.Topic, tp.Partition))
		}
		a.logger.Info().
			Strs("partitions", partitions).
			Msg("partitions assigned")

		if err := c.Assign(ev.Partitions); err != nil {
			a.logger.Error().Err(err).Msg("failed to assign partitions")
			return err
		}

	case kfk.RevokedPartitions:
		partitions := make([]string, 0, len(ev.Partitions))
		for _, tp := range ev.Partitions {
			partitions = append(partitions, fmt.Sprintf("%s[%d]", *tp.Topic, tp.Partition))
		}
		a.logger.Info().
			Strs("partitions", partitions).
			Msg("partitions revoked")

		// Commit current offsets before revocation
		committed, err := c.Commit()
		if err != nil {
			// It's okay if there's nothing to commit
			if kfkErr, ok := err.(kfk.Error); ok && kfkErr.Code() == kfk.ErrNoOffset {
				a.logger.Debug().Msg("no offsets to commit during revocation")
			} else {
				a.logger.Warn().Err(err).Msg("failed to commit offsets during revocation")
			}
		} else {
			a.logger.Debug().Int("committed", len(committed)).Msg("offsets committed during revocation")
		}

		if err := c.Unassign(); err != nil {
			a.logger.Error().Err(err).Msg("failed to unassign partitions")
			return err
		}

		a.mu.Lock()
		a.assignedPartitions = nil
		a.mu.Unlock()
	}

	return nil
}

// Poll retrieves messages from Kafka with timeout in milliseconds.
// Uses Poll() for proper event handling including rebalance events.
func (a *Adapter) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	if a.consumer == nil {
		return nil, fmt.Errorf("consumer not connected")
	}

	ev := a.consumer.Poll(timeoutMs)
	if ev == nil {
		// Timeout, no event available
		return nil, nil
	}

	switch e := ev.(type) {
	case *kfk.Message:
		// FR-042: Detect reconnection — receiving a message means broker is available
		a.mu.Lock()
		wasDisconnected := !a.brokerConnected
		a.brokerConnected = true
		a.mu.Unlock()
		if wasDisconnected {
			a.logger.Info().Msg("broker connection restored, resuming message consumption")
		}

		// Convert confluent message to our Message type
		headers := make(map[string]string)
		for _, h := range e.Headers {
			headers[h.Key] = string(h.Value)
		}

		return &types.Message{
			Topic:     *e.TopicPartition.Topic,
			Partition: e.TopicPartition.Partition,
			Offset:    int64(e.TopicPartition.Offset),
			Timestamp: e.Timestamp,
			Headers:   headers,
			Payload:   e.Value,
		}, nil

	case kfk.Error:
		// Handle Kafka errors
		if e.IsFatal() {
			return nil, fmt.Errorf("fatal kafka error: %w", e)
		}

		// FR-042: Reconnection-aware logging for broker transport errors.
		// confluent-kafka-go handles reconnection automatically via librdkafka;
		// we log state transitions so operators can observe disconnect/reconnect cycles.
		a.mu.Lock()
		wasConnected := a.brokerConnected
		a.mu.Unlock()

		switch e.Code() {
		case kfk.ErrTransport, kfk.ErrAllBrokersDown:
			if wasConnected {
				a.mu.Lock()
				a.brokerConnected = false
				a.mu.Unlock()
				a.logger.Warn().Err(e).Int("code", int(e.Code())).
					Msg("broker connection lost, librdkafka will reconnect automatically")
			} else {
				a.logger.Debug().Err(e).Int("code", int(e.Code())).
					Msg("broker still unavailable, reconnection in progress")
			}
		default:
			a.logger.Warn().Err(e).Int("code", int(e.Code())).Msg("non-fatal kafka error")
		}
		return nil, nil

	default:
		// Other events (rebalance, stats, etc.) handled via callbacks
		return nil, nil
	}
}

// CommitOffset commits an offset for a specific topic/partition.
// The committed offset is offset+1 (the next message to be consumed).
func (a *Adapter) CommitOffset(topic string, partition int32, offset int64) error {
	if a.consumer == nil {
		return fmt.Errorf("consumer not connected")
	}

	topicStr := topic
	tp := kfk.TopicPartition{
		Topic:     &topicStr,
		Partition: partition,
		Offset:    kfk.Offset(offset + 1), // Commit next offset to consume
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
