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
	// send the message straight to the DLQ. If it is left nil the engine
	// substitutes ErrUnspecified and routes the message anyway.
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

// Batch is the unit a BatchHandler is given: the messages the engine polled,
// each paired with the verdict the handler records for it.
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
// broker. The engine builds its own with NewBatchFromBuffer.
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

// NewBatchFromBuffer is the engine's constructor. The batch buffer already holds
// *Message, so this copies each message once, straight into its item, instead of
// first into a []Message for NewBatch to copy a second time. It is exported
// within internal/ so the engine can reach it; handler.go does not re-export it.
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
// It is a copy, so the topic, partition and offset the engine accounts against
// cannot be changed through it. The payload and headers are still shared with
// the engine, and a handler must treat both as read-only: writing into them
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
	Timestamp time.Time
	Key       []byte
	Headers   map[string]string
	Payload   []byte
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
	// f.Err is never nil: the engine substitutes ErrUnspecified first.
	//
	// Returns nil to continue consumption, error to stop consumer.
	HandleError(ctx context.Context, msgs []*Message, f Failure) error

	// Name returns strategy name for logging/debugging.
	Name() string
}

// Initializable is implemented by error strategies that need access to
// consumer configuration (e.g., broker addresses) before they can operate.
// Consumer.Start() calls Initialize() if the strategy implements this interface.
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
}

// LoggerAware can be implemented by error strategies that accept a logger
// after construction. The consumer wires the configured logger into strategies
// implementing this interface before starting the engine.
type LoggerAware interface {
	SetLogger(zerolog.Logger)
}

// ErrPartitionRevoked means an offset could not be stored because the partition
// is no longer assigned to this consumer. This is expected during a rebalance:
// the message will be redelivered to whichever consumer owns the partition now,
// so callers should tolerate it rather than treat it as a failure.
//
// It lives here rather than beside the adapter because it is part of the
// KafkaClient.StoreOffset contract, and an alternative implementation needs to
// be able to return it.
var ErrPartitionRevoked = errors.New("partition no longer assigned")

// KafkaClient abstracts the Kafka consumer adapter, so the engine can be driven
// by a fake in tests.
type KafkaClient interface {
	Connect(ctx context.Context) error
	SubscribeToTopic(ctx context.Context) error
	Poll(ctx context.Context, timeoutMs int) (*Message, error)

	// StoreOffset records that a message has been accounted for; CommitStored
	// publishes everything stored so far. Splitting the two is what stops a
	// rebalance from committing messages that were polled but never processed.
	//
	// StoreOffset returns ErrPartitionRevoked if the partition is no longer
	// assigned, which callers must tolerate rather than treat as a failure.
	StoreOffset(topic string, partition int32, offset int64) error
	CommitStored() error

	// MaybeCommitStored is CommitStored unless the client is committing on an
	// interval of its own, in which case it does nothing and the background
	// committer owns timing. Use it where a commit is progress-keeping and
	// skippable; use CommitStored where it must happen.
	MaybeCommitStored() error

	// SetOnRevoke registers a function invoked when partitions are revoked.
	// It is called synchronously from whichever goroutine calls Poll.
	SetOnRevoke(fn func())

	Close(ctx context.Context) error
}

// KafkaProducer abstracts producing messages to Kafka topics.
type KafkaProducer interface {
	Produce(ctx context.Context, msg *ProduceMessage) error
	Flush(timeoutMs int) int
	Close()
}

// ProduceMessage represents a message to be produced to Kafka.
type ProduceMessage struct {
	Topic   string
	Key     []byte
	Value   []byte
	Headers map[string]string
}

// DeliveryError describes a retry or DLQ write that never reached the broker.
//
// It reports a loss, it does not prevent one: the source offset has already
// advanced by the time a DeliveryError exists, because the library treats a
// message as accounted for once it is queued with the client rather than once
// the broker acknowledges it.
//
// The fields carry no confluent-kafka-go types, so an application can consume
// this without importing the Kafka client.
type DeliveryError struct {
	// Topic is the retry or DLQ topic the write was aimed at. There is no
	// separate field naming the producer, because the two use different topics.
	Topic string

	// Partition may be unassigned if the write never got that far.
	Partition int32

	Key []byte

	// Value is the record body. For a failed DLQ write these bytes are the last
	// copy that exists, which is why they are here — but they can be large, so
	// logging them wholesale is usually the wrong move.
	Value []byte

	// Headers carries the retry attempt count and the original topic.
	Headers map[string]string

	// Err is the underlying client error.
	Err error

	// Code names the kind of failure — "Broker: Message size too large",
	// "Local: Broker transport failure" — and is empty when Err did not come
	// from the Kafka client. It is offered as a metric label, because a broker
	// that is unreachable and a record that is too large want different alerts.
	//
	// It is a description, not advice. By the time a delivery report exists the
	// client has already retried to exhaustion against its own message timeout,
	// so even a transport failure here has been retried as far as it will be.
	Code string
}

// DeliveryErrorFunc is called for every retry or DLQ write that fails to reach
// the broker. It is supplied by the caller and invoked by the library, in the
// manner of BackoffFunc.
//
// It runs on the producer's event goroutine, once per failed record. Retry and
// DLQ have separate producers and therefore separate goroutines, so an
// implementation must be safe for concurrent use, and must not block: while it
// runs, no further event is drained for that producer — including the delivery
// failures it exists to report.
//
// A panic is recovered and logged rather than allowed to kill the goroutine.
type DeliveryErrorFunc func(DeliveryError)
