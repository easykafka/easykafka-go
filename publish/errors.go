package publish

import (
	"errors"
	"fmt"

	"github.com/easykafka/easykafka-go/internal/publishdriver"
)

// Sentinels for the ways a record can fail. Test with errors.Is: a returned
// error wraps the matching sentinel, alongside the underlying cause.
//
// The first five are defined in the library's internal driver, which attaches
// them to the errors it reports, and re-exported here; their values are what
// errors.Is compares, so the reference is all a caller needs.
var (
	// ErrClosed is returned for a record sent after Close.
	ErrClosed = publishdriver.ErrClosed

	// ErrQueueFull is returned when librdkafka's local queue has no room. The
	// send fails at once rather than waiting.
	ErrQueueFull = publishdriver.ErrQueueFull

	// ErrDeliveryTimeout means the record was not acknowledged within the
	// delivery timeout. The broker may still have written it.
	ErrDeliveryTimeout = publishdriver.ErrDeliveryTimeout

	// ErrNotDelivered means the record was purged before it was confirmed: at
	// Close, or after a fatal error. A record purged while in flight may still
	// have been written.
	ErrNotDelivered = publishdriver.ErrNotDelivered

	// ErrFatal means the producer failed fatally and cannot write any more.
	ErrFatal = publishdriver.ErrFatal

	// ErrEncode means the key or value could not be encoded. Nothing was sent.
	ErrEncode = errors.New("publish: encoding failed")

	// ErrTopicNotFound is returned by Ping for a bound topic that does not exist.
	ErrTopicNotFound = errors.New("publish: topic not found")
)

// DeliveryError describes a record that was not acknowledged. It is both the
// error a wait returns and what a DeliveryErrorFunc receives.
//
// Topic, Key, Value and Headers are the record as it was sent. Partition, Code
// and Err come from its delivery report.
type DeliveryError struct {
	Topic string
	// Partition is -1 when no partition was ever chosen.
	Partition int32
	Key       []byte
	// Value is nil for a tombstone.
	Value   []byte
	Headers []Header
	// Code is librdkafka's name for the error, stable enough for a metric
	// label.
	Code string
	// Err is the cause. It wraps the matching sentinel where one applies.
	Err error
}

// Error describes the failed record and its cause.
func (e *DeliveryError) Error() string {
	partition := "no partition"
	if e.Partition >= 0 {
		partition = fmt.Sprintf("partition %d", e.Partition)
	}
	cause := e.Code
	if e.Err != nil {
		cause = e.Err.Error()
	}
	return fmt.Sprintf("publish: record for %s (%s) not delivered: %s", e.Topic, partition, cause)
}

// Unwrap returns the cause, so errors.Is and errors.As reach the sentinel and
// the underlying error.
func (e *DeliveryError) Unwrap() error {
	return e.Err
}

// DeliveryErrorFunc receives every record that was not acknowledged. It runs
// on the publisher's single report goroutine: it must not block, since every
// later report waits behind it, and it must be safe for concurrent use when
// shared across publishers. It must not call Close, which waits for that
// goroutine and would never return. A panic in it is recovered and logged.
type DeliveryErrorFunc func(DeliveryError)
