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
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/easykafka/easykafka-go/internal/logcode"
	"github.com/easykafka/easykafka-go/internal/publishdriver"
)

// defaultPingTimeout bounds Ping when its context has no deadline.
const defaultPingTimeout = 5 * time.Second

// drainFlushTimeout is the timeout of each Flush in drain, so that ctx ending
// is noticed within that long.
const drainFlushTimeout = 100 * time.Millisecond

// purgeReportTimeout bounds the Flush after a purge. Purge reports are created
// locally and need no broker, so this is ample.
const purgeReportTimeout = time.Second

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
	// sends all take the read lock and do not block each other. Close takes
	// the write lock to set closed: it then waits for every enqueue in
	// progress, and every later one sees closed and returns ErrClosed.
	lifecycle sync.RWMutex
	closed    bool

	// closeDone is closed by the first Close once it has finished and set
	// closeErr, which every Close then returns.
	closeDone chan struct{}
	closeErr  error

	// reportsDone is closed when the report goroutine has settled its last
	// report.
	reportsDone chan struct{}

	// fatal holds the first fatal error, recorded once by the report
	// goroutine. It never clears: the producer cannot write any more.
	fatal atomic.Pointer[fatalError]

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

// fatalError is the first fatal error, as Err returns it.
//
// A struct rather than atomic.Pointer[error], which would hold a pointer to an
// interface value (*error) and need dereferencing on every read. Not
// atomic.Value either: its CompareAndSwap panics when the new value's concrete
// type differs from the stored one's, and a second fatal error need not have
// the first one's type. A pointer to this struct has one type, always.
type fatalError struct {
	err error
}

// New creates a publisher and starts reading its delivery reports. It does not
// contact a broker: librdkafka connects in the background, and the first
// records wait in its queue until it has. Use Ping after binding the topics to
// check connectivity and the topics at startup.
//
// A configuration librdkafka refuses fails New, with librdkafka's reason. A
// publisher is shut down with Close.
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
		closeDone:   make(chan struct{}),
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
//
// After a fatal error, Ping returns that error (errors.Is ErrFatal) without
// asking the cluster, whose answer could still be fine: a publisher that cannot
// write is not ready.
func (p *Publisher) Ping(ctx context.Context) error {
	if err := p.Err(); err != nil {
		return fmt.Errorf("publish: ping: %w", err)
	}
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
	if fatal := p.fatal.Load(); fatal != nil {
		return nil, fmt.Errorf("publish: record for %s not enqueued: %w", record.Topic, fatal.err)
	}
	delivery := newDelivery(record)
	if err := p.producer.Produce(record, delivery); err != nil {
		// The driver's error already wraps the matching sentinel (ErrQueueFull,
		// ErrFatal, ErrClosed), so errors.Is reaches it through this wrap.
		return nil, fmt.Errorf("publish: record for %s not enqueued: %w", record.Topic, err)
	}
	return delivery, nil
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
		Err:       report.Err, // unwraps to the matching sentinel, if any
	}
	if len(record.Headers) > 0 {
		deliveryError.Headers = make([]Header, len(record.Headers))
		for index, header := range record.Headers {
			deliveryError.Headers[index] = Header(header)
		}
	}
	return deliveryError
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
		p.noteFatal(kafkaError)
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

// noteFatal records the first fatal error, logs it, and calls the fatal
// handler, once. librdkafka has already purged its queue itself, and reports
// each pending record as purged, so nothing more is needed here for them.
func (p *Publisher) noteFatal(kafkaError *publishdriver.KafkaError) {
	var err error = kafkaError
	// Defensive: the driver attaches ErrFatal to every fatal error, so this is
	// not taken in practice. It keeps errors.Is(Err(), ErrFatal) true should a
	// fatal error ever come without the sentinel.
	if !errors.Is(err, ErrFatal) {
		err = fmt.Errorf("%w: %w", ErrFatal, kafkaError)
	}
	if !p.fatal.CompareAndSwap(nil, &fatalError{err: err}) {
		p.logger.Debug().Str("code", kafkaError.Code).Err(kafkaError).
			Msg("further fatal error after the first, ignored")
		return
	}
	p.logger.Error().Str(logcode.Field, logcode.PublishFatal).Str("code", kafkaError.Code).Err(err).
		Msg("producer failed fatally; every further write fails, the publisher does not recover")
	p.invokeFatalHandler(err)
}

// invokeFatalHandler calls the fatal handler, recovering a panic, for the same
// reason as invokeDeliveryErrorFunc.
func (p *Publisher) invokeFatalHandler(err error) {
	if p.config.onFatal == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			p.logger.Error().Str(logcode.Field, logcode.PublishCallbackPanic).
				Str("stack", string(debug.Stack())).
				Msgf("fatal handler panicked: %v", recovered)
		}
	}()
	p.config.onFatal(err)
}

// Err returns the first fatal error, or nil. Once it is non-nil, every write
// fails with it and Ping returns it; the publisher does not recover. A service
// typically fails its liveness probe on it.
func (p *Publisher) Err() error {
	if fatal := p.fatal.Load(); fatal != nil {
		return fatal.err
	}
	return nil
}

// Close shuts the publisher down and reports every record it accepted.
//
// It stops accepting records (Send returns ErrClosed), then waits, until ctx
// ends, for librdkafka to deliver what it holds. Whatever is still undelivered
// then is purged: each such record is reported as not delivered, through its
// Delivery and the WithDeliveryErrorFunc callback, rather than dropped
// silently. Close returns once the last report has been handled, so the
// callback has run for every record by then.
//
// It returns nil when every record was reported in time, an error wrapping
// ErrNotDelivered with the count when records were purged, and the fatal error
// too, if there was one. A purged record that was in flight may still have
// been written. Close is bounded even with a context without a deadline: every
// record is reported within the delivery timeout. It is idempotent: a second
// call waits for the first and returns the same error.
//
// Close it after whatever produces records has stopped (an HTTP server, the
// consumers), with a context of its own: the application's root context is
// usually cancelled already at shutdown.
func (p *Publisher) Close(ctx context.Context) error {
	p.lifecycle.Lock()
	if p.closed {
		p.lifecycle.Unlock()
		// Another Close got here first and is still shutting down. Wait until it
		// has finished and set closeErr: returning sooner would let the caller
		// tear down while records are still being reported, and reading
		// closeErr sooner would race with its write.
		<-p.closeDone
		return p.closeErr
	}
	p.closed = true // enqueue returns ErrClosed from here on
	p.lifecycle.Unlock()

	remaining := p.drain(ctx)
	if remaining > 0 {
		p.logger.Error().Str(logcode.Field, logcode.PublishRecordsPurged).Int("remaining", remaining).
			Msg("publisher closing with undelivered records; purging and reporting each")
		if err := p.producer.Purge(); err != nil { // queue + in flight, non-blocking
			p.logger.Error().Err(err).Msg("purging the producer's queue failed")
		}

		// This Flush waits for reports, not deliveries: after the purge nothing
		// is left to deliver. It is what makes the purge reports arrive:
		//  1. Purge removes the records and makes librdkafka create one purge
		//     report each, but queues them internally; with PurgeNonBlocking,
		//     some purging even finishes after Purge returns.
		//  2. Close, below, stops confluent's event pipeline: any report still
		//     inside librdkafka then never reaches Reports, and its record would
		//     go unreported, the very loss the purge exists to prevent.
		//  3. Flush counts what librdkafka still holds, reports not yet handed
		//     to Go included, and waits until that is zero, handing the reports
		//     on as it goes. So every purge report is out before Close.
		// Purge reports are local, so purgeReportTimeout (1 s) is ample.
		p.producer.Flush(purgeReportTimeout)
	}
	p.producer.Close() // closes the events channel
	<-p.reportsDone    // every report, purge reports included, has been settled

	var failures []error
	if remaining > 0 {
		failures = append(failures, fmt.Errorf("%w: %d record(s)", ErrNotDelivered, remaining))
	}
	if fatal := p.fatal.Load(); fatal != nil {
		failures = append(failures, fatal.err)
	}
	p.closeErr = errors.Join(failures...)
	close(p.closeDone)
	return p.closeErr
}

// drain flushes until every record is reported or ctx ends, and returns how
// many are still unreported.
func (p *Publisher) drain(ctx context.Context) int {
	remaining := p.producer.Len()
	for remaining > 0 && ctx.Err() == nil {
		timeout := drainFlushTimeout
		if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
			timeout = min(timeout, time.Until(deadline))
		}
		remaining = p.producer.Flush(timeout)
	}
	return remaining
}
