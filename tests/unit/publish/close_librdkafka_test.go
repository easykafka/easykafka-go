package publish_test

import (
	"context"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/unit/publish/helpers"
	"github.com/easykafka/easykafka-go/tests/unit/sharedhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishClosePurgesAgainstLibrdkafka verifies, with real librdkafka and
// no broker, that Close reports every record it cannot deliver: each reaches
// the callback as "Local: Purged in queue", and each delivery resolves.
//
// This is the test that fails against a plain Flush and Close, and against a
// Close without the flush after the purge: librdkafka then drops the records
// without a report, and the callback never hears of them.
func TestPublishClosePurgesAgainstLibrdkafka(t *testing.T) {
	recorder := &sharedhelpers.DeliveryErrorRecorder{}
	publisher, err := publish.New(
		publish.WithBrokers(sharedhelpers.PublishBroker),
		publish.WithDeliveryErrorFunc(recorder.CallbackFunc),
	)
	require.NoError(t, err)
	deliveries := helpers.SendInvoices(t, publisher.Bind(helpers.PublishInvoiceTopic()), 5)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = publisher.Close(ctx)
	require.ErrorIs(t, err, publish.ErrNotDelivered)
	// "record(s)" comes from the ErrNotDelivered error Publisher.Close builds
	// when it purges: "…: N record(s)", N being what drain left unreported.
	// Only the wording is checked, not N = 5: N comes from librdkafka's Len,
	// which can also count a "connection refused" event here, there being no
	// broker. TestPublishClosePurgesPendingRecords checks the exact N.
	assert.Contains(t, err.Error(), "record(s)")

	failures := recorder.Errors()
	require.Len(t, failures, 5)
	for _, failure := range failures {
		assert.Equal(t, "Local: Purged in queue", failure.Code)
	}
	for _, delivery := range deliveries {
		_, err := helpers.WaitForDelivery(t, delivery)
		require.ErrorIs(t, err, publish.ErrNotDelivered)
	}
}

// TestPublishCloseRightAfterNew verifies, under -race, that closing a
// publisher straight after creating it neither races nor panics: the report
// goroutine may not have started reading yet.
func TestPublishCloseRightAfterNew(t *testing.T) {
	for range 20 {
		publisher, err := publish.New(publish.WithBrokers(sharedhelpers.PublishBroker))
		require.NoError(t, err)
		require.NoError(t, publisher.Close(context.Background()))
	}
}
