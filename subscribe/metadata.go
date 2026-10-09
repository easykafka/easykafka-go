package subscribe

import (
	"context"
	"time"

	"github.com/easykafka/easykafka-go/internal/subscribe/metadata"
)

// MessageFromContext returns the Kafka message being handled, carrying its
// topic, partition, offset, timestamp and headers. The library attaches it to
// the context before dispatching, so a handler that needs more than the payload
// reads it from there:
//
//	func handle(ctx context.Context, payload []byte) *subscribe.Failure {
//	    if msg, ok := subscribe.MessageFromContext(ctx); ok {
//	        log.Printf("offset %d on partition %d", msg.Offset, msg.Partition)
//	    }
//	    return nil
//	}
//
// The message is the handler's own copy, so the topic, partition and offset the
// subscriber accounts against cannot be changed through it. The payload and
// headers are still shared with the subscriber, and a handler must treat both
// as read-only: writing into them changes what a retry or DLQ record carries.
//
// It reports false in batch mode, and has nothing to add there: one context is
// shared by the whole batch, so there is no single message it could describe,
// and a batch handler already reads each message — key, headers, topic,
// partition, offset — from [BatchItem.Message].
//
// There is no exported way to put a message onto a context: handlers read what
// the subscriber wrote, and cannot forge a value it will then dispatch.
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
//	func handle(ctx context.Context, payload []byte) *subscribe.Failure {
//	    msg, _ := subscribe.MessageFromContext(ctx)
//	    if err := subscribe.WaitUntilRetryTime(ctx, msg); err != nil {
//	        return &subscribe.Failure{Err: err}
//	    }
//	    // ... process payload ...
//	    return nil
//	}
//
// A batch handler calls it per item, as it reaches each one:
//
//	for _, item := range batch.Items() {
//	    msg := item.Message()
//	    if err := subscribe.WaitUntilRetryTime(ctx, &msg); err != nil {
//	        return &subscribe.Failure{Err: err}
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
//     default) gets the consumer removed from its group. The group then
//     rebalances, and the message may be processed twice: once by this handler
//     when its wait ends, and once by the consumer that takes over the
//     partition. It is not a loop, because the retry time is fixed and each
//     wait only covers what is left of it. The retry strategy's default
//     maximum delay is 30s. For longer delays, raise max.poll.interval.ms on
//     this consumer with WithKafkaConfig.
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
