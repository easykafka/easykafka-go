package unit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/internal/publishdriver"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/strategy"
	"github.com/easykafka/easykafka-go/tests/unit/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Retry and DLQ writes are confirmed before HandleError returns
//
// The fake producer reports nothing on its own here: each test scripts the
// broker's answer to every record, so it decides when HandleError may return.
// =============================================================================

// TestHandleErrorFailsWhenAWriteIsNotAcknowledged verifies a record the broker
// refuses fails HandleError, for the retry topic and for the DLQ alike, so the
// engine stops without storing the source offset. The callback has heard of
// the record by the time HandleError returns.
func TestHandleErrorFailsWhenAWriteIsNotAcknowledged(t *testing.T) {
	for _, tc := range []struct {
		name        string
		maxAttempts int
		topic       string
	}{
		{"retry", 3, "test.retry"},
		{"dlq", 1, "test.dlq"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &helpers.DeliveryErrorRecorder{}
			fake := helpers.NewFakePublishProducer()
			s := helpers.NewRetryStrategyOnFake(t, fake, tc.maxAttempts,
				strategy.WithDeliveryErrorFunc(recorder.CallbackFunc))

			msg := helpers.NewTestMessage("orders", 0, 100, "m")
			// No handler runs: the failure is simulated by calling HandleErrorInBackground -> HandleError explicitly.
			result := helpers.HandleErrorInBackground(context.Background(), s, []*types.Message{msg},
				types.Failure{Err: errors.New("handler err")})
			require.Eventually(t, func() bool { return fake.Produced() == 1 }, 2*time.Second, time.Millisecond)

			fake.Fail(0, 0, &publishdriver.KafkaError{Code: "Broker: Message size too large", Message: "too large"})

			err := <-result
			require.Error(t, err)
			var deliveryError *publish.DeliveryError
			require.ErrorAs(t, err, &deliveryError)
			assert.Equal(t, tc.topic, deliveryError.Topic)
			assert.Equal(t, "Broker: Message size too large", deliveryError.Code)

			delivered := recorder.Errors()
			require.Len(t, delivered, 1, "the callback hears of the record before HandleError returns")
			assert.Equal(t, tc.topic, delivered[0].Topic)
		})
	}
}

// TestHandleErrorReturnsOnlyAfterTheLastAcknowledgement verifies HandleError
// sends every record first and returns nil once the broker has acknowledged
// all of them, and not before.
func TestHandleErrorReturnsOnlyAfterTheLastAcknowledgement(t *testing.T) {
	fake := helpers.NewFakePublishProducer()
	s := helpers.NewRetryStrategyOnFake(t, fake, 3)

	msgs := []*types.Message{
		helpers.NewTestMessage("orders", 0, 100, "msg-1"),
		helpers.NewTestMessage("orders", 0, 101, "msg-2"),
	}
	// No handler runs: the failure is simulated by calling HandleErrorInBackground -> HandleError explicitly.
	result := helpers.HandleErrorInBackground(context.Background(), s, msgs, types.Failure{Err: errors.New("batch failed")})

	// Both records are sent before the first acknowledgement arrives.
	require.Eventually(t, func() bool { return fake.Produced() == 2 }, 2*time.Second, time.Millisecond)

	// The strategy handles both messages and waits for both deliveries, so acknowledging only the
	// first must leave nothing on result.
	fake.Succeed(0, 0, 0)
	select {
	case err := <-result:
		require.FailNow(t, "HandleError returned before the last record was acknowledged", "returned %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	fake.Succeed(1, 0, 1)
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "HandleError did not return once every record was acknowledged")
	}
}

// TestHandleErrorWaitsDespiteACancelledContext verifies a context cancelled at
// shutdown does not abandon a write about to be confirmed: HandleError still
// waits for the report, and returns nil when it is a success.
func TestHandleErrorWaitsDespiteACancelledContext(t *testing.T) {
	fake := helpers.NewFakePublishProducer()
	s := helpers.NewRetryStrategyOnFake(t, fake, 3)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msg := helpers.NewTestMessage("orders", 0, 100, "m")
	result := helpers.HandleErrorInBackground(ctx, s, []*types.Message{msg}, types.Failure{Err: errors.New("x")})
	require.Eventually(t, func() bool { return fake.Produced() == 1 }, 2*time.Second, time.Millisecond)

	// ctx was cancelled before HandleError ran, and the record is not reported yet. HandleError waits
	// on context.WithoutCancel(ctx) (made by HandleError itself, before awaitWrites), which ignores
	// the cancellation, so it must still be blocked and leave nothing on result. Waiting on ctx
	// itself would return context.Canceled at once. (In production the publisher's 30 s delivery
	// timeout bounds the wait. The fake has none: here only fake.Succeed below releases HandleError,
	// or, should the test fail first, the cleanup's Close purging the record after 10 s.)
	select {
	case err := <-result:
		require.FailNow(t, "HandleError gave up on the record because its context was cancelled", "returned %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	fake.Succeed(0, 0, 0)
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "HandleError did not return once the record was acknowledged")
	}
}

// TestNilPayloadIsWrittenAsATombstone verifies a message consumed with a nil
// payload reaches the retry topic and the DLQ as a record with a nil value,
// keeping its key and headers, rather than failing on publish.RawValue.
func TestNilPayloadIsWrittenAsATombstone(t *testing.T) {
	for _, tc := range []struct {
		name        string
		maxAttempts int
		topic       string
	}{
		{"retry", 3, "test.retry"},
		{"dlq", 1, "test.dlq"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fake := helpers.NewAcknowledgingRetryStrategy(t, tc.maxAttempts)

			msg := &types.Message{
				Topic:   "orders",
				Offset:  100,
				Key:     []byte("order-42"),
				Headers: map[string]string{"trace-id": "abc-123"},
			}
			require.NoError(t, s.HandleError(context.Background(), []*types.Message{msg}, types.Failure{Err: errors.New("x")}))

			records := fake.RecordsTo(tc.topic)
			require.Len(t, records, 1)
			assert.Nil(t, records[0].Value)
			assert.Equal(t, []byte("order-42"), records[0].Key)
			headers := helpers.HeadersOf(records[0])
			assert.Equal(t, "abc-123", headers["trace-id"])
			assert.Equal(t, "orders", headers[metadata.HeaderOriginalTopic])
		})
	}
}

// TestRetryRecordHeadersAreSortedByKey verifies the headers come out in one
// order every time, sorted by key, although they are built in a map.
func TestRetryRecordHeadersAreSortedByKey(t *testing.T) {
	s, fake := helpers.NewAcknowledgingRetryStrategy(t, 3)

	msg := helpers.NewTestMessage("orders", 0, 100, "m")
	msg.Headers = map[string]string{"zulu": "z", "alpha": "a", "mike": "m"}
	require.NoError(t, s.HandleError(context.Background(), []*types.Message{msg}, types.Failure{Err: errors.New("x")}))

	records := fake.RecordsTo("test.retry")
	require.Len(t, records, 1)
	keys := make([]string, len(records[0].Headers))
	for index, header := range records[0].Headers {
		keys[index] = header.Key
	}
	// Every header, application and library alike, sorted by key. No easykafka.retry.step: none was set.
	assert.Equal(t, []string{
		"alpha",
		"easykafka.error.code", "easykafka.error.message", "easykafka.failed.at",
		"easykafka.original.offset", "easykafka.original.partition", "easykafka.original.topic",
		"easykafka.retry.attempt", "easykafka.retry.time",
		"mike", "zulu",
	}, keys)
}

// TestCloseReportsAnUnreportedWrite verifies a record still unreported when the
// strategy closes is purged and reported, not dropped: the callback hears of
// it, the HandleError waiting on it fails, and Close returns ErrNotDelivered.
// It takes Close's full 10 s bound, so it runs in parallel.
func TestCloseReportsAnUnreportedWrite(t *testing.T) {
	t.Parallel()

	recorder := &helpers.DeliveryErrorRecorder{}
	fake := helpers.NewFakePublishProducer()
	s := helpers.NewRetryStrategyOnFake(t, fake, 3, strategy.WithDeliveryErrorFunc(recorder.CallbackFunc))

	msg := helpers.NewTestMessage("orders", 0, 100, "m")
	result := helpers.HandleErrorInBackground(context.Background(), s, []*types.Message{msg}, types.Failure{Err: errors.New("x")})
	require.Eventually(t, func() bool { return fake.Produced() == 1 }, 2*time.Second, time.Millisecond)

	// The fake emits nothing (no AutoAcknowledge, no Succeed or Fail), so the record is still unreported here.
	started := time.Now()
	err := s.Close()
	elapsed := time.Since(started)
	require.ErrorIs(t, err, publish.ErrNotDelivered)

	// Close first waits the strategy's whole 10 s closeTimeout for the record to be delivered: the
	// publisher drains until that deadline passes, never sooner, since the record stays pending. Only
	// then does it purge, and the purge report comes back at once, so Close ends shortly after 10 s.
	// The upper bound leaves room for a slow, loaded test run.
	assert.GreaterOrEqual(t, elapsed, 10*time.Second, "Close must wait out its timeout before purging")
	assert.Less(t, elapsed, 13*time.Second, "Close must purge as soon as its timeout has passed")
	assert.Equal(t, 1, fake.PurgeCalls())

	delivered := recorder.Errors()
	require.Len(t, delivered, 1)
	assert.Equal(t, "test.retry", delivered[0].Topic)
	require.ErrorIs(t, delivered[0].Err, publish.ErrNotDelivered)

	// The purge report resolved the delivery HandleError was waiting on, so it returns with that error.
	select {
	case handleErr := <-result:
		require.ErrorIs(t, handleErr, publish.ErrNotDelivered)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "HandleError still waiting after Close purged its record")
	}
}
