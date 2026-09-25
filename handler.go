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

// Failure is how a handler reports that a message failed: the error, and
// optionally a resume point and a domain error code. Re-export from
// internal/types.
type Failure = types.Failure

// Batch is what a BatchHandler is given: the polled messages, each paired with
// the verdict the handler records for it. Re-export from internal/types.
type Batch = types.Batch

// BatchItem is one message of a Batch and its verdict. Re-export from
// internal/types.
type BatchItem = types.BatchItem

// NewBatch builds a Batch over msgs, so a batch handler can be tested without a
// broker: run the handler over it, then check which items came back failed.
func NewBatch(msgs []Message) *Batch {
	return types.NewBatch(msgs)
}

var (
	// ErrPermanent marks a failure that reprocessing cannot fix. The retry
	// strategy sends it straight to the DLQ instead of walking the retry
	// ladder. Wrap it with %w, not %v, or the marker is lost:
	//
	//	item.Fail(easykafka.Failure{Err: fmt.Errorf("%w: unmarshal: %v", easykafka.ErrPermanent, err)})
	ErrPermanent = types.ErrPermanent

	// ErrUnspecified stands in for a failure a handler recorded without an
	// error. The message is still routed, under this error.
	ErrUnspecified = types.ErrUnspecified
)

// DeliveryError describes a retry or DLQ write that never reached the broker.
// Re-export from internal/types.
type DeliveryError = types.DeliveryError

// DeliveryErrorFunc is called for writes that fail to reach the broker.
// Re-export from internal/types.
type DeliveryErrorFunc = types.DeliveryErrorFunc

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
	WithRetryTopic        = strategy.WithRetryTopic
	WithDLQTopic          = strategy.WithDLQTopic
	WithMaxAttempts       = strategy.WithMaxAttempts
	WithInitialDelay      = strategy.WithInitialDelay
	WithMaxDelay          = strategy.WithMaxDelay
	WithBackoffMultiplier = strategy.WithBackoffMultiplier
	WithCustomBackoff     = strategy.WithCustomBackoff
	WithDeliveryErrorFunc = strategy.WithDeliveryErrorFunc
)

// ============================================================================
// MESSAGE METADATA
// ============================================================================

// MessageFromContext returns the Kafka message being handled, carrying its
// topic, partition, offset, timestamp and headers. The library attaches it to
// the context before dispatching, so a handler that needs more than the payload
// reads it from there:
//
//	func handle(ctx context.Context, payload []byte) *easykafka.Failure {
//	    if msg, ok := easykafka.MessageFromContext(ctx); ok {
//	        log.Printf("offset %d on partition %d", msg.Offset, msg.Partition)
//	    }
//	    return nil
//	}
//
// It reports false in batch mode, and has nothing to add there: one context is
// shared by the whole batch, so there is no single message it could describe,
// and a batch handler already reads each message — key, headers, topic,
// partition, offset — from [BatchItem.Message].
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

// WaitUntilRetryTime blocks until the message is due for reprocessing — the
// time [GetRetryTime] returns. It returns nil at once if the message carries no
// retry time, which is the case on first delivery, or if that time has already
// passed; so one handler can call it unconditionally and serve both the source
// topic and the retry topic. A nil message returns nil.
//
// It returns ctx.Err() if ctx is cancelled first, which is how a handler
// waiting on a record is released at shutdown:
//
//	func handle(ctx context.Context, payload []byte) *easykafka.Failure {
//	    msg, _ := easykafka.MessageFromContext(ctx)
//	    if err := easykafka.WaitUntilRetryTime(ctx, msg); err != nil {
//	        return &easykafka.Failure{Err: err}
//	    }
//	    // ... process payload ...
//	    return nil
//	}
//
// A batch handler calls it per item, as it reaches each one:
//
//	for _, item := range batch.Items() {
//	    msg := item.Message()
//	    if err := easykafka.WaitUntilRetryTime(ctx, &msg); err != nil {
//	        return &easykafka.Failure{Err: err}
//	    }
//	    // ... process item ...
//	}
//
// Records reach a retry topic roughly in retry-time order, so after the first
// wait the rest of a batch is usually due already.
//
// Three things to know:
//
//   - The retry time is stored to the second, so the wait can end up to a
//     second before the time the library computed.
//   - A message released by cancellation has not been processed. Returning the
//     error as a failure sends it to the error strategy like any other failure:
//     under retry it is republished and the attempt counts; under skip it is
//     written off.
//   - The handler runs on the goroutine that polls, so nothing is fetched while
//     it waits. A wait longer than librdkafka's max.poll.interval.ms (300s by
//     default) gets the consumer removed from its group. The retry strategy's
//     default maximum delay is 30s.
func WaitUntilRetryTime(ctx context.Context, msg *Message) error {
	return metadata.WaitUntilRetryTime(ctx, msg)
}

// GetRetryStep returns the resume point a previous attempt reported through
// [Failure.Step]: which step of a multi-step process failed, numbered from 1.
// The library carries it but never interprets it — skipping the steps that
// already succeeded is the handler's job. Returns 0 if absent.
func GetRetryStep(msg *Message) int32 {
	return metadata.GetRetryStep(msg)
}

// GetErrorCode returns the error code of the failure that republished this
// message: [Failure.Code] if the handler set one, HANDLER_ERROR otherwise.
// Returns the empty string if absent, which is the case on first delivery.
func GetErrorCode(msg *Message) string {
	return metadata.GetErrorCode(msg)
}

// GetOriginalTopic returns the topic the message was first consumed from,
// before any republishing. Returns the empty string if absent, which is the
// case for a message that has never been retried.
func GetOriginalTopic(msg *Message) string {
	return metadata.GetOriginalTopic(msg)
}
