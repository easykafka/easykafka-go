package strategy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/easykafka/easykafka-go/internal/kafka"
	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/rs/zerolog"
)

// BackoffFunc computes the delay for a given retry attempt (1-based).
type BackoffFunc func(attempt int) time.Duration

// RetryConfig holds configuration for the retry strategy.
type RetryConfig struct {
	RetryTopic      string
	DLQTopic        string
	MaxAttempts     int
	InitialDelay    time.Duration
	MaxDelay        time.Duration
	Multiplier      float64
	CustomBackoff   BackoffFunc
	PayloadEncoding types.PayloadEncoding
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

// WithMaxAttempts sets the maximum number of retry attempts. Default: 3.
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

// WithFailedMessagePayloadEncoding configures how message payloads are encoded
// when written to retry or dead-letter queues. Default: JSON.
func WithFailedMessagePayloadEncoding(encoding types.PayloadEncoding) RetryOption {
	return func(c *RetryConfig) error {
		switch encoding {
		case types.PayloadEncodingJSON, types.PayloadEncodingBase64:
			c.PayloadEncoding = encoding
		default:
			return fmt.Errorf("unsupported payload encoding: %s", encoding)
		}
		return nil
	}
}

// RetryStrategy implements retry logic using Kafka retry topics and DLQ.
type RetryStrategy struct {
	config        RetryConfig
	retryProducer types.KafkaProducer
	dlqProducer   types.KafkaProducer
	logger        zerolog.Logger
	initialized   bool
}

// NewRetryStrategy creates a retry strategy with the given options.
// The strategy must be initialized via Initialize() before use (called automatically by Consumer.Start).
func NewRetryStrategy(opts ...RetryOption) (*RetryStrategy, error) {
	cfg := RetryConfig{
		MaxAttempts:     3,
		InitialDelay:    1 * time.Second,
		MaxDelay:        30 * time.Second,
		Multiplier:      2.0,
		PayloadEncoding: types.PayloadEncodingJSON,
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

// NewRetryStrategyWithProducers creates a retry strategy with externally provided producers.
// Used for testing with mock producers.
func NewRetryStrategyWithProducers(cfg RetryConfig, retryProducer, dlqProducer types.KafkaProducer, logger zerolog.Logger) *RetryStrategy {
	return &RetryStrategy{
		config:        cfg,
		retryProducer: retryProducer,
		dlqProducer:   dlqProducer,
		logger:        logger,
		initialized:   true,
	}
}

// Initialize creates Kafka producers using broker addresses from the consumer.
func (r *RetryStrategy) Initialize(config types.InitConfig) error {
	r.logger = config.Logger

	retryProducer, err := kafka.NewProducer(config.Brokers, config.Logger)
	if err != nil {
		return fmt.Errorf("failed to create retry producer: %w", err)
	}
	r.retryProducer = retryProducer

	dlqProducer, err := kafka.NewProducer(config.Brokers, config.Logger)
	if err != nil {
		retryProducer.Close()
		return fmt.Errorf("failed to create DLQ producer: %w", err)
	}
	r.dlqProducer = dlqProducer

	r.initialized = true

	r.logger.Info().
		Str("retry_topic", r.config.RetryTopic).
		Str("dlq_topic", r.config.DLQTopic).
		Int("max_attempts", r.config.MaxAttempts).
		Msg("retry strategy initialized")

	return nil
}

// Close shuts down producers.
func (r *RetryStrategy) Close() error {
	if r.retryProducer != nil {
		r.retryProducer.Close()
	}
	if r.dlqProducer != nil {
		r.dlqProducer.Close()
	}
	return nil
}

// HandleError processes a handler failure by writing messages to the retry queue
// or DLQ depending on the attempt count.
func (r *RetryStrategy) HandleError(ctx context.Context, msgs []*types.Message, handlerErr error) error {
	if !r.initialized {
		return errors.New("retry strategy not initialized; call Initialize() first")
	}

	for _, msg := range msgs {
		attempt := metadata.GetRetryAttempt(msg)
		attempt++ // Increment for this failure

		r.logger.Warn().
			Str("topic", msg.Topic).
			Int32("partition", msg.Partition).
			Int64("offset", msg.Offset).
			Int("attempt", attempt).
			Int("max_attempts", r.config.MaxAttempts).
			Err(handlerErr).
			Msg("handler failed for message")

		if attempt >= r.config.MaxAttempts {
			// Max attempts exhausted → send to DLQ
			r.logger.Error().
				Str("topic", msg.Topic).
				Int64("offset", msg.Offset).
				Int("attempts", attempt).
				Msg("max retry attempts reached, sending to DLQ")

			if err := r.sendToDLQ(ctx, msg, handlerErr, attempt); err != nil {
				r.logger.Error().Err(err).Msg("failed to send message to DLQ")
				return fmt.Errorf("DLQ write failed: %w", err)
			}
			r.logger.Info().Msg("message sent to DLQ, continuing consumption")
		} else {
			// Write to retry queue
			if err := r.sendToRetryQueue(ctx, msg, handlerErr, attempt); err != nil {
				r.logger.Error().Err(err).Msg("failed to send message to retry queue")
				return fmt.Errorf("retry queue write failed: %w", err)
			}
			r.logger.Info().
				Int("attempt", attempt).
				Str("retry_topic", r.config.RetryTopic).
				Msg("message sent to retry queue")
		}
	}

	// All messages handled — continue consumption
	return nil
}

// Name returns the strategy name.
func (r *RetryStrategy) Name() string {
	return "retry"
}

// Config returns the retry configuration (for use by circuit breaker).
func (r *RetryStrategy) Config() RetryConfig {
	return r.config
}

// sendToRetryQueue writes a message to the retry topic with retry headers.
func (r *RetryStrategy) sendToRetryQueue(ctx context.Context, msg *types.Message, handlerErr error, attempt int) error {
	delay := r.computeBackoff(attempt)
	retryTime := time.Now().Add(delay)

	headers := metadata.BuildRetryHeaders(msg, attempt, retryTime, handlerErr)

	return r.retryProducer.Produce(ctx, &types.ProduceMessage{
		Topic:   r.config.RetryTopic,
		Key:     nil, // Retry messages use round-robin partitioning
		Value:   msg.Payload,
		Headers: headers,
	})
}

// sendToDLQ writes a message to the DLQ topic with a JSON envelope.
func (r *RetryStrategy) sendToDLQ(ctx context.Context, msg *types.Message, handlerErr error, attempt int) error {
	// Encode payload
	var payloadStr string
	var encoding string

	switch r.config.PayloadEncoding {
	case types.PayloadEncodingBase64:
		payloadStr = base64.StdEncoding.EncodeToString(msg.Payload)
		encoding = "base64"
	default:
		payloadStr = string(msg.Payload)
		encoding = "json"
	}

	origTopic := metadata.GetOriginalTopic(msg)
	if origTopic == "" {
		origTopic = msg.Topic
	}

	dlqMessage := map[string]interface{}{
		"originalTopic":     origTopic,
		"originalPartition": msg.Partition,
		"originalOffset":    msg.Offset,
		"payload":           payloadStr,
		"payloadEncoding":   encoding,
		"error":             handlerErr.Error(),
		"timestamp":         time.Now().Format(time.RFC3339),
		"attemptCount":      attempt,
	}

	dlqPayload, err := json.Marshal(dlqMessage)
	if err != nil {
		return fmt.Errorf("failed to marshal DLQ message: %w", err)
	}

	headers := metadata.BuildDLQHeaders(msg, attempt, handlerErr)

	return r.dlqProducer.Produce(ctx, &types.ProduceMessage{
		Topic:   r.config.DLQTopic,
		Value:   dlqPayload,
		Headers: headers,
	})
}

// computeBackoff calculates the delay for a given attempt.
func (r *RetryStrategy) computeBackoff(attempt int) time.Duration {
	if r.config.CustomBackoff != nil {
		return r.config.CustomBackoff(attempt)
	}

	// Exponential backoff: initialDelay * multiplier^(attempt-1)
	delay := float64(r.config.InitialDelay) * math.Pow(r.config.Multiplier, float64(attempt-1))
	d := time.Duration(delay)

	if d > r.config.MaxDelay {
		return r.config.MaxDelay
	}
	return d
}
