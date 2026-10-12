package subscribe_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/subscribe"
)

// Order is what the orders topic carries, as JSON.
type Order struct {
	ID     string  `json:"id"`
	Amount float64 `json:"amount"`
}

// Invoice is what a handler publishes for each order.
type Invoice struct {
	OrderID string  `json:"orderId"`
	Amount  float64 `json:"amount"`
}

// A complete subscriber: handle every record of a topic, retry failures
// through a retry topic and a dead-letter queue, and stop on a signal.
//
// Compiled with the tests but never run: it has no broker to reach, which is
// why it declares no Output. Adding one would make the test suite depend on a
// live Kafka.
func Example() {
	if err := runOrders(); err != nil {
		log.Fatal(err)
	}
}

// runOrders holds the work and returns an error rather than exiting, so that
// deferred cleanup runs: log.Fatal skips defers.
func runOrders() error {
	// Cancelling this context is the only way to stop the subscriber.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Give each subscriber its own strategy: Start claims it.
	retry, err := subscribe.NewRetryStrategy(
		subscribe.WithRetryTopic("orders.retry"),
		subscribe.WithDLQTopic("orders.dlq"),
		subscribe.WithMaxAttempts(3), // the first attempt and two retries, then the DLQ
	)
	if err != nil {
		return err
	}

	subscriber, err := subscribe.New(
		subscribe.WithTopic("orders"),
		subscribe.WithBrokers("localhost:9092"),
		subscribe.WithConsumerGroup("order-processors"),
		subscribe.WithErrorStrategy(retry),
		subscribe.WithHandler(func(ctx context.Context, payload []byte) *subscribe.Failure {
			var order Order
			if err := json.Unmarshal(payload, &order); err != nil {
				// No retry can fix a malformed record: straight to the DLQ.
				return &subscribe.Failure{Err: fmt.Errorf("%w: %v", subscribe.ErrPermanent, err)}
			}
			if err := process(ctx, order); err != nil {
				return &subscribe.Failure{Err: err}
			}
			return nil // the offset is stored once the handler returns nil
		}),
	)
	if err != nil {
		return err
	}

	// Start blocks until ctx is cancelled or a fatal error, and returns once
	// the final offsets are committed and the connection is closed.
	return subscriber.Start(ctx)
}

// process stands in for the application's work.
func process(_ context.Context, _ Order) error { return nil }

// Publishing from a handler, without losing a record. The subscriber stores a
// record's offset only after its handler returns, so a handler that returns
// only once its write is acknowledged makes the pair at-least-once: a write
// that fails fails the handler, and the record is not marked done.
func Example_publishFromHandler() {
	if err := runInvoicing(); err != nil {
		log.Fatal(err)
	}
}

// runInvoicing turns every order into an invoice, as runOrders does the work
// of Example.
func runInvoicing() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	publisher, err := publish.New(publish.WithBrokers("localhost:9092"))
	if err != nil {
		return err
	}
	// Closed after the subscriber has stopped, so no handler writes through a
	// closed publisher, and with a context of its own: ctx is cancelled by then.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := publisher.Close(closeCtx); err != nil {
			log.Printf("closing the publisher: %v", err)
		}
	}()

	invoices := publisher.Bind(publish.Topic[string, Invoice]{
		Name:        "invoices",
		EncodeKey:   publish.StringKey,
		EncodeValue: publish.JSONValue[Invoice],
	})

	subscriber, err := subscribe.New(
		subscribe.WithTopic("orders"),
		subscribe.WithBrokers("localhost:9092"),
		subscribe.WithConsumerGroup("invoicing"),
		subscribe.WithHandler(func(ctx context.Context, payload []byte) *subscribe.Failure {
			var order Order
			if err := json.Unmarshal(payload, &order); err != nil {
				return &subscribe.Failure{Err: fmt.Errorf("%w: %v", subscribe.ErrPermanent, err)}
			}
			invoice := Invoice{OrderID: order.ID, Amount: order.Amount}
			// WithoutCancel: at shutdown, finish confirming a record already
			// sent rather than abandon it and write it again on redelivery.
			// The publisher's delivery timeout still bounds the wait.
			if err := invoices.Publish(context.WithoutCancel(ctx), order.ID, invoice); err != nil {
				return &subscribe.Failure{Err: err} // not acknowledged: the offset is not stored
			}
			return nil
		}),
	)
	if err != nil {
		return err
	}
	return subscriber.Start(ctx)
}
