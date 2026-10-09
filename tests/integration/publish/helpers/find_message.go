package helpers

import (
	"testing"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
)

// FindMessage returns the message at partition and offset, failing the test
// if there is none.
func FindMessage(tb testing.TB, messages []*kfk.Message, partition int32, offset int64) *kfk.Message {
	tb.Helper()
	for _, message := range messages {
		if message.TopicPartition.Partition == partition && int64(message.TopicPartition.Offset) == offset {
			return message
		}
	}
	tb.Fatalf("no message at partition %d, offset %d among the %d read", partition, offset, len(messages))
	return nil
}
