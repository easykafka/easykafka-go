package publish_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/integration/publish/helpers"
	"github.com/easykafka/easykafka-go/tests/integration/sharedhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishVolume sends 10 000 records without waiting, then waits for all
// of them: every delivery resolves as acknowledged, the callback never fires,
// and Close has nothing left to purge.
func TestPublishVolume(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	const records = 10_000

	ctx := context.Background()
	cluster := sharedhelpers.SharedCluster(t)
	topicName := sharedhelpers.UniqueTopicName(t, "publish-volume")
	cluster.CreateTopic(ctx, t, topicName, 6)

	recorder := &sharedhelpers.PublishDeliveryErrorRecorder{}
	publisher := sharedhelpers.NewPublisher(t, cluster.Brokers, publish.WithDeliveryErrorFunc(recorder.CallbackFunc))
	writer := publisher.Bind(helpers.PublishInvoiceTopic(topicName))

	started := time.Now()
	deliveries := make([]*publish.Delivery, records)
	for index := range records {
		delivery, err := writer.Send("player-"+strconv.Itoa(index%100), helpers.NewPublishInvoice(strconv.Itoa(index)))
		require.NoError(t, err)
		deliveries[index] = delivery
	}

	waitCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	require.NoError(t, publish.WaitAll(waitCtx, deliveries...))
	t.Logf("%d records sent and acknowledged in %s", records, time.Since(started).Round(time.Millisecond))

	for index, delivery := range deliveries {
		select {
		case <-delivery.Done():
		default:
			require.FailNow(t, "delivery not resolved", "record %d", index)
		}
	}
	assert.Empty(t, recorder.Errors(), "no record failed, so the callback must not have fired")

	closeCtx, cancelClose := context.WithTimeout(ctx, 10*time.Second)
	defer cancelClose()
	require.NoError(t, publisher.Close(closeCtx))
}
