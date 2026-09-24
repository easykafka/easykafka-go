package helpers

import (
	"context"
	"sync"

	"github.com/easykafka/easykafka-go/internal/types"
)

// MockProducer implements types.KafkaProducer for testing. It records every
// record produced, or fails every Produce with Err when set.
type MockProducer struct {
	Err error

	mu       sync.Mutex
	messages []ProducedMessage
}

// ProducedMessage is one record written through MockProducer.
type ProducedMessage struct {
	Topic   string
	Key     []byte
	Value   []byte
	Headers map[string]string
}

func (m *MockProducer) Produce(_ context.Context, msg *types.ProduceMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	m.messages = append(m.messages, ProducedMessage{
		Topic:   msg.Topic,
		Key:     msg.Key,
		Value:   msg.Value,
		Headers: msg.Headers,
	})
	return nil
}

func (m *MockProducer) Flush(timeoutMs int) int { return 0 }
func (m *MockProducer) Close()                  {}

// Messages returns a copy of the records produced so far.
func (m *MockProducer) Messages() []ProducedMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]ProducedMessage, len(m.messages))
	copy(result, m.messages)
	return result
}
