package easykafka

import (
	"context"
	"time"

	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/easykafka/easykafka-go/strategy"
	"github.com/rs/zerolog"
)

// Handler is a re-export from internal/types
type Handler = types.Handler

// BatchHandler is a re-export from internal/types
type BatchHandler = types.BatchHandler

// ErrorStrategy is a re-export from internal/types
type ErrorStrategy = types.ErrorStrategy

// Message is a re-export from internal/types
type Message = types.Message

// DeliveryError describes a retry or DLQ write that never reached the broker.
// Re-export from internal/types.
type DeliveryError = types.DeliveryError

// DeliveryErrorFunc is called for writes that fail to reach the broker.
// Re-export from internal/types.
type DeliveryErrorFunc = types.DeliveryErrorFunc

// PayloadEncoding is a re-export from internal/types
type PayloadEncoding = types.PayloadEncoding

const (
	PayloadEncodingJSON   = types.PayloadEncodingJSON
	PayloadEncodingBase64 = types.PayloadEncodingBase64
)

// Re-export retry option types for public API
type RetryOption = strategy.RetryOption

// ============================================================================
// PUBLIC ERROR STRATEGY CONSTRUCTORS
// ============================================================================

// NewRetryStrategy retries failed messages with Kafka-based retry topics and a DLQ.
// RetryTopic and DLQTopic are required options.
func NewRetryStrategy(options ...RetryOption) (ErrorStrategy, error) {
	return strategy.NewRetryStrategy(options...)
}

// NewSkipStrategy logs errors and continues consumption, committing offsets.
func NewSkipStrategy(logger zerolog.Logger) ErrorStrategy {
	return strategy.NewSkipStrategy(logger)
}

// NewFailFastStrategy stops the consumer immediately on any handler error.
func NewFailFastStrategy() ErrorStrategy {
	return strategy.NewFailFastStrategy()
}

// Re-export retry option constructors
var (
	WithRetryTopic                   = strategy.WithRetryTopic
	WithDLQTopic                     = strategy.WithDLQTopic
	WithMaxAttempts                  = strategy.WithMaxAttempts
	WithInitialDelay                 = strategy.WithInitialDelay
	WithMaxDelay                     = strategy.WithMaxDelay
	WithBackoffMultiplier            = strategy.WithBackoffMultiplier
	WithCustomBackoff                = strategy.WithCustomBackoff
	WithFailedMessagePayloadEncoding = strategy.WithFailedMessagePayloadEncoding
	WithDeliveryErrorFunc            = strategy.WithDeliveryErrorFunc
)

// ============================================================================
// MESSAGE METADATA
// ============================================================================

// MessageFromContext returns the Kafka message being handled, carrying its
// topic, partition, offset, timestamp and headers. The library attaches it to
// the context before dispatching, so a handler that needs more than the payload
// reads it from there:
//
//	func handle(ctx context.Context, payload []byte) error {
//	    if msg, ok := easykafka.MessageFromContext(ctx); ok {
//	        log.Printf("offset %d on partition %d", msg.Offset, msg.Partition)
//	    }
//	    return nil
//	}
//
// It reports false in batch mode. A batch handler is given many messages at
// once and its context carries none of them — there is no single message it
// could describe. Per-message metadata for batches would mean changing
// [BatchHandler]'s signature away from [][]byte, which is separate work.
//
// There is no exported way to put a message onto a context: handlers read what
// the engine wrote, and cannot forge a value the engine will then dispatch.
func MessageFromContext(ctx context.Context) (*Message, bool) {
	return metadata.MessageFromContext(ctx)
}

// Headers the library sets on records it republishes to the retry and DLQ
// topics. They are part of the wire format — a retry topic outlives the
// deployment that wrote to it — so a second consumer reading that topic can
// rely on them.
//
// The values are spelled out rather than aliased to the internal constants so
// that they are readable in the generated documentation; TestHeaderKeysMatchInternal
// fails if the two ever drift.
const (
	HeaderRetryAttempt      = "easykafka.retry.attempt"
	HeaderRetryTime         = "easykafka.retry.time"
	HeaderRetryStep         = "easykafka.retry.step"
	HeaderErrorCode         = "easykafka.error.code"
	HeaderErrorMessage      = "easykafka.error.message"
	HeaderOriginalTopic     = "easykafka.original.topic"
	HeaderOriginalPartition = "easykafka.original.partition"
	HeaderOriginalOffset    = "easykafka.original.offset"
	HeaderFailedAt          = "easykafka.failed.at"
)

// GetRetryAttempt returns how many times this message has already been retried.
// It returns 0 for a message on first delivery, which is the value to branch on
// to tell an original from a republished record.
func GetRetryAttempt(msg *Message) int {
	return metadata.GetRetryAttempt(msg)
}

// GetRetryTime returns the time this message became due for reprocessing. The
// library sets it when republishing but never waits on it — honouring it is the
// job of whoever consumes the retry topic. Returns the zero Time if absent or
// unparseable.
func GetRetryTime(msg *Message) time.Time {
	return metadata.GetRetryTime(msg)
}

// GetRetryStep returns the backoff step used to schedule this retry. Returns 0
// if absent.
func GetRetryStep(msg *Message) int32 {
	return metadata.GetRetryStep(msg)
}

// GetOriginalTopic returns the topic the message was first consumed from,
// before any republishing. Returns the empty string if absent, which is the
// case for a message that has never been retried.
func GetOriginalTopic(msg *Message) string {
	return metadata.GetOriginalTopic(msg)
}
