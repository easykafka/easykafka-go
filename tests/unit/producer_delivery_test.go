package unit

import (
	"errors"
	"sync"
	"testing"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/easykafka/easykafka-go/internal/kafka"
	"github.com/easykafka/easykafka-go/internal/types"
)

// =============================================================================
// Delivery-error callback
//
// The callback is the only thing that makes a failed retry or DLQ write visible
// to the application: Produce returns as soon as the record is queued locally,
// so nothing downstream learns the broker never took it. These tests cover the
// mapping and the invocation contract; neither needs a broker.
// =============================================================================

func failedReport(topic string, err error) *kfk.Message {
	return &kfk.Message{
		TopicPartition: kfk.TopicPartition{
			Topic:     &topic,
			Partition: 7,
			Error:     err,
		},
		Key:   []byte("slip-1"),
		Value: []byte(`{"slipId":"slip-1"}`),
		Headers: []kfk.Header{
			{Key: "easykafka.retry.attempt", Value: []byte("2")},
			{Key: "easykafka.original.topic", Value: []byte("orders")},
		},
	}
}

func TestDeliveryErrorForMapsEveryField(t *testing.T) {
	reportErr := kfk.NewError(kfk.ErrMsgSizeTooLarge, "message too large", false)

	de := kafka.DeliveryErrorFor(failedReport("orders-retry", reportErr))

	require.NotNil(t, de)
	assert.Equal(t, "orders-retry", de.Topic)
	assert.Equal(t, int32(7), de.Partition)
	assert.Equal(t, []byte("slip-1"), de.Key)
	assert.JSONEq(t, `{"slipId":"slip-1"}`, string(de.Value))
	assert.Equal(t, map[string]string{
		"easykafka.retry.attempt":  "2",
		"easykafka.original.topic": "orders",
	}, de.Headers)
	assert.Equal(t, reportErr, de.Err)
}

// Code is what an application labels a metric with, so each kind of failure has
// to come through distinctly, and a non-Kafka error must not invent one.
//
// Note what is deliberately absent: kfk.Error.IsRetriable. That flag is only
// ever set by the transactional producer API, so on a delivery report it is
// always false — a field that would lie rather than inform.
func TestDeliveryErrorForNamesTheErrorCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{"transport failure", kfk.NewError(kfk.ErrTransport, "broker down", false), kfk.ErrTransport.String()},
		{"size rejection", kfk.NewError(kfk.ErrMsgSizeTooLarge, "too large", false), kfk.ErrMsgSizeTooLarge.String()},
		{"non-kafka error has no code", errors.New("something else"), ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			de := kafka.DeliveryErrorFor(failedReport("orders-dlt", tc.err))

			require.NotNil(t, de)
			assert.Equal(t, tc.code, de.Code)
			assert.Equal(t, tc.err, de.Err)
		})
	}

	assert.NotEqual(t, kfk.ErrTransport.String(), kfk.ErrMsgSizeTooLarge.String(),
		"codes must be distinguishable, or the metric label is useless")
}

// A successful report must not reach the callback: it is a delivery *error*
// hook, and firing it on every produced record would make it useless.
func TestDeliveryErrorForIgnoresSuccessAndOtherEvents(t *testing.T) {
	topic := "orders-retry"

	assert.Nil(t, kafka.DeliveryErrorFor(&kfk.Message{
		TopicPartition: kfk.TopicPartition{Topic: &topic, Error: nil},
	}), "successful delivery report")

	assert.Nil(t, kafka.DeliveryErrorFor(kfk.NewError(kfk.ErrAllBrokersDown, "all brokers down", false)),
		"client-level error event is a connection signal, not a per-record one")

	assert.Nil(t, kafka.DeliveryErrorFor(&kfk.Stats{}), "stats event")
}

// librdkafka does not return headers on a delivery report, so the record as we
// submitted it is carried through as the message opaque and preferred over the
// report's own copy. Without this the callback could never name the retry
// attempt or the original topic — the two things worth knowing about a lost
// retry write.
func TestDeliveryErrorForPrefersTheSubmittedRecord(t *testing.T) {
	report := failedReport("orders-retry", kfk.NewError(kfk.ErrMsgSizeTooLarge, "too large", false))
	report.Headers = nil // as a real delivery report arrives
	report.Opaque = &types.ProduceMessage{
		Topic:   "orders-retry",
		Key:     []byte("slip-9"),
		Value:   []byte("submitted body"),
		Headers: map[string]string{"easykafka.retry.attempt": "3"},
	}

	de := kafka.DeliveryErrorFor(report)

	require.NotNil(t, de)
	assert.Equal(t, map[string]string{"easykafka.retry.attempt": "3"}, de.Headers)
	assert.Equal(t, []byte("slip-9"), de.Key)
	assert.Equal(t, []byte("submitted body"), de.Value)

	// The error still comes from the report — the opaque knows nothing about it.
	assert.Equal(t, kfk.ErrMsgSizeTooLarge.String(), de.Code)
}

// Guards both halves of the opaque check in DeliveryErrorFor:
//
//	orig, ok := ev.Opaque.(*types.ProduceMessage)   // ok == false: no opaque, or another type
//	if ok && orig != nil {                          // orig == nil: a typed nil got through
//
// Drop the `, ok` and it still compiles — then panics when Opaque holds
// anything else. Drop the `orig != nil` and a typed nil dereferences. Either
// way it blows up on the producer's event goroutine, which is the only thing
// reporting delivery failures, so the crash takes the reporting with it.
//
// Only the "no opaque" case is reachable today. The other two are here so that
// a future change fails this test instead.
func TestDeliveryErrorForFallsBackWhenTheOpaqueIsUnusable(t *testing.T) {
	tests := []struct {
		name   string
		opaque any
	}{
		{"no opaque", nil},
		{"typed nil", (*types.ProduceMessage)(nil)},
		{"foreign type", "not a produce message"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := failedReport("orders-retry", kfk.NewError(kfk.ErrTransport, "broker down", false))
			report.Opaque = tc.opaque

			var de *types.DeliveryError
			require.NotPanics(t, func() { de = kafka.DeliveryErrorFor(report) })

			require.NotNil(t, de)
			assert.Equal(t, "orders-retry", de.Topic)
			assert.Equal(t, []byte("slip-1"), de.Key)
			assert.Equal(t, map[string]string{
				"easykafka.retry.attempt":  "2",
				"easykafka.original.topic": "orders",
			}, de.Headers, "headers come from the report when the opaque cannot supply them")
		})
	}
}

// A report can arrive before librdkafka has assigned a topic pointer. Mapping
// it must not dereference nil.
func TestDeliveryErrorForTolNilTopic(t *testing.T) {
	de := kafka.DeliveryErrorFor(&kfk.Message{
		TopicPartition: kfk.TopicPartition{
			Topic: nil,
			Error: kfk.NewError(kfk.ErrUnknownTopic, "unknown topic", false),
		},
	})

	require.NotNil(t, de)
	assert.Empty(t, de.Topic)
}

func TestInvokeDeliveryErrorCallsTheCallback(t *testing.T) {
	var got types.DeliveryError
	called := 0

	kafka.InvokeDeliveryError(func(de types.DeliveryError) {
		got = de
		called++
	}, zerolog.Nop(), types.DeliveryError{Topic: "orders-dlt"})

	assert.Equal(t, 1, called)
	assert.Equal(t, "orders-dlt", got.Topic)
}

// The default path. Every existing test exercises it implicitly; none asserts it.
func TestInvokeDeliveryErrorNilCallbackIsSafe(t *testing.T) {
	assert.NotPanics(t, func() {
		kafka.InvokeDeliveryError(nil, zerolog.Nop(), types.DeliveryError{Topic: "orders-dlt"})
	})
}

// The test that matters. The callback runs inside the range over Events(), so
// without the recover a panicking callback kills that goroutine and the
// producer drains nothing for the life of the process — silently, since the
// only thing that would have reported it is the goroutine that just died.
func TestInvokeDeliveryErrorContainsAPanic(t *testing.T) {
	calls := 0
	panicking := func(types.DeliveryError) {
		calls++
		panic("callback blew up")
	}

	assert.NotPanics(t, func() {
		kafka.InvokeDeliveryError(panicking, zerolog.Nop(), types.DeliveryError{Topic: "orders-dlt"})
	})

	// The loop survives, so a later failure still reaches the callback.
	assert.NotPanics(t, func() {
		kafka.InvokeDeliveryError(panicking, zerolog.Nop(), types.DeliveryError{Topic: "orders-retry"})
	})

	assert.Equal(t, 2, calls, "both reports should have been delivered to the callback")
}

// Retry and DLQ have separate producers, so the same func is invoked from two
// event goroutines. Run under -race.
func TestInvokeDeliveryErrorIsSafeForConcurrentUse(t *testing.T) {
	const producers, reports = 2, 50

	var mu sync.Mutex
	seen := map[string]int{}

	callback := func(de types.DeliveryError) {
		mu.Lock()
		defer mu.Unlock()
		seen[de.Topic]++
	}

	var wg sync.WaitGroup
	for _, topic := range []string{"orders-retry", "orders-dlt"} {
		wg.Go(func() {
			for range reports {
				kafka.InvokeDeliveryError(callback, zerolog.Nop(), types.DeliveryError{Topic: topic})
			}
		})
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, seen, producers)
	assert.Equal(t, reports, seen["orders-retry"])
	assert.Equal(t, reports, seen["orders-dlt"])
}
