package types

import (
	"context"
	"time"
)

// Handler processes a single message payload with context for cancellation support.
// Return nil for successful processing (offset will be committed).
// Return error for failed processing (error strategy will be applied).
type Handler func(ctx context.Context, payload []byte) error

// BatchHandler processes multiple messages together as a batch with context support.
// Return nil to commit all message offsets in the batch.
// Return error to apply error strategy to the entire batch.
type BatchHandler func(ctx context.Context, payloads [][]byte) error

// Message is the public metadata representation available to handlers.
type Message struct {
	Topic     string
	Partition int32
	Offset    int64
	Timestamp time.Time
	Headers   map[string]string
	Payload   []byte
}

// ErrorStrategy defines how message processing failures are handled.
type ErrorStrategy interface {
	// HandleError is called when a handler returns an error.
	// msgs contains 1 message in single-message mode, N messages in batch mode.
	// In batch mode, all messages in the slice failed together atomically.
	// Returns nil to continue consumption, error to stop consumer.
	HandleError(ctx context.Context, msgs []*Message, handlerErr error) error

	// Name returns strategy name for logging/debugging.
	Name() string
}

// PayloadEncoding defines how payloads are encoded for retry and DLQ messages.
type PayloadEncoding string

const (
	PayloadEncodingJSON   PayloadEncoding = "json"
	PayloadEncodingBase64 PayloadEncoding = "base64"
)
