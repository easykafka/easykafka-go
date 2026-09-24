package helpers

import kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"

// FailedReport builds a delivery report for a record the broker rejected with
// err: a keyed record on partition 7 of topic, carrying a retry attempt and an
// original-topic header.
func FailedReport(topic string, err error) *kfk.Message {
	return &kfk.Message{
		TopicPartition: kfk.TopicPartition{
			Topic:     &topic,
			Partition: 7,
			Error:     err,
		},
		Key:   []byte("slip-1"),
		Value: []byte(`{"slipId":"slip-1"}`),
		Headers: []kfk.Header{
			{Key: "easykafka.retry.attempt", Value: []byte("2")},
			{Key: "easykafka.original.topic", Value: []byte("orders")},
		},
	}
}
