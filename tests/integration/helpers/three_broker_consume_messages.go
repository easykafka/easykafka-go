package helpers

import (
	"fmt"
	"strings"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
)

// ConsumeMessages reads a topic from the beginning until count messages have
// arrived or timeout has passed, and returns what it read, in the order each
// partition holds it.
func (c *ThreeBrokerCluster) ConsumeMessages(tb testing.TB, topic string, count int, timeout time.Duration) []*kfk.Message {
	tb.Helper()

	consumer, err := kfk.NewConsumer(&kfk.ConfigMap{
		"bootstrap.servers":  strings.Join(c.Brokers, ","),
		"group.id":           fmt.Sprintf("read-back-%d", time.Now().UnixNano()),
		"auto.offset.reset":  "earliest",
		"enable.auto.commit": false,
	})
	if err != nil {
		tb.Fatalf("creating consumer for topic %s: %v", topic, err)
	}
	defer func() { _ = consumer.Close() }()

	if err := consumer.Subscribe(topic, nil); err != nil {
		tb.Fatalf("subscribing to topic %s: %v", topic, err)
	}

	var messages []*kfk.Message
	deadline := time.Now().Add(timeout)
	for len(messages) < count && time.Now().Before(deadline) {
		message, err := consumer.ReadMessage(200 * time.Millisecond)
		if err != nil {
			continue // a timeout, or an error librdkafka recovers from by itself
		}
		messages = append(messages, message)
	}
	tb.Logf("read %d of %d messages from %s", len(messages), count, topic)
	return messages
}
