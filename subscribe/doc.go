// Package subscribe is a simplified, handler-based Kafka subscriber built on
// top of confluent-kafka-go.
//
// It exposes a minimal public API: create a Subscriber with functional options,
// supply a message handler, and call Start. The library manages polling, offset
// commits, rebalancing, and error handling internally so callers can focus on
// business logic. A Subscriber joins a Kafka consumer group: every record it
// polls reaches the handler, a failure reaches the error strategy, and an
// offset is stored only once its record is handled or routed.
//
// # Quick Start
//
//	subscriber, err := subscribe.New(
//	    subscribe.WithTopic("orders"),
//	    subscribe.WithBrokers("localhost:9092"),
//	    subscribe.WithConsumerGroup("order-processors"),
//	    subscribe.WithHandler(func(ctx context.Context, payload []byte) *subscribe.Failure {
//	        if err := process(payload); err != nil {
//	            return &subscribe.Failure{Err: err}
//	        }
//	        return nil
//	    }),
//	)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	if err := subscriber.Start(ctx); err != nil {
//	    log.Fatal(err)
//	}
//
// # Batch Processing
//
// For high-throughput scenarios, use WithBatchHandler to process multiple
// messages at once. Each message carries its own verdict: fail the ones that
// failed, and the error strategy routes each of them on its own.
//
//	subscriber, _ := subscribe.New(
//	    subscribe.WithTopic("events"),
//	    subscribe.WithBrokers("localhost:9092"),
//	    subscribe.WithConsumerGroup("event-processors"),
//	    subscribe.WithBatchHandler(func(ctx context.Context, batch *subscribe.Batch) *subscribe.Failure {
//	        for _, item := range batch.Items() {
//	            if err := process(item.Message().Payload); err != nil {
//	                item.Fail(subscribe.Failure{Err: err})
//	            }
//	        }
//	        return nil
//	    }),
//	    subscribe.WithBatchSize(100),
//	    subscribe.WithBatchTimeout(5*time.Second),
//	)
//
// Return a *Failure only when the batch as a whole could not be processed —
// the database is down, say. Every message is then routed under it.
//
// # Error Strategies
//
// Pluggable error strategies control what happens when a handler reports a
// failure:
//
//   - [NewSkipStrategy]: logs the error and continues (default).
//   - [NewFailFastStrategy]: stops the subscriber immediately.
//   - [NewRetryStrategy]: retries via a Kafka retry topic with exponential
//     backoff, then routes to a dead-letter queue (DLQ).
//
// # Stopping
//
// Cancelling the context passed to Start is the only way to stop a subscriber,
// and Start returns once it has fully stopped — poll loop exited, final offsets
// committed, connection closed:
//
//	ctx, cancel := context.WithCancel(context.Background())
//	defer cancel()
//
//	signals := make(chan os.Signal, 1)
//	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
//	go func() {
//	    <-signals
//	    cancel() // this is what shuts the subscriber down
//	}()
//
//	if err := subscriber.Start(ctx); err != nil {
//	    log.Fatal(err)
//	}
//
// A message being handled when the context is cancelled has its context
// cancelled with it, and whatever the handler returns then goes to the error
// strategy like any other result: written off under Skip, republished under
// Retry. A handler that ignores its context blocks Start for as long as it
// runs, and bounding that wait is the caller's job — only the caller can decide
// to exit. The README has the pattern, including the shape for several
// subscribers at once.
package subscribe
