package engine

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/rs/zerolog"
)

// KafkaClient abstracts the Kafka adapter for testability.
type KafkaClient interface {
	Connect(ctx context.Context) error
	SubscribeToTopic(ctx context.Context) error
	Poll(ctx context.Context, timeoutMs int) (*types.Message, error)
	CommitOffset(topic string, partition int32, offset int64) error
	Close(ctx context.Context) error
}

// Engine manages the Kafka polling loop and message dispatch.
type Engine struct {
	adapter     KafkaClient
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
	adapter KafkaClient,
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

	var loopErr error

	// Main polling loop
	for e.state == engineStateRunning {
		select {
		case <-ctx.Done():
			e.state = engineStateStopping
			continue
		default:
		}

		if e.state != engineStateRunning {
			break
		}

		// Poll for messages
		msg, err := e.adapter.Poll(ctx, e.pollTimeout)
		if err != nil {
			e.logger.Error().Err(err).Msg("fatal polling error")
			loopErr = fmt.Errorf("polling error: %w", err)
			e.state = engineStateStopping
			break
		}

		if msg == nil {
			// Timeout or non-message event, continue polling
			continue
		}

		// Attach message metadata to context for handler access
		handlerCtx := metadata.WithMessage(ctx, msg)

		// Call handler with panic recovery
		handlerErr := e.dispatchMessage(handlerCtx, msg)

		if handlerErr != nil {
			// Handler failed, apply error strategy
			strategyErr := e.strategy.HandleError(ctx, []*types.Message{msg}, handlerErr)
			if strategyErr != nil {
				// Strategy says stop consumer (e.g., fail-fast)
				e.logger.Error().Err(strategyErr).Msg("error strategy returned fatal error, stopping")
				loopErr = fmt.Errorf("error strategy: %w", strategyErr)
				e.state = engineStateStopping
				break
			}
			// Strategy handled the error (e.g., skip) — do NOT commit offset
			// TODO: is this correct? I don't think so, we should commit
			continue
		}

		// Handler succeeded — commit the offset
		if err := e.adapter.CommitOffset(msg.Topic, msg.Partition, msg.Offset); err != nil {
			e.logger.Error().Err(err).
				Int64("offset", msg.Offset).
				Int32("partition", msg.Partition).
				Msg("failed to commit offset")
		}
	}

	// Cleanup
	if err := e.adapter.Close(ctx); err != nil {
		e.logger.Error().Err(err).Msg("error closing adapter")
	}

	e.state = engineStateStopped

	e.logger.Info().Msg("engine stopped")

	return loopErr
}

// dispatchMessage calls the handler with panic recovery.
// Any panics in the handler are recovered and returned as errors.
func (e *Engine) dispatchMessage(ctx context.Context, msg *types.Message) (err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			err = fmt.Errorf("handler panic: %v", r)
			e.logger.Error().
				Err(err).
				Str("stack", stack).
				Int64("offset", msg.Offset).
				Int32("partition", msg.Partition).
				Msg("handler panic recovered")
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
