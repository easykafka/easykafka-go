package publishdriver

import (
	"errors"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
)

// Record is one record to produce.
type Record struct {
	Topic string
	Key   []byte
	// Value nil is a tombstone.
	Value   []byte
	Headers []Header
}

// Header is one record header.
type Header struct {
	Key   string
	Value []byte
}

// Event is what Reports yields. It is sealed: Report and ClientError are the
// only cases, so a switch over them is complete.
//
// The unexported method is what seals it: only types in this package can
// implement Event, so the set of cases is fixed here, and each case carries
// only its own fields, rather than one struct holding every field of both. Go
// does not check that a switch covers every case, so adding one means
// updating the switch in the publisher's report loop.
type Event interface {
	sealedEvent()
}

// Report is the delivery report of one produced record.
type Report struct {
	// Token is the value passed to Produce for this record. It travels as
	// confluent's kfk.Message.Opaque: kept client-side, never sent to Kafka,
	// and handed back on the record's delivery report. It is how a report is
	// matched to its record without a per-record delivery channel.
	Token any
	// Partition is -1 when no partition was ever chosen.
	Partition int32
	Offset    int64
	// Err is nil on success.
	Err *KafkaError
}

func (Report) sealedEvent() {}

// ClientError is an error about the client rather than one record: a lost
// connection, a fatal error, and so on.
type ClientError struct {
	Err *KafkaError
}

func (ClientError) sealedEvent() {}

// KafkaError is a librdkafka error, translated so that confluent-kafka-go
// types never leave this package. It unwraps to its Sentinel, so errors.Is on
// any error built from it reaches the publish sentinel it means.
type KafkaError struct {
	// Code is librdkafka's name for the error (kfk.ErrorCode.String()).
	Code    string
	Message string
	// Sentinel is the publish sentinel this error means: ErrDeliveryTimeout,
	// ErrNotDelivered, ErrFatal or ErrQueueFull. Nil when none applies, as
	// for a broker rejecting the record.
	Sentinel error
	// Fatal: the producer has failed and cannot write any more. Read by the
	// publisher's handling of client errors.
	Fatal bool
	// Disconnected: the brokers, or one of them, could not be reached. Read
	// by the publisher's connection logging.
	Disconnected bool
}

// Error returns the sentinel's text, if any, then librdkafka's message, or the
// code when there is no message.
func (e *KafkaError) Error() string {
	message := e.Message
	if message == "" {
		message = e.Code
	}
	if e.Sentinel == nil {
		return message
	}
	return e.Sentinel.Error() + ": " + message
}

// Unwrap returns the sentinel, so errors.Is reaches it.
func (e *KafkaError) Unwrap() error {
	return e.Sentinel
}

// TranslateError turns an error from confluent-kafka-go into a KafkaError. An
// error from elsewhere keeps its message and has no code.
//
// Exported so it can be tested without a broker; internal/ keeps it out of the
// library's public API.
func TranslateError(err error) *KafkaError {
	kafkaError, isKafkaError := errors.AsType[kfk.Error](err)
	if !isKafkaError {
		return &KafkaError{Message: err.Error()}
	}
	code := kafkaError.Code()
	translated := &KafkaError{
		Code:    code.String(),
		Message: kafkaError.Error(),
		// Fatal errors arrive in two forms. The fatal error event carries the
		// underlying cause's code with only the fatal flag set, so IsFatal is
		// needed. A Produce after the producer has failed returns ErrFatal
		// without the flag, so the code is needed.
		Fatal:        kafkaError.IsFatal() || code == kfk.ErrFatal,
		Disconnected: code == kfk.ErrAllBrokersDown || code == kfk.ErrTransport,
	}
	switch {
	case code == kfk.ErrMsgTimedOut:
		translated.Sentinel = ErrDeliveryTimeout
	case code == kfk.ErrPurgeQueue || code == kfk.ErrPurgeInflight:
		translated.Sentinel = ErrNotDelivered
	case code == kfk.ErrQueueFull:
		translated.Sentinel = ErrQueueFull
	case translated.Fatal:
		translated.Sentinel = ErrFatal
	}
	return translated
}

// TranslateEvent turns one confluent-kafka-go event into an Event: a
// *kfk.Message into a Report, a kfk.Error into a ClientError. Any other event,
// statistics for example, is dropped, and ok is false.
//
// Exported for the same reason as TranslateError.
func TranslateEvent(event kfk.Event) (translated Event, ok bool) {
	switch event := event.(type) {
	case *kfk.Message:
		report := Report{
			Token:     event.Opaque,
			Partition: event.TopicPartition.Partition,
			Offset:    int64(event.TopicPartition.Offset),
		}
		if event.TopicPartition.Error != nil {
			report.Err = TranslateError(event.TopicPartition.Error)
		}
		return report, true
	case kfk.Error:
		return ClientError{Err: TranslateError(event)}, true
	default:
		return nil, false
	}
}
