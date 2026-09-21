package kafka

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/rs/zerolog"

	"github.com/easykafka/easykafka-go/internal/types"
)

// Adapter is the production KafkaClient: everything the engine needs from Kafka,
// and the only place confluent-kafka-go is touched.
var _ types.KafkaClient = (*Adapter)(nil)

// Adapter wraps confluent-kafka-go for the consumer.
type Adapter struct {
	consumer *kfk.Consumer
	config   *kfk.ConfigMap
	brokers  []string
	topic    string
	groupID  string
	logger   zerolog.Logger

	// autoCommit is set when the caller asked for interval commits, in which
	// case librdkafka's background committer owns commit timing and the
	// engine's per-message commits would be redundant work.
	autoCommit bool

	// mu guards brokerConnected, which Poll updates as the transport drops and
	// recovers.
	mu              sync.Mutex
	brokerConnected bool

	// onRevoke is invoked from the rebalance callback when partitions are
	// revoked, so the engine can discard work it no longer owns. The rebalance
	// callback runs on the goroutine that calls Poll, so this needs no locking.
	onRevoke func()
}

// SetOnRevoke registers a function invoked when partitions are revoked.
// Must be called before SubscribeToTopic.
func (a *Adapter) SetOnRevoke(fn func()) {
	a.onRevoke = fn
}

// NewAdapter creates a new Kafka adapter from configuration.
func NewAdapter(
	brokers []string,
	topic string,
	groupID string,
	kafkaConfig map[string]any,
	commitInterval time.Duration,
	logger zerolog.Logger,
) (*Adapter, error) {

	if len(brokers) == 0 {
		return nil, fmt.Errorf("at least one broker is required")
	}
	if topic == "" {
		return nil, fmt.Errorf("topic cannot be empty")
	}
	if groupID == "" {
		return nil, fmt.Errorf("group ID cannot be empty")
	}

	// Build confluent-kafka-go config
	config := &kfk.ConfigMap{}
	if err := config.SetKey("bootstrap.servers", strings.Join(brokers, ",")); err != nil {
		return nil, fmt.Errorf("setting bootstrap.servers: %w", err)
	}
	if err := config.SetKey("group.id", groupID); err != nil {
		return nil, fmt.Errorf("setting group.id: %w", err)
	}
	if err := config.SetKey("auto.offset.reset", "earliest"); err != nil {
		return nil, fmt.Errorf("setting auto.offset.reset: %w", err)
	}

	// Commit cadence. Unset, the library commits after every message and owns
	// timing entirely. With an interval, librdkafka's background committer owns
	// it instead and publishes whatever is in the store every commitInterval.
	//
	// Either way the store is ours — see enable.auto.offset.store below — so the
	// background committer can only ever publish offsets the engine put there
	// after a message was accounted for. Handing over the timing does not hand
	// over what gets committed.
	autoCommit := commitInterval > 0
	if err := config.SetKey("enable.auto.commit", autoCommit); err != nil {
		return nil, fmt.Errorf("setting enable.auto.commit: %w", err)
	}
	if autoCommit {
		intervalMs := int(commitInterval / time.Millisecond)
		if err := config.SetKey("auto.commit.interval.ms", intervalMs); err != nil {
			return nil, fmt.Errorf("setting auto.commit.interval.ms: %w", err)
		}
	}

	// Take over the offset store as well. Left at its default of true, librdkafka
	// stores the offset of every message the moment Poll hands it to the
	// application — before the handler has run, or even been called at all in
	// batch mode. Committing that store would then publish work that never
	// happened. With it off, the store holds only what the engine puts there
	// after a message is accounted for, which is what makes every commit path
	// safe. StoreOffsets also refuses to run at all while this is true.
	if err := config.SetKey("enable.auto.offset.store", false); err != nil {
		return nil, fmt.Errorf("setting enable.auto.offset.store: %w", err)
	}

	// Pin the eager rebalance protocol. Two things here depend on every partition
	// being revoked at once: the rebalance callback below calls Assign/Unassign
	// rather than their Incremental counterparts, and on revoke the engine drops
	// its whole batch buffer rather than the revoked partitions' share of it.
	//
	// That drop is only safe under an eager revoke. Everything is unassigned, so
	// the partitions come back with their fetch positions reset to the committed
	// offset, and the dropped messages are redelivered. Under a cooperative
	// strategy only a subset is revoked: the partitions we keep hold their fetch
	// positions, so buffered messages for them are never re-read — and the next
	// message on that partition stores a higher offset whose commit seals the gap.
	// The messages are not redelivered and not recorded as failed. They are simply
	// gone.
	//
	// librdkafka already defaults to these two strategies, but a default is not
	// something to rest a correctness argument on, so set it explicitly. Supporting
	// cooperative rebalancing means making both of those call sites
	// partition-aware; until then, reject it rather than lose messages quietly.
	if err := config.SetKey("partition.assignment.strategy", "range,roundrobin"); err != nil {
		return nil, fmt.Errorf("setting partition.assignment.strategy: %w", err)
	}

	// Configure reconnection backoff defaults.
	// confluent-kafka-go (librdkafka) handles automatic reconnection natively.
	// These defaults ensure reasonable backoff with exponential increase.
	reconnectDefaults := map[string]any{
		"reconnect.backoff.ms":     100,   //nolint:mnd // initial backoff
		"reconnect.backoff.max.ms": 10000, //nolint:mnd // max backoff (10s)
	}
	for key, value := range reconnectDefaults {
		if err := config.SetKey(key, value); err != nil {
			return nil, fmt.Errorf("setting %s: %w", key, err)
		}
	}

	// Apply any additional Kafka configuration (user can override defaults including reconnect settings)
	for key, value := range kafkaConfig {
		if err := config.SetKey(key, value); err != nil {
			return nil, fmt.Errorf("setting kafka config %s: %w", key, err)
		}
	}

	return &Adapter{
		config:     config,
		brokers:    brokers,
		topic:      topic,
		groupID:    groupID,
		logger:     logger,
		autoCommit: autoCommit,
	}, nil
}

// Connect establishes a connection to Kafka.
func (a *Adapter) Connect(ctx context.Context) error {
	if a.consumer != nil {
		return fmt.Errorf("consumer already connected")
	}

	consumer, err := kfk.NewConsumer(a.config)
	if err != nil {
		return fmt.Errorf("failed to create kafka consumer: %w", err)
	}

	a.consumer = consumer

	a.mu.Lock()
	a.brokerConnected = true
	a.mu.Unlock()

	a.logger.Info().
		Strs("brokers", a.brokers).
		Str("topic", a.topic).
		Str("group", a.groupID).
		Msg("kafka consumer connected")

	return nil
}

// SubscribeToTopic subscribes the consumer to the topic with rebalance handling.
func (a *Adapter) SubscribeToTopic(ctx context.Context) error {
	if a.consumer == nil {
		return fmt.Errorf("consumer not connected")
	}

	err := a.consumer.SubscribeTopics([]string{a.topic}, a.rebalanceCallback)
	if err != nil {
		return fmt.Errorf("failed to subscribe to topic %s: %w", a.topic, err)
	}

	a.logger.Info().Str("topic", a.topic).Msg("subscribed to topic")

	return nil
}

// rebalanceCallback handles partition assignment and revocation.
func (a *Adapter) rebalanceCallback(c *kfk.Consumer, event kfk.Event) error {
	switch ev := event.(type) {
	case kfk.AssignedPartitions:
		partitions := make([]string, 0, len(ev.Partitions))
		for _, tp := range ev.Partitions {
			partitions = append(partitions, fmt.Sprintf("%s[%d]", *tp.Topic, tp.Partition))
		}
		a.logger.Info().
			Strs("partitions", partitions).
			Msg("partitions assigned")

		if err := c.Assign(ev.Partitions); err != nil {
			a.logger.Error().Err(err).Msg("failed to assign partitions")
			return err
		}

	case kfk.RevokedPartitions:
		partitions := make([]string, 0, len(ev.Partitions))
		for _, tp := range ev.Partitions {
			partitions = append(partitions, fmt.Sprintf("%s[%d]", *tp.Topic, tp.Partition))
		}
		a.logger.Info().
			Strs("partitions", partitions).
			Msg("partitions revoked")

		// Stop caring about work we are about to give away. Buffered messages for
		// these partitions were never stored, so dropping them cannot affect the
		// commit below — it only avoids processing them for a partition that now
		// belongs to someone else, who will process them too.
		//
		// Under the eager protocol every partition is revoked at once, so the
		// engine drops its whole buffer. That equivalence would not hold under
		// cooperative-sticky, which revokes a subset.
		if a.onRevoke != nil {
			a.onRevoke()
		}

		// Commit current offsets before revocation. Safe because
		// enable.auto.offset.store is off: the store holds only offsets the
		// engine put there after a message was accounted for, so this can no
		// longer publish messages that were polled but never processed.
		if err := a.CommitStored(); err != nil {
			a.logger.Warn().Err(err).Msg("failed to commit offsets during revocation")
		}

		if err := c.Unassign(); err != nil {
			a.logger.Error().Err(err).Msg("failed to unassign partitions")
			return err
		}
	}

	return nil
}

// Poll retrieves messages from Kafka with timeout in milliseconds.
// Uses Poll() for proper event handling including rebalance events.
func (a *Adapter) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	if a.consumer == nil {
		return nil, fmt.Errorf("consumer not connected")
	}

	ev := a.consumer.Poll(timeoutMs)
	if ev == nil {
		// Timeout, no event available
		return nil, nil //nolint:nilnil // a nil message with a nil error means "nothing polled"
	}

	switch e := ev.(type) {
	case *kfk.Message:
		// Detect reconnection — receiving a message means the broker is available
		a.mu.Lock()
		wasDisconnected := !a.brokerConnected
		a.brokerConnected = true
		a.mu.Unlock()
		if wasDisconnected {
			a.logger.Info().Msg("broker connection restored, resuming message consumption")
		}

		// Convert confluent message to our Message type
		headers := make(map[string]string)
		for _, h := range e.Headers {
			headers[h.Key] = string(h.Value)
		}

		return &types.Message{
			Topic:     *e.TopicPartition.Topic,
			Partition: e.TopicPartition.Partition,
			Offset:    int64(e.TopicPartition.Offset),
			Timestamp: e.Timestamp,
			Headers:   headers,
			Payload:   e.Value,
		}, nil

	case kfk.Error:
		// Handle Kafka errors
		if e.IsFatal() {
			return nil, fmt.Errorf("fatal kafka error: %w", e)
		}

		// Reconnection-aware logging for broker transport errors.
		// confluent-kafka-go handles reconnection automatically via librdkafka;
		// we log state transitions so operators can observe disconnect/reconnect cycles.
		switch e.Code() {
		case kfk.ErrTransport, kfk.ErrAllBrokersDown:
			// Read and write in one critical section, as the message branch above
			// does: a split would decide on a value another goroutine could have
			// changed in between.
			a.mu.Lock()
			wasConnected := a.brokerConnected
			a.brokerConnected = false
			a.mu.Unlock()

			if wasConnected {
				a.logger.Warn().Err(e).Int("code", int(e.Code())).
					Msg("broker connection lost, librdkafka will reconnect automatically")
			} else {
				a.logger.Debug().Err(e).Int("code", int(e.Code())).
					Msg("broker still unavailable, reconnection in progress")
			}
		default:
			a.logger.Warn().Err(e).Int("code", int(e.Code())).Msg("non-fatal kafka error")
		}
		return nil, nil //nolint:nilnil // a nil message with a nil error means "nothing polled"

	case kfk.OffsetsCommitted:
		// The background committer's only channel back to us. Without this the
		// event falls to default: and a coordinator rejecting every commit is
		// silent, while the duplicate window grows with nothing to show for it.
		//
		// A failed commit cannot lose a message — the store advances only on
		// messages that were handled, so the committed offset can never run ahead
		// of the work, and the cost is replay. It is logged at warning rather than
		// error for that reason, and logged at all because a *persistent* failure
		// is operationally significant: a consumer that has not committed for an
		// hour replays an hour on restart.
		if e.Error != nil {
			a.logger.Warn().Err(e.Error).
				Int("partitions", len(e.Offsets)).
				Msg("auto-commit failed, offsets remain stored")
		} else {
			a.logger.Debug().
				Int("partitions", len(e.Offsets)).
				Msg("auto-commit published stored offsets")
		}
		return nil, nil //nolint:nilnil // a nil message with a nil error means "nothing polled"

	default:
		// Other events (rebalance, stats, etc.) handled via callbacks
		return nil, nil //nolint:nilnil // a nil message with a nil error means "nothing polled"
	}
}

// StoreOffset records that a message has been accounted for, so that the next
// commit will include it. The stored value is offset+1: Kafka's committed offset
// is a resume position — the next message to read — not a high-water mark of what
// has been done. This is the only place that +1 is applied.
//
// Returns ErrPartitionRevoked if the partition is no longer assigned, which is an
// ordinary rebalance race rather than a failure.
func (a *Adapter) StoreOffset(topic string, partition int32, offset int64) error {
	if a.consumer == nil {
		return fmt.Errorf("consumer not connected")
	}

	topicStr := topic
	stored, storeErr := a.consumer.StoreOffsets([]kfk.TopicPartition{{
		Topic:     &topicStr,
		Partition: partition,
		Offset:    kfk.Offset(offset + 1),
	}})

	// Check the result slice first: those entries name the partition, so the
	// error can too. Then the top-level error, which is where a call rejected
	// outright shows up — with the per-partition error left nil.
	for _, tp := range stored {
		if tp.Error != nil {
			return classifyStoreErr(tp.Error, *tp.Topic, tp.Partition)
		}
	}
	if storeErr != nil {
		return classifyStoreErr(storeErr, topic, partition)
	}

	return nil
}

// classifyStoreErr turns a librdkafka store error into either the revoked-partition
// sentinel or an ordinary wrapped error.
func classifyStoreErr(err error, topic string, partition int32) error {
	if isNotAssigned(err) {
		return fmt.Errorf("%w: %s[%d]", types.ErrPartitionRevoked, topic, partition)
	}
	return fmt.Errorf("failed to store offset for %s[%d]: %w", topic, partition, err)
}

// isNotAssigned reports whether err means "this consumer does not currently hold
// that partition".
//
// librdkafka answers ErrState both for a partition that was revoked and for one
// that never existed — it means "not in a state to accept this offset", not
// specifically "revoked". That ambiguity is safe here and only here, because the
// engine stores offsets only for messages Poll handed it, so the partition always
// existed and was always assigned. Called from anywhere else, this would quietly
// swallow a bad partition number.
func isNotAssigned(err error) bool {
	var kfkErr kfk.Error
	return errors.As(err, &kfkErr) && kfkErr.Code() == kfk.ErrState
}

// MaybeCommitStored publishes the store once a message or batch has been
// accounted for — unless librdkafka is already committing on an interval, in
// which case the background committer owns timing and this does nothing.
//
// It exists so the engine's flow reads the same in both modes. With
// WithAutoCommitEvery unset, which is the default, this commits after every
// message: the narrowest possible duplicate window, and the library's existing
// behaviour. Callers that must persist progress before it can be lost — a
// revocation, shutdown — want CommitStored instead, which always commits.
func (a *Adapter) MaybeCommitStored() error {
	if a.autoCommit {
		return nil
	}
	return a.CommitStored()
}

// CommitStored commits the offsets currently in librdkafka's store for this
// consumer's assignment. It always commits, whatever the configured cadence, and
// is for the points where progress must be persisted before it can be lost: a
// revocation, and shutdown.
//
// Because enable.auto.offset.store is off, the store holds only offsets the
// engine put there after a message was accounted for, so this is safe to call
// from any point. An empty store is not an error.
func (a *Adapter) CommitStored() error {
	if a.consumer == nil {
		return fmt.Errorf("consumer not connected")
	}

	committed, err := a.consumer.Commit()
	if err != nil {
		var kfkErr kfk.Error
		if errors.As(err, &kfkErr) && kfkErr.Code() == kfk.ErrNoOffset {
			a.logger.Debug().Msg("no stored offsets to commit")
			return nil
		}
		return fmt.Errorf("failed to commit stored offsets: %w", err)
	}

	a.logger.Debug().Int("committed", len(committed)).Msg("stored offsets committed")

	return nil
}

// Close gracefully closes the Kafka connection.
func (a *Adapter) Close(ctx context.Context) error {
	if a.consumer == nil {
		return nil
	}

	err := a.consumer.Close()
	a.consumer = nil

	if err != nil {
		return fmt.Errorf("failed to close kafka consumer: %w", err)
	}

	a.logger.Info().Msg("kafka consumer closed")

	return nil
}
