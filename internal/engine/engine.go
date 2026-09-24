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
	// away. Unconditional, unlike the per-message commits: in the default cadence
	// it is near-redundant and costs one call, but under WithAutoCommitEvery it
	// is what stands between a clean shutdown and replaying up to a full
	// interval. librdkafka's own Close() commits the store too, so this is the
	// first of two backstops rather than the only one.
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
		failure := e.dispatchMessage(handlerCtx, msg)

		if failure != nil {
			// Handler failed, apply error strategy
			failed := []*types.Message{msg}
			strategyErr := e.strategy.HandleError(ctx, failed, e.withReason(*failure, failed))
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
		//
		// Maybe, not must: under WithAutoCommitEvery this does nothing and
		// librdkafka's background committer publishes the store on its own
		// schedule. Read this line as "commit unless someone else is".
		if err := e.adapter.MaybeCommitStored(); err != nil {
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
// full or the batch timeout expires. Once every message in a batch is
// resolved, the highest offset per partition is stored and committed.
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

// dispatchBatch calls the batch handler with panic recovery, routes each
// failure to the error strategy, and stores the highest offset per partition.
func (e *Engine) dispatchBatch(ctx context.Context, msgs []*types.Message) error {
	batch := types.NewBatchFromBuffer(msgs)

	// Call batch handler with panic recovery; a panic comes back as a failure.
	whole := e.invokeBatchHandler(ctx, batch)

	// A strategy error skips the offset store below: a message the strategy
	// could not resolve exists nowhere else, and storing a higher offset from
	// the same partition would lose it.
	if err := e.routeBatchFailures(ctx, msgs, batch, whole); err != nil {
		e.logger.Error().Err(err).Msg("error strategy returned fatal error, stopping")
		return fmt.Errorf("error strategy: %w", err)
	}

	// Reachable only when every message in the batch is resolved.
	fatal := e.storeHighestOffsetPerPartition(msgs)

	// Commit whatever did store, including on the fatal path: those offsets are
	// legitimately processed, and committing them shrinks the replay on restart.
	//
	// Maybe, not must: under WithAutoCommitEvery this does nothing and
	// librdkafka's background committer publishes the store on its own schedule.
	if err := e.adapter.MaybeCommitStored(); err != nil {
		e.logger.Warn().Err(err).Msg("commit failed, batch offsets remain stored")
	}

	if fatal != nil {
		e.logger.Error().Err(fatal).Msg("failed to store batch offset, stopping consumer")
		return fmt.Errorf("store offset: %w", fatal)
	}

	return nil
}

// routeBatchFailures hands the batch's failures to the error strategy and
// returns the first error it reports.
//
// A whole-batch failure — returned by the handler, or a recovered panic — sends
// every message to the strategy in one call and discards any verdicts recorded
// per item. Otherwise each failed item goes on its own, in batch order, and the
// walk stops at the first strategy error without offering the rest.
func (e *Engine) routeBatchFailures(
	ctx context.Context,
	msgs []*types.Message,
	batch *types.Batch,
	whole *types.Failure,
) error {

	if whole != nil {
		return e.strategy.HandleError(ctx, msgs, e.withReason(*whole, msgs))
	}

	for _, item := range batch.Items() {
		failure := item.Failed()
		if failure == nil {
			continue
		}
		msg := item.Message()
		failed := []*types.Message{&msg}
		if err := e.strategy.HandleError(ctx, failed, e.withReason(*failure, failed)); err != nil {
			return err
		}
	}
	return nil
}

// storeHighestOffsetPerPartition stores the highest offset per topic-partition
// in the batch and returns every store failure other than a revoked partition,
// joined. It does not commit.
//
// A batch may span several partitions, so it finds the maximum offset for each
// (topic, partition) pair. Storing the maximum asserts that everything below it
// is accounted for, which holds only because the caller has resolved every
// message first: it succeeded, or the strategy has written it off. Anything that
// defers a message's outcome past the call turns the maximum into a claim about
// work not yet done.
//
// The offsets come from the buffered messages, not from the batch items, so
// nothing a handler does to its copies can move them.
func (e *Engine) storeHighestOffsetPerPartition(msgs []*types.Message) error {
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

	return fatal
}

// invokeBatchHandler calls the batch handler with panic recovery. A recovered
// panic comes back as a failure describing it, with no step and no code. The
// result is named so the deferred recover can set it; see dispatchMessage.
func (e *Engine) invokeBatchHandler(ctx context.Context, batch *types.Batch) (failure *types.Failure) {
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			err := fmt.Errorf("handler panic: %v", r)
			e.logger.Error().
				Err(err).
				Str("stack", stack).
				Int("batch_size", batch.Len()).
				Msg("batch handler panic recovered")
			failure = &types.Failure{Err: err}
		}
	}()

	return e.batchHandler(ctx, batch)
}

// dispatchMessage calls the handler with panic recovery.
// Any panics in the handler are recovered and returned as a failure.
//
// The result is named so the deferred recover can set it. A panicking handler
// never reaches the return, so failure is still nil when the deferred function
// runs; it assigns the panic, and that is what the caller gets. With an unnamed
// result the recover would still stop the panic, but the function would return
// nil — a panic read as success, and the message's offset stored. When the
// handler returns normally, recover yields nil and its result stands.
func (e *Engine) dispatchMessage(ctx context.Context, msg *types.Message) (failure *types.Failure) {
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			err := fmt.Errorf("handler panic: %v", r)
			e.logger.Error().
				Err(err).
				Str("stack", stack).
				Int64("offset", msg.Offset).
				Int32("partition", msg.Partition).
				Msg("handler panic recovered")
			failure = &types.Failure{Err: err}
		}
	}()

	return e.handler(ctx, msg.Payload)
}

// withReason fills in the error of a failure a handler recorded without one, so
// that no strategy ever sees a nil error. The messages are still routed: the
// handler said they failed, which is the part that matters.
func (e *Engine) withReason(f types.Failure, msgs []*types.Message) types.Failure {
	if f.Err != nil {
		return f
	}
	f.Err = types.ErrUnspecified

	event := e.logger.Warn()
	if len(msgs) == 1 {
		event = event.
			Str("topic", msgs[0].Topic).
			Int32("partition", msgs[0].Partition).
			Int64("offset", msgs[0].Offset)
	} else {
		event = event.Int("batch_size", len(msgs))
	}
	event.Msg("handler reported a failure without an error, routing it anyway")

	return f
}
