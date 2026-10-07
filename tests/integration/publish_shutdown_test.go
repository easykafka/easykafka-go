package integration

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishCloseDeliversQueuedRecords verifies that Close, given time,
// delivers what is still queued rather than purging it: records sent and not
// waited on are all acknowledged, Close returns nil, and every one is readable.
func TestPublishCloseDeliversQueuedRecords(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	const records = 1_000

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)
	topicName := helpers.UniqueTopicName(t, "publish-close")
	cluster.CreateTopic(ctx, t, topicName, 3)

	recorder := &helpers.PublishDeliveryErrorRecorder{}
	publisher := helpers.NewPublisher(t, cluster.Brokers, publish.WithDeliveryErrorFunc(recorder.CallbackFunc))
	writer := publisher.Bind(helpers.PublishInvoiceTopic(topicName))

	deliveries := make([]*publish.Delivery, records)
	for index := range records {
		delivery, err := writer.Send("player-"+strconv.Itoa(index%10), helpers.NewPublishInvoice(strconv.Itoa(index)))
		require.NoError(t, err)
		deliveries[index] = delivery
	}

	closeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	require.NoError(t, publisher.Close(closeCtx))

	// Close has returned, so every delivery is resolved: an immediately ended
	// context cannot win over an outcome already known.
	ended, cancelEnded := context.WithCancel(ctx)
	cancelEnded()
	require.NoError(t, publish.WaitAll(ended, deliveries...))
	assert.Empty(t, recorder.Errors())

	messages := cluster.ConsumeMessages(ctx, t, topicName, fmt.Sprintf("close-%d", time.Now().UnixNano()),
		records, 60*time.Second)
	assert.Len(t, messages, records)
}

// TestPublishClosePurgesWhenBrokerIsDown verifies Close against a broker that
// has gone away with records queued: Close stops waiting when its context
// ends, purges, and reports each record, through its delivery and the
// callback, as not delivered. None is dropped without a report.
func TestPublishClosePurgesWhenBrokerIsDown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	const records = 100

	ctx := context.Background()
	// Dedicated, because this test stops the broker.
	cluster := helpers.DedicatedCluster(t)
	topicName := helpers.UniqueTopicName(t, "publish-purge")
	cluster.CreateTopic(ctx, t, topicName, 1)

	recorder := &helpers.PublishDeliveryErrorRecorder{}
	publisher := helpers.NewPublisher(t, cluster.Brokers, publish.WithDeliveryErrorFunc(recorder.CallbackFunc))
	writer := publisher.Bind(helpers.PublishInvoiceTopic(topicName))

	// One record published, and acknowledged, while the broker is still up.
	// librdkafka connects in the background after New, so without this the
	// broker could be stopped before the publisher had connected, fetched the
	// topic's metadata (its partition and leader) or got its producer id. The
	// records below would then wait with no partition assigned, which is the
	// "no broker at all" case TestPublishClosePurgesAgainstLibrdkafka already
	// covers in the unit tests. A successful Publish proves that startup is
	// done, so the stop below hits a publisher that was writing normally, as a
	// service sees an outage at shutdown.
	require.NoError(t, writer.Publish(ctx, "player-1", helpers.NewPublishInvoice("before")))

	cluster.StopBroker(ctx, t)

	deliveries := make([]*publish.Delivery, records)
	for index := range records {
		delivery, err := writer.Send("player-1", helpers.NewPublishInvoice(strconv.Itoa(index)))
		require.NoError(t, err)
		deliveries[index] = delivery
	}

	// Well inside the 30 s delivery timeout, so no record times out first.
	closeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	started := time.Now()
	err := publisher.Close(closeCtx)
	t.Logf("Close returned after %s: %v", time.Since(started).Round(time.Millisecond), err)
	require.ErrorIs(t, err, publish.ErrNotDelivered)

	failures := recorder.Errors()
	require.Len(t, failures, records, "every queued record must reach the callback once")
	for _, failure := range failures {
		require.ErrorIs(t, failure.Err, publish.ErrNotDelivered)
		assert.Contains(t, []string{"Local: Purged in queue", "Local: Purged in flight"}, failure.Code)
	}
	ended, cancelEnded := context.WithCancel(ctx)
	cancelEnded()
	for _, delivery := range deliveries {
		_, err := delivery.Wait(ended)
		require.ErrorIs(t, err, publish.ErrNotDelivered)
	}
}
