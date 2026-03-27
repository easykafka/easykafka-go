package engine

import (
	"time"

	"github.com/easykafka/easykafka-go/internal/types"
)

// BatchBuffer accumulates messages until a batch is ready for dispatch.
// A batch is ready when the configured size limit is reached or the timeout expires.
type BatchBuffer struct {
	messages  []*types.Message
	batchSize int
	timeout   time.Duration
	firstAdd  time.Time // time of first Add after last Flush (zero means empty)
}

// NewBatchBuffer creates a buffer that flushes when batchSize messages
// accumulate or when timeout elapses since the first message was added.
func NewBatchBuffer(batchSize int, timeout time.Duration) *BatchBuffer {
	return &BatchBuffer{
		messages:  make([]*types.Message, 0, batchSize),
		batchSize: batchSize,
		timeout:   timeout,
	}
}

// Add appends a message to the buffer. If this is the first message since
// the last flush, the timeout clock starts.
func (b *BatchBuffer) Add(msg *types.Message) {
	if len(b.messages) == 0 {
		b.firstAdd = time.Now()
	}
	b.messages = append(b.messages, msg)
}

// Ready returns true when the buffer has accumulated batchSize messages.
func (b *BatchBuffer) Ready() bool {
	return len(b.messages) >= b.batchSize
}

// TimedOut returns true when the buffer is non-empty and the timeout
// duration has elapsed since the first message was added.
func (b *BatchBuffer) TimedOut() bool {
	if len(b.messages) == 0 {
		return false
	}
	return time.Since(b.firstAdd) >= b.timeout
}

// Len returns the number of messages currently in the buffer.
func (b *BatchBuffer) Len() int {
	return len(b.messages)
}

// Flush returns all buffered messages and resets the buffer.
// Returns nil if the buffer is empty.
func (b *BatchBuffer) Flush() []*types.Message {
	if len(b.messages) == 0 {
		return nil
	}
	msgs := b.messages
	b.messages = make([]*types.Message, 0, b.batchSize)
	b.firstAdd = time.Time{}
	return msgs
}
