// Package easykafka provides a simplified Kafka consumer API with handler-based message processing.
//
// This file defines the public API contract for the EasyKafka consumer library.
// All types and functions in this file represent the stable public interface.
package easykafka

import (
	"context"
	"time"
)

// ============================================================================
// CORE TYPES
// ============================================================================

// Consumer manages the lifecycle of Kafka message consumption.
// Create via New() with functional options, then call Start() to begin consuming.
type Consumer interface {
	// Start begins consuming messages from the configured topic.
	// Blocks until context is cancelled or a fatal error occurs.
	// Returns error on configuration validation failure or unrecoverable Kafka errors.
	Start(ctx context.Context) error

	// Shutdown gracefully stops the consumer within the configured timeout.
	// Completes in-flight message processing and commits final offsets.
	// Returns error if shutdown timeout is exceeded.
	Shutdown(ctx context.Context) error
}

// ============================================================================
// HANDLER FUNCTION TYPES
// ============================================================================

// Handler processes a single message payload with context for cancellation support.
// The context is cancelled during graceful shutdown to allow handlers to abort.
// Return nil for successful processing (offset will be committed).
// Return error for failed processing (error strategy will be applied).
type Handler func(ctx context.Context, payload []byte) error

// BatchHandler processes multiple messages together as a batch with context support.
// The context is cancelled during graceful shutdown.
// Return nil to commit all message offsets in the batch.
// Return error to apply error strategy to the entire batch.
type BatchHandler func(ctx context.Context, payloads [][]byte) error

// ============================================================================
// CONFIGURATION (FUNCTIONAL OPTIONS)
// ============================================================================

// Option configures a Consumer. Options are applied during New().
type Option func(*consumerConfig) error

// NEW creates a new Consumer with the provided options.
// Returns error if required options are missing or invalid.
//
// Required options: WithTopic, WithBrokers, WithConsumerGroup, WithHandler
//
// Example:
//
//	consumer, err := easykafka.New(
//	    easykafka.WithTopic("orders"),
//	    easykafka.WithBrokers("localhost:9092"),
//	    easykafka.WithConsumerGroup("order-processors"),
//	    easykafka.WithHandler(func(ctx context.Context, msg []byte) error {
//	        return processOrder(ctx, msg)
//	    }),
//	)
func New(options ...Option) (Consumer, error)

// ============================================================================
// REQUIRED OPTIONS
// ============================================================================

// WithTopic specifies the Kafka topic to consume from.
// Required. Must be non-empty.
func WithTopic(topic string) Option

// WithBrokers specifies the Kafka broker addresses.
// Required. Must provide at least one broker.
// Example: WithBrokers("localhost:9092", "localhost:9093")
func WithBrokers(brokers ...string) Option

// WithConsumerGroup specifies the consumer group ID.
// Required. Multiple consumers with the same group ID will share partition load.
func WithConsumerGroup(groupID string) Option

// WithHandler specifies the message processing function (single-message mode).
// Required (unless WithBatchHandler is used).
// The handler receives context and message payload.
// Context is cancelled during graceful shutdown.
func WithHandler(handler Handler) Option

// WithBatchHandler specifies a batch processing function.
// Required (unless WithHandler is used).
// The handler receives context and multiple message payloads.
// Context is cancelled during graceful shutdown.
// Enables batch mode - see WithBatchSize and WithBatchTimeout.
//
// IMPORTANT: Batch processing is ATOMIC. If the batch handler returns an error,
// the entire batch is subject to the error strategy:
//   - Retry: all messages in batch are re-processed together
//   - Skip: all messages in batch are skipped (all offsets committed)
//   - DLQ: entire batch is written to dead-letter queue
//
// No partial batch success/failure tracking.
func WithBatchHandler(handler BatchHandler) Option

// ============================================================================
// OPTIONAL CONFIGURATION
// ============================================================================

// WithErrorStrategy specifies how to handle message processing failures.
// Default: Retry(WithMaxAttempts(3), WithOnMaxAttemptsExceeded(FailConsumer)) - retry up to 3 times then stop.
//
// Example:
//
//	easykafka.WithErrorStrategy(easykafka.FailFast())
func WithErrorStrategy(strategy ErrorStrategy) Option

// WithBatchSize specifies the maximum number of messages per batch.
// Only applies when using WithBatchHandler.
// When batch size is reached, the batch is immediately processed.
// If handler returns error, entire batch is retried/skipped together (atomic).
// Default: 100
func WithBatchSize(size int) Option

// WithBatchTimeout specifies how long to wait before processing a partial batch.
// Only applies when using WithBatchHandler.
// Ensures low latency even when message rate is low.
// Default: 5 seconds
func WithBatchTimeout(timeout time.Duration) Option

// WithPollTimeout specifies the Kafka poll timeout.
// Lower values improve shutdown responsiveness but increase CPU usage.
// Default: 100ms
func WithPollTimeout(timeout time.Duration) Option

// WithShutdownTimeout specifies the maximum time to wait for graceful shutdown.
// Default: 30 seconds
func WithShutdownTimeout(timeout time.Duration) Option

// WithLogger specifies a custom zerolog logger.
// Default: zerolog.Nop() (no logging)
func WithLogger(logger Logger) Option

// WithKafkaConfig passes advanced configuration to confluent-kafka-go.
// Use this to set low-level Kafka consumer properties.
//
// Example:
//
//	easykafka.WithKafkaConfig(map[string]any{
//	    "session.timeout.ms": 6000,
//	    "auto.offset.reset": "earliest",
//	})
func WithKafkaConfig(config map[string]any) Option

// ============================================================================
// ERROR HANDLING STRATEGIES
// ============================================================================

// ErrorStrategy defines how message processing failures are handled.
// Implement this interface to create custom error handling behavior.
type ErrorStrategy interface {
	// HandleError is called when a handler returns an error.
	// In single-message mode, msgs contains one message.
	// In batch mode, msgs contains all messages from the failed batch.
	// Return nil to continue consumption, error to stop the consumer.
	HandleError(ctx context.Context, msgs []*Message, handlerErr error) error

	// Name returns the strategy name for logging purposes.
	Name() string
}

// FailFast returns an error strategy that stops the consumer immediately on any handler error.
// Use for critical processing where errors require manual intervention.
func FailFast() ErrorStrategy

// Skip returns an error strategy that logs errors and continues processing.
// Use for best-effort processing where message loss is acceptable.
func Skip() ErrorStrategy

// Retry returns an error strategy that retries failed messages with exponential backoff.
// After max attempts are exhausted, executes the configured action (stop consumer or send to DLQ).
// Use for transient failures (network timeouts, temporary service unavailability).
//
// Example (stop consumer after retries):
//
//	easykafka.Retry(
//	    easykafka.WithMaxAttempts(5),
//	    easykafka.WithInitialDelay(1 * time.Second),
//	    easykafka.WithMaxDelay(60 * time.Second),
//	    easykafka.WithOnMaxAttemptsExceeded(easykafka.FailConsumer),
//	)
//
// Example (send to DLQ after retries):
//
//	easykafka.Retry(
//	    easykafka.WithMaxAttempts(3),
//	    easykafka.WithOnMaxAttemptsExceeded(easykafka.SendToDLQ("orders-dlq")),
//	)
func Retry(options ...RetryOption) ErrorStrategy

// CircuitBreaker returns an error strategy that pauses consumption after consecutive failures.
// Use to protect downstream services from overload during incidents.
//
// IMPORTANT: CircuitBreaker is only supported in single-message mode (WithHandler).
// Using CircuitBreaker with batch mode (WithBatchHandler) will return an error at consumer creation.
// Batch mode support is planned for future releases.
//
// Example:
//
//	easykafka.CircuitBreaker(
//	    easykafka.WithFailureThreshold(10),
//	    easykafka.WithCooldownPeriod(60 * time.Second),
//	)
func CircuitBreaker(options ...CircuitBreakerOption) ErrorStrategy

// ============================================================================
// ERROR STRATEGY OPTIONS
// ============================================================================

// RetryOption configures a retry error strategy.
type RetryOption func(*retryConfig) error

// WithMaxAttempts sets the maximum number of retry attempts.
// Default: 3
func WithMaxAttempts(attempts int) RetryOption

// WithInitialDelay sets the initial delay before the first retry.
// Default: 1 second
func WithInitialDelay(delay time.Duration) RetryOption

// WithMaxDelay sets the maximum delay between retries (for exponential backoff).
// Default: 30 seconds
func WithMaxDelay(delay time.Duration) RetryOption

// WithBackoffMultiplier sets the exponential backoff multiplier.
// Default: 2.0 (delays: 1s, 2s, 4s, 8s, ...)
func WithBackoffMultiplier(multiplier float64) RetryOption

// WithCustomBackoff provides a custom backoff function.
// The function receives the attempt number (1-based) and returns the delay.
func WithCustomBackoff(fn func(attempt int) time.Duration) RetryOption

// WithOnMaxAttemptsExceeded configures what happens after all retry attempts are exhausted.
// Options:
//   - FailConsumer: Stop the consumer and return error (default)
//   - SendToDLQ("topic-name"): Write message to dead-letter queue and continue consumption
//
// Example:
//
//	easykafka.WithOnMaxAttemptsExceeded(easykafka.SendToDLQ("orders-dlq"))
func WithOnMaxAttemptsExceeded(action MaxAttemptsAction) RetryOption

// WithFailedMessagePayloadEncoding configures how message payloads are encoded when written to retry or dead-letter queues.
// This encoding applies to any failed messages, whether retried or sent to DLQ.
// Options:
//   - PayloadEncodingJSON: Write payload as human-readable JSON string (default for better debugging)
//   - PayloadEncodingBase64: Write payload as base64-encoded string (binary-safe for all payloads)
//
// Example:
//
//	easykafka.Retry(
//	    easykafka.WithMaxAttempts(3),
//	    easykafka.WithOnMaxAttemptsExceeded(easykafka.SendToDLQ("orders-dlq")),
//	    easykafka.WithFailedMessagePayloadEncoding(easykafka.PayloadEncodingJSON),
//	)
func WithFailedMessagePayloadEncoding(encoding PayloadEncoding) RetryOption

// MaxAttemptsAction defines what happens when retry attempts are exhausted.
type MaxAttemptsAction interface {
	isMaxAttemptsAction()
}

// FailConsumer stops the consumer when max attempts are exceeded.
// This is the default behavior.
var FailConsumer MaxAttemptsAction

// SendToDLQ returns an action that writes failed messages to a dead-letter queue.
// The message is written to the specified topic with error metadata, then consumption continues.
//
// Example:
//
//	easykafka.SendToDLQ("orders-dlq")
func SendToDLQ(dlqTopic string) MaxAttemptsAction

// PayloadEncoding specifies how message payloads are encoded in retry and DLQ messages.
type PayloadEncoding int

const (
	// PayloadEncodingJSON writes the payload as a JSON string for human readability.
	// Use this when messages are text or JSON for easier debugging and monitoring.
	// Applies to both retry queues and dead-letter queues.
	// Default encoding.
	PayloadEncodingJSON PayloadEncoding = iota

	// PayloadEncodingBase64 writes the payload as a base64-encoded string.
	// Use this for binary payloads or when binary-safe representation is required.
	// Applies to both retry queues and dead-letter queues.
	PayloadEncodingBase64
)

// CircuitBreakerOption configures a circuit breaker strategy.
type CircuitBreakerOption func(*circuitConfig) error

// WithFailureThreshold sets the number of consecutive failures before opening the circuit.
// Default: 10
func WithFailureThreshold(threshold int) CircuitBreakerOption

// WithCooldownPeriod sets how long the circuit stays open before attempting recovery.
// Default: 60 seconds
func WithCooldownPeriod(period time.Duration) CircuitBreakerOption

// WithHalfOpenAttempts sets how many successes required to close the circuit from half-open.
// Default: 3
func WithHalfOpenAttempts(attempts int) CircuitBreakerOption

// ============================================================================
// MESSAGE METADATA ACCESS
// ============================================================================

// Message represents a Kafka message with metadata.
// Access via MessageFromContext() inside context-aware handlers.
type Message struct {
	Topic     string
	Partition int32
	Offset    int64
	Timestamp time.Time
	Headers   []Header
	Key       []byte
	Value     []byte
}

// Header represents a Kafka message header (key-value pair).
type Header struct {
	Key   string
	Value []byte
}

// MessageFromContext extracts message metadata from the handler context.
// Returns nil if called outside a context-aware handler.
//
// Example:
//
//	func handler(ctx context.Context, payload []byte) error {
//	    msg := easykafka.MessageFromContext(ctx)
//	    log.Printf("Processing offset %d from partition %d", msg.Offset, msg.Partition)
//	    return processPayload(payload)
//	}
func MessageFromContext(ctx context.Context) *Message

// ============================================================================
// LOGGING INTERFACE
// ============================================================================

// Logger is a minimal interface for structured logging.
// Compatible with zerolog.Logger and other structured loggers.
type Logger interface {
	Info() Event
	Warn() Event
	Error() Event
	Debug() Event
}

// Event represents a log event builder.
type Event interface {
	Str(key, val string) Event
	Int(key string, val int) Event
	Int64(key string, val int64) Event
	Err(err error) Event
	Msg(msg string)
}

// ============================================================================
// VERSION INFORMATION
// ============================================================================

// Version returns the library version string (semantic versioning).
func Version() string
