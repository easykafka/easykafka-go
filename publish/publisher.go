package publish

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/easykafka/easykafka-go/internal/logcode"
	"github.com/easykafka/easykafka-go/internal/publishdriver"
)

// defaultPingTimeout bounds Ping when its context has no deadline.
const defaultPingTimeout = 5 * time.Second

// Publisher writes records to Kafka through one librdkafka producer, and reads
// the delivery report of every record it accepts. Safe for concurrent use.
type Publisher struct {
	config   config
	logger   zerolog.Logger
	producer publishdriver.Producer

	// lifecycle guards closed, so that no record is enqueued once shutdown
	// has begun: such a record would land behind the final flush and purge,
	// and never be reported.
	//
	// enqueue holds the read lock from its check of closed until Produce has
	// returned, so the check and the enqueue happen as one step. Concurrent
	// sends all take the read lock and do not block each other. Shutdown is
	// to take the write lock to set closed: it then waits for every enqueue in
	// progress, and every later one sees closed and returns ErrClosed. Nothing
	// sets closed yet; Close is still to be written.
	lifecycle sync.RWMutex
	closed    bool

	// reportsDone is closed when the report goroutine has settled its last
	// report.
	reportsDone chan struct{}

	// topics is the name of every topic bound so far, in binding order, each
	// once: a publisher writes to any number of topics, one Bind each. It is
	// used only by Ping, to know which topics to check exist; writing does not
	// need it, since each Writer carries its own topic. topicsMu guards it, as
	// Bind and Ping may run on different goroutines.
	topicsMu sync.Mutex
	topics   []string

	// brokerDown and suppressed track the connection for logging. Only the
	// report goroutine touches them.
	//
	// brokerDown is set once the loss is logged, and cleared on recovery.
	brokerDown bool
	// suppressed counts the connection errors that came in while brokerDown,
	// which would otherwise each have been logged; the recovery line reports
	// it, then it is reset.
	suppressed int
}

// New creates a publisher and starts reading its delivery reports. It does not
// contact a broker: librdkafka connects in the background, and the first
// records wait in its queue until it has. Use Ping after binding the topics to
// check connectivity and the topics at startup.
//
// A configuration librdkafka refuses fails New, with librdkafka's reason.
//
// Shutting a publisher down is not implemented yet: there is no Close, so a
// publisher lives as long as the process.
func New(options ...Option) (*Publisher, error) {
	built := defaultConfig()
	for _, option := range options {
		if option == nil {
			return nil, errors.New("option cannot be nil")
		}
		if err := option(&built); err != nil {
			return nil, err
		}
	}
	if err := built.validate(); err != nil {
		return nil, err
	}

	producer, err := built.newProducer(built.driverConfig())
	if err != nil {
		return nil, fmt.Errorf("publish: creating the producer: %w", err)
	}
	publisher := &Publisher{
		config:      built,
		logger:      built.logger,
		producer:    producer,
		reportsDone: make(chan struct{}),
	}
	// The channel is handed in, so the report goroutine never reads
	// publisher.producer.
	go publisher.readReports(producer.Reports())
	return publisher, nil
}

// Bind returns the typed writer for a topic. It does three things:
//
//   - returns a Writer[K, V] that writes the topic's records through this
//     publisher's producer. This is where the key and value types are fixed,
//     since the publisher itself is not generic;
//   - validates the topic, and panics on an invalid one (an empty Name, or a
//     nil encoder), because a running service cannot act on that. The topic's
//     Headers are copied, so changing the caller's slice later does not change
//     what the writer sends;
//   - adds the topic's name to those Ping checks.
//
// It does not contact Kafka. Binding the same topic more than once is allowed.
func (p *Publisher) Bind[K, V any](topic Topic[K, V]) *Writer[K, V] {
	if err := topic.validate(); err != nil {
		panic(err)
	}
	topic.Headers = slices.Clone(topic.Headers)

	p.topicsMu.Lock()
	if !slices.Contains(p.topics, topic.Name) {
		p.topics = append(p.topics, topic.Name)
	}
	p.topicsMu.Unlock()

	return &Writer[K, V]{publisher: p, topic: topic}
}

// Ping checks that the cluster is reachable and that every topic bound so far
// exists. It sends one metadata request, bounded by ctx, or by 5 s if ctx has
// no deadline. It returns nil once a broker has answered and every bound topic
// is in the answer. A missing topic is reported as ErrTopicNotFound, naming
// every missing topic.
//
// Ping changes nothing in the publisher, so it is safe to call repeatedly, from
// a readiness probe for example. A bound topic deleted later then fails it too.
// It does not check write permissions: those surface on the first record. Bind
// every topic before calling it; a topic bound later is checked on the next
// call.
func (p *Publisher) Ping(ctx context.Context) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultPingTimeout)
		defer cancel()
	}

	p.topicsMu.Lock()
	topics := slices.Clone(p.topics)
	p.topicsMu.Unlock()

	partitions, err := p.producer.TopicPartitions(ctx, topics)
	if err != nil {
		return fmt.Errorf("publish: ping: %w", err)
	}
	var missing []string
	for _, topic := range topics {
		if _, found := partitions[topic]; !found {
			missing = append(missing, strconv.Quote(topic))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrTopicNotFound, strings.Join(missing, ", "))
	}
	return nil
}

// enqueue hands one record to librdkafka and returns its Delivery. It never
// blocks on the broker: a full queue fails at once.
//
// Named for what it does: the record goes into librdkafka's local queue, and
// delivery to the broker happens later, reported on the Delivery. It is not
// called produce, to keep it apart from the driver's Produce it calls.
func (p *Publisher) enqueue(record publishdriver.Record) (*Delivery, error) {
	p.lifecycle.RLock()
	defer p.lifecycle.RUnlock()
	if p.closed {
		return nil, ErrClosed
	}
	delivery := newDelivery(record)
	if err := p.producer.Produce(record, delivery); err != nil {
		return nil, publicProduceError(record.Topic, err)
	}
	return delivery, nil
}

// publicProduceError maps a failed Produce from the driver's internal errors to
// the public ones, since a caller outside this module cannot inspect the
// driver's types. Each error names the topic:
//
//   - a full queue → ErrQueueFull, so the caller can back off or shed load;
//   - a fatal error (Produce after the producer has failed) → ErrFatal,
//     wrapping the Kafka error: retrying is pointless;
//   - a closed driver → ErrClosed;
//   - anything else, a record over message.max.bytes for example → the
//     error wrapped as it is, librdkafka's message saying why.
func publicProduceError(topic string, err error) error {
	if kafkaError, isKafkaError := errors.AsType[*publishdriver.KafkaError](err); isKafkaError {
		switch {
		case kafkaError.QueueFull:
			return fmt.Errorf("%w: record for %s not enqueued", ErrQueueFull, topic)
		case kafkaError.Fatal:
			return fmt.Errorf("%w: record for %s not enqueued: %w", ErrFatal, topic, kafkaError)
		}
	}
	if errors.Is(err, publishdriver.ErrClosed) {
		return fmt.Errorf("%w: record for %s not enqueued", ErrClosed, topic)
	}
	// A record over the client's message.max.bytes, for example: nothing was
	// enqueued, and librdkafka's message names the reason.
	return fmt.Errorf("publish: record for %s not enqueued: %w", topic, err)
}

// readReports settles every report until the driver closes events.
func (p *Publisher) readReports(events <-chan publishdriver.Event) {
	defer close(p.reportsDone)
	for event := range events {
		switch event := event.(type) {
		case publishdriver.Report:
			p.settle(event)
		case publishdriver.ClientError:
			p.noteClientError(event.Err)
		}
	}
}

// settle resolves the delivery a report belongs to. On a failure it logs the
// record and calls the callback first, so a caller whose Wait returns an error
// can rely on the callback having already seen it.
//
// A report that matches no unresolved delivery breaks the rule that every
// record is reported exactly once. It is logged and dropped: the first outcome
// stands, and the callback does not run again.
func (p *Publisher) settle(report publishdriver.Report) {
	delivery, isDelivery := report.Token.(*Delivery)
	if !isDelivery {
		p.logUnmatchedReport(report, fmt.Sprintf("its token is a %T, not a delivery", report.Token))
		return
	}
	if delivery.resolved() {
		// Logged under the same code as a foreign token above: both mean a
		// report broke the exactly-once rule, and call for the same action.
		// The message says which case it was.
		p.logUnmatchedReport(report, "second report for a record already resolved, topic "+delivery.record.Topic)
		return
	}
	result := Result{Topic: delivery.record.Topic, Partition: report.Partition, Offset: report.Offset}
	if report.Err == nil {
		p.noteConnected()
		delivery.resolve(result, nil)
		return
	}

	// The report carries an error: the record was not acknowledged.
	deliveryError := newDeliveryError(delivery.record, report)
	p.logger.Error().Str(logcode.Field, logcode.PublishDeliveryFailed).
		Str("topic", deliveryError.Topic).Int32("partition", deliveryError.Partition).
		Str("code", deliveryError.Code).Err(deliveryError.Err).
		Msg("record not acknowledged")
	p.invokeDeliveryErrorFunc(*deliveryError)
	delivery.resolve(result, deliveryError)
}

// logUnmatchedReport logs a report settle drops.
func (p *Publisher) logUnmatchedReport(report publishdriver.Report, reason string) {
	event := p.logger.Error().Str(logcode.Field, logcode.PublishUnmatchedReport).
		Int32("partition", report.Partition).Int64("offset", report.Offset)
	if report.Err != nil {
		event = event.Str("code", report.Err.Code)
	}
	event.Msg("delivery report dropped: " + reason)
}

// newDeliveryError describes a failed record: the record from its Delivery,
// the partition and the error from its report.
func newDeliveryError(record publishdriver.Record, report publishdriver.Report) *DeliveryError {
	deliveryError := &DeliveryError{
		Topic:     record.Topic,
		Partition: report.Partition,
		Key:       record.Key,
		Value:     record.Value,
		Code:      report.Err.Code,
		Err:       deliveryCause(report.Err),
	}
	if len(record.Headers) > 0 {
		deliveryError.Headers = make([]Header, len(record.Headers))
		for index, header := range record.Headers {
			deliveryError.Headers[index] = Header(header)
		}
	}
	return deliveryError
}

// deliveryCause wraps the sentinel matching a failed report, if one does,
// around librdkafka's error.
func deliveryCause(kafkaError *publishdriver.KafkaError) error {
	switch {
	case kafkaError.Purged:
		return fmt.Errorf("%w: %w", ErrNotDelivered, kafkaError)
	case kafkaError.Fatal:
		return fmt.Errorf("%w: %w", ErrFatal, kafkaError)
	case kafkaError.TimedOut:
		return fmt.Errorf("%w: %w", ErrDeliveryTimeout, kafkaError)
	default:
		return kafkaError
	}
}

// invokeDeliveryErrorFunc calls the callback, recovering a panic: an
// unrecovered one would kill the report goroutine, and no delivery would be
// resolved again.
func (p *Publisher) invokeDeliveryErrorFunc(deliveryError DeliveryError) {
	if p.config.onDeliveryError == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			p.logger.Error().Str(logcode.Field, logcode.PublishCallbackPanic).
				Str("topic", deliveryError.Topic).Str("stack", string(debug.Stack())).
				Msgf("delivery error callback panicked: %v", recovered)
		}
	}()
	p.config.onDeliveryError(deliveryError)
}

// noteClientError logs an error about the client. A lost connection is logged
// once when it goes down, and the errors repeating while it stays down are
// only counted, until noteConnected reports the recovery.
func (p *Publisher) noteClientError(kafkaError *publishdriver.KafkaError) {
	switch {
	case kafkaError.Fatal:
		p.logger.Error().Str(logcode.Field, logcode.PublishKafkaError).Bool("fatal", true).
			Str("code", kafkaError.Code).Err(kafkaError).
			Msg("producer failed fatally")
	case kafkaError.Disconnected:
		if p.brokerDown {
			p.suppressed++
			p.logger.Debug().Str("code", kafkaError.Code).Err(kafkaError).
				Msg("brokers still unreachable, librdkafka is reconnecting")
			return
		}
		p.brokerDown = true
		p.logger.Warn().Str(logcode.Field, logcode.PublishBrokerDown).
			Str("code", kafkaError.Code).Err(kafkaError).
			Msg("broker connection lost; librdkafka reconnects, records wait in the queue until their delivery timeout")
	default:
		p.logger.Warn().Str(logcode.Field, logcode.PublishKafkaError).
			Str("code", kafkaError.Code).Err(kafkaError).
			Msg("producer reported an error")
	}
}

// noteConnected logs the recovery after a lost connection, once a record has
// been acknowledged again.
func (p *Publisher) noteConnected() {
	if !p.brokerDown {
		return
	}
	p.logger.Info().Str(logcode.Field, logcode.PublishBrokerRestored).Int("suppressed", p.suppressed).
		Msg("broker connection restored, records are acknowledged again")
	p.brokerDown = false
	p.suppressed = 0
}
