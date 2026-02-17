package engine

import (
	"context"
	"fmt"

	"github.com/easykafka/easykafka-go/internal/kafka"
	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/internal/types"
)

// Engine manages the Kafka polling loop and message dispatch.
type Engine struct {
	adapter     *kafka.Adapter
	handler     types.Handler
	strategy    types.ErrorStrategy
	logger      types.Logger
	pollTimeout int
	state       engineState
}

type engineState int

const (
	engineStateCreated engineState = iota
	engineStateRunning
	engineStateStopping
	engineStateStopped
)

// NewEngine creates a new Engine instance.
func NewEngine(
	adapter *kafka.Adapter,
	handler types.Handler,
	strategy types.ErrorStrategy,
	logger types.Logger,
	pollTimeoutMs int,
) *Engine {
	return &Engine{
		adapter:     adapter,
		handler:     handler,
		strategy:    strategy,
		logger:      logger,
		pollTimeout: pollTimeoutMs,
		state:       engineStateCreated,
	}
}

// Start begins the polling loop.
// Blocks until context is cancelled or a fatal error occurs.
func (e *Engine) Start(ctx context.Context) error {
	if e.state != engineStateCreated {
		return fmt.Errorf("engine already started")
	}

	e.state = engineStateRunning

	if e.logger != nil {
		e.logger.Info("engine starting", "poll_timeout_ms", e.pollTimeout)
	}

	// Connect to Kafka
	if err := e.adapter.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	// Subscribe to topic
	if err := e.adapter.SubscribeToTopic(ctx); err != nil {
		_ = e.adapter.Close(ctx)
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	// Main polling loop
	for e.state == engineStateRunning {
		select {
		case <-ctx.Done():
			e.state = engineStateStopping
			break
		default:
		}

		if e.state != engineStateRunning {
			break
		}

		// Poll for messages
		msg, err := e.adapter.Poll(ctx, e.pollTimeout)
		if err != nil {
			if e.logger != nil {
				e.logger.Error("error polling message", "error", err)
			}
			e.state = engineStateStopping
			break
		}

		if msg == nil {
			// Timeout, no message available
			continue
		}

		// Attach message to context for handler
		handlerCtx := metadata.WithMessage(ctx, msg)

		// Call handler with panic recovery
		if err := e.dispatchMessage(handlerCtx, msg); err != nil {
			// Handler failed, apply error strategy
			if err := e.strategy.HandleError(ctx, []*types.Message{msg}, err); err != nil {
				if e.logger != nil {
					e.logger.Error("error strategy failed", "error", err)
				}
				e.state = engineStateStopping
				break
			}
			// Strategy decided to continue, don't commit offset
			continue
		}

		// Handler succeeded, commit offset
		if err := e.adapter.CommitOffset(msg.Topic, msg.Partition, msg.Offset); err != nil {
			if e.logger != nil {
				e.logger.Error("failed to commit offset", "error", err)
			}
		}
	}

	// Cleanup
	if err := e.adapter.Close(ctx); err != nil {
		if e.logger != nil {
			e.logger.Error("error closing adapter", "error", err)
		}
	}

	e.state = engineStateStopped

	if e.logger != nil {
		e.logger.Info("engine stopped")
	}

	return nil
}

// dispatchMessage calls the handler with panic recovery.
func (e *Engine) dispatchMessage(ctx context.Context, msg *types.Message) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
			if e.logger != nil {
				e.logger.Error("handler panic recovered", "error", err)
			}
		}
	}()

	return e.handler(ctx, msg.Payload)
}

// Stop gracefully stops the engine.
func (e *Engine) Stop(ctx context.Context) error {
	if e.state == engineStateRunning {
		e.state = engineStateStopping
	}
	return nil
}
