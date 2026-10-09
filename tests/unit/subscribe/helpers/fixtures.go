// Package helpers holds the fakes and fixtures the subscriber's unit tests use:
// fake consumers, fake error strategies, retry-strategy builders and message
// fixtures. It lives outside the test package so each fake can have a file of
// its own, which is why everything a test sets or reads is exported.
package helpers

import (
	"time"

	"github.com/easykafka/easykafka-go/internal/subscribe/types"
)

// NewTestMessage builds a message with the given position and payload, an
// empty header map and the current time as its timestamp.
func NewTestMessage(topic string, partition int32, offset int64, payload string) *types.Message {
	return &types.Message{
		Topic:     topic,
		Partition: partition,
		Offset:    offset,
		Timestamp: time.Now(),
		Headers:   make(map[string]string),
		Payload:   []byte(payload),
	}
}
