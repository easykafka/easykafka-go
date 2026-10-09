package publish

import (
	"context"
	"fmt"

	"github.com/easykafka/easykafka-go/internal/publish/publishdriver"
)

// Writer publishes typed records to one topic. Safe for concurrent use.
//
// It is separate from Publisher because of generics. A Publisher owns one
// producer shared by every topic it writes, and those topics have different
// key and value types, so it cannot itself be generic: a Publisher[K, V]
// would serve only one pair of types. A Writer[K, V] fixes the types of one
// topic instead, so every call is checked by the compiler, encodes the key
// and value with that topic's encoders, and hands the bytes to its
// publisher's shared producer. It holds only a pointer to the publisher and
// the topic, so it is cheap to create with Bind.
type Writer[K, V any] struct {
	publisher *Publisher
	topic     Topic[K, V]
}

// Topic returns the name of the topic the writer publishes to.
func (w *Writer[K, V]) Topic() string {
	return w.topic.Name
}

// Publish writes one record and waits for its delivery report. It returns nil
// once the broker has acknowledged the record under the publisher's acks mode,
// and a *DeliveryError (errors.As) when it was refused.
//
// If ctx ends first, Publish returns ctx.Err() and the record is abandoned, not
// cancelled: it may still be delivered. A caller inside a consumer handler
// usually passes context.WithoutCancel(ctx), so that shutdown does not abandon
// a record about to be confirmed; the delivery timeout still bounds the wait.
func (w *Writer[K, V]) Publish(ctx context.Context, key K, value V, headers ...Header) error {
	delivery, err := w.Send(key, value, headers...)
	if err != nil {
		return err
	}
	_, err = delivery.Wait(ctx)
	return err
}

// Send enqueues one record and returns without waiting for the broker. The
// error covers only what fails locally: encoding, a full queue, a closed
// publisher. The outcome arrives on the returned Delivery, and a failure also
// reaches the WithDeliveryErrorFunc callback, whether or not anyone waits.
//
// The topic's headers come first, then headers, in order. Both are copied, so
// changing the caller's slice afterwards does not change the record.
func (w *Writer[K, V]) Send(key K, value V, headers ...Header) (*Delivery, error) {
	encodedKey, err := w.topic.EncodeKey(key)
	if err != nil {
		return nil, fmt.Errorf("%w: key for %s: %w", ErrEncode, w.topic.Name, err)
	}
	encodedValue, err := w.topic.EncodeValue(value)
	if err != nil {
		return nil, fmt.Errorf("%w: value for %s: %w", ErrEncode, w.topic.Name, err)
	}
	if encodedValue == nil {
		return nil, fmt.Errorf("%w: value for %s encoded to nil; use SendDelete for a tombstone",
			ErrEncode, w.topic.Name)
	}
	return w.publisher.enqueue(w.record(encodedKey, encodedValue, headers))
}

// Delete writes a tombstone, a record with a nil value, for key and waits for
// its delivery report, as Publish does.
func (w *Writer[K, V]) Delete(ctx context.Context, key K, headers ...Header) error {
	delivery, err := w.SendDelete(key, headers...)
	if err != nil {
		return err
	}
	_, err = delivery.Wait(ctx)
	return err
}

// SendDelete enqueues a tombstone for key without waiting, as Send does.
func (w *Writer[K, V]) SendDelete(key K, headers ...Header) (*Delivery, error) {
	encodedKey, err := w.topic.EncodeKey(key)
	if err != nil {
		return nil, fmt.Errorf("%w: key for %s: %w", ErrEncode, w.topic.Name, err)
	}
	return w.publisher.enqueue(w.record(encodedKey, nil, headers))
}

// record builds the record to produce. The headers are copied into a slice of
// the record's own.
func (w *Writer[K, V]) record(key, value []byte, headers []Header) publishdriver.Record {
	record := publishdriver.Record{Topic: w.topic.Name, Key: key, Value: value}
	if count := len(w.topic.Headers) + len(headers); count > 0 {
		record.Headers = make([]publishdriver.Header, 0, count)
		for _, header := range w.topic.Headers {
			record.Headers = append(record.Headers, publishdriver.Header(header))
		}
		for _, header := range headers {
			record.Headers = append(record.Headers, publishdriver.Header(header))
		}
	}
	return record
}
