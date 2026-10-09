package types

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog"
)

// Handler processes a single message payload with context for cancellation support.
// Return nil for successful processing (offset will be committed).
// Return a *Failure for failed processing (error strategy will be applied).
//
// A handler that needs more than the payload — the key, headers, or the step
// a retried record resumes at — reads the message off the context with
// MessageFromContext.
type Handler func(ctx context.Context, payload []byte) *Failure

// BatchHandler processes a batch. Record each failed message with item.Fail and
// return nil; return a *Failure only when the batch as a whole could not be
// processed.
//
// A returned *Failure routes every message in the batch under it and discards
// any verdicts already recorded with item.Fail.
type BatchHandler func(ctx context.Context, batch *Batch) *Failure

// Failure is how a handler reports that a message failed, and what the error
// strategy is told about it.
//
// Failure deliberately does not implement error: a nil *Failure inside an
// error interface would not compare equal to nil and would read as a failure.
type Failure struct {
	// Err is why the message failed. Required; wrap ErrPermanent with %w to
	// send the message straight to the DLQ. If it is left nil the
	// subscriber substitutes ErrUnspecified and routes the message anyway.
	Err error

	// Step is the resume point: which step of a multi-step process failed,
	// numbered from 1. Written to the easykafka.retry.step header and read back
	// with GetRetryStep. Left at 0, the step the record arrived with carries
	// forward.
	Step int32

	// Code is a short name for what went wrong in the application's terms,
	// such as "not_found". Written to the easykafka.error.code header and read
	// back with GetErrorCode. Left empty, the library writes HANDLER_ERROR.
	Code string
}

// ErrPermanent marks a failure that reprocessing cannot fix. A strategy that
// would otherwise retry sends the message to the DLQ instead.
//
// It must be wrapped with %w — fmt.Errorf("%w: unmarshal: %v", ErrPermanent,
// err) — or the marker is lost and the message walks the retry ladder.
var ErrPermanent = errors.New("permanent failure")

// ErrUnspecified stands in for a failure a handler recorded without an error.
var ErrUnspecified = errors.New("handler failed the message without an error")

// Batch is the unit a BatchHandler is given: the messages the subscriber
// polled, each paired with the verdict the handler records for it.
//
// Items are in poll order. Partitions are interleaved in whatever order the
// broker delivered them; within one partition, offsets ascend.
type Batch struct {
	items []*BatchItem
}

// BatchItem is one message and its verdict. The message is read-only; the
// verdict is what the handler is there to supply.
type BatchItem struct {
	msg     Message
	failure *Failure // nil until the handler fails this item
}

// NewBatch builds a Batch over msgs, so a handler can be tested without a
// broker. The subscriber builds its own with NewBatchFromBuffer.
func NewBatch(msgs []Message) *Batch {
	// The obvious version gives every item a heap allocation of its own:
	//
	//     items := make([]*BatchItem, len(msgs))  // 1 allocation
	//     for i := range msgs {
	//         items[i] = &BatchItem{msg: msgs[i]} // + 1 allocation per item: N+1 in all
	//     }
	//
	// Here every item lives in one backing array instead, and items holds
	// pointers into it — two allocations for the whole batch, whatever its
	// size. Each message is still copied into its item either way: this saves
	// allocations, not copying. The pointers keep the array alive, and it is
	// never grown after they are taken, so none of them can go stale.
	backing := make([]BatchItem, len(msgs)) // 1 allocation: all N items
	items := make([]*BatchItem, len(msgs))  // 1 allocation: N pointers
	for i := range msgs {
		backing[i].msg = msgs[i] // a copy into memory that already exists
		items[i] = &backing[i]   // a pointer into backing, not a new object
	}
	return &Batch{items: items}
}

// NewBatchFromBuffer is the subscriber's constructor. The batch buffer already
// holds *Message, so this copies each message once, straight into its item,
// instead of first into a []Message for NewBatch to copy a second time. It is
// exported within internal/ so the subscriber can reach it; handler.go does not
// re-export it.
func NewBatchFromBuffer(msgs []*Message) *Batch {
	backing := make([]BatchItem, len(msgs))
	items := make([]*BatchItem, len(msgs))
	for i, m := range msgs {
		backing[i].msg = *m
		items[i] = &backing[i]
	}
	return &Batch{items: items}
}

// Len returns the number of messages in the batch.
func (b *Batch) Len() int { return len(b.items) }

// Items returns the batch's messages in poll order, as pointers so that
// ranging over them and failing one takes effect.
func (b *Batch) Items() []*BatchItem { return b.items }

// Message returns the Kafka message this item carries.
//
// It is a copy, so the topic, partition and offset the subscriber accounts
// against cannot be changed through it. The payload and headers are still shared with
// the subscriber, and a handler must treat both as read-only: writing into them
// changes what a retry or DLQ record carries.
func (item *BatchItem) Message() Message { return item.msg }

// Fail records that this message failed. Calling it twice keeps the last one.
//
// Items are independent, so a handler that fans out over its batch may fail
// different items concurrently. Failing the same item from two goroutines is
// not safe, and neither is returning before those goroutines have joined.
func (item *BatchItem) Fail(f Failure) { item.failure = &f }

// Failed returns the recorded failure, or nil if this message did not fail.
func (item *BatchItem) Failed() *Failure { return item.failure }

// Message is the public metadata representation available to handlers.
type Message struct {
	Topic     string
	Partition int32
	Offset    int64

	// Timestamp is the record's Kafka timestamp. On a retry or DLQ record it is
	// the time easykafka wrote that record, not the source record's.
	Timestamp time.Time

	Key []byte

	// Headers maps each header key to its value. A key sent more than once
	// keeps its last value, and a null value reads as "".
	Headers map[string]string

	Payload []byte
}

// ErrorStrategy defines how message processing failures are handled.
type ErrorStrategy interface {
	// HandleError is called when a handler reports a failure.
	//
	// msgs holds one message for a failure reported by a single-message handler
	// or recorded against one batch item. It holds every message of a batch
	// when the batch handler failed the batch as a whole, or panicked; all of
	// them then share f.
	//
	// f.Err is never nil: the subscriber substitutes ErrUnspecified first.
	//
	// Returns nil to continue consumption, error to stop consumer.
	HandleError(ctx context.Context, msgs []*Message, f Failure) error

	// Name returns strategy name for logging/debugging.
	Name() string
}

// Initializable is implemented by error strategies that need access to
// consumer configuration (e.g., broker addresses) before they can operate.
// Subscriber.Start calls Initialize if the strategy implements this interface.
type Initializable interface {
	Initialize(config InitConfig) error
	Close() error
}

// InitConfig provides consumer configuration to strategies during initialization.
type InitConfig struct {
	Brokers       []string
	ConsumerGroup string
	Handler       Handler
	Logger        zerolog.Logger

	// KafkaConfig is the consumer's WithKafkaConfig map, for strategies that
	// create Kafka clients of their own. Without it those clients would miss
	// the consumer's security, SASL and TLS settings. Read it; never modify it.
	KafkaConfig map[string]any
}

// LoggerAware can be implemented by error strategies that accept a logger
// after construction. The subscriber wires its configured logger into
// strategies implementing this interface before it starts polling.
type LoggerAware interface {
	SetLogger(zerolog.Logger)
}
