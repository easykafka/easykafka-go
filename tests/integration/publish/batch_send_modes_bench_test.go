package publish_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/integration/publish/helpers"
	"github.com/easykafka/easykafka-go/tests/integration/sharedhelpers"
)

// BenchmarkBatchSendModes measures what a caller pays to write a batch of
// records from one goroutine, in a loop, as a bulk import does, in the three
// ways the publisher allows:
//
//   - send-no-wait: Send each record and never wait for its delivery report.
//     The loop ends once every record is handed to librdkafka; a failure
//     reaches only the WithDeliveryErrorFunc callback.
//   - publish: Publish each record, which waits for its acknowledgement before
//     the next one is sent.
//   - send-wait-all: Send every record, then WaitAll on all the deliveries. The
//     loop ends once every record is acknowledged, as with publish, but the
//     records travel together.
//
// The batch is b.N records to one topic of two partitions, on a three-broker
// cluster configured like production (replication factor 3,
// min.insync.replicas=2, acks=all, idempotence on).
//
// Not part of the test run. Run it with:
//
//	go test -run '^$' -bench BenchmarkBatchSendModes -benchtime 20000x -count 3 -timeout 40m ./tests/integration/publish/
//
// 20000x sends a batch of 20 000 records per run. publish is the slow one, at
// about 7 ms a record (see BenchmarkIdempotenceOverhead): about two and a half
// minutes per run, which is why -timeout is set. -count 3 repeats each
// measurement, as one run on a laptop can be off by a third.
//
// A local cluster in Docker, so the numbers are for comparing the three modes,
// not for what production achieves.
//
// What each metric means: ms/record is the loop's wall time divided by the
// records, what the caller waits per record; records/s is the same, inverted.
// For send-no-wait the loop's time ends before the records are acknowledged;
// they are waited for afterwards, outside the measurement, so the next run
// starts with an empty queue.
//
// A run on an Apple M5 Pro (2026-10-15):
//
//	send-no-wait-15    20000      1020 ns/op   0.001020 ms/record   980088 records/s
//	send-no-wait-15    20000      1165 ns/op   0.001165 ms/record   858050 records/s
//	send-no-wait-15    20000      1177 ns/op   0.001177 ms/record   849586 records/s
//	publish-15         20000   7244927 ns/op      7.245 ms/record    138.0 records/s
//	publish-15         20000   7266912 ns/op      7.267 ms/record    137.6 records/s
//	publish-15         20000   7265554 ns/op      7.266 ms/record    137.6 records/s
//	send-wait-all-15   20000      2054 ns/op   0.002053 ms/record   486983 records/s
//	send-wait-all-15   20000      1644 ns/op   0.001644 ms/record   608408 records/s
//	send-wait-all-15   20000      1503 ns/op   0.001503 ms/record   665176 records/s
//
// How to read it: 20000 is the records per run; -15 is the CPU count Go used.
// A batch of 20 000 records took about 22 ms with send-no-wait, 145 s with
// publish and 35 ms with send-wait-all.
//
// What it shows: waiting per record is what costs, not waiting.
//
//   - publish: about 7.3 ms a record in every run, so a 20 000-record batch
//     takes two and a half minutes. Each record waits librdkafka's linger.ms
//     (5 ms by default) alone, plus the acks=all round trip, before the next
//     one is sent.
//   - send-wait-all: every record is acknowledged, as with publish, yet the
//     batch takes about 35 ms, some 4 000 times faster: the records travel
//     together, so the batch costs about one round of acknowledgements.
//   - send-no-wait: about 22 ms, so confirming the whole batch with
//     send-wait-all costs about 13 ms more than not confirming it at all.
//
// A caller writing a batch from one goroutine and needing every record
// confirmed should Send them all and wait once (WaitAll), never Publish them
// one by one.
func BenchmarkBatchSendModes(b *testing.B) {
	// Started once, here, for every sub-benchmark: Go runs this outer function
	// once, and each b.Run below as often as it needs.
	cluster := sharedhelpers.NewThreeBrokerCluster(b)
	topicName := sharedhelpers.UniqueTopicName(b, "batch-send-modes")
	cluster.CreateTopic(b, topicName, 2)

	// One publisher for all three modes: they differ only in how the caller
	// uses the writer, not in the publisher's configuration.
	publisher := sharedhelpers.NewPublisher(b, cluster.Brokers)
	writer := publisher.Bind(helpers.PublishInvoiceTopic(topicName))

	// The first bursts against a fresh cluster, and through a fresh publisher,
	// run several times slower; whichever mode came first would carry that cost.
	for range 2 {
		helpers.WarmUpWriter(b, writer, 50_000)
	}

	// One value for every record: encoding it is the same work in every mode,
	// so it is not what is being compared. Every record has its own key, as an
	// import of distinct entities does, so the records spread over both partitions.
	invoice := helpers.NewPublishInvoice("INV-1")
	ctx := context.Background()

	b.Run("send-no-wait", func(b *testing.B) {
		deliveries := make([]*publish.Delivery, 0, b.N)
		b.ResetTimer()
		started := time.Now()
		for index := range b.N {
			delivery, err := writer.Send("record-"+strconv.Itoa(index), invoice)
			if err != nil {
				b.Fatal(err)
			}
			deliveries = append(deliveries, delivery) // kept only to drain below
		}
		elapsed := time.Since(started)

		// Not measured: the caller does not wait for these. Waiting here keeps
		// this run's records from slowing the next run down, and shows that
		// they were delivered.
		b.StopTimer()
		if err := publish.WaitAll(ctx, deliveries...); err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(elapsed.Microseconds())/1000/float64(b.N), "ms/record")
		b.ReportMetric(float64(b.N)/elapsed.Seconds(), "records/s")
	})

	b.Run("publish", func(b *testing.B) {
		b.ResetTimer()
		started := time.Now()
		for index := range b.N {
			if err := writer.Publish(ctx, "record-"+strconv.Itoa(index), invoice); err != nil {
				b.Fatal(err)
			}
		}
		elapsed := time.Since(started)
		b.ReportMetric(float64(elapsed.Microseconds())/1000/float64(b.N), "ms/record")
		b.ReportMetric(float64(b.N)/elapsed.Seconds(), "records/s")
	})

	b.Run("send-wait-all", func(b *testing.B) {
		deliveries := make([]*publish.Delivery, 0, b.N)
		b.ResetTimer()
		started := time.Now()
		for index := range b.N {
			delivery, err := writer.Send("record-"+strconv.Itoa(index), invoice)
			if err != nil {
				b.Fatal(err)
			}
			deliveries = append(deliveries, delivery)
		}
		// Measured: the loop ends once every record is acknowledged.
		if err := publish.WaitAll(ctx, deliveries...); err != nil {
			b.Fatal(err)
		}
		elapsed := time.Since(started)
		b.ReportMetric(float64(elapsed.Microseconds())/1000/float64(b.N), "ms/record")
		b.ReportMetric(float64(b.N)/elapsed.Seconds(), "records/s")
	})
}
