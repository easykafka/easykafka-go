package engine

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

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
	adapter      KafkaClient
	handler      types.Handler
	batchHandler types.BatchHandler
	strategy     types.ErrorStrategy
	logger       zerolog.Logger
	pollTimeout  int
	state        engineState

	// done is closed when the engine has fully stopped (poll loop exited, adapter closed).
	done chan struct{}

	// Batch mode fields (nil/zero when in single-message mode)
	batchBuffer  *BatchBuffer
	batchSize    int
	batchTimeout time.Duration
}

type engineState int

const (
	engineStateCreated engineState = iota
	engineStateRunning
	engineStateStopping
	engineStateStopped
)

// NewEngine creates a new Engine instance for single-message mode.
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
		done:        make(chan struct{}),
	}
}

// NewBatchEngine creates a new Engine instance for batch mode.
func NewBatchEngine(
	adapter KafkaClient,
	batchHandler types.BatchHandler,
	strategy types.ErrorStrategy,
	logger zerolog.Logger,
	pollTimeoutMs int,
	batchSize int,
	batchTimeout time.Duration,
) *Engine {
	return &Engine{
		adapter:      adapter,
		batchHandler: batchHandler,
		strategy:     strategy,
		logger:       logger,
		pollTimeout:  pollTimeoutMs,
		state:        engineStateCreated,
		done:         make(chan struct{}),
		batchSize:    batchSize,
		batchTimeout: batchTimeout,
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

	if e.batchHandler != nil {
		loopErr = e.runBatchLoop(ctx)
	} else {
		loopErr = e.runSingleLoop(ctx)
	}

	// Cleanup
	if err := e.adapter.Close(ctx); err != nil {
		e.logger.Error().Err(err).Msg("error closing adapter")
	}

	e.state = engineStateStopped
	close(e.done)

	e.logger.Info().Msg("engine stopped")

	return loopErr
}

// runSingleLoop runs the single-message polling loop.
func (e *Engine) runSingleLoop(ctx context.Context) error {
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
		}

		// (Handler succeeded) or (handler failed and error strategy succeeded) => commit the offset
		if err := e.adapter.CommitOffset(msg.Topic, msg.Partition, msg.Offset); err != nil {
			// todo: can we afford to continue if offset is not commited?
			e.logger.Error().Err(err).
				Int64("offset", msg.Offset).
				Int32("partition", msg.Partition).
				Msg("failed to commit offset")
		}
	}

	return loopErr
}

// runBatchLoop runs the batch-mode polling loop.
// Messages are accumulated in a buffer and dispatched when the batch is
// full or the batch timeout expires. Offsets are committed atomically
// for the highest offset in each batch.
func (e *Engine) runBatchLoop(ctx context.Context) error {
	buf := NewBatchBuffer(e.batchSize, e.batchTimeout)
	var loopErr error

	for e.state == engineStateRunning {
		select {
		case <-ctx.Done():
			e.state = engineStateStopping
			// Flush any remaining buffered messages before stopping
			if msgs := buf.Flush(); msgs != nil {
				if err := e.dispatchBatch(ctx, msgs); err != nil {
					loopErr = err
				}
			}
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

		if msg != nil {
			buf.Add(msg)
		}

		// Dispatch batch when full or timed out
		if buf.Ready() || buf.TimedOut() {
			msgs := buf.Flush()
			if msgs != nil {
				if err := e.dispatchBatch(ctx, msgs); err != nil {
					loopErr = err
					e.state = engineStateStopping
					break
				}
			}
		}
	}

	return loopErr
}

// dispatchBatch calls the batch handler with panic recovery,
// applies the error strategy on failure, and commits offsets atomically.
func (e *Engine) dispatchBatch(ctx context.Context, msgs []*types.Message) error {
	// Build payloads slice
	payloads := make([][]byte, len(msgs))
	for i, m := range msgs {
		payloads[i] = m.Payload
	}

	// Call batch handler with panic recovery
	handlerErr := e.invokeBatchHandler(ctx, payloads)

	if handlerErr != nil {
		// Apply error strategy to the entire batch
		strategyErr := e.strategy.HandleError(ctx, msgs, handlerErr)
		if strategyErr != nil {
			e.logger.Error().Err(strategyErr).Msg("error strategy returned fatal error, stopping")
			return fmt.Errorf("error strategy: %w", strategyErr)
		}
	}

	// Commit the highest offset per topic-partition in the batch.
	// A batch may contain messages from multiple partitions, so we
	// find the maximum offset for each (topic, partition) pair and
	// commit each one individually.
	type tpKey struct {
		Topic     string
		Partition int32
	}
	highest := make(map[tpKey]int64)
	for _, m := range msgs {
		key := tpKey{Topic: m.Topic, Partition: m.Partition}
		if off, ok := highest[key]; !ok || m.Offset > off {
			highest[key] = m.Offset
		}
	}
	for key, offset := range highest {
		if err := e.adapter.CommitOffset(key.Topic, key.Partition, offset); err != nil {
			e.logger.Error().Err(err).
				Int64("offset", offset).
				Int32("partition", key.Partition).
				Msg("failed to commit batch offset")
		}
	}

	return nil
}

// invokeBatchHandler calls the batch handler with panic recovery.
func (e *Engine) invokeBatchHandler(ctx context.Context, payloads [][]byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			err = fmt.Errorf("handler panic: %v", r)
			e.logger.Error().
				Err(err).
				Str("stack", stack).
				Int("batch_size", len(payloads)).
				Msg("batch handler panic recovered")
		}
	}()

	return e.batchHandler(ctx, payloads)
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

// Stop gracefully stops the engine by transitioning to stopping state.
// Returns error if the engine is not currently running.
func (e *Engine) Stop(ctx context.Context) error {
	if e.state != engineStateRunning {
		return fmt.Errorf("engine is not running (state: %d)", e.state)
	}
	e.state = engineStateStopping
	return nil
}

// WaitForDone blocks until the engine has fully stopped or the context expires.
// Returns nil if the engine stopped cleanly, or ctx.Err() if the deadline/cancellation
// fires before the engine finishes.
func (e *Engine) WaitForDone(ctx context.Context) error {
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown timeout: %w", ctx.Err())
	}
}

// Done returns a channel that is closed when the engine has fully stopped.
// Useful for callers that need to select on engine completion.
func (e *Engine) Done() <-chan struct{} {
	return e.done
}
