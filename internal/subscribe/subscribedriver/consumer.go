// Package subscribedriver is the subscribe package's only contact with
// confluent-kafka-go: it wraps one librdkafka consumer behind the Consumer
// interface.
//
// It builds the librdkafka configuration, subscribes with a rebalance callback,
// polls records and translates them into the subscriber's Message, and stores
// and commits offsets. Consuming here means only that: Poll hands over the next
// record. What happens to it, and when its offset is stored, is the
// subscriber's.
//
// The subscriber depends on the interface only, so:
//
//   - confluent types never reach the public API;
//   - the subscriber's poll loop is unit-tested against a fake Consumer, with
//     no broker.
//
// The architecture test guards the boundary: on the subscribe side only this
// package imports confluent-kafka-go, and this package never imports subscribe.
package subscribedriver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/rs/zerolog"

	"github.com/easykafka/easykafka-go/internal/logcode"
	"github.com/easykafka/easykafka-go/internal/subscribe/types"
)

// ErrPartitionRevoked means an offset could not be stored because the partition
// is no longer assigned to this consumer. This is expected during a rebalance:
// the message will be redelivered to whichever consumer owns the partition now,
// so callers should tolerate it rather than treat it as a failure.
//
// It is part of the Consumer.StoreOffset contract, so a fake Consumer returns
// it too.
var ErrPartitionRevoked = errors.New("partition no longer assigned")

// Consumer is the part of librdkafka the subscriber uses.
type Consumer interface {
	Connect(ctx context.Context) error
	SubscribeToTopic(ctx context.Context) error
	Poll(ctx context.Context, timeoutMs int) (*types.Message, error)

	// StoreOffset records that a message has been accounted for; CommitStored
	// publishes everything stored so far. Splitting the two is what stops a
	// rebalance from committing messages that were polled but never processed.
	//
	// StoreOffset returns ErrPartitionRevoked if the partition is no longer
	// assigned, which callers must tolerate rather than treat as a failure.
	StoreOffset(topic string, partition int32, offset int64) error
	CommitStored() error

	// MaybeCommitStored is CommitStored unless the client is committing on an
	// interval of its own, in which case it does nothing and the background
	// committer owns timing. Use it where a commit is progress-keeping and
	// skippable; use CommitStored where it must happen.
	MaybeCommitStored() error

	// SetOnRevoke registers a function invoked when partitions are revoked.
	// It is called synchronously from whichever goroutine calls Poll.
	SetOnRevoke(fn func())

	Close(ctx context.Context) error
}

// Config describes one consumer. The subscribe package builds it from its
// options, which have already validated it and rejected from KafkaConfig every
// key New sets.
type Config struct {
	Brokers []string
	Topic   string
	GroupID string
	// KafkaConfig is passed through to librdkafka. It is only read.
	KafkaConfig map[string]any
	// CommitInterval hands commit timing to librdkafka's background committer
	// when positive; zero commits after every message or batch.
	CommitInterval time.Duration
	Logger         zerolog.Logger
}

// consumer is the confluent-kafka-go implementation of Consumer.
type consumer struct {
	kafkaConsumer *kfk.Consumer
	config        *kfk.ConfigMap
	brokers       []string
	topic         string
	groupID       string
	logger        zerolog.Logger

	// autoCommit is set when the caller asked for interval commits, in which
	// case librdkafka's background committer owns commit timing and the
	// subscriber's per-message commits would be redundant work.
	autoCommit bool

	// mu guards brokerConnected, which Poll updates as the transport drops and
	// recovers.
	mu              sync.Mutex
	brokerConnected bool

	// onRevoke is invoked from the rebalance callback when partitions are
	// revoked, so the subscriber can discard work it no longer owns. The
	// rebalance callback runs on the goroutine that calls Poll, so this needs
	// no locking.
	onRevoke func()
}

// Compile-time check that *consumer implements Consumer; no runtime cost.
var _ Consumer = (*consumer)(nil)

// SetOnRevoke registers a function invoked when partitions are revoked.
// Must be called before SubscribeToTopic.
func (c *consumer) SetOnRevoke(fn func()) {
	c.onRevoke = fn
}

// New builds a consumer from config. It does not contact a broker: Connect
// creates the librdkafka client.
func New(config Config) (Consumer, error) {
	brokers, topic, groupID := config.Brokers, config.Topic, config.GroupID
	commitInterval := config.CommitInterval

	if len(brokers) == 0 {
		return nil, errors.New("at least one broker is required")
	}
	if topic == "" {
		return nil, errors.New("topic cannot be empty")
	}
	if groupID == "" {
		return nil, errors.New("group ID cannot be empty")
	}

	// Build confluent-kafka-go config
	configMap := &kfk.ConfigMap{}
	if err := configMap.SetKey("bootstrap.servers", strings.Join(brokers, ",")); err != nil {
		return nil, fmt.Errorf("setting bootstrap.servers: %w", err)
	}
	if err := configMap.SetKey("group.id", groupID); err != nil {
		return nil, fmt.Errorf("setting group.id: %w", err)
	}
	if err := configMap.SetKey("auto.offset.reset", "earliest"); err != nil {
		return nil, fmt.Errorf("setting auto.offset.reset: %w", err)
	}

	// Commit cadence. Unset, the library commits after every message and owns
	// timing entirely. With an interval, librdkafka's background committer owns
	// it instead and publishes whatever is in the store every commitInterval.
	//
	// Either way the store is ours — see enable.auto.offset.store below — so the
	// background committer can only ever publish offsets the subscriber put there
	// after a message was accounted for. Handing over the timing does not hand
	// over what gets committed.
	autoCommit := commitInterval > 0
	if err := configMap.SetKey("enable.auto.commit", autoCommit); err != nil {
		return nil, fmt.Errorf("setting enable.auto.commit: %w", err)
	}
	if autoCommit {
		intervalMs := int(commitInterval / time.Millisecond)
		if err := configMap.SetKey("auto.commit.interval.ms", intervalMs); err != nil {
			return nil, fmt.Errorf("setting auto.commit.interval.ms: %w", err)
		}
	}

	// Take over the offset store as well. Left at its default of true, librdkafka
	// stores the offset of every message the moment Poll hands it to the
	// application — before the handler has run, or even been called at all in
	// batch mode. Committing that store would then publish work that never
	// happened. With it off, the store holds only what the subscriber puts there
	// after a message is accounted for, which is what makes every commit path
	// safe. StoreOffsets also refuses to run at all while this is true.
	if err := configMap.SetKey("enable.auto.offset.store", false); err != nil {
		return nil, fmt.Errorf("setting enable.auto.offset.store: %w", err)
	}

	// Pin the eager rebalance protocol. Two things here depend on every partition
	// being revoked at once: the rebalance callback below calls Assign/Unassign
	// rather than their Incremental counterparts, and on revoke the subscriber drops
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
	if err := configMap.SetKey("partition.assignment.strategy", "range,roundrobin"); err != nil {
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
		if err := configMap.SetKey(key, value); err != nil {
			return nil, fmt.Errorf("setting %s: %w", key, err)
		}
	}

	// Apply any additional Kafka configuration (user can override defaults including reconnect settings)
	for key, value := range config.KafkaConfig {
		if err := configMap.SetKey(key, value); err != nil {
			return nil, fmt.Errorf("setting kafka config %s: %w", key, err)
		}
	}

	return &consumer{
		config:     configMap,
		brokers:    brokers,
		topic:      topic,
		groupID:    groupID,
		logger:     config.Logger,
		autoCommit: autoCommit,
	}, nil
}

// Connect establishes a connection to Kafka.
func (c *consumer) Connect(ctx context.Context) error {
	if c.kafkaConsumer != nil {
		return fmt.Errorf("consumer already connected")
	}

	kafkaConsumer, err := kfk.NewConsumer(c.config)
	if err != nil {
		return fmt.Errorf("failed to create kafka consumer: %w", err)
	}

	c.kafkaConsumer = kafkaConsumer

	c.mu.Lock()
	c.brokerConnected = true
	c.mu.Unlock()

	c.logger.Info().
		Strs("brokers", c.brokers).
		Str("topic", c.topic).
		Str("group", c.groupID).
		Msg("kafka consumer connected")

	return nil
}

// SubscribeToTopic subscribes the consumer to the topic with rebalance handling.
func (c *consumer) SubscribeToTopic(ctx context.Context) error {
	if c.kafkaConsumer == nil {
		return fmt.Errorf("consumer not connected")
	}

	err := c.kafkaConsumer.SubscribeTopics([]string{c.topic}, c.rebalanceCallback)
	if err != nil {
		return fmt.Errorf("failed to subscribe to topic %s: %w", c.topic, err)
	}

	c.logger.Info().Str("topic", c.topic).Msg("subscribed to topic")

	return nil
}

// rebalanceCallback handles partition assignment and revocation.
func (c *consumer) rebalanceCallback(kafkaConsumer *kfk.Consumer, event kfk.Event) error {
	switch ev := event.(type) {
	case kfk.AssignedPartitions:
		partitions := make([]string, 0, len(ev.Partitions))
		for _, tp := range ev.Partitions {
			partitions = append(partitions, fmt.Sprintf("%s[%d]", *tp.Topic, tp.Partition))
		}
		c.logger.Info().
			Strs("partitions", partitions).
			Msg("partitions assigned")

		if err := kafkaConsumer.Assign(ev.Partitions); err != nil {
			c.logger.Error().Str(logcode.Field, logcode.RebalanceFailed).Err(err).Msg("failed to assign partitions")
			return err
		}

	case kfk.RevokedPartitions:
		partitions := make([]string, 0, len(ev.Partitions))
		for _, tp := range ev.Partitions {
			partitions = append(partitions, fmt.Sprintf("%s[%d]", *tp.Topic, tp.Partition))
		}
		c.logger.Info().
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
		if c.onRevoke != nil {
			c.onRevoke()
		}

		// Commit current offsets before revocation. Safe because
		// enable.auto.offset.store is off: the store holds only offsets the
		// engine put there after a message was accounted for, so this can no
		// longer publish messages that were polled but never processed.
		if err := c.CommitStored(); err != nil {
			c.logger.Warn().Str(logcode.Field, logcode.CommitFailed).Str("commit", "revoke").Err(err).
				Msg("failed to commit offsets during revocation")
		}

		if err := kafkaConsumer.Unassign(); err != nil {
			c.logger.Error().Str(logcode.Field, logcode.RebalanceFailed).Err(err).Msg("failed to unassign partitions")
			return err
		}
	}

	return nil
}

// Poll retrieves messages from Kafka with timeout in milliseconds.
// Uses Poll() for proper event handling including rebalance events.
func (c *consumer) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	if c.kafkaConsumer == nil {
		return nil, fmt.Errorf("consumer not connected")
	}

	ev := c.kafkaConsumer.Poll(timeoutMs)
	if ev == nil {
		// Timeout, no event available
		return nil, nil //nolint:nilnil // a nil message with a nil error means "nothing polled"
	}

	switch e := ev.(type) {
	case *kfk.Message:
		// The client's own ReadMessage checks for a message carrying an error
		// instead of data. No such message has been seen from Poll, which reports
		// consume errors as error events, but dispatching one would run the handler
		// on nothing, store an offset for a position that was never read, and panic
		// on a nil topic. Log it and poll on.
		if e.TopicPartition.Error != nil {
			c.logger.Warn().Str(logcode.Field, logcode.KafkaError).Err(e.TopicPartition.Error).
				Int32("partition", e.TopicPartition.Partition).
				Msg("kafka error reported on a message, not dispatched")
			return nil, nil //nolint:nilnil,nilerr // logged, not fatal: "nothing polled"
		}

		// Detect reconnection — receiving a message means the broker is available
		c.mu.Lock()
		wasDisconnected := !c.brokerConnected
		c.brokerConnected = true
		c.mu.Unlock()
		if wasDisconnected {
			c.logger.Info().Str(logcode.Field, logcode.BrokerReconnected).
				Msg("broker connection restored, resuming message consumption")
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
			Key:       e.Key,
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
			c.mu.Lock()
			wasConnected := c.brokerConnected
			c.brokerConnected = false
			c.mu.Unlock()

			if wasConnected {
				c.logger.Warn().Str(logcode.Field, logcode.BrokerDisconnected).Err(e).Int("code", int(e.Code())).
					Msg("broker connection lost, librdkafka will reconnect automatically")
			} else {
				c.logger.Debug().Err(e).Int("code", int(e.Code())).
					Msg("broker still unavailable, reconnection in progress")
			}
		default:
			c.logger.Warn().Str(logcode.Field, logcode.KafkaError).Err(e).Int("code", int(e.Code())).
				Msg("non-fatal kafka error")
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
			c.logger.Warn().Str(logcode.Field, logcode.CommitFailed).Str("commit", "auto").Err(e.Error).
				Int("partitions", len(e.Offsets)).
				Msg("auto-commit failed, offsets remain stored")
		} else {
			c.logger.Debug().
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
func (c *consumer) StoreOffset(topic string, partition int32, offset int64) error {
	if c.kafkaConsumer == nil {
		return fmt.Errorf("consumer not connected")
	}

	topicStr := topic
	stored, storeErr := c.kafkaConsumer.StoreOffsets([]kfk.TopicPartition{{
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
		return fmt.Errorf("%w: %s[%d]", ErrPartitionRevoked, topic, partition)
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
// It exists so the subscriber's flow reads the same in both modes. With
// WithAutoCommitEvery unset, which is the default, this commits after every
// message: the narrowest possible duplicate window, and the library's existing
// behaviour. Callers that must persist progress before it can be lost — a
// revocation, shutdown — want CommitStored instead, which always commits.
func (c *consumer) MaybeCommitStored() error {
	if c.autoCommit {
		return nil
	}
	return c.CommitStored()
}

// CommitStored commits the offsets currently in librdkafka's store for this
// consumer's assignment. It always commits, whatever the configured cadence, and
// is for the points where progress must be persisted before it can be lost: a
// revocation, and shutdown.
//
// Because enable.auto.offset.store is off, the store holds only offsets the
// engine put there after a message was accounted for, so this is safe to call
// from any point. An empty store is not an error.
func (c *consumer) CommitStored() error {
	if c.kafkaConsumer == nil {
		return fmt.Errorf("consumer not connected")
	}

	committed, err := c.kafkaConsumer.Commit()
	if err != nil {
		var kfkErr kfk.Error
		if errors.As(err, &kfkErr) && kfkErr.Code() == kfk.ErrNoOffset {
			c.logger.Debug().Msg("no stored offsets to commit")
			return nil
		}
		return fmt.Errorf("failed to commit stored offsets: %w", err)
	}

	c.logger.Debug().Int("committed", len(committed)).Msg("stored offsets committed")

	return nil
}

// Close gracefully closes the Kafka connection.
func (c *consumer) Close(ctx context.Context) error {
	if c.kafkaConsumer == nil {
		return nil
	}

	err := c.kafkaConsumer.Close()
	c.kafkaConsumer = nil

	if err != nil {
		return fmt.Errorf("failed to close kafka consumer: %w", err)
	}

	c.logger.Info().Msg("kafka consumer closed")

	return nil
}
