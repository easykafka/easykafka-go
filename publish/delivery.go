package publish

import (
	"context"
	"errors"

	"github.com/easykafka/easykafka-go/internal/publish/publishdriver"
)

// Delivery is the outcome of one record. The publisher's report goroutine
// resolves it exactly once, from the record's delivery report, whether or not
// anyone waits on it. Safe for concurrent use.
type Delivery struct {
	done chan struct{}
	// record is the record as Send built it, set once by newDelivery in
	// enqueue: the same slices that were produced, not copies. The Delivery
	// itself is the token passed to Produce, so settle gets it back from the
	// report and builds a DeliveryError from this record, since a report
	// carries neither headers nor, with go.delivery.report.fields=none, key
	// and value.
	record publishdriver.Record
	result Result
	err    error
}

func newDelivery(record publishdriver.Record) *Delivery {
	return &Delivery{done: make(chan struct{}), record: record}
}

// resolve records the outcome. Only the report goroutine calls it, and only
// for a delivery not yet resolved: settle checks resolved first.
func (d *Delivery) resolve(result Result, err error) {
	d.result, d.err = result, err
	close(d.done) // happens before every <-d.done, so result and err need no lock
}

// resolved reports whether the outcome is already known, without blocking. A
// receive from a closed channel is always ready, so once resolve has closed
// done the first case is taken; while done is open nothing is ever sent on
// it, so the select falls through to default.
func (d *Delivery) resolved() bool {
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

// Done is closed when the outcome is known.
func (d *Delivery) Done() <-chan struct{} {
	return d.done
}

// Wait blocks until the outcome is known or ctx ends. It returns nil once the
// broker has acknowledged the record under the publisher's acks mode, and a
// *DeliveryError (errors.As) when the broker or librdkafka refused it.
//
// If ctx ends first, Wait returns ctx.Err(). The record is abandoned, not
// cancelled: it may still be delivered, and it is still reported.
func (d *Delivery) Wait(ctx context.Context) (Result, error) {
	// Checked first because select picks at random among ready cases: with
	// only the select below, an outcome already known could lose to an ended
	// context.
	if d.resolved() {
		return d.result, d.err
	}
	select {
	case <-d.done:
		return d.result, d.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Result is where an acknowledged record was written.
type Result struct {
	Topic     string
	Partition int32
	Offset    int64
}

// WaitAll waits for every delivery and joins their errors, so no outcome goes
// unseen behind the first failure. It returns nil once every record is
// acknowledged.
//
// If ctx ends first, WaitAll stops there: the error joins the failures seen so
// far and ctx.Err(). Every delivery must be non-nil.
func WaitAll(ctx context.Context, deliveries ...*Delivery) error {
	var failures []error
	for _, delivery := range deliveries {
		_, err := delivery.Wait(ctx)
		if err == nil {
			continue
		}
		failures = append(failures, err)
		// Wait gave up because ctx ended, not because of this record's own
		// failure: every later Wait would return the same, so stop. ctx alone
		// is not enough, since it can end just after a record failed.
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			break
		}
	}
	return errors.Join(failures...)
}
