package subscribe

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/easykafka/easykafka-go/internal/logcode"
	"github.com/easykafka/easykafka-go/internal/subscribe/batch"
	"github.com/easykafka/easykafka-go/internal/subscribe/metadata"
	"github.com/easykafka/easykafka-go/internal/subscribe/subscribedriver"
	"github.com/easykafka/easykafka-go/internal/subscribe/types"
)

// defaultPollTimeoutMs is the poll timeout used should the configured one round
// down to zero milliseconds.
const defaultPollTimeoutMs = 100

// Subscriber reads a topic as a member of a consumer group and hands every
// record to the handler. Create it with New, then call Start.
type Subscriber struct {
	config Config

	// state is the subscriber's lifecycle: created, then running from the first
	// Start, then stopping and stopped. It never goes back, so a subscriber that
	// has run cannot be restarted. Stopping is the caller cancelling their own
	// context, and they need nothing from us to know they did it.
	state atomic.Int32

	// driver is the Kafka consumer, built by Start.
	driver subscribedriver.Consumer

	// buf accumulates messages in batch mode. It lives on the Subscriber rather
	// than inside runBatchLoop so the revoke hook can discard it. Only ever
	// touched from the polling goroutine — see the hook registration in run.
	buf *batch.Buffer
}

const (
	stateCreated int32 = iota
	stateRunning
	stateStopping
	stateStopped
)

// New creates a new Subscriber with the provided options.
// Returns error if required options are missing or invalid.
//
// Required options: WithTopic, WithBrokers, WithConsumerGroup, and exactly one of WithHandler/WithBatchHandler.
func New(options ...Option) (*Subscriber, error) {
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

	return &Subscriber{config: cfg}, nil
}

// Start begins consuming messages from the configured topic. It blocks until
// the context is cancelled or a fatal error occurs, and returns only once the
// subscriber has fully stopped: the poll loop has exited, the final offsets are
// committed and the Kafka connection is closed — so its return is the caller's
// join point, with nothing left running.
//
// Cancelling the context is the only way to stop a subscriber. A subscriber is
// single-use — a second Start returns an error.
func (s *Subscriber) Start(ctx context.Context) error {
	// Claim the subscriber. Exactly one caller can win the swap, so a second
	// Start — including one racing the first — is rejected here. Every state
	// other than created means Start has already run.
	if !s.state.CompareAndSwap(stateCreated, stateRunning) {
		return errors.New("subscriber already started or stopped")
	}
	defer s.state.Store(stateStopped)

	s.config.Logger.Info().
		Str("topic", s.config.Topic).
		Strs("brokers", s.config.Brokers).
		Str("group", s.config.ConsumerGroup).
		Str("mode", string(s.config.Mode)).
		Str("error_strategy", s.config.ErrorStrategy.Name()).
		Int("poll_timeout_ms", s.pollTimeoutMs()).
		Msg("subscriber starting")

	// Initialize the error strategy first. A retry strategy that another running
	// subscriber holds rejects the claim here, before SetLogger below could
	// replace that subscriber's logger.
	if init, ok := s.config.ErrorStrategy.(types.Initializable); ok {
		initCfg := types.InitConfig{
			Brokers:       s.config.Brokers,
			ConsumerGroup: s.config.ConsumerGroup,
			Handler:       s.config.Handler,
			Logger:        s.config.Logger,
			KafkaConfig:   s.config.KafkaConfig,
		}
		if err := init.Initialize(initCfg); err != nil {
			return fmt.Errorf("failed to initialize error strategy: %w", err)
		}
	}

	// Wire logger into error strategy if it supports it
	if la, ok := s.config.ErrorStrategy.(types.LoggerAware); ok {
		la.SetLogger(s.config.Logger)
	}

	driver, err := s.config.newConsumer(s.driverConfig())
	if err != nil {
		// Clean up strategy if initialized
		if init, ok := s.config.ErrorStrategy.(types.Initializable); ok {
			_ = init.Close()
		}
		return fmt.Errorf("failed to create kafka consumer: %w", err)
	}
	s.driver = driver
	if s.config.Mode == ModeBatch {
		s.buf = batch.NewBuffer(s.config.BatchSize, s.config.BatchTimeout)
	}

	s.config.Logger.Info().Msg("subscriber running")

	// Run the poll loop (blocks until the caller's context is cancelled or a
	// fatal error occurs). The context goes through as given: there is nothing
	// left that would cancel it from this side.
	err = s.run(ctx)

	// Clean up strategy resources
	if init, ok := s.config.ErrorStrategy.(types.Initializable); ok {
		_ = init.Close()
	}

	s.config.Logger.Info().Err(err).Msg("subscriber stopped")

	return err
}

// GetConfig returns the Config of a Subscriber for testing/inspection purposes.
func GetConfig(s *Subscriber) Config {
	return s.config
}

// driverConfig is what the driver needs to build the consumer.
func (s *Subscriber) driverConfig() subscribedriver.Config {
	return subscribedriver.Config{
		Brokers:        s.config.Brokers,
		Topic:          s.config.Topic,
		GroupID:        s.config.ConsumerGroup,
		KafkaConfig:    s.config.KafkaConfig,
		CommitInterval: s.config.AutoCommitEvery,
		Logger:         s.config.Logger,
	}
}

// pollTimeoutMs is the poll timeout in milliseconds, as the driver takes it.
func (s *Subscriber) pollTimeoutMs() int {
	pollTimeoutMs := int(s.config.PollTimeout / time.Millisecond)
	if pollTimeoutMs < 1 {
		return defaultPollTimeoutMs
	}
	return pollTimeoutMs
}

// run connects, subscribes and runs the poll loop until it ends, then commits
// what is stored and closes the connection.
func (s *Subscriber) run(ctx context.Context) error {
	// Connect to Kafka
	if err := s.driver.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	// Discard buffered work when partitions are revoked, before subscribing so the
	// hook is in place for the first rebalance. Only batch mode buffers anything;
	// single mode holds at most the message it is currently handling.
	//
	// The rebalance callback runs on the goroutine that calls Poll, and this
	// subscriber polls from a single goroutine, so the buffer needs no locking.
	if s.config.Mode == ModeBatch {
		s.driver.SetOnRevoke(func() {
			if dropped := s.buf.Drop(); dropped > 0 {
				s.config.Logger.Debug().Int("dropped", dropped).
					Msg("discarded buffered messages for revoked partitions")
			}
		})
	}

	// Subscribe to topic
	if err := s.driver.SubscribeToTopic(ctx); err != nil {
		_ = s.driver.Close(ctx)
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	var loopErr error

	if s.config.Mode == ModeBatch {
		loopErr = s.runBatchLoop(ctx)
	} else {
		loopErr = s.runSingleLoop(ctx)
	}

	// Publish anything stored but not yet committed, before the consumer goes
	// away. Unconditional, unlike the per-message commits: in the default cadence
	// it is near-redundant and costs one call, but under WithAutoCommitEvery it
	// is what stands between a clean shutdown and replaying up to a full
	// interval. librdkafka's own Close() commits the store too, so this is the
	// first of two backstops rather than the only one.
	if err := s.driver.CommitStored(); err != nil {
		s.config.Logger.Warn().Str(logcode.Field, logcode.CommitFailed).Str("commit", "final").Err(err).
			Msg("final commit failed, offsets remain stored")
	}

	// Cleanup
	if err := s.driver.Close(ctx); err != nil {
		s.config.Logger.Error().Str(logcode.Field, logcode.ConsumerCloseFailed).Err(err).
			Msg("error closing the kafka consumer")
	}

	return loopErr
}

// runSingleLoop runs the single-message polling loop.
func (s *Subscriber) runSingleLoop(ctx context.Context) error {
	var loopErr error

	// Main polling loop
	for s.state.Load() == stateRunning {
		select {
		case <-ctx.Done():
			s.state.Store(stateStopping)
			continue
		default:
		}

		if s.state.Load() != stateRunning {
			break
		}

		// Poll for messages
		msg, err := s.driver.Poll(ctx, s.pollTimeoutMs())
		if err != nil {
			s.config.Logger.Error().Str(logcode.Field, logcode.PollFatal).Err(err).Msg("fatal polling error")
			loopErr = fmt.Errorf("polling error: %w", err)
			s.state.Store(stateStopping)
			break
		}

		if msg == nil {
			// Timeout or non-message event, continue polling
			continue
		}

		// Attach a copy of the message to the handler's context, so nothing the
		// handler does to it can move the offset stored below from msg. The
		// payload and headers are still shared, as in batch mode.
		view := *msg
		handlerCtx := metadata.WithMessage(ctx, &view)

		// Call handler with panic recovery
		failure := s.dispatchMessage(handlerCtx, msg)

		if failure != nil {
			// Handler failed, apply error strategy
			failed := []*types.Message{msg}
			strategyErr := s.config.ErrorStrategy.HandleError(ctx, failed, s.withReason(*failure, failed))
			if strategyErr != nil {
				// Strategy says stop consumer (e.g., fail-fast)
				s.config.Logger.Error().Str(logcode.Field, logcode.StoppedByStrategy).Err(strategyErr).
					Msg("error strategy returned fatal error, stopping")
				loopErr = fmt.Errorf("error strategy: %w", strategyErr)
				s.state.Store(stateStopping)
				break
			}
		}

		// (Handler succeeded) or (handler failed and error strategy succeeded) =>
		// the message is accounted for, so record it and publish the record.
		if err := s.driver.StoreOffset(msg.Topic, msg.Partition, msg.Offset); err != nil {
			if !errors.Is(err, subscribedriver.ErrPartitionRevoked) {
				// Continuing here would lose this message: the next message on
				// this partition stores a higher offset, and committing that
				// silently declares this one done. Stop instead.
				s.config.Logger.Error().Str(logcode.Field, logcode.OffsetStoreFailed).Err(err).
					Int64("offset", msg.Offset).
					Int32("partition", msg.Partition).
					Msg("failed to store offset, stopping consumer")
				loopErr = fmt.Errorf("store offset: %w", err)
				s.state.Store(stateStopping)
				break
			}

			// Expected during a rebalance, and nothing is lost. The offset was
			// never stored, so the partition's committed offset still points at
			// this message, and no later message from it can reach us to store a
			// higher one — it is no longer assigned. It is therefore redelivered
			// to whichever consumer holds the partition next, including this one:
			// an eager rebalance unassigns every partition, so a reassignment
			// resets the fetch position back to the committed offset.
			s.config.Logger.Debug().Err(err).
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
		if err := s.driver.MaybeCommitStored(); err != nil {
			s.config.Logger.Warn().Str(logcode.Field, logcode.CommitFailed).Str("commit", "per_message").Err(err).
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
func (s *Subscriber) runBatchLoop(ctx context.Context) error {
	buf := s.buf
	var loopErr error

	for s.state.Load() == stateRunning {
		select {
		case <-ctx.Done():
			s.state.Store(stateStopping)
			// The buffer is discarded, not dispatched. Its messages were polled
			// but never stored, so they are re-read by whoever holds the
			// partition next. Dispatching them instead would run a bulk handler
			// while the subscriber is stopping, hand it a dead context, and let
			// the error strategy advance offsets over work that never happened.
			continue
		default:
		}

		if s.state.Load() != stateRunning {
			break
		}

		// Poll for messages
		msg, err := s.driver.Poll(ctx, s.pollTimeoutMs())
		if err != nil {
			s.config.Logger.Error().Str(logcode.Field, logcode.PollFatal).Err(err).Msg("fatal polling error")
			loopErr = fmt.Errorf("polling error: %w", err)
			s.state.Store(stateStopping)
			break
		}

		if msg != nil {
			buf.Add(msg)
		}

		// Dispatch batch when full or timed out
		if buf.Ready() || buf.TimedOut() {
			msgs := buf.Flush()
			if msgs != nil {
				if err := s.dispatchBatch(ctx, msgs); err != nil {
					loopErr = err
					s.state.Store(stateStopping)
					break
				}
			}
		}
	}

	return loopErr
}

// dispatchBatch calls the batch handler with panic recovery, routes each
// failure to the error strategy, and stores the highest offset per partition.
func (s *Subscriber) dispatchBatch(ctx context.Context, msgs []*types.Message) error {
	polled := types.NewBatchFromBuffer(msgs)

	// Call batch handler with panic recovery; a panic comes back as a failure.
	whole := s.invokeBatchHandler(ctx, polled)

	// A strategy error skips the offset store below: a message the strategy
	// could not resolve exists nowhere else, and storing a higher offset from
	// the same partition would lose it.
	if err := s.routeBatchFailures(ctx, msgs, polled, whole); err != nil {
		s.config.Logger.Error().Str(logcode.Field, logcode.StoppedByStrategy).Err(err).
			Msg("error strategy returned fatal error, stopping")
		return fmt.Errorf("error strategy: %w", err)
	}

	// Reachable only when every message in the batch is resolved.
	fatal := s.storeHighestOffsetPerPartition(msgs)

	// Commit whatever did store, including on the fatal path: those offsets are
	// legitimately processed, and committing them shrinks the replay on restart.
	//
	// Maybe, not must: under WithAutoCommitEvery this does nothing and
	// librdkafka's background committer publishes the store on its own schedule.
	if err := s.driver.MaybeCommitStored(); err != nil {
		s.config.Logger.Warn().Str(logcode.Field, logcode.CommitFailed).Str("commit", "batch").Err(err).
			Msg("commit failed, batch offsets remain stored")
	}

	if fatal != nil {
		s.config.Logger.Error().Str(logcode.Field, logcode.OffsetStoreFailed).Err(fatal).
			Msg("failed to store batch offset, stopping consumer")
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
func (s *Subscriber) routeBatchFailures(
	ctx context.Context,
	msgs []*types.Message,
	polled *types.Batch,
	whole *types.Failure,
) error {

	if whole != nil {
		return s.config.ErrorStrategy.HandleError(ctx, msgs, s.withReason(*whole, msgs))
	}

	for _, item := range polled.Items() {
		failure := item.Failed()
		if failure == nil {
			continue
		}
		msg := item.Message()
		failed := []*types.Message{&msg}
		if err := s.config.ErrorStrategy.HandleError(ctx, failed, s.withReason(*failure, failed)); err != nil {
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
func (s *Subscriber) storeHighestOffsetPerPartition(msgs []*types.Message) error {
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
		err := s.driver.StoreOffset(tp.Topic, tp.Partition, offset)
		switch {
		case err == nil:
		case errors.Is(err, subscribedriver.ErrPartitionRevoked):
			// Routine: this partition moved to another consumer mid-batch. It
			// resumes from the last commit and redelivers.
			s.config.Logger.Debug().Err(err).
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
func (s *Subscriber) invokeBatchHandler(ctx context.Context, polled *types.Batch) (failure *types.Failure) {
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			err := fmt.Errorf("handler panic: %v", r)
			s.config.Logger.Error().
				Str(logcode.Field, logcode.HandlerPanic).
				Err(err).
				Str("stack", stack).
				Int("batch_size", polled.Len()).
				Msg("batch handler panic recovered")
			failure = &types.Failure{Err: err}
		}
	}()

	return s.config.BatchHandler(ctx, polled)
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
func (s *Subscriber) dispatchMessage(ctx context.Context, msg *types.Message) (failure *types.Failure) {
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			err := fmt.Errorf("handler panic: %v", r)
			s.config.Logger.Error().
				Str(logcode.Field, logcode.HandlerPanic).
				Err(err).
				Str("stack", stack).
				Int64("offset", msg.Offset).
				Int32("partition", msg.Partition).
				Msg("handler panic recovered")
			failure = &types.Failure{Err: err}
		}
	}()

	return s.config.Handler(ctx, msg.Payload)
}

// withReason fills in the error of a failure a handler recorded without one, so
// that no strategy ever sees a nil error. The messages are still routed: the
// handler said they failed, which is the part that matters.
func (s *Subscriber) withReason(f types.Failure, msgs []*types.Message) types.Failure {
	if f.Err != nil {
		return f
	}
	f.Err = types.ErrUnspecified

	event := s.config.Logger.Warn().Str(logcode.Field, logcode.FailureWithoutError)
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
