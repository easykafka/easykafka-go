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

// TestPublishAcksAndOutages verifies, on a three-broker cluster configured like
// production (replication factor 3, min.insync.replicas=2), that the default
// publisher (acks=all, idempotence on) keeps publishing without a failure
// while any one broker is down, whether stopped for maintenance or crashed,
// and that two brokers down fails within the delivery timeout rather than
// hanging.
//
// The subtests share one cluster, since starting it costs about half a
// minute, and run one after another. Each heals the cluster when it ends, so
// the next finds all three brokers up.
func TestPublishAcksAndOutages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	// rate is the steady load, in records per second, the probes measured at.
	const rate = 200
	// loadSettleTimeout bounds the wait for the last records' reports once the
	// load stops: the 30 s delivery timeout, with margin.
	const loadSettleTimeout = 45 * time.Second

	cluster := sharedhelpers.NewThreeBrokerCluster(t)

	// The requirement: a rolling update, one broker restarted at a time, costs
	// latency but not a single failed write.
	t.Run("PublishingThroughRollingRestart", func(t *testing.T) {
		t.Cleanup(func() { cluster.Heal(t) })

		topicName := sharedhelpers.UniqueTopicName(t, "publish-rolling")
		cluster.CreateTopic(t, topicName, 6)
		publisher := sharedhelpers.NewPublisher(t, cluster.Brokers)
		writer := publisher.Bind(helpers.PublishInvoiceTopic(topicName))

		// The load calls this once per record, with its sequence number. The
		// records cycle through 50 keys, so they spread over the partitions.
		sendRecord := func(sequence int) (*publish.Delivery, error) {
			key := "player-" + strconv.Itoa(sequence%50) // player-0 … player-49, then again
			invoice := helpers.NewPublishInvoice(strconv.Itoa(sequence))
			return writer.Send(key, invoice)
		}
		load := sharedhelpers.StartPublishLoad(rate, sendRecord)
		time.Sleep(3 * time.Second)
		cluster.RollingRestart(t, topicName, 5*time.Second)
		time.Sleep(3 * time.Second)

		result := load.Stop(t, loadSettleTimeout)
		result.Log(t)
		assert.Empty(t, result.Failures, "no write may fail during a rolling restart")
		assert.Positive(t, result.Sent)
	})

	// A crashed broker stays in the in-sync replicas until the controller
	// notices, so acks=all writes to the partitions it followed stall for
	// seconds. The default 30 s delivery timeout must cover that stall.
	t.Run("PublishingThroughBrokerCrash", func(t *testing.T) {
		t.Cleanup(func() { cluster.Heal(t) })

		topicName := sharedhelpers.UniqueTopicName(t, "publish-crash")
		cluster.CreateTopic(t, topicName, 6)
		publisher := sharedhelpers.NewPublisher(t, cluster.Brokers)
		writer := publisher.Bind(helpers.PublishInvoiceTopic(topicName))

		// The load calls this once per record, with its sequence number. The
		// records cycle through 50 keys, so they spread over the partitions.
		sendRecord := func(sequence int) (*publish.Delivery, error) {
			key := "player-" + strconv.Itoa(sequence%50) // player-0 … player-49, then again
			invoice := helpers.NewPublishInvoice(strconv.Itoa(sequence))
			return writer.Send(key, invoice)
		}
		load := sharedhelpers.StartPublishLoad(rate, sendRecord)
		time.Sleep(3 * time.Second)
		cluster.KillBroker(t, 1)
		// Long enough for the stall to end and the cluster to carry on without
		// the broker.
		time.Sleep(20 * time.Second)
		cluster.StartBroker(t, 1)
		cluster.WaitForFullISR(t, topicName, time.Minute)
		time.Sleep(2 * time.Second)

		result := load.Stop(t, loadSettleTimeout)
		result.Log(t)
		t.Logf("longest stall: %s", result.Max.Round(time.Millisecond))
		assert.Empty(t, result.Failures, "no write may fail while one broker is down")
		assert.Positive(t, result.Sent)
	})

	// Idempotence keeps a key's records in order, and without duplicates,
	// through the internal retries every leader move causes.
	t.Run("OrderKeptThroughRollingRestart", func(t *testing.T) {
		t.Cleanup(func() { cluster.Heal(t) })

		topicName := sharedhelpers.UniqueTopicName(t, "publish-order")
		cluster.CreateTopic(t, topicName, 3)
		publisher := sharedhelpers.NewPublisher(t, cluster.Brokers)
		writer := publisher.Bind(publish.Topic[string, []byte]{
			Name:        topicName,
			EncodeKey:   publish.StringKey,
			EncodeValue: publish.RawValue,
		})

		// Faster than the other loads, so several batches of the one key are
		// in flight whenever a leader moves.
		load := sharedhelpers.StartPublishLoad(4*rate, func(sequence int) (*publish.Delivery, error) {
			return writer.Send("ordered", []byte(strconv.Itoa(sequence)))
		})
		time.Sleep(2 * time.Second)
		cluster.RollingRestart(t, topicName, 3*time.Second)
		time.Sleep(2 * time.Second)

		result := load.Stop(t, loadSettleTimeout)
		result.Log(t)
		require.Empty(t, result.Failures, "a failed write would leave a gap, and the order check would mean nothing")

		messages := cluster.ConsumeMessages(t, topicName, result.Sent, time.Minute)
		require.Len(t, messages, result.Sent, "every acknowledged record must be read back exactly once")
		for index, message := range messages {
			if !assert.Equal(t, strconv.Itoa(index), string(message.Value), "record at position %d", index) {
				break // the first one out of place says enough
			}
		}
	})

	// Out of scope of the requirement, but pinned: with two brokers down,
	// acks=all cannot be met, and a write fails once its delivery timeout has
	// passed instead of hanging. Idempotent reports can come up to about 10 s
	// after the outage starts, whatever the timeout.
	t.Run("TwoBrokersDownFailsAfterTimeout", func(t *testing.T) {
		t.Cleanup(func() { cluster.Heal(t) })

		const deliveryTimeout = 5 * time.Second

		ctx := context.Background()
		topicName := sharedhelpers.UniqueTopicName(t, "publish-two-down")
		cluster.CreateTopic(t, topicName, 3)
		publisher := sharedhelpers.NewPublisher(t, cluster.Brokers, publish.WithDeliveryTimeout(deliveryTimeout))
		writer := publisher.Bind(helpers.PublishInvoiceTopic(topicName))
		require.NoError(t, writer.Publish(ctx, "player-1", helpers.NewPublishInvoice("before")))

		cluster.StopBroker(t, 0)
		cluster.StopBroker(t, 1)

		started := time.Now()
		err := writer.Publish(ctx, "player-1", helpers.NewPublishInvoice("after"))
		elapsed := time.Since(started)
		t.Logf("Publish returned after %s: %v", elapsed.Round(time.Millisecond), err)

		var deliveryError *publish.DeliveryError
		require.ErrorAs(t, err, &deliveryError)
		require.ErrorIs(t, err, publish.ErrDeliveryTimeout)
		assert.GreaterOrEqual(t, elapsed, deliveryTimeout)
		assert.LessOrEqual(t, elapsed, deliveryTimeout+10*time.Second)
	})
}
