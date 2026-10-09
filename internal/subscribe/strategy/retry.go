package strategy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sync/atomic"
	"time"

	"github.com/easykafka/easykafka-go/internal/logcode"
	"github.com/easykafka/easykafka-go/internal/publish/publishdriver"
	"github.com/easykafka/easykafka-go/internal/subscribe/metadata"
	"github.com/easykafka/easykafka-go/internal/subscribe/types"
	"github.com/easykafka/easykafka-go/publish"
	"github.com/rs/zerolog"
)

// closeTimeout bounds Close: how long records still unreported may take to be
// delivered before they are purged and reported as not delivered. Longer than
// the old producers' 7 s flush, and short enough that a shutdown during an
// outage fits a pod's termination grace period (30 s by default on Kubernetes).
const closeTimeout = 10 * time.Second

// BackoffFunc computes the delay for a given retry attempt (1-based).
type BackoffFunc func(attempt int) time.Duration

// RetryConfig holds configuration for the retry strategy.
type RetryConfig struct {
	RetryTopic      string
	DLQTopic        string
	MaxAttempts     int // attempts in total, the first included; see WithMaxAttempts
	InitialDelay    time.Duration
	MaxDelay        time.Duration
	Multiplier      float64
	CustomBackoff   BackoffFunc
	OnDeliveryError publish.DeliveryErrorFunc

	// newProducer builds the publisher's producer; nil means librdkafka. See
	// WithProducerFactory.
	newProducer func(publishdriver.Config) (publishdriver.Producer, error)
}

// RetryOption configures the retry strategy.
type RetryOption func(*RetryConfig) error

// WithRetryTopic sets the Kafka retry topic. Required.
func WithRetryTopic(topic string) RetryOption {
	return func(c *RetryConfig) error {
		if topic == "" {
			return errors.New("retry topic cannot be empty")
		}
		c.RetryTopic = topic
		return nil
	}
}

// WithDLQTopic sets the Kafka dead-letter topic. Required.
func WithDLQTopic(topic string) RetryOption {
	return func(c *RetryConfig) error {
		if topic == "" {
			return errors.New("DLQ topic cannot be empty")
		}
		c.DLQTopic = topic
		return nil
	}
}

// WithMaxAttempts sets how many times in total a failing message is handled
// before it goes to the DLQ, the first attempt included. It counts attempts, not
// retries: 3 (the default) means the first attempt and 2 retries, and 1 sends a
// failed message straight to the DLQ with no retry at all.
func WithMaxAttempts(attempts int) RetryOption {
	return func(c *RetryConfig) error {
		if attempts <= 0 {
			return errors.New("max attempts must be positive")
		}
		c.MaxAttempts = attempts
		return nil
	}
}

// WithInitialDelay sets the initial delay before the first retry. Default: 1s.
func WithInitialDelay(delay time.Duration) RetryOption {
	return func(c *RetryConfig) error {
		if delay <= 0 {
			return errors.New("initial delay must be positive")
		}
		c.InitialDelay = delay
		return nil
	}
}

// WithMaxDelay sets the maximum delay between retries. Default: 30s.
//
// The retry consumer's handler waits up to this long in WaitUntilRetryTime, on
// the goroutine that polls. Keep it below that consumer's max.poll.interval.ms
// (300s by default), or raise that setting on the retry consumer with
// WithKafkaConfig. Otherwise a long wait gets the retry consumer removed from
// its group (see WaitUntilRetryTime).
func WithMaxDelay(delay time.Duration) RetryOption {
	return func(c *RetryConfig) error {
		if delay <= 0 {
			return errors.New("max delay must be positive")
		}
		c.MaxDelay = delay
		return nil
	}
}

// WithBackoffMultiplier sets the exponential backoff multiplier. Default: 2.0.
func WithBackoffMultiplier(multiplier float64) RetryOption {
	return func(c *RetryConfig) error {
		if multiplier < 1.0 {
			return errors.New("backoff multiplier must be >= 1.0")
		}
		c.Multiplier = multiplier
		return nil
	}
}

// WithCustomBackoff provides a custom backoff function.
// Overrides InitialDelay, MaxDelay, and Multiplier.
func WithCustomBackoff(fn BackoffFunc) RetryOption {
	return func(c *RetryConfig) error {
		if fn == nil {
			return errors.New("custom backoff function cannot be nil")
		}
		c.CustomBackoff = fn
		return nil
	}
}

// WithDeliveryErrorFunc registers fn to be called for every retry or DLQ record
// the broker did not acknowledge, so the application can log it in its own
// format, count it and alert on it. Default: none, and failures are logged on
// the library's own logger only. That logging continues when fn is set — the
// library does not go quiet because a caller asked to be told as well.
//
// A record fn hears of has also failed HandleError, so the consumer stops
// without storing the source offset and the message is consumed again after a
// restart: fn reports a failure the consumer stops on, not a lost message. It
// is also called for a record still unreported when the strategy closes, which
// is then purged.
//
// The record's headers arrive in the order they were written, which is sorted
// by key. See publish.DeliveryErrorFunc for the contract fn must honour: it
// runs on the publisher's report goroutine and must not block.
func WithDeliveryErrorFunc(fn publish.DeliveryErrorFunc) RetryOption {
	return func(c *RetryConfig) error {
		if fn == nil {
			return errors.New("delivery error function cannot be nil")
		}
		c.OnDeliveryError = fn
		return nil
	}
}

// WithProducerFactory replaces the function that builds the strategy's
// librdkafka producer.
//
// This is a testing seam, as publish.WithProducerFactory is, to which it is
// passed through. It is exported only because the tests live in a separate
// package; its argument names types under internal/, so no other module can
// build one.
func WithProducerFactory(factory func(publishdriver.Config) (publishdriver.Producer, error)) RetryOption {
	return func(c *RetryConfig) error {
		if factory == nil {
			return errors.New("producer factory cannot be nil")
		}
		c.newProducer = factory
		return nil
	}
}

// RetryStrategy implements retry logic using Kafka retry topics and DLQ.
type RetryStrategy struct {
	config RetryConfig
	logger zerolog.Logger

	// publisher writes the retry and DLQ records through one producer, one
	// writer per topic. Set by Initialize; nil until then.
	publisher   *publish.Publisher
	retryWriter *publish.Writer[[]byte, []byte]
	dlqWriter   *publish.Writer[[]byte, []byte]

	// inUse is set while a consumer holds the strategy, from Initialize to
	// Close. The publisher and logger belong to that consumer: a second one
	// would replace them under it, and its Close would shut them down.
	inUse atomic.Bool
}

// write is one record HandleError sent, and what it needs to log the outcome.
type write struct {
	delivery *publish.Delivery
	msg      *types.Message
	attempt  int
	toDLQ    bool
}

// ErrStrategyInUse is returned by Initialize when another consumer is still
// running with the strategy.
var ErrStrategyInUse = errors.New(
	"retry strategy is already in use by another consumer; give each consumer its own NewRetryStrategy")

// NewRetryStrategy creates a retry strategy with the given options.
// The strategy must be initialized via Initialize() before use (called automatically by Subscriber.Start).
func NewRetryStrategy(opts ...RetryOption) (*RetryStrategy, error) {
	// The defaults, before the options are applied over them.
	cfg := RetryConfig{
		MaxAttempts:  3, //nolint:mnd
		InitialDelay: 1 * time.Second,
		MaxDelay:     30 * time.Second, //nolint:mnd
		Multiplier:   2.0,              //nolint:mnd
	}

	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, fmt.Errorf("invalid retry option: %w", err)
		}
	}

	if cfg.RetryTopic == "" {
		return nil, errors.New("retry topic is required (use WithRetryTopic)")
	}
	if cfg.DLQTopic == "" {
		return nil, errors.New("DLQ topic is required (use WithDLQTopic)")
	}

	return &RetryStrategy{
		config: cfg,
		logger: zerolog.Nop(),
	}, nil
}

// SetLogger sets the logger for the retry strategy.
func (r *RetryStrategy) SetLogger(logger zerolog.Logger) {
	r.logger = logger
}

// Initialize creates the publisher the retry and DLQ records are written
// through, connected the way the consumer is: its brokers, and its
// WithKafkaConfig map minus consumer-only keys and the keys the publisher
// manages (see PublisherConfig). The publisher keeps its defaults:
// acks=all, idempotence on, and a 30 s delivery timeout.
//
// It does not contact a broker; a missing retry or DLQ topic surfaces on the
// first write, which then fails HandleError.
//
// It returns ErrStrategyInUse while another consumer holds the strategy, and
// then touches nothing. Close releases it for the next consumer.
func (r *RetryStrategy) Initialize(config types.InitConfig) error {
	if !r.inUse.CompareAndSwap(false, true) {
		return ErrStrategyInUse
	}

	r.logger = config.Logger

	options := []publish.Option{
		publish.WithBrokers(config.Brokers...),
		publish.WithKafkaConfig(PublisherConfig(config.KafkaConfig)),
		publish.WithLogger(config.Logger),
	}
	if r.config.OnDeliveryError != nil {
		options = append(options, publish.WithDeliveryErrorFunc(r.config.OnDeliveryError))
	}
	if r.config.newProducer != nil {
		options = append(options, publish.WithProducerFactory(r.config.newProducer))
	}
	publisher, err := publish.New(options...)
	if err != nil {
		r.inUse.Store(false)
		return fmt.Errorf("failed to create the retry/DLQ publisher: %w", err)
	}

	r.publisher = publisher
	r.retryWriter = publisher.Bind(publish.Topic[[]byte, []byte]{
		Name: r.config.RetryTopic, EncodeKey: publish.BytesKey, EncodeValue: publish.RawValue,
	})
	r.dlqWriter = publisher.Bind(publish.Topic[[]byte, []byte]{
		Name: r.config.DLQTopic, EncodeKey: publish.BytesKey, EncodeValue: publish.RawValue,
	})

	r.logger.Info().
		Str("retry_topic", r.config.RetryTopic).
		Str("dlq_topic", r.config.DLQTopic).
		Int("max_attempts", r.config.MaxAttempts).
		Msg("retry strategy initialized")

	return nil
}

// Close shuts the publisher down and releases the strategy for another
// consumer.
//
// Normally nothing is pending: every HandleError waited for its records. Should
// a record still be unreported, it gets up to closeTimeout to be delivered,
// then is purged and reported through the WithDeliveryErrorFunc callback and
// the log. Close then returns an error wrapping publish.ErrNotDelivered with
// the count, and the fatal error too, if the producer failed fatally.
func (r *RetryStrategy) Close() error {
	var err error
	if r.publisher != nil {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		err = r.publisher.Close(ctx)
	}
	r.inUse.Store(false)
	return err
}

// HandleError processes a handler failure by writing messages to the retry queue
// or DLQ depending on the attempt count. A failure wrapping types.ErrPermanent
// goes straight to the DLQ, whatever the attempt count.
//
// It returns nil only once the broker has acknowledged every record it wrote,
// under acks=all. Any record that is not acknowledged, or cannot even be
// queued, fails it, and the subscriber then stops without storing the source
// offset, so the message is consumed again after a restart.
//
// Every record is sent first and the acknowledgements are awaited together, so
// a batch costs one round of broker round trips, not one per message. The wait
// ignores ctx being cancelled: a shutdown must not abandon a record about to be
// confirmed. The publisher's delivery timeout, 30 s, bounds it instead.
func (r *RetryStrategy) HandleError(ctx context.Context, msgs []*types.Message, f types.Failure) error {
	if r.publisher == nil {
		return errors.New("retry strategy not initialized; call Initialize() first")
	}

	permanent := errors.Is(f.Err, types.ErrPermanent)

	writes := make([]write, 0, len(msgs))
	for _, msg := range msgs {
		attempt := metadata.GetRetryAttempt(msg)
		attempt++ // Increment for this failure

		r.logger.Warn().
			Str("topic", msg.Topic).
			Int32("partition", msg.Partition).
			Int64("offset", msg.Offset).
			Int("attempt", attempt).
			Int("max_attempts", r.config.MaxAttempts).
			Str("error_code", f.Code).
			Err(f.Err).
			Msg("handler failed for message")

		if permanent || attempt >= r.config.MaxAttempts {
			reason, code := "max attempts reached, sending to DLQ", logcode.DLQMaxAttempts
			if permanent {
				reason, code = "permanent failure, sending to DLQ without retrying", logcode.DLQPermanent
			}
			r.logger.Error().
				Str(logcode.Field, code).
				Str("topic", msg.Topic).
				Int64("offset", msg.Offset).
				Int("attempts", attempt).
				Msg(reason)

			delivery, err := r.sendToDLQ(msg, f, attempt)
			if err != nil {
				// Failed locally: a full queue, a closed or failed publisher.
				r.logger.Error().Str(logcode.Field, logcode.DLQWriteFailed).Err(err).Msg("failed to send message to DLQ")
				return fmt.Errorf("DLQ write failed: %w", err)
			}
			writes = append(writes, write{delivery: delivery, msg: msg, attempt: attempt, toDLQ: true})
		} else {
			delivery, err := r.sendToRetryQueue(msg, f, attempt)
			if err != nil {
				r.logger.Error().Str(logcode.Field, logcode.RetryWriteFailed).Err(err).Msg("failed to send message to retry queue")
				return fmt.Errorf("retry queue write failed: %w", err)
			}
			writes = append(writes, write{delivery: delivery, msg: msg, attempt: attempt})
		}
	}

	return r.awaitWrites(context.WithoutCancel(ctx), writes)
}

// awaitWrites waits for the outcome of every write, logs each, and joins the
// failures, so no outcome goes unseen behind the first one. Each failed record
// has already reached the WithDeliveryErrorFunc callback and the publisher's
// log by the time its wait returns.
func (r *RetryStrategy) awaitWrites(ctx context.Context, writes []write) error {
	var failures []error
	for _, written := range writes {
		_, err := written.delivery.Wait(ctx)
		switch {
		case err != nil && written.toDLQ:
			r.logger.Error().Str(logcode.Field, logcode.DLQWriteFailed).Err(err).
				Str("topic", written.msg.Topic).Int64("offset", written.msg.Offset).
				Msg("DLQ write not acknowledged")
			failures = append(failures, err)
		case err != nil:
			r.logger.Error().Str(logcode.Field, logcode.RetryWriteFailed).Err(err).
				Str("topic", written.msg.Topic).Int64("offset", written.msg.Offset).
				Msg("retry queue write not acknowledged")
			failures = append(failures, err)
		case written.toDLQ:
			r.logger.Info().Msg("message sent to DLQ, continuing consumption")
		default:
			r.logger.Info().
				Int("attempt", written.attempt).
				Str("retry_topic", r.config.RetryTopic).
				Msg("message sent to retry queue")
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("retry or DLQ write not acknowledged: %w", errors.Join(failures...))
	}
	return nil
}

// Name returns the strategy name.
func (r *RetryStrategy) Name() string {
	return "retry"
}

// Config returns the retry configuration.
func (r *RetryStrategy) Config() RetryConfig {
	return r.config
}

// sendToRetryQueue sends a message to the retry topic with retry headers. The
// record keeps the consumed key, so it can be found by the same key as its
// source.
func (r *RetryStrategy) sendToRetryQueue(msg *types.Message, f types.Failure, attempt int) (*publish.Delivery, error) {
	delay := r.computeBackoff(attempt)
	retryTime := time.Now().Add(delay)

	return send(r.retryWriter, msg, metadata.BuildRetryHeaders(msg, attempt, retryTime, f))
}

// sendToDLQ sends a message to the DLQ topic with the key and bytes it was
// consumed with, and every fact about the failure in headers. Replaying it is a
// re-publish of the key and body to the source topic.
//
// The record is not a byte-for-byte copy of the consumed one: its application
// headers are a key → value copy (see buildFailureHeaders), and its Kafka
// timestamp is the time it is written here.
func (r *RetryStrategy) sendToDLQ(msg *types.Message, f types.Failure, attempt int) (*publish.Delivery, error) {
	return send(r.dlqWriter, msg, metadata.BuildDLQHeaders(msg, attempt, f))
}

// send writes msg's key and payload through writer, without waiting for the
// broker. A nil payload, a tombstone as consumed, is written as one: Send
// refuses a nil value, so it goes through SendDelete, keeping key and headers.
func send(
	writer *publish.Writer[[]byte, []byte], msg *types.Message, headers map[string]string,
) (*publish.Delivery, error) {

	recordHeaders := sortedHeaders(headers)
	if msg.Payload == nil {
		return writer.SendDelete(msg.Key, recordHeaders...)
	}
	return writer.Send(msg.Key, msg.Payload, recordHeaders...)
}

// sortedHeaders turns the header map into record headers sorted by key, so a
// record's headers come out in the same order every time.
func sortedHeaders(headers map[string]string) []publish.Header {
	recordHeaders := make([]publish.Header, 0, len(headers))
	for _, key := range slices.Sorted(maps.Keys(headers)) {
		recordHeaders = append(recordHeaders, publish.Header{Key: key, Value: []byte(headers[key])})
	}
	return recordHeaders
}

// computeBackoff calculates the delay for a given attempt.
func (r *RetryStrategy) computeBackoff(attempt int) time.Duration {
	if r.config.CustomBackoff != nil {
		return r.config.CustomBackoff(attempt)
	}

	// Exponential backoff: initialDelay * multiplier^(attempt-1)
	delay := float64(r.config.InitialDelay) * math.Pow(r.config.Multiplier, float64(attempt-1))

	// Cap before converting. A float beyond time.Duration's range converts to
	// an implementation-defined value, which on amd64 is negative and would
	// slip under the cap as an immediate retry.
	if delay >= float64(r.config.MaxDelay) {
		return r.config.MaxDelay
	}
	return time.Duration(delay)
}
