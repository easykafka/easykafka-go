package kafka

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/rs/zerolog"

	"github.com/easykafka/easykafka-go/internal/logcode"
	"github.com/easykafka/easykafka-go/internal/types"
)

// Producer wraps confluent-kafka-go producer for writing to retry and DLQ topics.
type Producer struct {
	producer        *kfk.Producer
	logger          zerolog.Logger
	onDeliveryError types.DeliveryErrorFunc
}

// consumerOnlyPrefixes and consumerOnlyKeys name the librdkafka properties that
// apply to consumers only — scope "C" in librdkafka's CONFIGURATION.md. They are
// dropped when a producer inherits the consumer's configuration.
//
// This is a deny-list rather than an allowlist of shared keys on purpose. A
// consumer-only key missing from it reaches the producer, and librdkafka logs
// one CONFWARN line ("is a consumer property and will be ignored") and carries
// on. A shared key missing from an allowlist — a security, SASL or TLS setting —
// would be silently withheld, and every retry and DLQ write would fail.
var (
	consumerOnlyPrefixes = []string{"group.", "fetch.", "queued.", "auto.", "enable.auto.", "offset.store."}
	consumerOnlyKeys     = map[string]bool{
		"session.timeout.ms":            true,
		"heartbeat.interval.ms":         true,
		"max.poll.interval.ms":          true,
		"coordinator.query.interval.ms": true,
		"partition.assignment.strategy": true,
		"max.partition.fetch.bytes":     true,
		"isolation.level":               true,
		"consume.callback.max.messages": true,
		"enable.partition.eof":          true,
		"check.crcs":                    true,
	}
)

// ProducerConfig builds the configuration for a retry or DLQ producer from the
// consumer's brokers and its WithKafkaConfig map, so the producer connects the
// way the consumer does — with its security, SASL and TLS settings above all.
//
// Every key is carried over except:
//
//   - consumer-only keys, which a producer ignores anyway (see consumerOnlyKeys);
//   - "go." keys, confluent-kafka-go's own client options. Some that a consumer
//     accepts, such as go.application.rebalance.enable, make a producer fail to
//     start, and the library sets its producers' own.
//
// bootstrap.servers and acks=all are set last, so kafkaConfig cannot change
// them. kafkaConfig is only read; it also configures the consumer.
//
// Exported within internal/ so that tests can reach it.
func ProducerConfig(brokers []string, kafkaConfig map[string]any) (*kfk.ConfigMap, error) {
	config := &kfk.ConfigMap{}

	for key, value := range kafkaConfig {
		if strings.HasPrefix(key, "go.") || isConsumerOnly(key) {
			continue
		}
		if err := config.SetKey(key, value); err != nil {
			return nil, fmt.Errorf("setting kafka config %s: %w", key, err)
		}
	}

	if err := config.SetKey("bootstrap.servers", strings.Join(brokers, ",")); err != nil {
		return nil, fmt.Errorf("setting bootstrap.servers: %w", err)
	}
	if err := config.SetKey("acks", "all"); err != nil {
		return nil, fmt.Errorf("setting acks: %w", err)
	}
	return config, nil
}

// isConsumerOnly reports whether key is a consumer-only librdkafka property.
func isConsumerOnly(key string) bool {
	if consumerOnlyKeys[key] {
		return true
	}
	for _, prefix := range consumerOnlyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// NewProducer creates a new Kafka producer for the given brokers, configured
// from the consumer's kafkaConfig as ProducerConfig describes.
//
// onDeliveryError, if non-nil, is called for every write that fails to reach
// the broker. See types.DeliveryErrorFunc for the contract it must honour.
func NewProducer(
	brokers []string,
	kafkaConfig map[string]any,
	logger zerolog.Logger,
	onDeliveryError types.DeliveryErrorFunc,
) (*Producer, error) {

	if len(brokers) == 0 {
		return nil, fmt.Errorf("at least one broker is required")
	}

	config, err := ProducerConfig(brokers, kafkaConfig)
	if err != nil {
		return nil, err
	}

	p, err := kfk.NewProducer(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kafka producer: %w", err)
	}

	prod := &Producer{
		producer:        p,
		logger:          logger,
		onDeliveryError: onDeliveryError,
	}

	// Read the events channel here and hand it to the goroutine. Close sets
	// prod.producer to nil, so the goroutine must never read that field.
	go prod.handleDeliveryReports(p.Events())

	return prod, nil
}

// handleDeliveryReports processes delivery reports from the producer's events
// channel until the client closes it.
//
// This is the only place a failed write is visible: Produce returns as soon as
// the record is queued locally, so nothing downstream learns that the broker
// never took it.
func (p *Producer) handleDeliveryReports(events chan kfk.Event) {
	for e := range events {
		de := DeliveryErrorFor(e)
		if de == nil {
			// Not a failed delivery report. Other event types are drained
			// rather than handled — leaving them would fill the channel.
			continue
		}

		p.logger.Error().
			Str(logcode.Field, logcode.ProducerDeliveryFailed).
			Err(de.Err).
			Str("topic", de.Topic).
			Msg("delivery failed")

		InvokeDeliveryError(p.onDeliveryError, p.logger, *de)
	}
}

// DeliveryErrorFor returns the payload describing a failed delivery report, or
// nil if the event is not one — a different event type, or a write that
// succeeded.
//
// Exported so it can be tested without a broker; internal/ keeps it out of the
// library's public API.
func DeliveryErrorFor(e kfk.Event) *types.DeliveryError {
	ev, ok := e.(*kfk.Message)
	if !ok || ev.TopicPartition.Error == nil {
		return nil
	}

	de := &types.DeliveryError{
		Partition: ev.TopicPartition.Partition,
		Key:       ev.Key,
		Value:     ev.Value,
		Headers:   headersToMap(ev.Headers),
		Err:       ev.TopicPartition.Error,
		Code:      errorCode(ev.TopicPartition.Error),
	}

	if ev.TopicPartition.Topic != nil {
		de.Topic = *ev.TopicPartition.Topic
	}

	// The record as we submitted it, carried through by Produce. Preferred over
	// the report's own copy because librdkafka does not return headers on a
	// delivery report — without this the retry attempt and original topic, the
	// two things worth knowing about a failed retry write, would always be
	// missing.
	if orig, ok := ev.Opaque.(*types.ProduceMessage); ok && orig != nil {
		de.Topic = orig.Topic
		de.Key = orig.Key
		de.Value = orig.Value
		de.Headers = orig.Headers
	}

	return de
}

// InvokeDeliveryError calls fn with de, recovering from a panic. A nil fn is a
// no-op.
//
// The recovery is not a licence, it is a defence: the caller runs this inside
// the range over Events(), so an unrecovered panic would kill that goroutine
// and leave the producer draining nothing for the life of the process.
//
// Exported for the same reason as DeliveryErrorFor.
func InvokeDeliveryError(fn types.DeliveryErrorFunc, logger zerolog.Logger, de types.DeliveryError) {
	if fn == nil {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			logger.Error().
				Str(logcode.Field, logcode.DeliveryCallbackPanic).
				Str("stack", string(debug.Stack())).
				Str("topic", de.Topic).
				Msgf("delivery error callback panic recovered: %v", r)
		}
	}()

	fn(de)
}

// errorCode names the kind of failure, for use as a metric label. It is empty
// when err did not come from the Kafka client.
//
// Deliberately not kfk.Error.IsRetriable: that flag is only ever set by the
// transactional producer API, so on a delivery report it is always false and
// would be a field that lies.
func errorCode(err error) string {
	var kerr kfk.Error
	if !errors.As(err, &kerr) {
		return ""
	}
	return kerr.Code().String()
}

// headersToMap flattens Kafka headers for the delivery error payload. Duplicate
// keys collapse, which matches how the rest of the library represents headers.
func headersToMap(headers []kfk.Header) map[string]string {
	if len(headers) == 0 {
		return nil
	}

	out := make(map[string]string, len(headers))
	for _, h := range headers {
		out[h.Key] = string(h.Value)
	}
	return out
}

// Produce sends a message to a Kafka topic.
func (p *Producer) Produce(_ context.Context, msg *types.ProduceMessage) error {
	if p.producer == nil {
		return fmt.Errorf("producer is nil")
	}

	// Convert headers
	var headers []kfk.Header
	for k, v := range msg.Headers {
		headers = append(headers, kfk.Header{
			Key:   k,
			Value: []byte(v),
		})
	}

	topic := msg.Topic
	err := p.producer.Produce(&kfk.Message{
		TopicPartition: kfk.TopicPartition{
			Topic:     &topic,
			Partition: kfk.PartitionAny,
		},
		Key:     msg.Key,
		Value:   msg.Value,
		Headers: headers,

		// Carried through to the delivery report, so a failed write can be
		// described in full. The client does not return headers on a report, so
		// without this the callback could not say which retry attempt or which
		// original topic the lost record belonged to.
		//
		// Costs one map entry in the client for as long as the record is in
		// flight; the entry is released when the report arrives.
		Opaque: msg,
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to produce message to %s: %w", msg.Topic, err)
	}

	return nil
}

// Flush waits for all outstanding messages to be delivered.
func (p *Producer) Flush(timeoutMs int) int {
	if p.producer == nil {
		return 0
	}
	return p.producer.Flush(timeoutMs)
}

// closeFlushTimeoutMs is how long Close waits for queued records to be
// delivered before closing the producer.
const closeFlushTimeoutMs = 7000

// Close shuts down the producer, first waiting up to closeFlushTimeoutMs for
// queued records to be delivered.
//
// Records still unsent after that are dropped when the client closes, and no
// delivery report is emitted for them, so the delivery-error callback does not
// hear about them either. Their source offsets were committed when they were
// queued, so those messages are lost. The count is logged at error level so the
// loss is at least visible; confirming each write before its offset is stored
// is the job of the producer API, not of this wrapper.
func (p *Producer) Close() {
	if p.producer == nil {
		return
	}

	unflushed := p.producer.Flush(closeFlushTimeoutMs)
	if unflushed > 0 {
		p.logger.Error().
			Str(logcode.Field, logcode.ProducerRecordsDropped).
			Int("unflushed", unflushed).
			Int("flush_timeout_ms", closeFlushTimeoutMs).
			Msg("retry/DLQ records not delivered before the producer closed; they are dropped")
	} else {
		p.logger.Info().
			Int("unflushed", unflushed).
			Msg("retry/DLQ producer flushed")
	}

	p.producer.Close()
	p.producer = nil
}
