// Package batch holds the subscriber's batch buffer: the messages polled in batch
// mode, until a batch is full or its timeout expires.
package batch

import (
	"time"

	"github.com/easykafka/easykafka-go/internal/subscribe/types"
)

// Buffer accumulates messages until a batch is ready for dispatch.
// A batch is ready when the configured size limit is reached or the timeout expires.
type Buffer struct {
	messages  []*types.Message
	batchSize int
	timeout   time.Duration
	firstAdd  time.Time // time of first Add after last Flush (zero means empty)
}

// NewBuffer creates a buffer that flushes when batchSize messages
// accumulate or when timeout elapses since the first message was added.
func NewBuffer(batchSize int, timeout time.Duration) *Buffer {
	return &Buffer{
		messages:  make([]*types.Message, 0, batchSize),
		batchSize: batchSize,
		timeout:   timeout,
	}
}

// Add appends a message to the buffer. If this is the first message since
// the last flush, the timeout clock starts.
func (b *Buffer) Add(msg *types.Message) {
	if len(b.messages) == 0 {
		b.firstAdd = time.Now()
	}
	b.messages = append(b.messages, msg)
}

// Ready returns true when the buffer has accumulated batchSize messages.
func (b *Buffer) Ready() bool {
	return len(b.messages) >= b.batchSize
}

// TimedOut returns true when the buffer is non-empty and the timeout
// duration has elapsed since the first message was added.
func (b *Buffer) TimedOut() bool {
	if len(b.messages) == 0 {
		return false
	}
	return time.Since(b.firstAdd) >= b.timeout
}

// Len returns the number of messages currently in the buffer.
func (b *Buffer) Len() int {
	return len(b.messages)
}

// Drop discards all buffered messages without dispatching them and returns how
// many were discarded. Used when partitions are revoked: the messages belong to
// whichever consumer owns those partitions now, and it will process them.
// Nothing is lost — their offsets were never stored, so they are redelivered.
func (b *Buffer) Drop() int {
	dropped := len(b.messages)
	b.messages = make([]*types.Message, 0, b.batchSize)
	b.firstAdd = time.Time{}
	return dropped
}

// Flush returns all buffered messages and resets the buffer.
// Returns nil if the buffer is empty.
func (b *Buffer) Flush() []*types.Message {
	if len(b.messages) == 0 {
		return nil
	}
	msgs := b.messages
	b.messages = make([]*types.Message, 0, b.batchSize)
	b.firstAdd = time.Time{}
	return msgs
}
