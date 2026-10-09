package publish_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/unit/publish/helpers"
	"github.com/easykafka/easykafka-go/tests/unit/sharedhelpers"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishCloseWithNothingPending verifies that Close returns nil and purges
// nothing when every record has been reported, or none was sent.
func TestPublishCloseWithNothingPending(t *testing.T) {
	logs := &sharedhelpers.SyncBuffer{}
	publisher, fake := helpers.NewFakePublisher(t, publish.WithLogger(zerolog.New(logs)))
	delivery := helpers.SendInvoices(t, publisher.Bind(helpers.PublishInvoiceTopic()), 1)[0]
	fake.Succeed(0, 0, 1)
	_, err := helpers.WaitForDelivery(t, delivery)
	require.NoError(t, err)

	require.NoError(t, publisher.Close(context.Background()))
	assert.Zero(t, fake.PurgeCalls())
	assert.NotContains(t, logs.String(), "EK_PUBLISH_RECORDS_PURGED")

	empty, _ := helpers.NewFakePublisher(t)
	require.NoError(t, empty.Close(context.Background()))
}

// TestPublishCloseWaitsForPendingRecords verifies that Close gives librdkafka
// the time its context allows: records reported meanwhile need no purge.
func TestPublishCloseWaitsForPendingRecords(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)

	// Two records enqueued, not yet reported: the fake now counts 2 pending
	// (its Len), as librdkafka would while they are on their way to the broker.
	deliveries := helpers.SendInvoices(t, publisher.Bind(helpers.PublishInvoiceTopic()), 2)

	// Plays the broker: 20 ms from now, both records are acknowledged. That is
	// after Close has started, so Close has to wait for them.
	go func() {
		time.Sleep(20 * time.Millisecond)
		fake.Succeed(0, 0, 1) // record 0 reported: pending count 1
		fake.Succeed(1, 0, 2) // record 1 reported: pending count 0
	}()

	// A generous budget: 2 s, far more than the 20 ms the records need.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Inside Close:
	//  - closed is set, so no new record can be enqueued;
	//  - drain: Len() is 2, so it calls Flush(100 ms) in a loop. The fake's
	//    Flush waits until the pending count reaches 0 or its timeout ends;
	//    after about 20 ms the two Succeed calls bring it to 0, and Flush
	//    returns 0;
	//  - remaining is 0: no purge, no purge flush;
	//  - the driver closes, the report goroutine finishes, and Close returns nil.
	require.NoError(t, publisher.Close(ctx))

	// The proof that Close waited rather than giving up: it never purged.
	assert.Zero(t, fake.PurgeCalls())

	// Both deliveries were resolved by their own success reports, not as purged.
	for _, delivery := range deliveries {
		_, err := helpers.WaitForDelivery(t, delivery)
		require.NoError(t, err)
	}
}

// TestPublishClosePurgesPendingRecords verifies that records still pending
// when the context ends are purged and each reported: its delivery resolves
// with ErrNotDelivered, the callback runs once for it, the purge is logged,
// and Close returns ErrNotDelivered with the count.
func TestPublishClosePurgesPendingRecords(t *testing.T) {
	logs := &sharedhelpers.SyncBuffer{}
	recorder := &sharedhelpers.DeliveryErrorRecorder{}
	publisher, _ := helpers.NewFakePublisher(t,
		publish.WithLogger(zerolog.New(logs)), publish.WithDeliveryErrorFunc(recorder.CallbackFunc))
	deliveries := helpers.SendInvoices(t, publisher.Bind(helpers.PublishInvoiceTopic()), 3)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := publisher.Close(ctx)
	require.ErrorIs(t, err, publish.ErrNotDelivered)
	require.EqualError(t, err, "publish: purged before delivery was confirmed: 3 record(s)")

	for index, delivery := range deliveries {
		select {
		case <-delivery.Done():
		default:
			require.FailNow(t, "delivery not resolved when Close returned", "delivery %d", index)
		}
		_, err := delivery.Wait(context.Background())
		require.ErrorIs(t, err, publish.ErrNotDelivered)
	}
	failures := recorder.Errors()
	require.Len(t, failures, 3)
	for _, failure := range failures {
		assert.Equal(t, "Local: Purged in queue", failure.Code)
	}
	// Close logs this just before purging: remaining is the number still
	// undelivered when its context ended, i.e. about to be purged.
	assert.Contains(t, logs.String(), `"ek_code":"EK_PUBLISH_RECORDS_PURGED","remaining":3`)
}

// TestPublishCloseWaitsForTheLastCallback verifies that Close returns only
// after the callback has run for every purged record.
//
// The callback runs on the report goroutine, not on Close's: a purge report
// carries an error, so settle takes its failure branch, which calls the
// callback through invokeDeliveryErrorFunc. Close must wait for that goroutine
// to finish (reportsDone), not just for its flush:
//
//	time     Close                     report goroutine
//	──────   ─────                     ────────────────
//	  0 ms   purge 5 records,          takes report 1, callback 1 (20 ms)
//	         flush waits for all
//	         5 to be taken
//	 20 ms                             takes report 2, callback 2
//	 40 ms                             takes report 3, callback 3
//	 60 ms                             takes report 4, callback 4
//	 80 ms   flush returns: nothing    takes report 5, callback 5 starts
//	         pending any more
//	         wait reportsDone          callback 5 still running
//	100 ms   return                    callback 5 done, loop ends,
//	                                   reportsDone closed
//
// The fake sends the purge reports on an unbuffered channel, so each one is
// taken only once the previous callback has returned: the reports move in
// step with the callbacks. The flush (bounded at 1 s) returns when report 5
// is taken, at about 80 ms, while callback 5 still has 20 ms to run. Without
// the wait on reportsDone, Close would return then, with four calls counted,
// and this test fails.
func TestPublishCloseWaitsForTheLastCallback(t *testing.T) {
	var calls atomic.Int32
	publisher, _ := helpers.NewFakePublisher(t, publish.WithDeliveryErrorFunc(func(publish.DeliveryError) {
		time.Sleep(20 * time.Millisecond)
		calls.Add(1)
	}))
	helpers.SendInvoices(t, publisher.Bind(helpers.PublishInvoiceTopic()), 5)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, publisher.Close(ctx), publish.ErrNotDelivered)
	assert.Equal(t, int32(5), calls.Load(), "Close returned before the last callback")
}

// TestPublishCloseWithEndedContextPurgesAtOnce verifies that a context already
// ended skips the wait: the only Flush is the one that pushes the purge
// reports through.
//
// An ended context means "purge now", never "skip reporting": drain's loop
// does not run, so nothing is flushed for delivery, but the records are still
// purged and the flush after the purge still runs, whatever the context, so
// each one is reported.
func TestPublishCloseWithEndedContextPurgesAtOnce(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	helpers.SendInvoices(t, publisher.Bind(helpers.PublishInvoiceTopic()), 2) // pending, never reported

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ended before Close starts
	require.ErrorIs(t, publisher.Close(ctx), publish.ErrNotDelivered)

	// The only Flush is the 1 s one after the purge (purgeReportTimeout). Had drain flushed despite
	// the ended context, a 100 ms one would come before it.
	assert.Equal(t, []time.Duration{time.Second}, fake.FlushTimeouts())
	assert.Equal(t, 1, fake.PurgeCalls())
}

// TestPublishCloseNoticesCancellationPromptly verifies that a context
// cancelled while Close waits is noticed within about one short flush.
//
// Flush takes a timeout, not a context, so drain checks the context between
// flushes of at most 100 ms. "Promptly" means at the end of the flush already
// running, not the instant of cancellation:
//
//	  0 ms   drain: Flush(100 ms), the record never reported
//	 30 ms   context cancelled, Flush still waiting
//	100 ms   Flush returns, drain sees the context ended, Close purges
//	~100 ms  Close returns ErrNotDelivered
func TestPublishCloseNoticesCancellationPromptly(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	helpers.SendInvoices(t, publisher.Bind(helpers.PublishInvoiceTopic()), 1) // never reported

	ctx, cancel := context.WithCancel(context.Background()) // no deadline
	time.AfterFunc(30*time.Millisecond, cancel)
	started := time.Now()
	require.ErrorIs(t, publisher.Close(ctx), publish.ErrNotDelivered)
	// Generous, for slow machines: a drain that ignored the cancellation, or
	// waited in one long flush, would take up to the delivery timeout (30 s).
	assert.Less(t, time.Since(started), 500*time.Millisecond)

	// Every flush before the last (the purge flush, 1 s) was a short one.
	timeouts := fake.FlushTimeouts()
	require.GreaterOrEqual(t, len(timeouts), 2, "at least one short flush, then the purge flush")
	for _, timeout := range timeouts[:len(timeouts)-1] {
		assert.LessOrEqual(t, timeout, 100*time.Millisecond)
	}
}

// TestPublishSendAfterClose verifies that every write after Close fails with
// ErrClosed, enqueuing nothing.
func TestPublishSendAfterClose(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	writer := publisher.Bind(helpers.PublishInvoiceTopic())
	require.NoError(t, publisher.Close(context.Background()))

	_, err := writer.Send("k", helpers.NewPublishInvoice())
	require.ErrorIs(t, err, publish.ErrClosed)
	_, err = writer.SendDelete("k")
	require.ErrorIs(t, err, publish.ErrClosed)
	require.ErrorIs(t, writer.Publish(context.Background(), "k", helpers.NewPublishInvoice()), publish.ErrClosed)
	assert.Zero(t, fake.Produced())
}

// TestPublishCloseIsIdempotent verifies that a second Close returns the first
// one's error, and that concurrent Closes all return it.
func TestPublishCloseIsIdempotent(t *testing.T) {
	publisher, _ := helpers.NewFakePublisher(t)
	helpers.SendInvoices(t, publisher.Bind(helpers.PublishInvoiceTopic()), 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	errs := make([]error, 4)
	var wait sync.WaitGroup
	for index := range errs {
		wait.Go(func() { errs[index] = publisher.Close(ctx) })
	}
	wait.Wait()
	for _, err := range errs {
		require.ErrorIs(t, err, publish.ErrNotDelivered)
		assert.Equal(t, errs[0], err, "every Close returns the same error")
	}
	assert.Equal(t, errs[0], publisher.Close(context.Background()))
}

// TestPublishConcurrentSendAndClose verifies, under -race, that a record is
// either refused with ErrClosed or accepted and then resolved: Close never
// leaves an accepted record unreported, which would happen if one were
// enqueued behind the purge. The senders keep sending until they are refused,
// and Close starts once records are flowing, so the two overlap.
//
// The interleaving it tries to provoke, which enqueue's read lock prevents:
//
//	sender   checks closed: false
//	Close    sets closed, drains, purges what is queued now
//	sender   Produce: the record lands behind the purge, never reported
//
// The window is tiny, hence 1000 rounds, and -race catches any unsynchronised
// access to closed.
func TestPublishConcurrentSendAndClose(t *testing.T) {
	for range 1000 {
		publisher, _ := helpers.NewFakePublisher(t)
		writer := publisher.Bind(helpers.PublishInvoiceTopic())
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // drain flushes nothing: Close purges at once

		var mu sync.Mutex
		var accepted []*publish.Delivery
		// flowing is closed by the first successful Send, whichever sender
		// makes it; Close waits for it, so that it starts while the senders are
		// in their loops rather than before they are scheduled. flowingOnce
		// makes that close happen once: closing a closed channel panics.
		flowing := make(chan struct{})
		var flowingOnce sync.Once
		var senders sync.WaitGroup
		for range 4 {
			senders.Go(func() {
				for {
					delivery, err := writer.Send("k", helpers.NewPublishInvoice())
					if err != nil {
						assert.ErrorIs(t, err, publish.ErrClosed)
						return
					}
					mu.Lock()
					accepted = append(accepted, delivery)
					mu.Unlock()
					flowingOnce.Do(func() { close(flowing) })
				}
			})
		}
		<-flowing                        // Close starts only once sends succeed,
		closeErr := publisher.Close(ctx) // so it runs while the senders loop
		senders.Wait()                   // every sender has been refused by now

		// Close returns only after the last report is handled, so an accepted
		// delivery still open here is a record enqueued behind the purge.
		for _, delivery := range accepted {
			select {
			case <-delivery.Done():
			default:
				require.FailNow(t, "an accepted record was left unresolved by Close")
			}
		}
		require.ErrorIs(t, closeErr, publish.ErrNotDelivered)
	}
}
