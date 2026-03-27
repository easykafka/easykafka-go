package kafka

import (
	"context"
	"fmt"
	"strings"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/rs/zerolog"

	"github.com/easykafka/easykafka-go/internal/types"
)

// Producer wraps confluent-kafka-go producer for writing to retry and DLQ topics.
type Producer struct {
	producer *kfk.Producer
	logger   zerolog.Logger
}

// NewProducer creates a new Kafka producer for the given brokers.
func NewProducer(brokers []string, logger zerolog.Logger) (*Producer, error) {
	if len(brokers) == 0 {
		return nil, fmt.Errorf("at least one broker is required")
	}

	config := &kfk.ConfigMap{
		"bootstrap.servers": strings.Join(brokers, ","),
		"acks":              "all",
	}

	p, err := kfk.NewProducer(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kafka producer: %w", err)
	}

	prod := &Producer{
		producer: p,
		logger:   logger,
	}

	// Start delivery report handler in background
	go prod.handleDeliveryReports()

	return prod, nil
}

// handleDeliveryReports processes delivery reports from the producer.
func (p *Producer) handleDeliveryReports() {
	for e := range p.producer.Events() {
		switch ev := e.(type) {
		case *kfk.Message:
			if ev.TopicPartition.Error != nil {
				p.logger.Error().
					Err(ev.TopicPartition.Error).
					Str("topic", *ev.TopicPartition.Topic).
					Msg("delivery failed")
			}
		}
	}
}

// Produce sends a message to a Kafka topic.
func (p *Producer) Produce(_ context.Context, msg *types.ProduceMessage) error {
	if p.producer == nil {
		return fmt.Errorf("producer is nil")
	}

	// Convert headers
	var headers []kfk.Header
	for k, v := range msg.Headers {
		headers = append(headers, kfk.Header{
			Key:   k,
			Value: []byte(v),
		})
	}

	topic := msg.Topic
	err := p.producer.Produce(&kfk.Message{
		TopicPartition: kfk.TopicPartition{
			Topic:     &topic,
			Partition: kfk.PartitionAny,
		},
		Key:     msg.Key,
		Value:   msg.Value,
		Headers: headers,
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to produce message to %s: %w", msg.Topic, err)
	}

	return nil
}

// Flush waits for all outstanding messages to be delivered.
func (p *Producer) Flush(timeoutMs int) int {
	if p.producer == nil {
		return 0
	}
	return p.producer.Flush(timeoutMs)
}

// Close gracefully shuts down the producer, flushing pending messages.
func (p *Producer) Close() {
	if p.producer == nil {
		return
	}
	p.producer.Flush(5000) // Wait up to 5s for pending messages
	p.producer.Close()
	p.producer = nil
}
