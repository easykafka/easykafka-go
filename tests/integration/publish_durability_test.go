package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishSurfacesBrokerRejection is the durability regression test: a
// record the broker refuses must fail the Publish call, reach the callback
// once, and be logged. A fire-and-forget producer, with no delivery channel,
// reports this write as a success.
//
// The topic's max.message.bytes=1 lets the record past librdkafka's own size
// check, which would fail Send at once, so the broker refuses it and the
// refusal arrives on the delivery report: the path under test.
func TestPublishSurfacesBrokerRejection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)
	topicName := helpers.UniqueTopicName(t, "publish-rejected")
	cluster.CreateTopicRejectingEverything(ctx, t, topicName)

	recorder := &helpers.PublishDeliveryErrorRecorder{}
	logs := &helpers.SyncBuffer{}
	publisher := helpers.NewPublisher(t, cluster.Brokers,
		publish.WithDeliveryErrorFunc(recorder.CallbackFunc),
		publish.WithLogger(zerolog.New(logs)),
	)
	writer := publisher.Bind(helpers.PublishInvoiceTopic(topicName))

	err := writer.Publish(ctx, "player-1", helpers.NewPublishInvoice("INV-1"),
		publish.Header{Key: "trace-id", Value: []byte("abc")})

	var deliveryError *publish.DeliveryError
	require.ErrorAs(t, err, &deliveryError, "a refused record must fail Publish")
	assert.Equal(t, "Broker: Message size too large", deliveryError.Code)
	assert.Equal(t, topicName, deliveryError.Topic)
	assert.Equal(t, []byte("player-1"), deliveryError.Key)
	assert.NotEmpty(t, deliveryError.Value)
	assert.Equal(t, []publish.Header{
		{Key: "__TypeId__", Value: []byte("Invoice")},
		{Key: "trace-id", Value: []byte("abc")},
	}, deliveryError.Headers)
	for _, sentinel := range []error{publish.ErrDeliveryTimeout, publish.ErrNotDelivered, publish.ErrFatal} {
		require.NotErrorIs(t, err, sentinel, "a broker refusal is not %v", sentinel)
	}

	// The callback runs before the delivery resolves, so it has run by now.
	failures := recorder.Errors()
	require.Len(t, failures, 1, "the callback must fire once for the refused record")
	assert.Equal(t, *deliveryError, failures[0])

	assert.Equal(t, 1, strings.Count(logs.String(), "EK_PUBLISH_DELIVERY_FAILED"),
		"the refused record must be logged once")
}
