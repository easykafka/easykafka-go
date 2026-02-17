package easykafka

import (
	"context"
	"errors"
	"time"

	"github.com/easykafka/easykafka-go/internal/types"
)

// Config holds the consumer configuration derived from functional options.
type Config struct {
	Topic           string
	Brokers         []string
	ConsumerGroup   string
	Handler         types.Handler
	BatchHandler    types.BatchHandler
	Mode            ConsumptionMode
	BatchSize       int
	BatchTimeout    time.Duration
	PollTimeout     time.Duration
	ShutdownTimeout time.Duration
	ErrorStrategy   types.ErrorStrategy
	KafkaConfig     map[string]any
	Logger          types.Logger
}

// ConsumptionMode represents single-message or batch consumption mode.
type ConsumptionMode string

const (
	ModeSingleMessage ConsumptionMode = "single"
	ModeBatch         ConsumptionMode = "batch"
)

// ApplyDefaults applies sensible defaults to optional configuration fields.
func (c *Config) ApplyDefaults() {
	if c.PollTimeout == 0 {
		c.PollTimeout = 100 * time.Millisecond
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 30 * time.Second
	}
	if c.Logger == nil {
		// Use no-op logger by default
		c.Logger = &noOpLogger{}
	}
	if c.Mode == ModeSingleMessage && c.ErrorStrategy == nil {
		// Default error strategy is skip for single message mode
		c.ErrorStrategy = &defaultSkipStrategy{logger: c.Logger}
	}
	if c.KafkaConfig == nil {
		c.KafkaConfig = make(map[string]any)
	}
}

// noOpLogger is a minimal logger that discards all messages.
type noOpLogger struct{}

func (n *noOpLogger) Debug(msg string, keyvals ...any) {}
func (n *noOpLogger) Info(msg string, keyvals ...any)  {}
func (n *noOpLogger) Warn(msg string, keyvals ...any)  {}
func (n *noOpLogger) Error(msg string, keyvals ...any) {}

// defaultSkipStrategy is the default strategy that logs and continues.
type defaultSkipStrategy struct {
	logger types.Logger
}

func (s *defaultSkipStrategy) HandleError(ctx context.Context, msgs []*types.Message, handlerErr error) error {
	if s.logger != nil {
		s.logger.Warn("skipping failed message", "count", len(msgs), "error", handlerErr.Error())
	}
	return nil // Continue consumption
}

func (s *defaultSkipStrategy) Name() string {
	return "default-skip"
}

// Validate checks that required configuration is set correctly.
func (c *Config) Validate() error {
	if c.Topic == "" {
		return errors.New("topic is required")
	}
	if len(c.Brokers) == 0 {
		return errors.New("at least one broker is required")
	}
	if c.ConsumerGroup == "" {
		return errors.New("consumer group is required")
	}
	if c.Handler == nil && c.BatchHandler == nil {
		return errors.New("exactly one of Handler or BatchHandler must be set")
	}
	if c.Handler != nil && c.BatchHandler != nil {
		return errors.New("only one of Handler or BatchHandler can be set")
	}
	if c.Mode == ModeBatch {
		if c.BatchSize <= 0 {
			return errors.New("batch size must be positive")
		}
		if c.BatchTimeout <= 0 {
			return errors.New("batch timeout must be positive")
		}
	}
	return nil
}

// Option configures a Consumer. Options are applied during New().
type Option func(*Config) error

// WithTopic specifies the Kafka topic to consume from.
// Required. Must be non-empty.
func WithTopic(topic string) Option {
	return func(c *Config) error {
		if topic == "" {
			return errors.New("topic cannot be empty")
		}
		c.Topic = topic
		return nil
	}
}

// WithBrokers specifies the Kafka broker addresses.
// Required. Must provide at least one broker.
func WithBrokers(brokers ...string) Option {
	return func(c *Config) error {
		if len(brokers) == 0 {
			return errors.New("at least one broker must be provided")
		}
		c.Brokers = brokers
		return nil
	}
}

// WithConsumerGroup specifies the consumer group ID.
// Required. Multiple consumers with the same group ID will share partition load.
func WithConsumerGroup(groupID string) Option {
	return func(c *Config) error {
		if groupID == "" {
			return errors.New("consumer group cannot be empty")
		}
		c.ConsumerGroup = groupID
		return nil
	}
}

// WithHandler specifies the message processing function (single-message mode).
// Required unless WithBatchHandler is used.
func WithHandler(handler Handler) Option {
	return func(c *Config) error {
		if handler == nil {
			return errors.New("handler cannot be nil")
		}
		c.Handler = handler
		c.Mode = ModeSingleMessage
		return nil
	}
}

// WithBatchHandler specifies a batch processing function.
// Required unless WithHandler is used. Enables batch mode.
func WithBatchHandler(handler BatchHandler) Option {
	return func(c *Config) error {
		if handler == nil {
			return errors.New("batch handler cannot be nil")
		}
		c.BatchHandler = handler
		c.Mode = ModeBatch
		if c.BatchSize <= 0 {
			c.BatchSize = 100 // default batch size
		}
		if c.BatchTimeout <= 0 {
			c.BatchTimeout = 5 * time.Second // default batch timeout
		}
		return nil
	}
}

// WithErrorStrategy specifies how to handle message processing failures.
// Default: Skip strategy (log and continue).
func WithErrorStrategy(strategy types.ErrorStrategy) Option {
	return func(c *Config) error {
		if strategy == nil {
			return errors.New("error strategy cannot be nil")
		}
		c.ErrorStrategy = strategy
		return nil
	}
}

// WithBatchSize specifies the maximum number of messages per batch.
// Only applies when using WithBatchHandler.
// Default: 100
func WithBatchSize(size int) Option {
	return func(c *Config) error {
		if size <= 0 {
			return errors.New("batch size must be positive")
		}
		c.BatchSize = size
		return nil
	}
}

// WithBatchTimeout specifies how long to wait before processing a partial batch.
// Only applies when using WithBatchHandler.
// Default: 5 seconds
func WithBatchTimeout(timeout time.Duration) Option {
	return func(c *Config) error {
		if timeout <= 0 {
			return errors.New("batch timeout must be positive")
		}
		c.BatchTimeout = timeout
		return nil
	}
}

// WithPollTimeout specifies the Kafka poll timeout.
// Default: 100ms
func WithPollTimeout(timeout time.Duration) Option {
	return func(c *Config) error {
		if timeout < 10*time.Millisecond {
			return errors.New("poll timeout must be at least 10ms")
		}
		c.PollTimeout = timeout
		return nil
	}
}

// WithShutdownTimeout specifies the maximum time to wait for graceful shutdown.
// Default: 30 seconds
func WithShutdownTimeout(timeout time.Duration) Option {
	return func(c *Config) error {
		if timeout <= 0 {
			return errors.New("shutdown timeout must be positive")
		}
		c.ShutdownTimeout = timeout
		return nil
	}
}

// WithLogger specifies a custom logger. Default is no-op.
func WithLogger(logger types.Logger) Option {
	return func(c *Config) error {
		if logger == nil {
			return errors.New("logger cannot be nil")
		}
		c.Logger = logger
		return nil
	}
}

// WithKafkaConfig passes advanced configuration to confluent-kafka-go.
// Use this to set low-level Kafka consumer properties.
func WithKafkaConfig(config map[string]any) Option {
	return func(c *Config) error {
		if config == nil {
			return errors.New("kafka config cannot be nil")
		}
		c.KafkaConfig = config
		return nil
	}
}
