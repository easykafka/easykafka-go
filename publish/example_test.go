package publish_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/easykafka/easykafka-go/publish"
)

// Invoice is the value one topic carries, encoded as JSON.
type Invoice struct {
	ID     string  `json:"id"`
	Amount float64 `json:"amount"`
}

// A complete publisher: create it, declare a topic, check the cluster, write a
// record and wait for the broker to acknowledge it, and shut down without
// losing anything still queued.
//
// Compiled with the tests but never run: it has no broker to reach, which is
// why it declares no Output. Adding one would make the test suite depend on a
// live Kafka.
func Example() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run holds the work and returns an error rather than exiting, so that the
// deferred Close runs: log.Fatal skips defers, and a publisher not closed drops
// whatever librdkafka still holds.
func run() error {
	// The defaults favour durability: acks=all, idempotence on, and a 30 s
	// delivery timeout.
	publisher, err := publish.New(publish.WithBrokers("localhost:9092"))
	if err != nil {
		return err
	}
	defer closePublisher(publisher)

	// Bind fixes the topic's key and value types. Spell out JSONValue's type
	// argument: the compiler needs it here.
	invoices := publisher.Bind(publish.Topic[string, Invoice]{
		Name:        "invoices",
		EncodeKey:   publish.StringKey,
		EncodeValue: publish.JSONValue[Invoice],
	})

	ctx := context.Background()

	// Check at startup that a broker answers and every bound topic exists.
	if err := publisher.Ping(ctx); err != nil {
		return fmt.Errorf("kafka not ready: %w", err)
	}

	// Publish returns once the broker has acknowledged the record. Send would
	// return at once, with a Delivery to wait on later.
	invoice := Invoice{ID: "INV-1", Amount: 12.5}
	if err := invoices.Publish(ctx, invoice.ID, invoice); err != nil {
		if deliveryErr, ok := errors.AsType[*publish.DeliveryError](err); ok {
			return fmt.Errorf("invoice %s not delivered (%s): %w", invoice.ID, deliveryErr.Code, err)
		}
		return err
	}
	return nil
}

// closePublisher shuts the publisher down with a context of its own: at
// shutdown the application's root context is usually cancelled already. Close
// delivers what it can within the timeout, then reports every record still
// undelivered rather than drop it.
func closePublisher(publisher *publish.Publisher) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := publisher.Close(ctx); err != nil {
		log.Printf("closing the publisher: %v", err)
	}
}

// Many records, one wait: Send each, then WaitAll. Waiting for each record in
// turn would cost a broker round trip per record.
func ExampleWaitAll() {
	publisher, err := publish.New(publish.WithBrokers("localhost:9092"))
	if err != nil {
		log.Fatal(err)
	}
	defer closePublisher(publisher)

	invoices := publisher.Bind(publish.Topic[string, Invoice]{
		Name:        "invoices",
		EncodeKey:   publish.StringKey,
		EncodeValue: publish.JSONValue[Invoice],
	})

	batch := []Invoice{{ID: "INV-1", Amount: 12.5}, {ID: "INV-2", Amount: 7}}
	deliveries := make([]*publish.Delivery, 0, len(batch))
	for _, invoice := range batch {
		// Send fails only locally: encoding, a full queue, a closed publisher.
		delivery, err := invoices.Send(invoice.ID, invoice)
		if err != nil {
			log.Printf("invoice %s not sent: %v", invoice.ID, err)
			return
		}
		deliveries = append(deliveries, delivery)
	}

	// nil once every record is acknowledged; otherwise every failure, joined.
	if err := publish.WaitAll(context.Background(), deliveries...); err != nil {
		log.Printf("some invoices not delivered: %v", err)
	}
}
