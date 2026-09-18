package engine

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/rs/zerolog"
)

// KafkaClient is an alias for the port the engine is driven through. The
// interface itself lives in internal/types so that the adapter can assert it
// implements it without the two packages importing each other.
type KafkaClient = types.KafkaClient

// Engine manages the Kafka polling loop and message dispatch.
type Engine struct {
	adapter      KafkaClient
	handler      types.Handler
	batchHandler types.BatchHandler
	strategy     types.ErrorStrategy
	logger       zerolog.Logger
	pollTimeout  int
	state        atomic.Int32 // protected; use Load()/Store() everywhere

	batchSize    int
	batchTimeout time.Duration

	// buf accumulates messages in batch mode. It lives on the Engine rather than
	// inside runBatchLoop so the revoke hook can discard it. Only ever touched
	// from the polling goroutine — see the hook registration in Start.
	buf *BatchBuffer
}

const (
	engineStateCreated int32 = iota
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
		// state defaults to 0 i.e. engineStateCreated
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
		// state defaults to 0 i.e. engineStateCreated
		batchSize:    batchSize,
		batchTimeout: batchTimeout,
		buf:          NewBatchBuffer(batchSize, batchTimeout),
	}
}

// Start begins the polling loop.
// Blocks until context is cancelled or a fatal error occurs, and returns only
// once the loop has exited, the final offsets are committed and the adapter is
// closed — so its return is the caller's join point, with nothing left running.
func (e *Engine) Start(ctx context.Context) error {
	// Claim the engine. Exactly one caller can win the swap, so a second Start —
	// including one racing the first — is rejected here. Every state other than
	// created means Start has already run.
	if !e.state.CompareAndSwap(engineStateCreated, engineStateRunning) {
		return errors.New("engine already started")
	}

	e.logger.Info().Int("poll_timeout_ms", e.pollTimeout).Msg("engine starting")

	// Connect to Kafka
	if err := e.adapter.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	// Discard buffered work when partitions are revoked, before subscribing so the
	// hook is in place for the first rebalance. Only batch mode buffers anything;
	// single mode holds at most the message it is currently handling.
	//
	// The rebalance callback runs on the goroutine that calls Poll, and this
	// engine polls from a single goroutine, so the buffer needs no locking.
	if e.batchHandler != nil {
		e.adapter.SetOnRevoke(func() {
			if dropped := e.buf.Drop(); dropped > 0 {
				e.logger.Debug().Int("dropped", dropped).
					Msg("discarded buffered messages for revoked partitions")
			}
		})
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

	// Publish anything stored but not yet committed, before the consumer goes
	// away. Mostly redundant given the commit after every message or batch, but
	// it costs one call and saves a replay.
	if err := e.adapter.CommitStored(); err != nil {
		e.logger.Warn().Err(err).Msg("final commit failed, offsets remain stored")
	}

	// Cleanup
	if err := e.adapter.Close(ctx); err != nil {
		e.logger.Error().Err(err).Msg("error closing adapter")
	}

	e.state.Store(engineStateStopped)

	e.logger.Info().Msg("engine stopped")

	return loopErr
}

// runSingleLoop runs the single-message polling loop.
func (e *Engine) runSingleLoop(ctx context.Context) error {
	var loopErr error

	// Main polling loop
	for e.state.Load() == engineStateRunning {
		select {
		case <-ctx.Done():
			e.state.Store(engineStateStopping)
			continue
		default:
		}

		if e.state.Load() != engineStateRunning {
			break
		}

		// Poll for messages
		msg, err := e.adapter.Poll(ctx, e.pollTimeout)
		if err != nil {
			e.logger.Error().Err(err).Msg("fatal polling error")
			loopErr = fmt.Errorf("polling error: %w", err)
			e.state.Store(engineStateStopping)
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
				e.state.Store(engineStateStopping)
				break
			}
		}

		// (Handler succeeded) or (handler failed and error strategy succeeded) =>
		// the message is accounted for, so record it and publish the record.
		if err := e.adapter.StoreOffset(msg.Topic, msg.Partition, msg.Offset); err != nil {
			if !errors.Is(err, types.ErrPartitionRevoked) {
				// Continuing here would lose this message: the next message on
				// this partition stores a higher offset, and committing that
				// silently declares this one done. Stop instead.
				e.logger.Error().Err(err).
					Int64("offset", msg.Offset).
					Int32("partition", msg.Partition).
					Msg("failed to store offset, stopping consumer")
				loopErr = fmt.Errorf("store offset: %w", err)
				e.state.Store(engineStateStopping)
				break
			}

			// Expected during a rebalance, and nothing is lost. The offset was
			// never stored, so the partition's committed offset still points at
			// this message, and no later message from it can reach us to store a
			// higher one — it is no longer assigned. It is therefore redelivered
			// to whichever consumer holds the partition next, including this one:
			// an eager rebalance unassigns every partition, so a reassignment
			// resets the fetch position back to the committed offset.
			e.logger.Debug().Err(err).
				Int64("offset", msg.Offset).
				Int32("partition", msg.Partition).
				Msg("offset not stored, partition revoked")
			continue
		}

		// Unlike a store failure, a failed commit loses nothing: the offset stays
		// in the store and the next commit covers it. The cost is a replay if the
		// process dies first, which at-least-once already allows.
		if err := e.adapter.CommitStored(); err != nil {
			e.logger.Warn().Err(err).
				Int64("offset", msg.Offset).
				Int32("partition", msg.Partition).
				Msg("commit failed, offset remains stored")
		}
	}

	return loopErr
}

// runBatchLoop runs the batch-mode polling loop.
// Messages are accumulated in a buffer and dispatched when the batch is
// full or the batch timeout expires. Offsets are committed atomically
// for the highest offset in each batch.
func (e *Engine) runBatchLoop(ctx context.Context) error {
	buf := e.buf
	var loopErr error

	for e.state.Load() == engineStateRunning {
		select {
		case <-ctx.Done():
			e.state.Store(engineStateStopping)
			// The buffer is discarded, not dispatched. Its messages were polled
			// but never stored, so they are re-read by whoever holds the
			// partition next. Dispatching them instead would run a bulk handler
			// while the consumer is stopping, hand it a dead context, and let
			// the error strategy advance offsets over work that never happened.
			continue
		default:
		}

		if e.state.Load() != engineStateRunning {
			break
		}

		// Poll for messages
		msg, err := e.adapter.Poll(ctx, e.pollTimeout)
		if err != nil {
			e.logger.Error().Err(err).Msg("fatal polling error")
			loopErr = fmt.Errorf("polling error: %w", err)
			e.state.Store(engineStateStopping)
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
					e.state.Store(engineStateStopping)
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

	// Store the highest offset per topic-partition in the batch. A batch may span
	// several partitions, so find the maximum offset for each (topic, partition)
	// pair. Storing the maximum asserts that everything below it is accounted for,
	// which holds because the batch is handled — or written off — as a unit.
	type topicPartition struct {
		Topic     string
		Partition int32
	}
	highest := make(map[topicPartition]int64)
	for _, m := range msgs {
		tp := topicPartition{Topic: m.Topic, Partition: m.Partition}
		if off, ok := highest[tp]; !ok || m.Offset > off {
			highest[tp] = m.Offset
		}
	}

	// One call per partition, so each gets its own classified result. A single
	// multi-partition call would surface only one error and collapse the
	// distinction between a revoked partition and a real failure — which now
	// have opposite handling.
	var fatal error
	for tp, offset := range highest {
		err := e.adapter.StoreOffset(tp.Topic, tp.Partition, offset)
		switch {
		case err == nil:
		case errors.Is(err, types.ErrPartitionRevoked):
			// Routine: this partition moved to another consumer mid-batch. It
			// resumes from the last commit and redelivers.
			e.logger.Debug().Err(err).
				Int64("offset", offset).
				Int32("partition", tp.Partition).
				Msg("batch offset not stored, partition revoked")
		default:
			// Remember it, but keep storing the partitions still ours — their
			// offsets are legitimately processed, and no break here is deliberate.
			fatal = errors.Join(fatal, err)
		}
	}

	// Commit whatever did store, including on the fatal path: those offsets are
	// legitimately processed, and committing them shrinks the replay on restart.
	if err := e.adapter.CommitStored(); err != nil {
		e.logger.Warn().Err(err).Msg("commit failed, batch offsets remain stored")
	}

	if fatal != nil {
		e.logger.Error().Err(fatal).Msg("failed to store batch offset, stopping consumer")
		return fmt.Errorf("store offset: %w", fatal)
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
