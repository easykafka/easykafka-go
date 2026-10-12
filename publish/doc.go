// Package publish writes typed records to Kafka and confirms every one of them.
//
// From the top down:
//
//   - Publisher: created by New, owns one librdkafka producer and the settings
//     that belong to it: brokers, acks, idempotence, partitioner, delivery
//     timeout, logger and callbacks. A service that needs two partitioners
//     creates two publishers. It also checks the cluster and the bound topics
//     with Ping, reports a fatal producer error with Err, and is shut down with
//     Close, which reports every record still undelivered rather than drop it.
//   - Topic[K, V]: a declaration of one Kafka topic: its name, how to encode a
//     key of type K and a value of type V into bytes (StringKey, JSONValue and
//     the other encoders cover the common cases), and headers added to every
//     record.
//   - Writer[K, V]: what Publisher.Bind returns for a Topic. It writes that
//     topic's records with typed keys and values, through its publisher's
//     producer: Publish and Delete wait for the broker, Send and SendDelete
//     return at once. One publisher can have any number of writers.
//   - Delivery: what Send and SendDelete return, the outcome of one record. It
//     resolves exactly once, from the record's delivery report, whether or not
//     anyone waits on it; Wait, or WaitAll for several, waits for it.
//   - DeliveryError: why a record was not acknowledged. It is what a failed
//     wait returns, and what the function given to WithDeliveryErrorFunc
//     receives, for every failure.
//
// Defaults favour durability: acks=all, idempotence on, and a 30 s delivery
// timeout. WithAcksLeader, WithoutIdempotence and WithDeliveryTimeout change
// them.
//
// # Quick Start
//
//	publisher, err := publish.New(publish.WithBrokers("localhost:9092"))
//	if err != nil {
//	    log.Fatal(err)
//	}
//	invoices := publisher.Bind(publish.Topic[string, Invoice]{
//	    Name:        "invoices",
//	    EncodeKey:   publish.StringKey,
//	    EncodeValue: publish.JSONValue[Invoice], // spell out the type argument
//	})
//	if err := publisher.Ping(ctx); err != nil { // the cluster answers, the topic exists
//	    log.Fatal(err)
//	}
//	if err := invoices.Publish(ctx, invoice.ID, invoice); err != nil {
//	    // not acknowledged: a *DeliveryError says why
//	}
//	...
//	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
//	defer cancel()
//	err = publisher.Close(closeCtx) // delivers or reports every record it accepted
//
// # Waiting, and not waiting
//
// Writing a record only queues it in librdkafka, which sends it in the
// background; the outcome arrives later, on the record's Delivery. Publish
// waits for that outcome and returns nil only once the broker has
// acknowledged the record. Send returns at once with the Delivery; send many,
// then wait for all of them with WaitAll, at the cost of one round trip
// rather than one per record.
//
// # Publishing from a consumer handler
//
// A handler that publishes, then returns, makes the pair at-least-once: the
// subscriber stores the consumed record's offset only after its handler has
// returned, so a write that was not acknowledged fails the handler and the
// record is consumed again. Wait under context.WithoutCancel(ctx), so a
// shutdown does not abandon a record about to be confirmed; the delivery
// timeout still bounds the wait.
package publish
