package integration

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// outageTrafficRate is the steady load the consumer outage tests publish, in
// records per second.
const outageTrafficRate = 200

// outageTraffic is a consumer reading a topic while a steady load is published
// into it, as startTraffic starts them.
type outageTraffic struct {
	topicName    string
	recorder     *helpers.ConsumptionRecorder
	load         *helpers.PublishLoad
	stopConsumer func() error
}

// startTraffic creates a topic led by each broker in turn, starts a consumer
// on it and a steady load of records into it.
func startTraffic(t *testing.T, cluster *helpers.ThreeBrokerCluster) outageTraffic {
	t.Helper()

	topicName := helpers.UniqueTopicName(t, "consumer-outage")
	cluster.CreateTopicLedByEachBroker(t, topicName)
	require.Equal(t, map[int32]int32{0: 1, 1: 2, 2: 3}, cluster.PartitionLeaders(t, topicName))

	recorder := helpers.NewConsumptionRecorder()
	consumer, err := easykafka.New(
		easykafka.WithTopic(topicName),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("consumer-outage-%d", time.Now().UnixNano())),
		easykafka.WithHandler(recorder.Handler),
	)
	require.NoError(t, err)
	stopConsumer := helpers.RunUntil(context.Background(), t, consumer)

	publisher := helpers.NewPublisher(t, cluster.Brokers)
	writer := publisher.Bind(publish.Topic[string, []byte]{
		Name:        topicName,
		EncodeKey:   publish.StringKey,
		EncodeValue: publish.RawValue,
	})
	// The value is the record's sequence number, which the recorder reads
	// back. The 50 keys spread the records over the three partitions.
	sendRecord := func(sequence int) (*publish.Delivery, error) {
		key := "player-" + strconv.Itoa(sequence%50) // player-0 … player-49, then again
		return writer.Send(key, []byte(strconv.Itoa(sequence)))
	}

	return outageTraffic{
		topicName:    topicName,
		recorder:     recorder,
		load:         helpers.StartPublishLoad(outageTrafficRate, sendRecord),
		stopConsumer: stopConsumer,
	}
}

// stop stops the load, waits until the consumer has read every record the
// load got acknowledged, stops the consumer, logs each partition's longest
// pause, and returns those pauses.
//
// outageStarted is when the test stopped or killed the first broker. It is
// used only in the log, to say how long after it each pause ended, e.g.
// "ended 13.1s after the outage began".
func (traffic outageTraffic) stop(t *testing.T, outageStarted time.Time) map[int32]helpers.Gap {
	t.Helper()

	result := traffic.load.Stop(t, 45*time.Second)
	result.Log(t)
	require.Empty(t, result.Failures, "the publisher must not lose a record while one broker is down")

	traffic.recorder.WaitForSequences(t, result.Sent, time.Minute)
	require.NoError(t, traffic.stopConsumer())
	t.Logf("records consumed more than once: %d", traffic.recorder.Redelivered())

	gaps := traffic.recorder.LongestGaps()
	for partition := range int32(3) {
		gap := gaps[partition]
		t.Logf("partition %d: longest pause %s, ended %s after the outage began",
			partition, gap.Length.Round(time.Millisecond), gap.EndedAt.Sub(outageStarted).Round(time.Millisecond))
	}
	return gaps
}

// TestConsumerThroughBrokerOutages shows what a consumer sees while one of
// three brokers is down, on a topic whose three partitions are each led by a
// different broker, replication factor 3 and min.insync.replicas=2. A
// publisher writes at a steady rate throughout; the consumer reads, and
// records when each partition delivered a message.
//
// Not run in parallel with the rest of the suite: the publisher's outage
// test starts a three-broker cluster too, and two at once is a lot of
// memory for one Docker host.
func TestConsumerThroughBrokerOutages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	cluster := helpers.NewThreeBrokerCluster(t)

	// A rolling restart stops each broker gracefully: it hands the leadership
	// of its partition to another in-sync replica before it goes, so the
	// consumer keeps reading every partition, from its new leader.
	t.Run("ConsumingThroughRollingRestart", func(t *testing.T) {
		t.Cleanup(func() { cluster.Heal(t) })

		traffic := startTraffic(t, cluster)
		time.Sleep(3 * time.Second)

		outageStarted := time.Now()
		for index := range 3 {
			cluster.StopBroker(t, index)
			leaders := cluster.PartitionLeaders(t, traffic.topicName)
			t.Logf("leaders with broker %d down: %v", index+1, leaders)
			// Broker index+1 led partition index when the topic was created;
			// a graceful stop moves that leadership to a live broker at once.
			for partition, leader := range leaders {
				require.NotEqual(t, int32(index+1), leader,
					"partition %d still led by the stopped broker %d", partition, index+1)
				require.NotEqual(t, int32(-1), leader, "partition %d has no leader", partition)
			}
			time.Sleep(5 * time.Second)
			cluster.StartBroker(t, index)
			cluster.WaitForFullISR(t, traffic.topicName, time.Minute)
		}
		time.Sleep(3 * time.Second)

		gaps := traffic.stop(t, outageStarted)
		require.Len(t, gaps, 3, "every partition must have delivered messages")
		// Measured at about 1.5 s at most: a leader move, not an outage.
		for partition, gap := range gaps {
			assert.Less(t, gap.Length, 5*time.Second, "partition %d paused too long", partition)
		}
	})

	// A crashed broker hands nothing over. Until the controller notices it is
	// gone, its partition has no live leader, and it is still counted among
	// every partition's in-sync replicas, so no partition can mark new records
	// as fully replicated, and a consumer only reads records that are.
	t.Run("ConsumingThroughBrokerCrash", func(t *testing.T) {
		t.Cleanup(func() { cluster.Heal(t) })

		traffic := startTraffic(t, cluster)
		time.Sleep(3 * time.Second)

		outageStarted := time.Now()
		cluster.KillBroker(t, 0) // broker 1, leader of partition 0
		time.Sleep(3 * time.Second)
		t.Logf("leaders 3 s after the crash: %v", cluster.PartitionLeaders(t, traffic.topicName))
		time.Sleep(17 * time.Second)
		t.Logf("leaders 20 s after the crash: %v", cluster.PartitionLeaders(t, traffic.topicName))
		cluster.StartBroker(t, 0)
		cluster.WaitForFullISR(t, traffic.topicName, time.Minute)
		time.Sleep(2 * time.Second)

		gaps := traffic.stop(t, outageStarted)
		require.Len(t, gaps, 3, "every partition must have delivered messages")
		// The timeline, in seconds since the kill (outageStarted):
		//
		//	        -3    0     3              ~13            20
		//	test:   load  kill  log leaders                   broker 1 started again
		//	        │     │     {0:1,1:2,2:3}   │              │
		//	        │     │◄─ controller notices the crash ─►│ │
		//	        │     │   (~10 s), drops broker 1 from    │ │
		//	        │     │   the ISRs, elects broker 2 for 0 │ │
		//	p0:     ■■■■■■│░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░│■■■■■■■■■■■■■■■■■
		//	p1:     ■■■■■■│░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░│■■■■■■■■■■■■■■■■■■■
		//	p2:     ■■■■■■│░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░│■■■■■■■■■■■■■■■■■■
		//	              │◄──────── gap.Length ────────────►│
		//	              outageStarted               gap.EndedAt
		//
		//	■ messages arriving   ░ pause
		//
		// So gap.Length > 5 s shows the crash stalled every partition, and
		// gap.EndedAt before 20 s shows they resumed without broker 1 back.
		//
		// Measured at about 12 to 14 s on every partition, not only the dead
		// broker's: the controller needs about 10 s to notice it, then a new
		// leader is elected and the in-sync replicas shrink. The pause then
		// ends on its own, long before the broker is back at 20 s.
		for partition, gap := range gaps {
			assert.Greater(t, gap.Length, 5*time.Second,
				"partition %d did not pause: the crash must stall every partition", partition)
			assert.Less(t, gap.EndedAt.Sub(outageStarted), 20*time.Second,
				"partition %d must resume before the broker is back", partition)
		}
	})
}
