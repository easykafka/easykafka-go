package engine

import (
	"context"
	"fmt"

	"github.com/easykafka/easykafka-go/internal/kafka"
	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/rs/zerolog"
)

// Engine manages the Kafka polling loop and message dispatch.
type Engine struct {
	adapter     *kafka.Adapter
	handler     types.Handler
	strategy    types.ErrorStrategy
	logger      zerolog.Logger
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
	logger zerolog.Logger,
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

	e.logger.Info().Int("poll_timeout_ms", e.pollTimeout).Msg("engine starting")

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
			e.logger.Error().Err(err).Msg("error polling message")
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
				e.logger.Error().Err(err).Msg("error strategy failed")
				e.state = engineStateStopping
				break
			}
			// TODO: review this laters, seems kind of strange to me
			// Strategy decided to continue, don't commit offset
			continue
		}

		// Handler succeeded, commit offset
		if err := e.adapter.CommitOffset(msg.Topic, msg.Partition, msg.Offset); err != nil {
			e.logger.Error().Err(err).Msg("failed to commit offset")
		}
	}

	// Cleanup
	if err := e.adapter.Close(ctx); err != nil {
		e.logger.Error().Err(err).Msg("error closing adapter")
	}

	e.state = engineStateStopped

	e.logger.Info().Msg("engine stopped")

	return nil
}

// dispatchMessage calls the handler with panic recovery.
func (e *Engine) dispatchMessage(ctx context.Context, msg *types.Message) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
			e.logger.Error().Err(err).Msg("handler panic recovered")
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
