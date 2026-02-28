package easykafka

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/easykafka/easykafka-go/internal/engine"
	"github.com/easykafka/easykafka-go/internal/kafka"
)

// Consumer manages the lifecycle of Kafka message consumption.
// Create via New() with functional options, then call Start() to begin consuming.
type Consumer interface {
	// Start begins consuming messages from the configured topic.
	// Blocks until context is cancelled or a fatal error occurs.
	Start(ctx context.Context) error

	// Shutdown gracefully stops the consumer within the configured timeout.
	// Completes in-flight message processing and commits final offsets.
	Shutdown(ctx context.Context) error
}

// consumerImpl is the internal implementation of Consumer.
type consumerImpl struct {
	config Config
	eng    *engine.Engine
	state  atomic.Value // State (Created, Running, ShuttingDown, Stopped)
	cancel context.CancelFunc
}

// ConsumerState represents the lifecycle state of a consumer.
type ConsumerState string

const (
	StateCreated      ConsumerState = "created"
	StateRunning      ConsumerState = "running"
	StateShuttingDown ConsumerState = "shutting_down"
	StateStopped      ConsumerState = "stopped"
	StateError        ConsumerState = "error"
)

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

	c := &consumerImpl{
		config: cfg,
	}
	c.state.Store(StateCreated)

	return c, nil
}

// Start begins consuming messages from the configured topic.
// Blocks until context is cancelled or a fatal error occurs.
func (c *consumerImpl) Start(ctx context.Context) error {
	if c.state.Load().(ConsumerState) != StateCreated {
		return errors.New("consumer already started or stopped")
	}

	// Create Kafka adapter
	adapter, err := kafka.NewAdapter(
		c.config.Brokers,
		c.config.Topic,
		c.config.ConsumerGroup,
		c.config.KafkaConfig,
		c.config.Logger,
	)
	if err != nil {
		c.state.Store(StateError)
		return fmt.Errorf("failed to create kafka adapter: %w", err)
	}

	// Compute poll timeout in milliseconds
	pollTimeoutMs := int(c.config.PollTimeout / time.Millisecond)
	if pollTimeoutMs < 1 {
		pollTimeoutMs = 100
	}

	// Create the engine
	c.eng = engine.NewEngine(
		adapter,
		c.config.Handler,
		c.config.ErrorStrategy,
		c.config.Logger,
		pollTimeoutMs,
	)

	c.state.Store(StateRunning)

	// Create a cancellable context for shutdown support
	engineCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	// Run the engine (blocks until cancelled or fatal error)
	err = c.eng.Start(engineCtx)

	c.state.Store(StateStopped)

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

// Shutdown gracefully stops the consumer within the configured timeout.
// Completes in-flight message processing and commits final offsets.
func (c *consumerImpl) Shutdown(ctx context.Context) error {
	state := c.state.Load().(ConsumerState)
	if state != StateRunning {
		return errors.New("consumer is not running")
	}

	c.state.Store(StateShuttingDown)

	// Signal the engine to stop
	if c.eng != nil {
		if err := c.eng.Stop(ctx); err != nil {
			return fmt.Errorf("engine stop error: %w", err)
		}
	}

	// Cancel the engine context to break the poll loop
	if c.cancel != nil {
		c.cancel()
	}

	return nil
}
