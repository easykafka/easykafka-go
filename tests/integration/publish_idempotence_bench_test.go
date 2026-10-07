package integration

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
)

// BenchmarkIdempotenceOverhead measures what idempotence, on by default, costs
// over WithoutIdempotence (acks=all either way, so only idempotence differs),
// on a three-broker cluster configured like production. Two shapes: many
// goroutines calling Send concurrently, and one goroutine calling Publish in a
// loop. Each reports records/s and the p99 delivery latency.
//
// Not part of the test run. Run it with:
//
//	go test -run '^$' -bench BenchmarkIdempotenceOverhead -benchtime 5000x -count 3 -timeout 20m ./tests/integration/
//
// -run '^$' skips the package's tests. The run takes about four minutes,
// mostly the cluster start and the warm-up, so -timeout is needed where an IDE
// sets a short one (VS Code's go.testTimeout is 30s by default). -count 3
// repeats each measurement, because one run on a laptop can be off by a third.
//
// A local cluster in Docker, so the numbers are for comparing the two modes,
// not for what production achieves.
//
// A run on an Apple M5 Pro (2026-10-07):
//
//	idempotent/concurrent-send-15            5000   3704 ns/op   17.36 p99-ms   272672 records/s
//	idempotent/concurrent-send-15            5000   3217 ns/op   15.05 p99-ms   314404 records/s
//	idempotent/concurrent-send-15            5000   2995 ns/op   14.01 p99-ms   337812 records/s
//	idempotent/sequential-publish-15         5000   6868194 ns/op 8.801 p99-ms    145.6 records/s
//	idempotent/sequential-publish-15         5000   6880010 ns/op 8.694 p99-ms    145.4 records/s
//	idempotent/sequential-publish-15         5000   7156619 ns/op 8.911 p99-ms    139.7 records/s
//	without-idempotence/concurrent-send-15   5000   4556 ns/op   19.29 p99-ms   221706 records/s
//	without-idempotence/concurrent-send-15   5000   3161 ns/op   14.43 p99-ms   321686 records/s
//	without-idempotence/concurrent-send-15   5000   3050 ns/op   13.95 p99-ms   333077 records/s
//	without-idempotence/sequential-publish-15 5000  6967059 ns/op 8.473 p99-ms    143.5 records/s
//	without-idempotence/sequential-publish-15 5000  7036948 ns/op 8.592 p99-ms    142.1 records/s
//	without-idempotence/sequential-publish-15 5000  7067685 ns/op 8.449 p99-ms    141.5 records/s
//
// How to read it: 5000 is the records per run; ns/op is the run's wall time
// divided by them, so for concurrent sends it measures throughput, not one
// record's latency; p99-ms is the time within which 99% of the records were
// acknowledged; -15 is the CPU count Go used.
//
// What it shows: idempotence costs nothing measurable.
//
//   - Concurrent Send: 273k to 338k records/s idempotent, 222k to 333k without;
//     the ranges overlap, and once settled (the second and third runs) the two
//     are within a few percent. The first run of each mode is the slowest,
//     some warm-up left over, so compare the settled runs.
//   - Sequential Publish: about 140 to 146 records/s, 7 ms a record, either
//     way. That is mostly librdkafka's linger.ms (5 ms by default, the wait
//     for more records to batch) plus the round trip, not idempotence. The
//     idempotent p99 is about 0.3 ms higher in every run: consistent, but
//     about 3%, too small to read anything into from a laptop run. A service
//     that needs a lower Publish latency lowers linger.ms with
//     WithKafkaConfig, giving up some batching.
func BenchmarkIdempotenceOverhead(b *testing.B) {
	// Started once, here, for every sub-benchmark: Go runs this outer function
	// once, and each b.Run below as often as it needs. Six partitions, so the
	// records spread over all three brokers' leaders.
	cluster := helpers.NewThreeBrokerCluster(b)
	topicName := helpers.UniqueTopicName(b, "publish-bench")
	cluster.CreateTopic(b, topicName, 6)

	// The two modes compared. Both keep acks=all, so idempotence is the only
	// difference. writer is filled in below, one publisher per mode.
	modes := []struct {
		name    string
		options []publish.Option
		writer  *publish.Writer[string, helpers.PublishInvoice]
	}{
		{name: "idempotent"},
		{name: "without-idempotence", options: []publish.Option{publish.WithoutIdempotence()}},
	}
	// Every mode's publisher is created and warmed up before anything is
	// measured: the first bursts against a fresh cluster, and through a fresh
	// publisher, run several times slower, and whichever mode came first would
	// carry that cost.
	for index := range modes {
		publisher := helpers.NewPublisher(b, cluster.Brokers, modes[index].options...)
		modes[index].writer = publisher.Bind(helpers.PublishInvoiceTopic(topicName))
	}
	// Two rounds, alternating the modes, so neither gets all of the warm-up.
	for range 2 {
		for _, mode := range modes {
			helpers.WarmUpWriter(b, mode.writer, 50_000)
		}
	}

	// One value for every record: encoding it is the same work in both modes,
	// so it is not what is being compared.
	invoice := helpers.NewPublishInvoice("INV-1")
	for _, mode := range modes {
		writer := mode.writer

		// Throughput: a busy service sending from many goroutines and looking
		// at the outcomes later. If idempotence slowed librdkafka's
		// pipelining (it allows at most 5 requests in flight per broker
		// connection, against practically no limit without it), records/s
		// would drop here.
		//
		// b.N is the number of records to send in this run, set by Go or by
		// -benchtime. In our case b.N = 5000, from -benchtime 5000x in the
		// command above (Go also makes one throwaway run with b.N = 1 first).
		b.Run(mode.name+"/concurrent-send", func(b *testing.B) {
			const senders = 32
			ctx := context.Background()
			// One slot per record, written by that record's waiter only, so it
			// needs no lock.
			latencies := make([]time.Duration, b.N)
			// senderGroup waits for the goroutines sending; pending for the
			// goroutines waiting on each record's outcome.
			var senderGroup, pending sync.WaitGroup
			b.ResetTimer() // the setup above is not part of the measurement
			started := time.Now()
			for sender := range senders {
				senderGroup.Go(func() {
					// Sender s takes records s, s+32, s+64, ..., so the 32
					// senders together send each of 0..b.N-1 exactly once.
					// 1000 distinct keys spread the records over the partitions.
					for index := sender; index < b.N; index += senders {
						sentAt := time.Now()
						delivery, err := writer.Send("player-"+strconv.Itoa(index%1000), invoice)
						if err != nil {
							b.Error(err)
							return
						}
						// One waiter per record, so each latency ends at its own
						// report rather than at an earlier record's.
						pending.Go(func() {
							if _, err := delivery.Wait(ctx); err != nil {
								b.Error(err)
							}
							latencies[index] = time.Since(sentAt)
						})
					}
				})
			}
			// Done once every record is sent and acknowledged, so the elapsed
			// time covers delivery, not just handing records to librdkafka.
			senderGroup.Wait()
			pending.Wait()
			helpers.ReportPublishThroughput(b, time.Since(started), latencies)
		})

		// Latency: a caller that waits for each record before going on, such
		// as a REST handler publishing before it answers, or a consumer
		// handler writing a retry record. Publish is Send plus Wait, so only
		// one record is ever in flight, and any per-request cost of
		// idempotence would show in the p99.
		b.Run(mode.name+"/sequential-publish", func(b *testing.B) {
			ctx := context.Background()
			latencies := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			started := time.Now()
			for index := range b.N {
				sentAt := time.Now()
				if err := writer.Publish(ctx, "player-"+strconv.Itoa(index%1000), invoice); err != nil {
					b.Fatal(err)
				}
				latencies = append(latencies, time.Since(sentAt)) // send to acknowledgement
			}
			helpers.ReportPublishThroughput(b, time.Since(started), latencies)
		})
	}
}
