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

	c.config.Logger.Info().
		Str("topic", c.config.Topic).
		Strs("brokers", c.config.Brokers).
		Str("group", c.config.ConsumerGroup).
		Str("mode", string(c.config.Mode)).
		Str("error_strategy", c.config.ErrorStrategy.Name()).
		Msg("consumer starting")

	// Wire logger into error strategy if it supports it (FR-045)
	if la, ok := c.config.ErrorStrategy.(types.LoggerAware); ok {
		la.SetLogger(c.config.Logger)
	}

	// Initialize error strategy if it implements Initializable (e.g., retry, circuit breaker)
	if init, ok := c.config.ErrorStrategy.(types.Initializable); ok {
		initCfg := types.InitConfig{
			Brokers:       c.config.Brokers,
			ConsumerGroup: c.config.ConsumerGroup,
			Handler:       c.config.Handler,
			Logger:        c.config.Logger,
		}
		if err := init.Initialize(initCfg); err != nil {
			c.state.Store(StateError)
			return fmt.Errorf("failed to initialize error strategy: %w", err)
		}
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
	if c.config.Mode == ModeBatch {
		c.eng = engine.NewBatchEngine(
			adapter,
			c.config.BatchHandler,
			c.config.ErrorStrategy,
			c.config.Logger,
			pollTimeoutMs,
			c.config.BatchSize,
			c.config.BatchTimeout,
		)
	} else {
		c.eng = engine.NewEngine(
			adapter,
			c.config.Handler,
			c.config.ErrorStrategy,
			c.config.Logger,
			pollTimeoutMs,
		)
	}

	c.state.Store(StateRunning)
	c.config.Logger.Info().Msg("consumer running")

	// Create a cancellable context for shutdown support
	engineCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	// Run the engine (blocks until cancelled or fatal error)
	err = c.eng.Start(engineCtx)

	// Clean up strategy resources
	if init, ok := c.config.ErrorStrategy.(types.Initializable); ok {
		_ = init.Close()
	}

	c.state.Store(StateStopped)
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

// Shutdown gracefully stops the consumer within the configured timeout.
// Completes in-flight message processing and commits final offsets.
// If the shutdown timeout expires before in-flight work completes,
// the consumer force-stops and returns a timeout error (FR-040).
func (c *consumerImpl) Shutdown(ctx context.Context) error {
	state := c.state.Load().(ConsumerState)
	if state != StateRunning {
		return errors.New("consumer is not running")
	}

	c.state.Store(StateShuttingDown)
	c.config.Logger.Info().Msg("consumer shutdown initiated")

	// Signal the engine to stop fetching new messages (FR-036)
	if c.eng != nil {
		if err := c.eng.Stop(ctx); err != nil {
			return fmt.Errorf("engine stop error: %w", err)
		}
	}

	// Cancel the engine context to break the poll loop
	if c.cancel != nil {
		c.cancel()
	}

	// Wait for the engine to complete in-flight work within the shutdown timeout (FR-037)
	if c.eng != nil {
		waitCtx, waitCancel := context.WithTimeout(ctx, c.config.ShutdownTimeout)
		defer waitCancel()

		if err := c.eng.WaitForDone(waitCtx); err != nil {
			c.config.Logger.Error().Err(err).Dur("timeout", c.config.ShutdownTimeout).Msg("consumer shutdown timed out")
			return fmt.Errorf("shutdown timeout: %w", err)
		}
	}

	c.config.Logger.Info().Msg("consumer shutdown complete")
	return nil
}
