package easykafka

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/easykafka/easykafka-go/internal/engine"
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

	c.state.Store(StateRunning)

	// TODO: Initialize engine and start polling
	return nil
}

// Shutdown gracefully stops the consumer within the configured timeout.
// Completes in-flight message processing and commits final offsets.
func (c *consumerImpl) Shutdown(ctx context.Context) error {
	if c.state.Load().(ConsumerState) != StateRunning {
		return errors.New("consumer is not running")
	}

	c.state.Store(StateShuttingDown)

	// TODO: Implement graceful shutdown
	c.state.Store(StateStopped)
	return nil
}
