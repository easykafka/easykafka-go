# Phase 0: Research & Technology Decisions

**Feature**: Easy Kafka Consumer Library  
**Date**: 2026-02-09  
**Purpose**: Document technology choices, best practices, and architectural patterns

## Technology Decisions

### 1. Go 1.19+ as Implementation Language

**Decision**: Use Go 1.19 or later as the implementation language

**Rationale**:
- Native concurrency primitives (goroutines, channels) perfect for message consumption workload
- Strong standard library for context management, error handling, and synchronization
- Excellent performance characteristics for high-throughput scenarios
- Native Kafka client (confluent-kafka-go) provides production-grade reliability
- Generics support (1.19+) enables type-safe strategy patterns without reflection
- Simple deployment model (single binary) beneficial for library users

**Alternatives Considered**:
- **Java/Kotlin**: More mature Kafka ecosystem but violates simplicity principle with heavyweight runtime
- **Python**: Easier prototyping but performance inadequate for 10k+ msg/sec target
- **Rust**: Superior performance but steep learning curve would reduce library adoption

**Best Practices Applied**:
- Use `context.Context` for cancellation and timeout propagation
- Follow Go Code Review Comments and Effective Go guidelines
- Accept interfaces, return structs for API design
- Use functional options pattern for optional configuration
- Avoid `panic()` in library code; always return errors

---

### 2. confluent-kafka-go v2 as Kafka Client

**Decision**: Wrap confluent-kafka-go v2.x as the underlying Kafka protocol implementation

**Rationale**:
- Production-proven with years of battle-testing in high-scale environments
- Based on librdkafka C library - one of the fastest Kafka clients available
- Excellent performance characteristics meet our <5% overhead target
- Comprehensive feature support (consumer groups, rebalancing, offset management)
- Active maintenance by Confluent ensures compatibility with new Kafka versions
- Constitutional requirement (Principle V: Thin Wrapper Philosophy)

**Alternatives Considered**:
- **sarama (Shopify)**: Pure Go but historically more bugs and slower performance
- **segmentio/kafka-go**: Good pure-Go option but lacks feature parity with librdkafka
- **franz-go**: Modern and fast but less ecosystem adoption

**Integration Pattern**:
- Wrap `kafka.Consumer` type from confluent-kafka-go
- Expose passthrough for advanced `kafka.ConfigMap` options
- Hide all Kafka-specific types from public API
- Use adapter pattern to translate confluent-kafka-go events to internal events

**Best Practices Applied**:
- Pin to major version (v2.x) for stability
- Use consumer group protocol for load balancing
- Enable auto-commit only when error strategy allows
- Configure adequate poll timeout for responsive shutdown

---

### 3. github.com/rs/zerolog for Structured Logging

**Decision**: Use zerolog for internal logging with minimal allocations

**Rationale**:
- Zero-allocation JSON logger critical for high-throughput scenarios
- Structured logging enables better observability in production
- Level-based logging (debug, info, warn, error) for operational control
- Contextual logging with fields supports tracing message flow
- Very small library footprint aligns with thin wrapper philosophy

**Alternatives Considered**:
- **Go stdlib log**: Too basic, no structured logging or levels
- **zap (Uber)**: Comparable performance but more complex API
- **logrus**: Slower performance, allocates more

**Logging Strategy**:
- Log lifecycle events (startup, shutdown, rebalancing) at INFO level
- Log handler errors at WARN/ERROR with message metadata
- Log retry attempts at DEBUG with attempt count
- Provide callback hooks for users to integrate custom observability
- Never log sensitive message payloads by default

**Best Practices Applied**:
- Use logger chaining for contextual fields (consumer group, topic, partition)
- Respect standard log level conventions
- Make logging configurable (allow disabling or custom writer)
- Log structured data, not concatenated strings

---

### 4. github.com/golang/mock for Interface Mocking

**Decision**: Use gomock for generating mocks of internal interfaces

**Rationale**:
- Official Go mocking framework with strong community adoption
- Generates type-safe mocks from interface definitions
- Supports expectation-based testing for complex interactions
- Integrates with `go generate` for automated mock updates
- Enables testing strategy implementations without real Kafka

**Alternatives Considered**:
- **testify/mock**: Manual mock creation, more boilerplate
- **mockery**: Good alternative but gomock more widely adopted
- **Hand-written mocks**: Too much maintenance burden

**Mocking Strategy**:
- Mock internal interfaces (ErrorStrategy, KafkaConsumer adapter)
- Do NOT mock public API (test the real implementation)
- Generate mocks via `//go:generate` directives
- Store generated mocks in `tests/mocks/` directory

**Interfaces to Mock**:
```go
// Internal Kafka client adapter (wraps confluent-kafka-go)
type KafkaClient interface {
    Poll(timeout time.Duration) Message
    Commit(msg Message) error
    Close() error
}

// Error strategy interface
type ErrorStrategy interface {
    HandleError(ctx context.Context, msg Message, err error) error
}
```

**Best Practices Applied**:
- Mock external dependencies (Kafka), test internal logic
- Use table-driven tests with different mock behaviors
- Verify mock expectations to catch incorrect usage

---

### 5. testcontainers-go for Integration Testing

**Decision**: Use testcontainers-go to spin up real Kafka clusters for integration tests

**Rationale**:
- Tests against real Kafka behavior catch edge cases mocks miss
- Eliminates manual Docker Compose management
- Automatic cleanup prevents port conflicts and zombie containers
- Constitutional testing requirement: integration tests must use real Kafka
- Supports testing consumer group rebalancing, offset commits, broker failures

**Alternatives Considered**:
- **Manual Docker Compose**: Brittle, hard to parallelize, no automatic cleanup
- **Embedded Kafka**: Doesn't exist for Go, would be impractical
- **Mock-only testing**: Insufficient for wrapper library correctness

**Integration Test Strategy**:
```go
// Test lifecycle: Start container → Produce messages → Start consumer → Verify
func TestSingleMessageConsumption(t *testing.T) {
    // Start Kafka container
    kafka := testcontainers.StartKafka(ctx)
    defer kafka.Terminate(ctx)
    
    // Produce test message
    producer := kafka.Producer()
    producer.Produce("test-topic", []byte("message"))
    
    // Test consumer receives message
    received := make(chan []byte, 1)
    consumer := easykafka.New(
        easykafka.WithHandler(func(msg []byte) error {
            received <- msg
            return nil
        }),
        easykafka.WithTopic("test-topic"),
        easykafka.WithBrokers(kafka.Brokers()),
    )
    
    consumer.Start(ctx)
    assert.Equal(t, "message", <-received)
}
```

**Integration Test Coverage**:
1. Basic consumption (single-message and batch)
2. Error strategy behaviors (retry, skip, DLQ, fail-fast)
3. Consumer group rebalancing
4. Graceful shutdown and offset commits
5. Handler panic recovery
6. Broker connection failures and reconnection

**Best Practices Applied**:
- Tag integration tests: `//go:build integration`
- Run in CI with `go test -tags=integration`
- Use parallel test execution where possible
- Clean up resources in deferred functions
- Use short timeouts to fail fast on issues

---

### 6. Functional Options Pattern for Configuration

**Decision**: Use functional options pattern for consumer configuration

**Rationale**:
- Idiomatic Go pattern for optional configuration
- Backwards compatible - can add new options without breaking changes
- Type-safe at compile time
- Self-documenting API (option names are function names)
- Supports required vs optional parameters clearly

**Pattern Implementation**:
```go
// Option configures a Consumer
type Option func(*Consumer) error

// WithTopic specifies the Kafka topic to consume
func WithTopic(topic string) Option {
    return func(c *Consumer) error {
        if topic == "" {
            return errors.New("topic cannot be empty")
        }
        c.config.Topic = topic
        return nil
    }
}

// Consumer creation with options
consumer, err := easykafka.New(
    easykafka.WithTopic("orders"),
    easykafka.WithBrokers("localhost:9092"),
    easykafka.WithConsumerGroup("processors"),
    easykafka.WithRetryStrategy(easykafka.ExponentialBackoff(3)),
)
```

**Alternatives Considered**:
- **Struct with fields**: Requires breaking changes for new options
- **Builder pattern**: More verbose, not idiomatic Go
- **Config struct**: Works but less discoverable than functions

**Best Practices Applied**:
- Validate options in option functions, not in New()
- Return errors from options for invalid configuration
- Provide sensible defaults for all optional settings
- Group related options (e.g., all retry config in one option)

---

### 7. Strategy Pattern for Error Handling

**Decision**: Use strategy pattern for pluggable error handling behaviors

**Rationale**:
- Constitutional requirement (Principle IV: Strategy Pattern Support)
- Different use cases need different failure semantics
- Allows users to implement custom strategies if needed
- Clean separation of concerns (handlers vs error handling)
- Each strategy testable in isolation

**Strategy Interface**:
```go
// ErrorStrategy defines how to handle message processing failures
type ErrorStrategy interface {
    // HandleError is called when a handler returns an error
    // Returns nil to continue consumption, error to stop consumer
    HandleError(ctx context.Context, msg *Message, handlerErr error) error
    
    // Name returns the strategy name for logging
    Name() string
}
```

**Built-in Strategies**:
1. **Fail-Fast**: Return error immediately, stop consumer
2. **Skip**: Log error, commit offset, continue
3. **Retry**: Retry with configurable backoff (fixed/exponential/custom) and configurable action after max attempts (stop consumer or write to DLQ and continue)
4. **Circuit-Breaker**: Pause after consecutive failures threshold

**Strategy Configuration**:
```go
// Retry strategy with exponential backoff and fail-fast after max attempts
retry := easykafka.NewRetryStrategy(
    easykafka.WithMaxAttempts(3),
    easykafka.WithBackoff(easykafka.ExponentialBackoff{
        InitialDelay: 1 * time.Second,
        MaxDelay:     30 * time.Second,
        Multiplier:   2.0,
    }),
    easykafka.WithOnMaxAttemptsExceeded(easykafka.FailConsumer), // Stop consumer
)

// OR retry with dead-letter queue fallback
retryWithDLQ := easykafka.NewRetryStrategy(
    easykafka.WithMaxAttempts(3),
    easykafka.WithBackoff(easykafka.ExponentialBackoff{
        InitialDelay: 1 * time.Second,
        MaxDelay:     30 * time.Second,
        Multiplier:   2.0,
    }),
    easykafka.WithOnMaxAttemptsExceeded(easykafka.SendToDLQ("orders-dlq")), // Write to DLQ and continue
)

consumer, _ := easykafka.New(
    // ... other options
    easykafka.WithErrorStrategy(retryWithDLQ),
)
```

**Best Practices Applied**:
- Strategies are stateless (or thread-safe if stateful)
- Each strategy logs its actions for observability
- Strategies respect context cancellation for shutdown
- DLQ writes (when configured in retry strategy) include metadata (original topic, error, timestamp)
- Retry strategy allows choosing action after max attempts: stop consumer or write to DLQ and continue

---

## Architectural Patterns

### 1. Adapter Pattern for Kafka Client Abstraction

**Purpose**: Isolate confluent-kafka-go dependency for testability

**Implementation**:
```go
// internal/client/adapter.go
type KafkaClient interface {
    Subscribe(topic string) error
    Poll(timeout time.Duration) (*Message, error)
    CommitOffset(msg *Message) error
    Close() error
}

// Production adapter wraps confluent-kafka-go
type ConfluentAdapter struct {
    consumer *kafka.Consumer
}

// Mock adapter for unit tests
type MockAdapter struct {
    messages chan *Message
}
```

**Benefits**:
- Unit tests don't need real Kafka or testcontainers
- Can simulate error conditions easily in tests
- Future-proof if we need to support multiple Kafka clients

---

### 2. Engine Pattern for Message Processing Loop

**Purpose**: Centralize polling, dispatch, error handling, and offset commit logic

**Implementation**:
```go
// internal/consumer/engine.go
type Engine struct {
    client    KafkaClient
    handler   Handler
    strategy  ErrorStrategy
    batchMode bool
    shutdown  chan struct{}
}

func (e *Engine) Run(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return e.gracefulShutdown()
        default:
            msg, err := e.client.Poll(100 * time.Millisecond)
            if err != nil {
                continue // Handle poll errors
            }
            if err := e.dispatch(msg); err != nil {
                if err := e.strategy.HandleError(ctx, msg, err); err != nil {
                    return err // Fatal error, stop consumer
                }
            }
        }
    }
}
```

**Benefits**:
- Single responsibility for consumption loop
- Easy to test with mock client and strategy
- Handles shutdown signals consistently

---

### 3. Recovery Wrapper for Panic Handling

**Purpose**: Prevent handler panics from crashing consumer process

**Implementation**:
```go
// internal/recovery/recovery.go
func SafeCall(ctx context.Context, handler Handler, msg []byte) (err error) {
    defer func() {
        if r := recover(); r != nil {
            err = fmt.Errorf("handler panicked: %v\nstack: %s", r, debug.Stack())
        }
    }()
    return handler(ctx, msg)
}
```

**Benefits**:
- FR-040 requirement: recover from panics
- Panics treated as errors for error strategy
- Stack trace logged for debugging

---

## Performance Considerations

### 1. Batch Processing Optimization

**Approach**: Accumulate messages in a buffer until batch size or timeout

**Implementation Strategy**:
```go
type BatchAccumulator struct {
    buffer   [][]byte
    batchSize int
    timeout  time.Duration
    timer    *time.Timer
}

func (b *BatchAccumulator) Add(msg []byte) bool {
    b.buffer = append(b.buffer, msg)
    return len(b.buffer) >= b.batchSize
}
```

**Target**: 3x throughput improvement via reduced offset commit overhead

---

### 2. Zero-Copy Message Passing

**Approach**: Pass message byte slices without copying where possible

**Considerations**:
- Kafka message lifetime managed by confluent-kafka-go
- Must copy if handler needs to retain message async
- Document that handlers should not store `[]byte` references

---

### 3. Configurable Poll Timeout

**Approach**: Balance latency vs CPU efficiency with tunable poll timeout

**Default**: 100ms poll timeout (good balance for most use cases)
**Configurable**: Allow users to tune via WithPollTimeout option

---

## Testing Strategy Summary

### Unit Tests (~80% of code coverage)
- Mock Kafka client adapter
- Test each error strategy in isolation
- Test configuration validation
- Test batch accumulation logic
- Test graceful shutdown sequences

### Integration Tests (Real Kafka)
- Single-message consumption end-to-end
- Batch processing end-to-end
- Each error strategy with actual failures
- Consumer group rebalancing simulation
- Broker failure and reconnection
- Offset commit verification

### Performance Benchmarks
- Throughput measurement (msg/sec)
- Latency measurement (p50, p95, p99)
- Memory profiling
- Comparison vs raw confluent-kafka-go

---

## Open Questions & Future Work

### Phase 1 Design Decisions Needed
1. **Handler registration API**: How do users provide handlers? Constructor parameter vs RegisterHandler method?
2. **Context propagation**: Always pass context or make it optional variant?
3. **Message metadata access**: How do users access headers, offset, timestamp if needed?
4. **Batch error handling**: Partial batch failure - retry all or just failed? (Decision: retry all for simplicity)
5. **Graceful shutdown timeout**: Default value? (Decision: 30 seconds)

### Future Enhancements (Out of Scope v1)
- Multi-topic consumption via regex patterns
- Custom partition assignment strategies
- Exactly-once semantics support
- Built-in metrics exporters (Prometheus, StatsD)
- Message transformation pipelines
- Automatic retry with dead-letter queue chaining

---

## References

- [Effective Go](https://go.dev/doc/effective_go)
- [confluent-kafka-go Documentation](https://github.com/confluentinc/confluent-kafka-go)
- [zerolog Documentation](https://github.com/rs/zerolog)
- [testcontainers-go Documentation](https://golang.testcontainers.org/)
- [Go Functional Options](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [Kafka Consumer Best Practices](https://docs.confluent.io/platform/current/clients/consumer.html)
