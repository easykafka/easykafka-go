package publish_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/easykafka/easykafka-go/internal/publish/publishdriver"
	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/unit/publish/helpers"
	"github.com/easykafka/easykafka-go/tests/unit/sharedhelpers"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishFatalErrorIsRecordedOnce verifies that the first fatal error is
// what Err returns, is logged as EK_PUBLISH_FATAL, and reaches the fatal
// handler, once, however many fatal events follow.
func TestPublishFatalErrorIsRecordedOnce(t *testing.T) {
	logs := &sharedhelpers.SyncBuffer{}
	var handled atomic.Int32
	var handledErr atomic.Value
	publisher, fake := helpers.NewFakePublisher(t,
		publish.WithLogger(zerolog.New(logs)),
		publish.WithFatalHandler(func(err error) {
			handled.Add(1)
			handledErr.Store(err)
		}),
	)
	barrier := helpers.SendInvoices(t, publisher.Bind(helpers.PublishInvoiceTopic()), 1)[0]
	require.NoError(t, publisher.Err())

	fake.Emit(helpers.FatalClientError())
	fake.Emit(helpers.FatalClientError())
	// A barrier before checking the handler. Emit returns once the report
	// goroutine has taken an event, not once it has handled it. That goroutine
	// handles events one at a time, in order, so once this later report's
	// delivery has resolved, both fatal events above have been handled, and
	// the handler has run for the first. A record sent before the fatal error
	// is used, since sends fail after it.
	fake.Succeed(0, 0, 1)
	_, err := helpers.WaitForDelivery(t, barrier)
	require.NoError(t, err)

	require.ErrorIs(t, publisher.Err(), publish.ErrFatal)
	assert.Contains(t, publisher.Err().Error(), "Local: Fatal error")
	assert.Equal(t, int32(1), handled.Load())
	assert.Equal(t, publisher.Err(), handledErr.Load())
	assert.Equal(t, 1, strings.Count(logs.String(), "EK_PUBLISH_FATAL"))
}

// TestPublishFatalErrorWithoutSentinel verifies that Err wraps ErrFatal even
// for a fatal error that does not carry the sentinel itself.
//
// It covers the defensive branch in noteFatal, and nothing else does. The real
// driver attaches ErrFatal to every fatal error (TranslateError), so this input
// cannot come from it: the test builds a KafkaError with Fatal set and no
// Sentinel, so errors.Is(kafkaError, ErrFatal) is false and noteFatal must wrap
// it. The other fatal tests use helpers.FatalClientError, which carries the
// sentinel as the driver does, and skip the branch.
func TestPublishFatalErrorWithoutSentinel(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	// Fatal, but no Sentinel: what the driver never sends.
	fake.Emit(publishdriver.ClientError{Err: &publishdriver.KafkaError{Code: "Local: Fatal error", Fatal: true}})
	helpers.WaitForFatal(t, publisher)
	require.ErrorIs(t, publisher.Err(), publish.ErrFatal)
}

// TestPublishWritesFailAfterFatal verifies that after a fatal error every
// write fails with ErrFatal, naming the topic, and nothing is enqueued.
func TestPublishWritesFailAfterFatal(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	writer := publisher.Bind(helpers.PublishInvoiceTopic())
	fake.Emit(helpers.FatalClientError())
	helpers.WaitForFatal(t, publisher)

	_, err := writer.Send("k", helpers.NewPublishInvoice())
	require.ErrorIs(t, err, publish.ErrFatal)
	assert.Contains(t, err.Error(), "invoices")
	_, err = writer.SendDelete("k")
	require.ErrorIs(t, err, publish.ErrFatal)
	require.ErrorIs(t, writer.Publish(context.Background(), "k", helpers.NewPublishInvoice()), publish.ErrFatal)
	require.ErrorIs(t, writer.Delete(context.Background(), "k"), publish.ErrFatal)
	assert.Zero(t, fake.Produced())
}

// TestPublishPingFailsAfterFatal verifies that Ping returns the fatal error
// without asking the cluster, whose answer would be fine: a publisher that
// cannot write is not ready.
func TestPublishPingFailsAfterFatal(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	fake.Partitions = map[string]int{"invoices": 1}
	publisher.Bind(helpers.PublishInvoiceTopic())
	require.NoError(t, publisher.Ping(context.Background()))

	fake.Emit(helpers.FatalClientError())
	helpers.WaitForFatal(t, publisher)
	require.ErrorIs(t, publisher.Ping(context.Background()), publish.ErrFatal)
	assert.Equal(t, 1, fake.PingCalls(), "the cluster is not asked after the fatal error")
}

// TestPublishPendingDeliveriesResolveAroundFatal verifies that records
// librdkafka purges after a fatal error resolve with ErrNotDelivered, whether
// their purge reports arrive before or after the fatal event.
//
// What it tests is the publisher's reaction to that sequence of events, which
// the fake emits as scripted: reports keep being settled after a fatal error
// is recorded, so no waiter hangs; the order makes no difference; a purged
// record resolves with ErrNotDelivered, not ErrFatal; and the fatal error is
// recorded wherever it falls. It catches, for example, a report loop changed
// to stop or skip reports once the publisher has failed.
//
// What it does not test is the premise: that librdkafka really purges and
// reports every outstanding record on a fatal error. That was observed once,
// against a real broker, while the publisher was designed, and nothing in the
// suite checks it again, so a librdkafka upgrade that changed the behaviour
// would go unnoticed here.
func TestPublishPendingDeliveriesResolveAroundFatal(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	deliveries := helpers.SendInvoices(t, publisher.Bind(helpers.PublishInvoiceTopic()), 2)
	purged := &publishdriver.KafkaError{Code: "Local: Purged in queue", Sentinel: publishdriver.ErrNotDelivered}

	// The fake plays librdkafka's part. When the producer fails fatally,
	// librdkafka purges every outstanding record, emitting one purge report
	// per record (each carrying its Delivery), and emits the fatal event. The
	// three reach the publisher in an arbitrary order. This order, purge,
	// fatal, purge, covers in one run a purge report arriving before the fatal
	// event and one arriving after it.
	fake.Fail(0, -1, purged) // record 0's purge report, before the fatal event
	fake.Emit(helpers.FatalClientError())
	fake.Fail(1, -1, purged) // record 1's purge report, after it

	for _, delivery := range deliveries {
		_, err := helpers.WaitForDelivery(t, delivery)
		require.ErrorIs(t, err, publish.ErrNotDelivered)
		// A purge is a purge, whatever caused it: the fatal error is reported
		// through Err and the fatal handler, not on each purged record.
		require.NotErrorIs(t, err, publish.ErrFatal)
	}
	require.ErrorIs(t, publisher.Err(), publish.ErrFatal)
}

// TestPublishFatalHandlerPanicIsRecovered verifies that a panicking fatal
// handler is logged, and later reports are still read.
func TestPublishFatalHandlerPanicIsRecovered(t *testing.T) {
	logs := &sharedhelpers.SyncBuffer{}
	publisher, fake := helpers.NewFakePublisher(t,
		publish.WithLogger(zerolog.New(logs)),
		publish.WithFatalHandler(func(error) { panic("handler bug") }),
	)
	delivery := helpers.SendInvoices(t, publisher.Bind(helpers.PublishInvoiceTopic()), 1)[0]

	fake.Emit(helpers.FatalClientError())
	// A barrier, as in TestPublishFatalErrorIsRecordedOnce: once this later
	// report resolves, the panic has been handled and logged, and the report
	// goroutine has survived it.
	fake.Succeed(0, 0, 1)
	_, err := helpers.WaitForDelivery(t, delivery)
	require.NoError(t, err)
	assert.Contains(t, logs.String(), "EK_PUBLISH_CALLBACK_PANIC")
	assert.Contains(t, logs.String(), "handler bug")
}

// TestPublishCloseAfterFatal verifies that Close returns the fatal error, and
// ErrNotDelivered too when records had to be purged.
//
// Close's error tells how the publisher's life ended, not only what happened
// during shutdown: a fatal error recorded at any point is joined into it. So a
// service that only checks Close's result at shutdown still learns that its
// records stopped going out.
func TestPublishCloseAfterFatal(t *testing.T) {
	// Failed, with nothing pending: Close purges nothing, and returns the
	// fatal error alone.
	publisher, fake := helpers.NewFakePublisher(t)
	fake.Emit(helpers.FatalClientError())
	helpers.WaitForFatal(t, publisher)
	err := publisher.Close(context.Background())
	require.ErrorIs(t, err, publish.ErrFatal)
	require.NotErrorIs(t, err, publish.ErrNotDelivered)

	// Failed, with records still pending: Close purges them, and returns both.
	publisher, fake = helpers.NewFakePublisher(t)
	helpers.SendInvoices(t, publisher.Bind(helpers.PublishInvoiceTopic()), 2)
	fake.Emit(helpers.FatalClientError())
	helpers.WaitForFatal(t, publisher)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = publisher.Close(ctx)
	require.ErrorIs(t, err, publish.ErrFatal)
	require.ErrorIs(t, err, publish.ErrNotDelivered)
}
