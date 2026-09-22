package kafka

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/rs/zerolog"

	"github.com/easykafka/easykafka-go/internal/types"
)

// Producer wraps confluent-kafka-go producer for writing to retry and DLQ topics.
type Producer struct {
	producer        *kfk.Producer
	logger          zerolog.Logger
	onDeliveryError types.DeliveryErrorFunc
}

// NewProducer creates a new Kafka producer for the given brokers.
//
// onDeliveryError, if non-nil, is called for every write that fails to reach
// the broker. See types.DeliveryErrorFunc for the contract it must honour.
func NewProducer(
	brokers []string,
	logger zerolog.Logger,
	onDeliveryError types.DeliveryErrorFunc,
) (*Producer, error) {

	if len(brokers) == 0 {
		return nil, fmt.Errorf("at least one broker is required")
	}

	config := &kfk.ConfigMap{
		"bootstrap.servers": strings.Join(brokers, ","),
		"acks":              "all",
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

	// Start delivery report handler in background
	go prod.handleDeliveryReports()

	return prod, nil
}

// handleDeliveryReports processes delivery reports from the producer.
//
// This is the only place a failed write is visible: Produce returns as soon as
// the record is queued locally, so nothing downstream learns that the broker
// never took it.
func (p *Producer) handleDeliveryReports() {
	for e := range p.producer.Events() {
		de := DeliveryErrorFor(e)
		if de == nil {
			// Not a failed delivery report. Other event types are drained
			// rather than handled — leaving them would fill the channel.
			continue
		}

		p.logger.Error().
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

// Close gracefully shuts down the producer, flushing pending messages.
func (p *Producer) Close() {
	if p.producer == nil {
		return
	}
	p.producer.Flush(5000) //nolint:mnd // Wait up to 5s for pending messages
	p.producer.Close()
	p.producer = nil
}
