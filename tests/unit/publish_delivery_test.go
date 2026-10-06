package unit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/publishdriver"
	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/unit/helpers"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishReturnsNilOnSuccessReport verifies that Publish waits for the
// report and returns nil when the record was acknowledged.
func TestPublishReturnsNilOnSuccessReport(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	writer := publisher.Bind(helpers.PublishInvoiceTopic())

	// Publish blocks until its report arrives, and here the report comes only
	// when the test emits it, so Publish runs on its own goroutine. Its result
	// comes back on published, buffered so the goroutine can always exit.
	published := make(chan error, 1)
	go func() { published <- writer.Publish(context.Background(), "INV-1", helpers.NewPublishInvoice()) }()

	// The record is enqueued: Publish is now waiting.
	require.Eventually(t, func() bool { return fake.Len() == 1 }, time.Second, time.Millisecond)
	// It must not have returned yet: Publish waits for the report, unlike Send.
	select {
	case err := <-published:
		require.FailNow(t, "Publish returned before the report", "err: %v", err)
	default:
	}
	fake.Succeed(0, 2, 41)          // emit the report
	require.NoError(t, <-published) // now Publish returns nil
}

// TestPublishReturnsDeliveryErrorOnFailureReport verifies that a failure
// report surfaces from Publish as a *DeliveryError holding the record exactly
// as it was sent.
func TestPublishReturnsDeliveryErrorOnFailureReport(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	writer := publisher.Bind(helpers.PublishInvoiceTopic())

	published := make(chan error, 1)
	go func() {
		published <- writer.Publish(context.Background(), "INV-1", helpers.NewPublishInvoice(),
			publish.Header{Key: "trace", Value: []byte("t-1")})
	}()
	require.Eventually(t, func() bool { return fake.Len() == 1 }, time.Second, time.Millisecond)
	fake.Fail(0, 3, &publishdriver.KafkaError{Code: "Broker: Message size too large", Message: "Broker: Message size too large"})

	err := <-published
	deliveryError, isDeliveryError := errors.AsType[*publish.DeliveryError](err)
	require.True(t, isDeliveryError, "got %v", err)
	assert.Equal(t, "invoices", deliveryError.Topic)
	assert.Equal(t, int32(3), deliveryError.Partition)
	assert.Equal(t, []byte("INV-1"), deliveryError.Key)
	assert.Equal(t, fake.Records()[0].Value, deliveryError.Value)
	assert.Equal(t, []publish.Header{
		{Key: "__TypeId__", Value: []byte("Invoice")},
		{Key: "trace", Value: []byte("t-1")},
	}, deliveryError.Headers)
	assert.Equal(t, "Broker: Message size too large", deliveryError.Code)
}

// TestPublishSendReturnsBeforeReport verifies that Send does not wait, and the
// delivery resolves when its report arrives, with where the record landed.
func TestPublishSendReturnsBeforeReport(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	writer := publisher.Bind(helpers.PublishInvoiceTopic())

	delivery, err := writer.Send("INV-1", helpers.NewPublishInvoice())
	require.NoError(t, err)
	select {
	case <-delivery.Done():
		require.FailNow(t, "delivery resolved before any report")
	default:
	}

	fake.Succeed(0, 1, 7)
	result, err := helpers.WaitForDelivery(t, delivery)
	require.NoError(t, err)
	assert.Equal(t, publish.Result{Topic: "invoices", Partition: 1, Offset: 7}, result)
}

// TestPublishReportsOutOfOrderResolveTheirOwnDeliveries verifies that the
// token, not the order of arrival, decides which delivery a report resolves.
//
// The order is not something the publisher depends on: each report carries
// its record's Delivery as its token. It is there so the test can fail. A
// publisher that matched reports by arrival order, resolving its pending
// deliveries first in, first out, would pass every test whose reports come in
// order; only reports out of order tell the two apart.
//
// Within one partition librdkafka reports records in the order they were
// produced. Across partitions it does not: each partition's batch goes to its
// own leader, and reports come back as each request completes. So the three
// records land on three partitions, and their reports arrive out of order.
func TestPublishReportsOutOfOrderResolveTheirOwnDeliveries(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	writer := publisher.Bind(helpers.PublishInvoiceTopic())

	deliveries := make([]*publish.Delivery, 3)
	for index := range deliveries {
		delivery, err := writer.Send("INV", helpers.NewPublishInvoice())
		require.NoError(t, err)
		deliveries[index] = delivery
	}
	// Record n lands on partition n, at offset 100+n.
	fake.Succeed(2, 2, 102)
	fake.Succeed(0, 0, 100)
	fake.Succeed(1, 1, 101)

	for index, delivery := range deliveries {
		result, err := helpers.WaitForDelivery(t, delivery)
		require.NoError(t, err)
		assert.Equal(t, publish.Result{Topic: "invoices", Partition: int32(index), Offset: int64(100 + index)}, result,
			"delivery %d", index)
	}
}

// TestPublishCallbackRunsForEveryFailureBeforeWaitReturns verifies that the
// callback sees each failure, including for deliveries nobody waits on, and
// has already run when a Wait returns the error.
func TestPublishCallbackRunsForEveryFailureBeforeWaitReturns(t *testing.T) {
	recorder := &helpers.DeliveryErrorRecorder{}
	publisher, fake := helpers.NewFakePublisher(t, publish.WithDeliveryErrorFunc(recorder.CallbackFunc))
	writer := publisher.Bind(helpers.PublishInvoiceTopic())

	ignored, err := writer.Send("ignored", helpers.NewPublishInvoice())
	require.NoError(t, err)
	waited, err := writer.Send("waited", helpers.NewPublishInvoice())
	require.NoError(t, err)

	fake.Fail(0, -1, &publishdriver.KafkaError{Code: "Local: Message timed out", Sentinel: publishdriver.ErrDeliveryTimeout})
	fake.Fail(1, -1, &publishdriver.KafkaError{Code: "Local: Message timed out", Sentinel: publishdriver.ErrDeliveryTimeout})

	_, err = helpers.WaitForDelivery(t, waited)
	require.ErrorIs(t, err, publish.ErrDeliveryTimeout)
	failures := recorder.Errors()
	require.Len(t, failures, 2, "the callback must have run for both before Wait returned")
	assert.Equal(t, []byte("ignored"), failures[0].Key)
	assert.Equal(t, []byte("waited"), failures[1].Key)

	// Nobody was waiting on ignored when its report arrived, and it was
	// resolved all the same. Checking it now returns at once, with its error.
	_, err = helpers.WaitForDelivery(t, ignored)
	require.ErrorIs(t, err, publish.ErrDeliveryTimeout)
}

// TestPublishCallbackPanicIsRecovered verifies that a panicking callback is
// logged, the delivery still resolves, and later reports are still read.
func TestPublishCallbackPanicIsRecovered(t *testing.T) {
	logs := &helpers.SyncBuffer{}
	publisher, fake := helpers.NewFakePublisher(t,
		publish.WithLogger(zerolog.New(logs)),
		publish.WithDeliveryErrorFunc(func(publish.DeliveryError) { panic("callback bug") }),
	)
	writer := publisher.Bind(helpers.PublishInvoiceTopic())

	failed, err := writer.Send("INV-1", helpers.NewPublishInvoice())
	require.NoError(t, err)
	later, err := writer.Send("INV-2", helpers.NewPublishInvoice())
	require.NoError(t, err)

	fake.Fail(0, 0, &publishdriver.KafkaError{Code: "Broker: Unknown topic or partition"})
	fake.Succeed(1, 0, 5)

	_, err = helpers.WaitForDelivery(t, failed)
	require.Error(t, err)
	_, err = helpers.WaitForDelivery(t, later)
	require.NoError(t, err)
	assert.Contains(t, logs.String(), "EK_PUBLISH_CALLBACK_PANIC")
	assert.Contains(t, logs.String(), "callback bug")
}

// TestPublishSlowCallbackDelaysButLosesNothing verifies that a callback that
// blocks holds back later reports without dropping any.
func TestPublishSlowCallbackDelaysButLosesNothing(t *testing.T) {
	var calls atomic.Int32
	publisher, fake := helpers.NewFakePublisher(t, publish.WithDeliveryErrorFunc(func(publish.DeliveryError) {
		time.Sleep(20 * time.Millisecond)
		calls.Add(1)
	}))
	writer := publisher.Bind(helpers.PublishInvoiceTopic())

	const count = 5
	deliveries := make([]*publish.Delivery, count)
	for index := range deliveries {
		delivery, err := writer.Send("INV", helpers.NewPublishInvoice())
		require.NoError(t, err)
		deliveries[index] = delivery
	}
	go func() {
		for index := range count {
			fake.Fail(index, 0, &publishdriver.KafkaError{Code: "Broker: Not enough in-sync replicas"})
		}
	}()

	for _, delivery := range deliveries {
		_, err := helpers.WaitForDelivery(t, delivery)
		require.Error(t, err)
	}
	assert.Equal(t, int32(count), calls.Load())
}

// TestPublishWaitWithCancelledContext verifies that Wait gives up with
// ctx.Err(), and a later report still resolves the delivery and reaches the
// callback.
func TestPublishWaitWithCancelledContext(t *testing.T) {
	recorder := &helpers.DeliveryErrorRecorder{}
	publisher, fake := helpers.NewFakePublisher(t, publish.WithDeliveryErrorFunc(recorder.CallbackFunc))
	writer := publisher.Bind(helpers.PublishInvoiceTopic())

	delivery, err := writer.Send("INV-1", helpers.NewPublishInvoice())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = delivery.Wait(ctx)
	require.ErrorIs(t, err, context.Canceled)

	fake.Fail(0, 0, &publishdriver.KafkaError{Code: "Local: Message timed out", Sentinel: publishdriver.ErrDeliveryTimeout})
	_, err = helpers.WaitForDelivery(t, delivery)
	require.ErrorIs(t, err, publish.ErrDeliveryTimeout)
	assert.Len(t, recorder.Errors(), 1)

	// Once known, the outcome wins over an ended context.
	_, err = delivery.Wait(ctx)
	require.ErrorIs(t, err, publish.ErrDeliveryTimeout)
}

// TestPublishPublishWithCancelledContextAbandonsTheRecord verifies that
// Publish returns ctx.Err() and leaves the record enqueued.
func TestPublishPublishWithCancelledContextAbandonsTheRecord(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	writer := publisher.Bind(helpers.PublishInvoiceTopic())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := writer.Publish(ctx, "INV-1", helpers.NewPublishInvoice())
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, fake.Len(), "the record is enqueued all the same")
	fake.Succeed(0, 0, 1)
}

// TestPublishHeadersOrderAndCopy verifies that the topic's headers come first,
// then the per-call ones, and that the caller changing its slice afterwards
// does not change the record.
func TestPublishHeadersOrderAndCopy(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	topic := helpers.PublishInvoiceTopic()
	writer := publisher.Bind(topic)
	topic.Headers[0] = publish.Header{Key: "changed", Value: []byte("after Bind")}

	headers := []publish.Header{{Key: "trace", Value: []byte("t-1")}, {Key: "trace", Value: []byte("t-2")}}
	_, err := writer.Send("INV-1", helpers.NewPublishInvoice(), headers...)
	require.NoError(t, err)
	headers[0] = publish.Header{Key: "changed", Value: []byte("after Send")}

	assert.Equal(t, []publishdriver.Header{
		{Key: "__TypeId__", Value: []byte("Invoice")},
		{Key: "trace", Value: []byte("t-1")},
		{Key: "trace", Value: []byte("t-2")},
	}, fake.Records()[0].Headers)
	fake.Succeed(0, 0, 1)
}

// TestPublishRecordWithoutHeaders verifies that a record with no headers at
// all carries none.
func TestPublishRecordWithoutHeaders(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	writer := publisher.Bind(publish.Topic[string, []byte]{Name: "raw", EncodeKey: publish.StringKey, EncodeValue: publish.RawValue})

	_, err := writer.Send("k", []byte("v"))
	require.NoError(t, err)
	record := fake.Records()[0]
	assert.Equal(t, publishdriver.Record{Topic: "raw", Key: []byte("k"), Value: []byte("v")}, record)
	fake.Succeed(0, 0, 1)
}

// TestPublishEncodeFailuresEnqueueNothing verifies that a key or value that
// fails to encode, or a value that encodes to nil, is ErrEncode and nothing is
// produced.
func TestPublishEncodeFailuresEnqueueNothing(t *testing.T) {
	failing := func(string) ([]byte, error) { return nil, errors.New("cannot encode") }
	cases := []struct {
		name  string
		topic publish.Topic[string, string]
		want  string
	}{
		{name: "key", topic: publish.Topic[string, string]{Name: "t", EncodeKey: failing, EncodeValue: publish.StringKey}, want: "key for t"},
		{name: "value", topic: publish.Topic[string, string]{Name: "t", EncodeKey: publish.StringKey, EncodeValue: failing}, want: "value for t"},
		{
			name: "nil value",
			topic: publish.Topic[string, string]{Name: "t", EncodeKey: publish.StringKey,
				EncodeValue: func(string) ([]byte, error) { return nil, nil }},
			want: "use SendDelete",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			publisher, fake := helpers.NewFakePublisher(t)
			delivery, err := publisher.Bind(testCase.topic).Send("k", "v")
			require.ErrorIs(t, err, publish.ErrEncode)
			assert.Contains(t, err.Error(), testCase.want)
			assert.Nil(t, delivery)
			assert.Zero(t, fake.Len())
		})
	}
}

// TestPublishDeleteWritesNilValue verifies that SendDelete and Delete write a
// tombstone, keeping the key and the headers.
func TestPublishDeleteWritesNilValue(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	writer := publisher.Bind(helpers.PublishInvoiceTopic())

	delivery, err := writer.SendDelete("INV-1", publish.Header{Key: "trace", Value: []byte("t-1")})
	require.NoError(t, err)
	record := fake.Records()[0]
	assert.Equal(t, []byte("INV-1"), record.Key)
	assert.Nil(t, record.Value)
	assert.Len(t, record.Headers, 2)
	fake.Succeed(0, 0, 9)
	_, err = helpers.WaitForDelivery(t, delivery)
	require.NoError(t, err)

	deleted := make(chan error, 1)
	go func() { deleted <- writer.Delete(context.Background(), "INV-2") }()
	require.Eventually(t, func() bool { return fake.Len() == 2 }, time.Second, time.Millisecond)
	assert.Nil(t, fake.Records()[1].Value)
	fake.Succeed(1, 0, 10)
	require.NoError(t, <-deleted)
}

// TestPublishDeleteKeyEncodeFailure verifies that a tombstone whose key fails
// to encode is ErrEncode.
func TestPublishDeleteKeyEncodeFailure(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	writer := publisher.Bind(publish.Topic[string, string]{
		Name:        "t",
		EncodeKey:   func(string) ([]byte, error) { return nil, errors.New("cannot encode") },
		EncodeValue: publish.JSONValue[string],
	})
	err := writer.Delete(context.Background(), "k")
	require.ErrorIs(t, err, publish.ErrEncode)
	assert.Zero(t, fake.Len())
}

// TestPublishReportErrorMapping verifies the sentinel each failure report
// wraps, and that the report's code is kept.
func TestPublishReportErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		err     *publishdriver.KafkaError
		want    []error
		notWant []error
	}{
		{
			name: "timed out",
			err:  &publishdriver.KafkaError{Code: "Local: Message timed out", Sentinel: publishdriver.ErrDeliveryTimeout},
			want: []error{publish.ErrDeliveryTimeout}, notWant: []error{publish.ErrNotDelivered, publish.ErrFatal},
		},
		{
			name: "purged",
			err:  &publishdriver.KafkaError{Code: "Local: Purged in queue", Sentinel: publishdriver.ErrNotDelivered},
			want: []error{publish.ErrNotDelivered}, notWant: []error{publish.ErrFatal, publish.ErrDeliveryTimeout},
		},
		{
			name: "fatal",
			err:  &publishdriver.KafkaError{Code: "Broker: Producer fenced", Fatal: true, Sentinel: publishdriver.ErrFatal},
			want: []error{publish.ErrFatal}, notWant: []error{publish.ErrNotDelivered, publish.ErrDeliveryTimeout},
		},
		{
			name:    "anything else",
			err:     &publishdriver.KafkaError{Code: "Broker: Topic authorization failed"},
			notWant: []error{publish.ErrNotDelivered, publish.ErrFatal, publish.ErrDeliveryTimeout},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			publisher, fake := helpers.NewFakePublisher(t)
			delivery, err := publisher.Bind(helpers.PublishInvoiceTopic()).Send("k", helpers.NewPublishInvoice())
			require.NoError(t, err)
			fake.Fail(0, -1, testCase.err)

			_, err = helpers.WaitForDelivery(t, delivery)
			deliveryError, isDeliveryError := errors.AsType[*publish.DeliveryError](err)
			require.True(t, isDeliveryError, "got %v", err)
			assert.Equal(t, testCase.err.Code, deliveryError.Code)
			assert.Equal(t, int32(-1), deliveryError.Partition)
			require.ErrorIs(t, err, testCase.err, "the Kafka error stays reachable")
			for _, sentinel := range testCase.want {
				require.ErrorIs(t, err, sentinel)
			}
			for _, sentinel := range testCase.notWant {
				assert.NotErrorIs(t, err, sentinel)
			}
		})
	}
}

// TestPublishEnqueueErrorMapping verifies the errors Send returns when Produce
// fails, and that no delivery is returned.
func TestPublishEnqueueErrorMapping(t *testing.T) {
	tooLarge := &publishdriver.KafkaError{Code: "Broker: Message size too large", Message: "Broker: Message size too large"}
	cases := []struct {
		name        string
		err         error
		want        error
		wantMessage string
	}{
		{
			name: "queue full",
			err:  &publishdriver.KafkaError{Code: "Local: Queue full", Sentinel: publishdriver.ErrQueueFull},
			want: publish.ErrQueueFull,
			wantMessage: "publish: record for invoices not enqueued: " +
				"publish: local producer queue is full: Local: Queue full",
		},
		{
			name: "fatal",
			err:  &publishdriver.KafkaError{Code: "Local: Fatal error", Fatal: true, Sentinel: publishdriver.ErrFatal},
			want: publish.ErrFatal,
			wantMessage: "publish: record for invoices not enqueued: " +
				"publish: producer failed fatally: Local: Fatal error",
		},
		{
			name:        "driver closed",
			err:         publishdriver.ErrClosed,
			want:        publish.ErrClosed,
			wantMessage: "publish: record for invoices not enqueued: publish: publisher is closed",
		},
		{
			name:        "too large, no sentinel",
			err:         tooLarge,
			want:        tooLarge,
			wantMessage: "publish: record for invoices not enqueued: Broker: Message size too large",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			publisher, fake := helpers.NewFakePublisher(t)
			fake.ProduceErr = testCase.err
			delivery, err := publisher.Bind(helpers.PublishInvoiceTopic()).Send("k", helpers.NewPublishInvoice())
			require.ErrorIs(t, err, testCase.want)
			require.EqualError(t, err, testCase.wantMessage)
			assert.Nil(t, delivery)
		})
	}
}

// TestPublishPublishReturnsEnqueueError verifies that Publish and Delete
// return a failed enqueue at once, with nothing to wait for.
func TestPublishPublishReturnsEnqueueError(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	fake.ProduceErr = &publishdriver.KafkaError{Code: "Local: Queue full", Sentinel: publishdriver.ErrQueueFull}
	writer := publisher.Bind(helpers.PublishInvoiceTopic())

	require.ErrorIs(t, writer.Publish(context.Background(), "k", helpers.NewPublishInvoice()), publish.ErrQueueFull)
	require.ErrorIs(t, writer.Delete(context.Background(), "k"), publish.ErrQueueFull)
}

// TestPublishDeliveryFailureIsLogged verifies that every failed record is
// logged with its code, topic and partition.
func TestPublishDeliveryFailureIsLogged(t *testing.T) {
	logs := &helpers.SyncBuffer{}
	publisher, fake := helpers.NewFakePublisher(t, publish.WithLogger(zerolog.New(logs)))
	delivery, err := publisher.Bind(helpers.PublishInvoiceTopic()).Send("k", helpers.NewPublishInvoice())
	require.NoError(t, err)
	fake.Fail(0, 4, &publishdriver.KafkaError{Code: "Broker: Not enough in-sync replicas"})
	_, _ = helpers.WaitForDelivery(t, delivery)

	assert.Contains(t, logs.String(), `"ek_code":"EK_PUBLISH_DELIVERY_FAILED"`)
	assert.Contains(t, logs.String(), `"topic":"invoices"`)
	assert.Contains(t, logs.String(), `"partition":4`)
	assert.Contains(t, logs.String(), `"code":"Broker: Not enough in-sync replicas"`)
}

// TestPublishDuplicateReportIsDropped verifies that a second report for one
// record is logged and dropped: the first outcome stands, the callback does
// not run again, and later reports are still read.
func TestPublishDuplicateReportIsDropped(t *testing.T) {
	logs := &helpers.SyncBuffer{}
	recorder := &helpers.DeliveryErrorRecorder{}
	publisher, fake := helpers.NewFakePublisher(t,
		publish.WithLogger(zerolog.New(logs)), publish.WithDeliveryErrorFunc(recorder.CallbackFunc))
	writer := publisher.Bind(helpers.PublishInvoiceTopic())
	first, err := writer.Send("k", helpers.NewPublishInvoice())
	require.NoError(t, err)
	later, err := writer.Send("k", helpers.NewPublishInvoice())
	require.NoError(t, err)

	fake.Succeed(0, 1, 7)
	fake.Fail(0, 1, &publishdriver.KafkaError{Code: "Local: Message timed out", Sentinel: publishdriver.ErrDeliveryTimeout})
	fake.Succeed(1, 1, 8)

	_, err = helpers.WaitForDelivery(t, later)
	require.NoError(t, err)
	result, err := helpers.WaitForDelivery(t, first)
	require.NoError(t, err, "the first outcome stands")
	assert.Equal(t, int64(7), result.Offset)
	assert.Empty(t, recorder.Errors(), "the dropped failure does not reach the callback")
	assert.Equal(t, 1, strings.Count(logs.String(), "EK_PUBLISH_UNMATCHED_REPORT"))
	assert.Contains(t, logs.String(), "already resolved")
	assert.NotContains(t, logs.String(), "EK_PUBLISH_DELIVERY_FAILED")
}

// TestPublishReportWithoutDeliveryIsDropped verifies that a report whose token
// is not a delivery is logged and dropped, and later reports are still read.
func TestPublishReportWithoutDeliveryIsDropped(t *testing.T) {
	logs := &helpers.SyncBuffer{}
	publisher, fake := helpers.NewFakePublisher(t, publish.WithLogger(zerolog.New(logs)))
	delivery, err := publisher.Bind(helpers.PublishInvoiceTopic()).Send("k", helpers.NewPublishInvoice())
	require.NoError(t, err)

	fake.Emit(publishdriver.Report{Token: "not a delivery", Partition: 2, Offset: 3})
	fake.Emit(publishdriver.Report{Token: nil, Partition: -1, Offset: -1})
	fake.Succeed(0, 0, 1)

	_, err = helpers.WaitForDelivery(t, delivery)
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(logs.String(), "EK_PUBLISH_UNMATCHED_REPORT"))
	assert.Contains(t, logs.String(), "token is a string")
	assert.Contains(t, logs.String(), "token is a <nil>")
}

// TestPublishWaitAllJoinsEveryFailure verifies that WaitAll waits for every
// delivery and returns all the failures, not only the first.
func TestPublishWaitAllJoinsEveryFailure(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	writer := publisher.Bind(helpers.PublishInvoiceTopic())

	deliveries := make([]*publish.Delivery, 3)
	for index := range deliveries {
		delivery, err := writer.Send("k", helpers.NewPublishInvoice())
		require.NoError(t, err)
		deliveries[index] = delivery
	}
	go func() {
		fake.Fail(0, 0, &publishdriver.KafkaError{Code: "Local: Message timed out", Sentinel: publishdriver.ErrDeliveryTimeout})
		fake.Succeed(1, 0, 1)
		fake.Fail(2, 0, &publishdriver.KafkaError{Code: "Local: Purged in queue", Sentinel: publishdriver.ErrNotDelivered})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := publish.WaitAll(ctx, deliveries...)
	require.ErrorIs(t, err, publish.ErrDeliveryTimeout)
	require.ErrorIs(t, err, publish.ErrNotDelivered)
}

// TestPublishWaitAllSucceeds verifies that WaitAll returns nil when every
// record is acknowledged, and for no deliveries at all.
func TestPublishWaitAllSucceeds(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	writer := publisher.Bind(helpers.PublishInvoiceTopic())
	first, err := writer.Send("k", helpers.NewPublishInvoice())
	require.NoError(t, err)
	second, err := writer.Send("k", helpers.NewPublishInvoice())
	require.NoError(t, err)
	fake.Succeed(1, 0, 2)
	fake.Succeed(0, 0, 1)

	require.NoError(t, publish.WaitAll(context.Background(), first, second))
	require.NoError(t, publish.WaitAll(context.Background()))
}

// TestPublishWaitAllWithEndedContext verifies that WaitAll stops once ctx has
// ended, keeping the failures seen before it.
func TestPublishWaitAllWithEndedContext(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	writer := publisher.Bind(helpers.PublishInvoiceTopic())
	deliveries := make([]*publish.Delivery, 3)
	for index := range deliveries {
		delivery, err := writer.Send("k", helpers.NewPublishInvoice())
		require.NoError(t, err)
		deliveries[index] = delivery
	}
	fake.Fail(0, 0, &publishdriver.KafkaError{Code: "Local: Message timed out", Sentinel: publishdriver.ErrDeliveryTimeout})
	<-deliveries[0].Done()

	// Delivery 0 has failed; 1 and 2 have no report yet. With ctx already
	// cancelled, WaitAll still returns delivery 0's known failure, then stops
	// at delivery 1 with ctx.Err(), and joins the two. Each assertion checks
	// one part: that it stopped because ctx ended, and that the failure seen
	// before was kept.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := publish.WaitAll(ctx, deliveries...)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, publish.ErrDeliveryTimeout, "the failure seen before ctx ended is kept")

	fake.Succeed(1, 0, 1)
	fake.Succeed(2, 0, 2)
}

// TestPublishConcurrentSends verifies, under -race, that concurrent Sends each
// get their own delivery, resolved by their own report.
func TestPublishConcurrentSends(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	writer := publisher.Bind(helpers.PublishInvoiceTopic())

	const count = 200
	var wait sync.WaitGroup
	// Each goroutine writes only its own index, so the slice needs no lock.
	deliveries := make([]*publish.Delivery, count)
	for index := range count {
		wait.Go(func() {
			delivery, err := writer.Send("k", helpers.NewPublishInvoice())
			assert.NoError(t, err)
			deliveries[index] = delivery
		})
	}
	wait.Wait()
	for index := range count {
		fake.Succeed(index, 0, int64(index))
	}
	for _, delivery := range deliveries {
		_, err := helpers.WaitForDelivery(t, delivery)
		require.NoError(t, err)
	}
}
