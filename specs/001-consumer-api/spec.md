# Feature Specification: Easy Kafka Consumer Library

**Feature Branch**: `001-consumer-api`  
**Created**: 2026-02-09  
**Status**: Draft  
**Input**: User description: "Golang library for simple Kafka message consumption with handler-based interface, metadata-driven configuration, pluggable error strategies, and batch processing support"

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Simple Handler Registration (Priority: P1)

A developer wants to consume messages from a Kafka topic without learning Kafka internals. They write a simple handler function with signature `func([]byte) error`, provide metadata (topic name, brokers, consumer group), and start consuming. The library handles all Kafka complexity including connections, consumer group coordination, offset commits, and error recovery.

**Why this priority**: This is the MVP - the absolute minimum functionality that delivers value. If this doesn't work, nothing else matters.

**Independent Test**: Write a handler, configure topic/brokers/group, produce test messages to Kafka, verify handler receives raw bytes and processes them. Success = handler is called with correct message payloads.

**Acceptance Scenarios**:

1. **Given** a handler `func(msg []byte) error { return nil }` and metadata specifying topic "orders", brokers, and consumer group "processors", **When** the consumer starts, **Then** messages from "orders" topic are delivered as byte arrays to the handler
2. **Given** the handler returns `nil`, **When** message processing completes, **Then** the offset is automatically committed and the next message is consumed
3. **Given** the handler returns an error, **When** processing fails, **Then** the configured error handling strategy is applied (default: retry with exponential backoff)
4. **Given** multiple consumer instances in the same group, **When** they start, **Then** Kafka partitions are automatically distributed across instances without user intervention

---

### User Story 2 - Metadata-Driven Configuration (Priority: P1)

A developer wants to configure consumer behavior without writing configuration files or parsing environment variables. They use Go struct tags like `kafka:"topic=orders,consumer-group=processors"` or functional options like `WithRetryStrategy(Retry(WithMaxAttempts(3), WithOnMaxAttemptsExceeded(SendToDLQ("orders-dlq"))))` to specify all settings at handler registration.

**Why this priority**: Metadata-driven design is a core constitutional principle. Without this, the library just becomes another wrapper with the same complexity as existing solutions.

**Independent Test**: Register handlers with various metadata combinations (different topics, error strategies, batch sizes) and verify the consumer behaves according to each configuration.

**Acceptance Scenarios**:

1. **Given** struct tags with topic and consumer group metadata, **When** the consumer is initialized, **Then** it connects to the specified topic in the specified group
2. **Given** functional options for error strategy, **When** a handler error occurs, **Then** the specified strategy (retry with optional DLQ/skip/fail-fast/circuit-breaker) is executed
3. **Given** invalid metadata (empty topic, negative batch size), **When** the consumer is created, **Then** validation errors are returned before connecting to Kafka
4. **Given** advanced confluent-kafka-go options via passthrough, **When** the consumer starts, **Then** those low-level settings are applied to the underlying Kafka consumer

---

### User Story 3 - Pluggable Error Handling Strategies (Priority: P1)

A developer needs different failure handling for different use cases. For critical order processing, they configure fail-fast to stop on any error. For analytics ingestion, they configure skip to log and continue. For user action tracking, they configure retry with exponential backoff and, after exhausting retries, either stop the consumer or write to a dead-letter queue and continue.

**Why this priority**: Error handling is not optional in production systems. Different domains require different failure semantics, making this a P1 requirement alongside basic consumption.

**Independent Test**: Configure each strategy independently, force handler failures, and verify the expected behavior (retries, skips, DLQ writes, or shutdown).

**Acceptance Scenarios**:

1. **Given** fail-fast strategy, **When** handler returns an error, **Then** the consumer stops immediately and returns the error to the caller
2. **Given** skip strategy, **When** handler returns an error, **Then** the error is logged, offset is committed, and consumption continues with the next message
3. **Given** retry strategy with 3 attempts and exponential backoff, **When** handler fails, **Then** the message is retried up to 3 times with delays of 1s, 2s, 4s before executing the configured max-attempts action
4. **Given** retry strategy with SendToDLQ action and DLQ topic "orders-dlq", **When** handler fails after all retries, **Then** the failed message is written to the DLQ topic with error metadata and normal consumption continues
5. **Given** retry strategy with FailConsumer action, **When** handler fails after all retries, **Then** the consumer stops and returns an error
6. **Given** circuit-breaker strategy with threshold of 10 failures, **When** 10 consecutive errors occur, **Then** consumption pauses for a cooldown period before resuming

---

### User Story 4 - Batch Processing for High Throughput (Priority: P2)

A developer processing analytics events needs maximum throughput. They write a batch handler `func([][]byte) error` and configure batch size (100 messages) and batch timeout (5 seconds). The library accumulates messages and delivers them together, achieving 3x higher throughput than single-message processing.

**Why this priority**: Essential for high-volume scenarios but not needed for MVP. Single-message processing must work first before adding batch mode complexity.

**Independent Test**: Register a batch handler, produce multiple messages rapidly, verify they arrive grouped according to batch size/timeout rules.

**Acceptance Scenarios**:

1. **Given** batch handler and batch size of 100, **When** 100 messages are available, **Then** all 100 messages are delivered together to the handler as `[][]byte`
2. **Given** batch handler and batch timeout of 5 seconds, **When** only 50 messages arrive within 5 seconds, **Then** those 50 messages are delivered without waiting for more
3. **Given** batch handler returns `nil`, **When** processing succeeds, **Then** offsets for all messages in the batch are committed atomically
4. **Given** batch handler returns an error, **When** processing fails, **Then** the entire batch is treated as a unit for the error strategy (retry whole batch or skip whole batch)

---

### User Story 5 - Graceful Shutdown and Context Management (Priority: P2)

A developer wants their service to shut down cleanly on SIGTERM. They pass a context to the consumer, and when cancelled, the consumer stops accepting new messages, allows in-flight handlers to complete (with timeout), commits final offsets, and closes connections before exiting.

**Why this priority**: Critical for production but not needed for initial testing. Can be added after basic consumption works.

**Independent Test**: Start consumption, send cancellation signal, verify in-flight messages complete, offsets are committed, and resources are cleaned up within the timeout period.

**Acceptance Scenarios**:

1. **Given** a running consumer with context support, **When** the context is cancelled, **Then** no new messages are fetched and the consumer waits for in-flight handlers to complete
2. **Given** a graceful shutdown timeout of 30 seconds, **When** handlers complete within 30 seconds, **Then** offsets are committed and the consumer exits cleanly
3. **Given** handlers taking longer than the timeout, **When** the timeout expires, **Then** the consumer force-stops and returns a timeout error
4. **Given** a handler with context parameter `func(ctx context.Context, msg []byte) error`, **When** shutdown begins, **Then** the context is cancelled allowing the handler to abort long operations

---

### Edge Cases

- What happens when a handler panics during message processing?
- How does the system handle Kafka broker failures or network partitions during consumption?
- What occurs when consumer group rebalancing happens while a message is being processed?
- How are messages handled when they exceed reasonable size limits (e.g., 10MB+ messages)?
- What happens if the dead-letter topic itself is unavailable or full?
- How does the system behave when offset commits fail repeatedly?
- What occurs if multiple conflicting error strategies are configured?
- How does batch processing handle partial batch failures (some messages succeed, some fail)?
- What happens when a consumer joins a group that has never committed offsets (no starting position)?

## Requirements *(mandatory)*

### Functional Requirements

#### Core Handler Interface

- **FR-001**: Library MUST accept single-message handlers with signature `func([]byte) error`
- **FR-002**: Library MUST accept batch handlers with signature `func([][]byte) error`
- **FR-003**: Library MUST accept context-aware handlers with signatures `func(context.Context, []byte) error` and `func(context.Context, [][]byte) error`
- **FR-004**: Library MUST deliver message payloads as raw byte arrays without automatic deserialization
- **FR-005**: Library MUST interpret `nil` error return as successful processing and commit the offset
- **FR-006**: Library MUST interpret non-nil error return as processing failure and apply the configured error strategy

#### Metadata-Driven Configuration

- **FR-007**: Library MUST accept configuration via Go struct tags for handler registration
- **FR-008**: Library MUST accept configuration via functional options pattern for consumer creation
- **FR-009**: Library MUST require minimum configuration: Kafka broker addresses, topic name, consumer group ID
- **FR-010**: Library MUST validate all configuration before connecting to Kafka and return clear errors for invalid values
- **FR-011**: Library MUST support optional configuration including batch size, batch timeout, error strategies, and custom offset behavior
- **FR-012**: Library MUST provide passthrough access to confluent-kafka-go configuration for advanced users
- **FR-013**: Library MUST NOT require configuration files or environment variable parsing

#### Consumer Group Management

- **FR-014**: Library MUST automatically join the specified consumer group on startup
- **FR-015**: Library MUST automatically handle partition assignment and rebalancing without user intervention
- **FR-016**: Library MUST support multiple consumer instances in the same group for load balancing
- **FR-017**: Library MUST commit offsets only after successful message processing (or per error strategy rules)
- **FR-018**: Library MUST handle rebalancing gracefully, finishing in-flight messages before releasing partitions

#### Error Handling Strategies

- **FR-019**: Library MUST provide a fail-fast strategy that stops consumption immediately on handler errors
- **FR-020**: Library MUST provide a skip strategy that logs errors, commits offsets, and continues consumption
- **FR-021**: Library MUST provide a retry strategy with configurable attempts, delay type (fixed/exponential/custom), and backoff parameters
- **FR-022**: Retry strategy MUST support configurable action after max attempts are exhausted: either stop consumer (FailConsumer) or write to dead-letter queue and continue (SendToDLQ)
- **FR-023**: Library MUST provide a circuit-breaker strategy that pauses consumption after a threshold of consecutive failures
- **FR-024**: Library MUST allow users to select error strategy at consumer creation time via functional options
- **FR-025**: Retry strategy MUST respect maximum attempt limits and not retry indefinitely
- **FR-026**: Retry strategy with SendToDLQ action MUST include original message payload, error details, and timestamps in DLQ messages

#### Batch Processing Mode

- **FR-027**: Batch mode MUST accumulate messages up to the configured batch size
- **FR-028**: Batch mode MUST deliver partial batches when the configured timeout expires, even if batch size not reached
- **FR-029**: Batch mode MUST treat the entire batch as an atomic unit for offset commits
- **FR-030**: Batch mode MUST apply error strategies at batch level (retry/skip entire batch)
- **FR-031**: Batch mode MUST maintain message ordering within each batch as received from Kafka

#### Lifecycle and Shutdown

- **FR-032**: Library MUST provide a Start method to begin consuming messages
- **FR-033**: Library MUST provide a Shutdown method for graceful termination with a timeout parameter
- **FR-034**: Library MUST support context cancellation as a trigger for graceful shutdown
- **FR-035**: During shutdown, library MUST stop fetching new messages immediately
- **FR-036**: During shutdown, library MUST wait for in-flight handlers to complete before exiting
- **FR-037**: During shutdown, library MUST commit final offsets for all completed messages
- **FR-038**: During shutdown, library MUST close all Kafka connections and release resources
- **FR-039**: If shutdown timeout expires, library MUST force-stop and return a timeout error

#### Safety and Reliability

- **FR-040**: Library MUST recover from handler panics and treat them as errors for the error handling strategy
- **FR-041**: Library MUST handle temporary Kafka broker unavailability with automatic reconnection attempts
- **FR-042**: Library MUST provide at-least-once delivery semantics (messages may be redelivered but never lost)
- **FR-043**: Library MUST prevent offset commits for messages that failed processing (unless skip strategy is used)
- **FR-044**: Library MUST log significant lifecycle events (startup, shutdown, rebalancing, errors) for observability

#### Architecture Compliance

- **FR-045**: Library MUST wrap confluent-kafka-go and NOT reimplement Kafka protocol operations
- **FR-046**: Library MUST use confluent-kafka-go for all Kafka consumer operations (fetch, commit, group management)
- **FR-047**: Library MUST expose only simplified APIs and hide confluent-kafka-go complexity from users
- **FR-048**: Library MUST be usable by developers with zero Kafka knowledge

### Key Entities

- **Consumer**: Main library type managing the consumption lifecycle, wrapping confluent-kafka-go consumer, coordinating strategy execution
- **Handler**: User-provided function processing message payloads, with signature `func([]byte) error` or batch equivalent
- **HandlerMetadata**: Configuration struct containing topic, consumer group, Kafka brokers, error strategy, batch settings, and advanced options
- **ErrorStrategy**: Interface defining failure handling behavior with implementations for fail-fast, skip, retry (with optional DLQ action), and circuit-breaker
- **ConsumptionMode**: Configuration determining single-message vs batch processing behavior
- **Message**: Internal representation of Kafka message containing payload bytes plus optional metadata (offset, partition, timestamp, headers)
- **StrategyContext**: Context object passed to error strategies containing message details, attempt count, and error information

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Developers can implement a working Kafka consumer in fewer than 10 lines of code (handler function + metadata + consumer creation + start)
- **SC-002**: Library handles single-message throughput of at least 10,000 messages per second on standard hardware (4 CPU, 8GB RAM)
- **SC-003**: Batch processing achieves at least 3x throughput improvement compared to single-message mode for the same workload
- **SC-004**: Consumer startup time from initialization to first message consumption is under 2 seconds
- **SC-005**: Graceful shutdown completes within the configured timeout in 99% of normal scenarios
- **SC-006**: Zero messages are lost during consumer group rebalancing events (at-least-once guarantee maintained)
- **SC-007**: Error strategies execute correctly in 100% of failure scenarios (correct retry counts, DLQ writes, etc.)
- **SC-008**: Library achieves greater than 80% code coverage with comprehensive unit and integration tests
- **SC-009**: Library introduces less than 5% performance overhead compared to direct use of confluent-kafka-go
- **SC-010**: Documentation enables Kafka-unfamiliar developers to implement their first consumer in under 15 minutes
- **SC-011**: Handler panics are recovered without crashing the consumer process in 100% of cases
- **SC-012**: Offset commits maintain consistency such that zero duplicate processing occurs in normal operation

## Assumptions

1. **Kafka Infrastructure**: Kafka cluster is already deployed and accessible; library does not provision infrastructure
2. **Consumer Groups**: Users understand consumer groups enable load balancing; library handles coordination but users choose group IDs
3. **Message Format**: Users handle their own serialization/deserialization; library only works with raw bytes to remain format-agnostic
4. **Network Stability**: Reasonably stable network exists between consumers and Kafka brokers; library retries transient failures but cannot compensate for sustained partitions
5. **Resource Allocation**: Users deploy with adequate memory/CPU for their throughput requirements; library does not enforce resource limits
6. **Single Topic**: Initial implementation assumes one topic per consumer instance; multi-topic support is future work
7. **Offset Management**: Automatic offset commits are acceptable for most users; advanced manual control can be added later
8. **Error Strategy Selection**: Error handling strategy is chosen at consumer creation, not changed dynamically per message
9. **Go Version**: Library requires Go 1.19 or later for modern features
10. **Confluent Kafka Go**: confluent-kafka-go v2.x is the underlying client library
11. **At-Least-Once Semantics**: Users accept potential duplicate processing; exactly-once semantics are out of scope initially
12. **Handler Statelessness**: Handlers are stateless or manage their own state; library does not provide state management

## Out of Scope

- **Message Production**: Library focuses on consumption only; use confluent-kafka-go directly or separate producer library for publishing messages
- **Schema Registry**: No built-in support for Avro, Protobuf, schema evolution, or schema registry integration; users handle serialization
- **Kafka Administration**: No cluster management, topic creation, ACL management, or administrative operations
- **Message Transformation**: No built-in filtering, routing, or transformation pipelines; users implement logic in handlers
- **Observability Platform**: No built-in Prometheus exporters or OpenTelemetry instrumentation; library provides hooks for users to integrate their own
- **Multi-Topic Consumption**: Single topic per consumer in initial version; pattern-based or multi-topic subscription is future enhancement
- **Exactly-Once Semantics**: No support for Kafka transactions or exactly-once processing in initial version
- **Custom Partition Assignment**: Uses default Kafka partition assignment; custom assignors out of scope
- **Stream Processing**: No windowing, aggregations, joins, or complex stream operations; library is for simple message consumption
- **Message Replay**: No built-in time-travel or message replay from arbitrary timestamps; users can implement via offset management if needed

## Dependencies

- **confluent-kafka-go** (v2.x): Required - provides Kafka protocol implementation and consumer client
- **Go standard library**: Required - context, sync, time, errors, fmt packages
- **testcontainers-go**: Development only - for integration tests with real Kafka cluster
- **testify**: Development only - for test assertions and mocking interfaces

## Architecture Principles (from Constitution)

This specification adheres to the EasyKafka Go Constitution:

1. **Simplicity First** (NON-NEGOTIABLE): All Kafka complexity hidden behind simple handler interface
2. **Metadata-Driven Configuration**: All configuration via struct tags or functional options, no config files
3. **Minimal Handler Interface**: Handler signatures are just `func([]byte) error` or batch equivalent
4. **Strategy Pattern Support**: Pluggable consumption and error handling strategies
5. **Thin Wrapper Philosophy**: Leverages confluent-kafka-go, focuses on ergonomics not reimplementation
