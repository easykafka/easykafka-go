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
	Start(ctx context.Context) error

	// Shutdown gracefully stops the consumer within the configured timeout.
	// Completes in-flight message processing and commits final offsets.
	Shutdown(ctx context.Context) error
}

// ============================================================================
// HANDLER FUNCTION TYPES
// ============================================================================

// Handler processes a single message payload with context for cancellation support.
// Return nil for successful processing (offset will be committed).
// Return error for failed processing (error strategy will be applied).
type Handler func(ctx context.Context, payload []byte) error

// BatchHandler processes multiple messages together as a batch with context support.
// Return nil to commit all message offsets in the batch.
// Return error to apply error strategy to the entire batch.
type BatchHandler func(ctx context.Context, payloads [][]byte) error

// ============================================================================
// MESSAGE METADATA
// ============================================================================

// Message is the public metadata representation available to handlers.
type Message struct {
	Topic     string
	Partition int32
	Offset    int64
	Timestamp time.Time
	Headers   map[string]string
}

// MessageFromContext extracts message metadata from a handler context.
func MessageFromContext(ctx context.Context) (*Message, bool)

// PayloadEncoding defines how payloads are encoded for retry and DLQ messages.
type PayloadEncoding string

const (
	PayloadEncodingJSON   PayloadEncoding = "json"
	PayloadEncodingBase64 PayloadEncoding = "base64"
)

// ============================================================================
// CONFIGURATION (FUNCTIONAL OPTIONS)
// ============================================================================

// Option configures a Consumer. Options are applied during New().
type Option func(*consumerConfig) error

// New creates a new Consumer with the provided options.
// Returns error if required options are missing or invalid.
//
// Required options: WithTopic, WithBrokers, WithConsumerGroup, and exactly one of WithHandler/WithBatchHandler.
func New(options ...Option) (Consumer, error)

// ============================================================================
// REQUIRED OPTIONS
// ============================================================================

// WithTopic specifies the Kafka topic to consume from.
// Required. Must be non-empty.
func WithTopic(topic string) Option

// WithBrokers specifies the Kafka broker addresses.
// Required. Must provide at least one broker.
func WithBrokers(brokers ...string) Option

// WithConsumerGroup specifies the consumer group ID.
// Required. Multiple consumers with the same group ID will share partition load.
func WithConsumerGroup(groupID string) Option

// WithHandler specifies the message processing function (single-message mode).
// Required unless WithBatchHandler is used.
func WithHandler(handler Handler) Option

// WithBatchHandler specifies a batch processing function.
// Required unless WithHandler is used. Enables batch mode.
func WithBatchHandler(handler BatchHandler) Option

// ============================================================================
// OPTIONAL CONFIGURATION
// ============================================================================

// WithErrorStrategy specifies how to handle message processing failures.
// Default: Retry(WithMaxAttempts(3), WithRetryTopic("<topic>.retry"), WithDLQTopic("<topic>.dlq")).
func WithErrorStrategy(strategy ErrorStrategy) Option

// WithBatchSize specifies the maximum number of messages per batch.
// Only applies when using WithBatchHandler.
// Default: 100
func WithBatchSize(size int) Option

// WithBatchTimeout specifies how long to wait before processing a partial batch.
// Only applies when using WithBatchHandler.
// Default: 5 seconds
func WithBatchTimeout(timeout time.Duration) Option

// WithPollTimeout specifies the Kafka poll timeout.
// Default: 100ms
func WithPollTimeout(timeout time.Duration) Option

// WithShutdownTimeout specifies the maximum time to wait for graceful shutdown.
// Default: 30 seconds
func WithShutdownTimeout(timeout time.Duration) Option

// Logger allows users to plug in their own logging implementation.
type Logger interface {
	Debug(msg string, keyvals ...any)
	Info(msg string, keyvals ...any)
	Warn(msg string, keyvals ...any)
	Error(msg string, keyvals ...any)
}

// WithLogger specifies a custom logger. Default is no-op.
func WithLogger(logger Logger) Option

// WithKafkaConfig passes advanced configuration to confluent-kafka-go.
// Use this to set low-level Kafka consumer properties.
func WithKafkaConfig(config map[string]any) Option

// ============================================================================
// ERROR HANDLING STRATEGIES
// ============================================================================

// ErrorStrategy defines how message processing failures are handled.
type ErrorStrategy interface {
	// HandleError is called when a handler returns an error.
	// In single-message mode, msgs contains one message.
	// In batch mode, msgs contains all messages from the failed batch.
	// Return nil to continue consumption, error to stop the consumer.
	HandleError(ctx context.Context, msgs []*Message, handlerErr error) error

	// Name returns the strategy name for logging purposes.
	Name() string
}

// FailFast stops the consumer immediately on any handler error.
func FailFast() ErrorStrategy

// Skip logs errors, commits offsets, and continues consumption.
func Skip() ErrorStrategy

// Retry retries failed messages with Kafka-based retry topics and a DLQ.
// RetryTopic and DLQTopic are required options.
// After max attempts are exhausted, the message is sent to the DLQ and consumption continues.
func Retry(options ...RetryOption) ErrorStrategy

// CircuitBreaker pauses consumption after consecutive failures.
// CircuitBreaker uses the retry + DLQ flow and adds pause/resume behavior.
// Only supported in single-message mode.
func CircuitBreaker(options ...CircuitBreakerOption) ErrorStrategy

// ============================================================================
// ERROR STRATEGY OPTIONS
// ============================================================================

// RetryOption configures the retry error strategy.
type RetryOption func(*retryConfig) error

// WithRetryTopic sets the Kafka retry topic. Required for Retry and CircuitBreaker.
func WithRetryTopic(topic string) RetryOption

// WithDLQTopic sets the Kafka dead-letter topic. Required for Retry and CircuitBreaker.
func WithDLQTopic(topic string) RetryOption

// WithMaxAttempts sets the maximum number of retry attempts.
// Default: 3
func WithMaxAttempts(attempts int) RetryOption

// WithInitialDelay sets the initial delay before the first retry.
// Default: 1 second
func WithInitialDelay(delay time.Duration) RetryOption

// WithMaxDelay sets the maximum delay between retries.
// Default: 30 seconds
func WithMaxDelay(delay time.Duration) RetryOption

// WithBackoffMultiplier sets the exponential backoff multiplier.
// Default: 2.0
func WithBackoffMultiplier(multiplier float64) RetryOption

// WithCustomBackoff provides a custom backoff function.
// The function receives the attempt number (1-based) and returns the delay.
func WithCustomBackoff(fn func(attempt int) time.Duration) RetryOption

// WithFailedMessagePayloadEncoding configures how message payloads are encoded
// when written to retry or dead-letter queues.
func WithFailedMessagePayloadEncoding(encoding PayloadEncoding) RetryOption

// CircuitBreakerOption configures the circuit-breaker error strategy.
type CircuitBreakerOption func(*circuitBreakerConfig) error

// WithFailureThreshold sets the consecutive failure threshold before opening the circuit.
func WithFailureThreshold(threshold int) CircuitBreakerOption

// WithCooldownPeriod sets the cooldown period before transitioning to half-open.
func WithCooldownPeriod(cooldown time.Duration) CircuitBreakerOption

// WithRetryOptions configures the retry behavior used by circuit breaker.
// Includes retry topic, DLQ topic, and backoff settings.
func WithRetryOptions(options ...RetryOption) CircuitBreakerOption

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
