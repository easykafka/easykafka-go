// Package easykafka provides a simplified, handler-based Kafka consumer library
// built on top of confluent-kafka-go.
//
// It exposes a minimal public API: create a Consumer with functional options,
// supply a message handler, and call Start. The library manages polling, offset
// commits, rebalancing, and error handling internally so callers can focus on
// business logic.
//
// # Quick Start
//
//	consumer, err := easykafka.New(
//	    easykafka.WithTopic("orders"),
//	    easykafka.WithBrokers("localhost:9092"),
//	    easykafka.WithConsumerGroup("order-processors"),
//	    easykafka.WithHandler(func(ctx context.Context, payload []byte) error {
//	        fmt.Printf("received: %s\n", payload)
//	        return nil
//	    }),
//	)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	if err := consumer.Start(ctx); err != nil {
//	    log.Fatal(err)
//	}
//
// # Batch Processing
//
// For high-throughput scenarios, use WithBatchHandler to process multiple
// messages at once:
//
//	consumer, _ := easykafka.New(
//	    easykafka.WithTopic("events"),
//	    easykafka.WithBrokers("localhost:9092"),
//	    easykafka.WithConsumerGroup("event-processors"),
//	    easykafka.WithBatchHandler(func(ctx context.Context, payloads [][]byte) error {
//	        return bulkInsert(ctx, payloads)
//	    }),
//	    easykafka.WithBatchSize(100),
//	    easykafka.WithBatchTimeout(5*time.Second),
//	)
//
// # Error Strategies
//
// Pluggable error strategies control what happens when a handler returns an
// error:
//
//   - [NewSkipStrategy]: logs the error and continues (default).
//   - [NewFailFastStrategy]: stops the consumer immediately.
//   - [NewRetryStrategy]: retries via a Kafka retry topic with exponential
//     backoff, then routes to a dead-letter queue (DLQ).
//
// # Stopping
//
// Cancelling the context passed to Start is the only way to stop a consumer,
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
//	    cancel() // this is what shuts the consumer down
//	}()
//
//	if err := consumer.Start(ctx); err != nil {
//	    log.Fatal(err)
//	}
//
// A message being handled when the context is cancelled has its context
// cancelled with it, and whatever the handler returns then goes to the error
// strategy like any other result: written off under Skip, republished under
// Retry. A handler that ignores its context blocks Start for as long as it
// runs, and bounding that wait is the caller's job — only the caller can decide
// to exit. The README has the pattern, including the shape for several
// consumers at once.
package easykafka

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/easykafka/easykafka-go/internal/engine"
	"github.com/easykafka/easykafka-go/internal/kafka"
	"github.com/easykafka/easykafka-go/internal/types"
)

// Consumer manages the lifecycle of Kafka message consumption.
// Create via New() with functional options, then call Start() to begin consuming.
type Consumer interface {
	// Start begins consuming messages from the configured topic. It blocks
	// until the context is cancelled or a fatal error occurs, and returns only
	// once the consumer has fully stopped: the poll loop has exited, the final
	// offsets are committed and the Kafka connection is closed.
	//
	// Cancelling the context is the only way to stop a consumer. A consumer is
	// single-use — a second Start returns an error.
	Start(ctx context.Context) error
}

// consumerImpl is the internal implementation of Consumer.
type consumerImpl struct {
	config Config
	// started is set by the first Start and never cleared, so a consumer that
	// has run cannot be restarted. It is the whole of the lifecycle state the
	// library needs: stopping is the caller cancelling their own context, and
	// they need nothing from us to know they did it.
	started atomic.Bool
}

// New creates a new Consumer with the provided options.
// Returns error if required options are missing or invalid.
//
// Required options: WithTopic, WithBrokers, WithConsumerGroup, and exactly one of WithHandler/WithBatchHandler.
func New(options ...Option) (Consumer, error) {
	cfg := Config{}

	// Apply all options
	for _, opt := range options {
		if err := opt(&cfg); err != nil {
			return nil, fmt.Errorf("invalid option: %w", err)
		}
	}

	// Validate required fields
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Apply sensible defaults for optional fields
	cfg.ApplyDefaults()

	return &consumerImpl{config: cfg}, nil
}

// Start begins consuming messages from the configured topic.
// Blocks until context is cancelled or a fatal error occurs.
func (c *consumerImpl) Start(ctx context.Context) error {
	// Claim the consumer. Exactly one caller can win the swap, so a second
	// Start — including one racing the first — is rejected here
	// (CompareAndSwap return value - reports whether the swap actually happened)
	if !c.started.CompareAndSwap(false, true) {
		return errors.New("consumer already started or stopped")
	}

	c.config.Logger.Info().
		Str("topic", c.config.Topic).
		Strs("brokers", c.config.Brokers).
		Str("group", c.config.ConsumerGroup).
		Str("mode", string(c.config.Mode)).
		Str("error_strategy", c.config.ErrorStrategy.Name()).
		Msg("consumer starting")

	// Wire logger into error strategy if it supports it
	if la, ok := c.config.ErrorStrategy.(types.LoggerAware); ok {
		la.SetLogger(c.config.Logger)
	}

	// Initialize error strategy if it implements Initializable (e.g., retry)
	if init, ok := c.config.ErrorStrategy.(types.Initializable); ok {
		initCfg := types.InitConfig{
			Brokers:       c.config.Brokers,
			ConsumerGroup: c.config.ConsumerGroup,
			Handler:       c.config.Handler,
			Logger:        c.config.Logger,
		}
		if err := init.Initialize(initCfg); err != nil {
			return fmt.Errorf("failed to initialize error strategy: %w", err)
		}
	}

	// Create Kafka adapter
	adapter, err := kafka.NewAdapter(
		c.config.Brokers,
		c.config.Topic,
		c.config.ConsumerGroup,
		c.config.KafkaConfig,
		c.config.AutoCommitEvery,
		c.config.Logger,
	)
	if err != nil {
		// Clean up strategy if initialized
		if init, ok := c.config.ErrorStrategy.(types.Initializable); ok {
			_ = init.Close()
		}
		return fmt.Errorf("failed to create kafka adapter: %w", err)
	}

	// Compute poll timeout in milliseconds
	pollTimeoutMs := int(c.config.PollTimeout / time.Millisecond)
	if pollTimeoutMs < 1 {
		pollTimeoutMs = 100
	}

	// Create the engine based on consumption mode
	var eng *engine.Engine
	if c.config.Mode == ModeBatch {
		eng = engine.NewBatchEngine(
			adapter,
			c.config.BatchHandler,
			c.config.ErrorStrategy,
			c.config.Logger,
			pollTimeoutMs,
			c.config.BatchSize,
			c.config.BatchTimeout,
		)
	} else {
		eng = engine.NewEngine(
			adapter,
			c.config.Handler,
			c.config.ErrorStrategy,
			c.config.Logger,
			pollTimeoutMs,
		)
	}

	c.config.Logger.Info().Msg("consumer running")

	// Run the engine (blocks until the caller's context is cancelled or a fatal
	// error occurs). The context goes through as given: there is nothing left
	// that would cancel it from this side.
	err = eng.Start(ctx)

	// Clean up strategy resources
	if init, ok := c.config.ErrorStrategy.(types.Initializable); ok {
		_ = init.Close()
	}

	c.config.Logger.Info().Err(err).Msg("consumer stopped")

	return err
}

// GetConfig returns the Config of a Consumer for testing/inspection purposes.
// Panics if the consumer is not a *consumerImpl.
func GetConfig(c Consumer) Config {
	impl, ok := c.(*consumerImpl)
	if !ok {
		panic("consumer is not a *consumerImpl")
	}
	return impl.config
}
