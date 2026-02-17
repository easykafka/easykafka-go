# Phase 0: Research & Technology Decisions

**Feature**: Easy Kafka Consumer Library  
**Date**: 2026-02-17  
**Purpose**: Document technology choices and implementation patterns used by the plan

## Technology Decisions

### 1. Go 1.19+ as Implementation Language

**Decision**: Use Go 1.19 or later for the library implementation.

**Rationale**:
- Concurrency primitives fit polling + worker dispatch architecture.
- Strong `context.Context` support for shutdown and cancellation semantics.
- Performance targets (10k msg/s) are reachable with Go + librdkafka.

**Alternatives Considered**:
- Java/Kotlin: heavier runtime and less ergonomic for the target user base.
- Python: throughput and latency risks for the performance goals.
- Rust: steeper adoption curve for a developer-experience-focused library.

---

### 2. confluent-kafka-go v2.x for Kafka Operations

**Decision**: Wrap `confluent-kafka-go` v2.x and delegate all Kafka protocol work to it.

**Rationale**:
- Aligns with Thin Wrapper Philosophy and existing dependencies in the spec.
- Provides robust consumer group handling, rebalancing, and offset commits.
- Production-grade performance via `librdkafka`.

**Alternatives Considered**:
- `segmentio/kafka-go`, `sarama`, `franz-go`: less aligned with spec and constitution.

**Best Practices Applied**:
- Keep the poll loop responsive; dispatch handler work to workers.
- Commit offsets only after successful processing or strategy-defined success.
- On rebalance revoke, stop dispatching and complete in-flight work before commit.
- Use pause/resume on partitions for backpressure while continuing to poll.

---

### 3. Kafka Retry + DLQ Pattern

**Decision**: Use a Kafka retry topic and DLQ with a dedicated internal retry consumer.

**Rationale**:
- Avoids head-of-line blocking on the primary topic.
- Retry scheduling is durable across restarts.
- Separation keeps retry lag from affecting primary consumption.

**Alternatives Considered**:
- In-place retries within the primary consumer (risking poll timeouts).
- Multi-tier retry topics (adds complexity without immediate need).

**Best Practices Applied**:
- Store retry metadata in headers: `x-original-topic`, `x-original-offset`,
  `x-retry-at`, `x-retry-attempt`, `x-error-message`, `x-payload-encoding`.
- Exponential backoff with jitter; encode the next due time in `x-retry-at`.
- Requeue early retry messages rather than sleeping in the retry consumer.
- DLQ messages include original payload, error details, and attempt count.

---

### 4. Integration Testing with testcontainers-go

**Decision**: Use `testcontainers-go` for Kafka integration tests.

**Rationale**:
- Required by the constitution for real Kafka testing.
- Provides clean setup/teardown and deterministic test environments.

**Alternatives Considered**:
- Manual Docker Compose: brittle in CI and harder to clean up.

**Best Practices Applied**:
- Use a fixed Kafka image version and wait on readiness + broker probe.
- Reuse a container across tests via `TestMain` or `sync.Once`.
- Use unique topics per test to avoid cross-test interference.

---

### 5. Unit Testing with testify

**Decision**: Use `testify` for assertions and helpers in unit tests.

**Rationale**:
- Already specified as a dev dependency.
- Improves readability and reduces boilerplate in table-driven tests.

**Alternatives Considered**:
- Standard library `testing` only: more verbose assertions.

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
    // msgs contains 1 message in single mode, N messages in batch mode
    // Returns nil to continue consumption, error to stop consumer
    HandleError(ctx context.Context, msgs []*Message, handlerErr error) error
    
    // Name returns the strategy name for logging
    Name() string
}
```

**Built-in Strategies**:
1. **Fail-Fast**: Return error immediately, stop consumer
2. **Skip**: Log error, commit offset(s), continue
3. **Retry**: Uses Kafka-based retry queue with configurable backoff (fixed/exponential/custom). Failed messages written to retry topic, automatically processed by internal retry consumer. After max attempts exhausted, messages sent to DLQ and consumption continues. In batch mode, each message in failed batch written to retry queue individually.
4. **Circuit-Breaker**: Same as Retry strategy but adds consumption pausing based on consecutive failure thresholds. Tracks failures from primary topic only, suspends retry queue during circuit open/half-open states.

**Strategy Configuration**:
```go
// Retry strategy with Kafka-based retry queue and DLQ
retry := easykafka.NewRetryStrategy(
    easykafka.WithRetryTopic("orders.retry"),    // Kafka retry queue topic
    easykafka.WithDLQTopic("orders.dlq"),        // Dead-letter queue topic
    easykafka.WithMaxAttempts(3),                 // Retry up to 3 times
    easykafka.WithBackoff(easykafka.ExponentialBackoff{
        InitialDelay: 1 * time.Second,
        MaxDelay:     30 * time.Second,
        Multiplier:   2.0,
    }),
    easykafka.WithFailedMessagePayloadEncoding(easykafka.PayloadEncodingJSON), // Optional: JSON or Base64
)

consumer, _ := easykafka.New(
    easykafka.WithTopic("orders"),
    easykafka.WithBrokers("localhost:9092"),
    easykafka.WithConsumerGroup("processors"),
    easykafka.WithHandler(processOrder),
    easykafka.WithErrorStrategy(retry),
)

// User creates ONE consumer - library internally spawns TWO:
// 1. Main consumer: processes "orders" topic
// 2. Retry consumer: processes "orders.retry" topic (automatic)
```

**How Retry Strategy Works**:
1. Handler fails on message from main topic → write to retry queue with headers (attempt count, retry time, error)
2. Internal retry consumer polls retry queue → waits until retry time elapsed → calls same handler
3. Handler succeeds → commit offset (done)
4. Handler fails again → increment attempt, write back to retry queue with updated headers
5. After max attempts → write to DLQ topic with full metadata → commit offset → continue

**Best Practices Applied**:
- Retry strategy is stateless - state stored in Kafka message headers
- Library automatically manages internal retry consumer lifecycle
- Each strategy logs its actions for observability
- Strategies respect context cancellation for shutdown
- DLQ messages include metadata (original topic, partition, offset, error, payload encoding, timestamp)
- Retry queue consumption pauses during circuit breaker open/half-open states

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
                // In single-message mode: wrap message in slice []*Message{msg}
                // In batch mode: pass all messages from failed batch
                msgs := []*Message{msg}  // Single message mode
                if err := e.strategy.HandleError(ctx, msgs, err); err != nil {
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

**Atomic Batch Processing**:
- Batches are atomic units for handler invocation - all messages in batch processed together
- If BatchHandler returns error, error strategy applies to entire batch
- **Retry strategy**: Each message in failed batch written to retry queue individually with retry metadata. Messages can then be retried individually by retry consumer, not as a batch.
- **Skip strategy**: Entire batch is skipped (all offsets committed together)
- **FailFast strategy**: Consumer stops immediately (batch atomic failure)
- **CircuitBreaker**: NOT SUPPORTED in batch mode (validation error at consumer creation)
- Simplifies error handling and maintains message ordering guarantees within each batch

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
1. **Handler registration API**: How do users provide handlers? Constructor parameter vs RegisterHandler method? (Decision: Constructor parameter via WithHandler option)
2. **Message metadata access**: How do users access headers, offset, timestamp if needed? (Decision: Via MessageFromContext(ctx))
3. **Batch error handling**: How to handle partial batch failures? (Decision: Batch processing is ATOMIC for handler invocation - BatchHandler receives all messages together and succeeds/fails atomically. With Retry strategy, if batch fails, each message is written to retry queue individually and can be retried separately. Skip/FailFast strategies treat batch atomically. No partial batch success tracking in v1 for simplicity.)
4. **Graceful shutdown timeout**: Default value? (Decision: 30 seconds)
5. **Retry mechanism**: In-memory state vs Kafka-based? (Decision: Kafka-based retry queue using dedicated topic. Library automatically spawns internal retry consumer. Stateless design with retry metadata in message headers. Survives consumer restarts.)

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
