package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// timeoutReportMargin is how late after its delivery timeout a record without
// idempotence may be reported. librdkafka checks for timed-out records
// periodically rather than at the deadline: up to about 1 s late on a laptop
// (probe T-0.1), and nearly 3 s on a loaded CI runner. The margin only has to
// tell "about at the timeout" from librdkafka's 300 s default, or never.
const timeoutReportMargin = 5 * time.Second

// TestPublishDeliveryTimeout verifies that WithDeliveryTimeout bounds a record
// no broker ever acknowledges: Publish fails with ErrDeliveryTimeout soon after
// the timeout, rather than after librdkafka's 300 s default or never. It also
// shows that Send does not wait for the broker: it returns at once although
// no broker is reachable.
//
// Without idempotence the report comes about when the timeout says. With it,
// the default, librdkafka can report up to about 10 s after an outage starts,
// whatever the timeout, so only that looser bound is checked there.
func TestPublishDeliveryTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	const deliveryTimeout = 2 * time.Second

	cases := []struct {
		name     string
		options  []publish.Option
		earliest time.Duration
		latest   time.Duration
	}{
		{
			name:     "without idempotence",
			options:  []publish.Option{publish.WithoutIdempotence()},
			earliest: deliveryTimeout,
			latest:   deliveryTimeout + timeoutReportMargin,
		},
		{
			name:     "default publisher",
			earliest: deliveryTimeout,
			latest:   12 * time.Second,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			recorder := &helpers.PublishDeliveryErrorRecorder{}
			publisher := helpers.NewPublisher(t, []string{helpers.UnreachableBroker},
				append(testCase.options,
					publish.WithDeliveryTimeout(deliveryTimeout),
					publish.WithDeliveryErrorFunc(recorder.CallbackFunc))...)
			writer := publisher.Bind(helpers.PublishInvoiceTopic("never-reached"))

			started := time.Now()
			delivery, err := writer.Send("player-1", helpers.NewPublishInvoice("INV-1"))
			require.NoError(t, err)
			assert.Less(t, time.Since(started), 100*time.Millisecond, "Send must not wait for a broker")

			_, err = delivery.Wait(context.Background())
			elapsed := time.Since(started)
			t.Logf("reported after %s", elapsed.Round(time.Millisecond))

			require.ErrorIs(t, err, publish.ErrDeliveryTimeout)
			var deliveryError *publish.DeliveryError
			require.ErrorAs(t, err, &deliveryError)
			assert.Equal(t, "Local: Message timed out", deliveryError.Code)
			assert.GreaterOrEqual(t, elapsed, testCase.earliest)
			assert.LessOrEqual(t, elapsed, testCase.latest)
			assert.Len(t, recorder.Errors(), 1)
		})
	}
}

// TestPublishDeliveryTimeoutAfterBrokerStops is the same check against a
// broker that was reachable and then stopped, the case in which an idempotent
// producer's reports can come late: it has a producer id and in-flight state
// to settle first. Both publishers write once before the stop, so each has
// connected and knows the topic.
func TestPublishDeliveryTimeoutAfterBrokerStops(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	const deliveryTimeout = 2 * time.Second

	ctx := context.Background()
	// Dedicated, because this test stops the broker.
	cluster := helpers.DedicatedCluster(t)
	topicName := helpers.UniqueTopicName(t, "publish-timeout")
	cluster.CreateTopic(ctx, t, topicName, 1)

	withoutIdempotence := helpers.NewPublisher(t, cluster.Brokers,
		publish.WithoutIdempotence(), publish.WithDeliveryTimeout(deliveryTimeout))
	idempotent := helpers.NewPublisher(t, cluster.Brokers, publish.WithDeliveryTimeout(deliveryTimeout))
	writers := map[string]*publish.Writer[string, helpers.PublishInvoice]{
		"without idempotence": withoutIdempotence.Bind(helpers.PublishInvoiceTopic(topicName)),
		"default publisher":   idempotent.Bind(helpers.PublishInvoiceTopic(topicName)),
	}
	// One acknowledged record per publisher before the stop, so each has
	// connected, fetched the topic's metadata and got its producer id: the
	// state an idempotent producer's late report comes from. Without it, this
	// would be the never-reached case of the test above. See
	// TestPublishClosePurgesWhenBrokerIsDown for the full reasoning.
	for name, writer := range writers {
		require.NoError(t, writer.Publish(ctx, "player-1", helpers.NewPublishInvoice("INV-1")), name)
	}

	cluster.StopBroker(ctx, t)

	latest := map[string]time.Duration{
		"without idempotence": deliveryTimeout + timeoutReportMargin,
		"default publisher":   12 * time.Second,
	}
	started := time.Now()
	deliveries := make(map[string]*publish.Delivery, len(writers))
	for name, writer := range writers {
		delivery, err := writer.Send("player-1", helpers.NewPublishInvoice("INV-2"))
		require.NoError(t, err, name)
		deliveries[name] = delivery
	}
	// Waited on concurrently, so that each is timed from its own report rather
	// than from the later of the two.
	type outcome struct {
		err     error
		elapsed time.Duration
	}
	var mu sync.Mutex
	outcomes := make(map[string]outcome, len(deliveries))
	var wg sync.WaitGroup
	for name, delivery := range deliveries {
		wg.Go(func() {
			_, err := delivery.Wait(ctx)
			elapsed := time.Since(started)
			mu.Lock()
			defer mu.Unlock()
			outcomes[name] = outcome{err: err, elapsed: elapsed}
		})
	}
	wg.Wait()

	for name, outcome := range outcomes {
		t.Logf("%s: reported after %s", name, outcome.elapsed.Round(time.Millisecond))
		require.ErrorIs(t, outcome.err, publish.ErrDeliveryTimeout, name)
		assert.GreaterOrEqual(t, outcome.elapsed, deliveryTimeout, name)
		assert.LessOrEqual(t, outcome.elapsed, latest[name], name)
	}
}
