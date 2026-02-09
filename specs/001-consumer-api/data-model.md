# Phase 1: Data Model & Core Entities

**Feature**: Easy Kafka Consumer Library  
**Date**: 2026-02-09  
**Purpose**: Define core entities, relationships, and interactions

## Entity Overview

The EasyKafka consumer library is structured around 7 core entities:

1. **Consumer** - Main orchestrator managing consumption lifecycle
2. **Config** - Configuration data structure with validation
3. **Handler** - User-provided message processing function
4. **ErrorStrategy** - Interface for pluggable failure handling
5. **Message** - Internal representation of Kafka messages
6. **Engine** - Internal consumption loop coordinator
7. **KafkaClient** - Abstraction over confluent-kafka-go

## Entity Definitions

### 1. Consumer (Public API)

**Purpose**: Primary entry point for users, manages consumer lifecycle

**Responsibilities**:
- Accept configuration via functional options
- Validate configuration before connecting to Kafka
- Initialize Kafka client and internal engine
- Provide Start() and Shutdown() methods
- Coordinate graceful shutdown with timeout

**State**:
- Configuration (topic, brokers, consumer group, error strategy)
- Kafka client adapter
- Internal engine reference
- Shutdown signal channel
- Running state flag

**State Transitions**:
```
Created → Started → Running → ShuttingDown → Stopped
                                    ↓
                                 Error
```

**Relationships**:
- Has-one Config (immutable after creation)
- Has-one KafkaClient (lifecycle managed)
- Has-one Engine (lifecycle managed)
- Has-one ErrorStrategy (pluggable)
- Has-one Handler (user-provided)

**Validation Rules**:
- Topic name must be non-empty
- At least one broker address required
- Consumer group ID must be non-empty
- Handler function must be non-nil
- Batch size must be positive if batch mode enabled
- Batch timeout must be positive if batch mode enabled

**Concurrency**:
- Start() can only be called once
- Shutdown() is idempotent (safe to call multiple times)
- Internal state protected by mutex

---

### 2. Config (Public API)

**Purpose**: Immutable configuration container built via functional options

**Fields**:
```go
type Config struct {
    // Required fields
    Topic         string              // Kafka topic to consume
    Brokers       []string            // Kafka broker addresses
    ConsumerGroup string              // Consumer group ID
    Handler       Handler             // Message processing function
    
    // Optional fields with defaults
    ErrorStrategy ErrorStrategy       // Default: ExponentialRetry(3 attempts)
    BatchMode     bool                // Default: false (single-message)
    BatchSize     int                 // Default: 100 (if batch mode)
    BatchTimeout  time.Duration       // Default: 5 seconds (if batch mode)
    PollTimeout   time.Duration       // Default: 100ms
    ShutdownTimeout time.Duration     // Default: 30 seconds
    Logger        zerolog.Logger      // Default: zerolog.Nop()
    
    // Advanced passthrough to confluent-kafka-go
    KafkaConfig   map[string]any      // Default: empty
}
```

**Validation**:
- Perform validation in Option functions, not in Consumer.New()
- Return descriptive errors for invalid configurations
- Fail fast on invalid config before connecting to Kafka

**Defaults Strategy**:
- Use sensible production-ready defaults
- Document all defaults in godoc
- Allow disabling defaults where appropriate (e.g., WithNoRetry())

**Immutability**:
- Config cannot be changed after Consumer creation
- Want different config? Create a new Consumer instance
- Rationale: Thread-safety without locking, clear semantics

---

### 3. Handler (Public API)

**Purpose**: User-provided function that processes message payloads

**Function Signatures**:
```go
// Single-message handler (context always provided)
type Handler func(ctx context.Context, payload []byte) error

// Batch handler (context always provided)
type BatchHandler func(ctx context.Context, payloads [][]byte) error
```

**Contracts**:
- Return `nil` for successful processing → offset committed
- Return `error` for failed processing → error strategy applied
- Must not panic (but library recovers if they do)
- Should respect context cancellation for graceful shutdown
- Should not retain references to `[]byte` slices (may be reused)
- Context is always provided and cancelled during shutdown

**Batch Processing Atomicity**:
- Batches are atomic units: all messages in batch succeed or fail together
- If BatchHandler returns error, error strategy applies to entire batch
- Retry: all messages in batch are re-processed together
- Skip: all messages in batch are skipped together (all offsets committed)
- DLQ: entire batch is written to dead-letter queue as a unit
- No partial batch success/failure tracking in v1 for simplicity

**Processing Guarantees**:
- At-least-once delivery: message may be redelivered on failure
- No message loss: offset only committed after successful processing
- Ordering: messages from same partition processed in order

**Performance Expectations**:
- Should complete quickly (seconds, not minutes)
- Long-running handlers should check context cancellation
- Blocking indefinitely prevents graceful shutdown

**Context Usage**:
- Check `ctx.Done()` for cancellation during long operations
- Use context for timeouts: `ctx, cancel := context.WithTimeout(ctx, 5*time.Second)`
- Access message metadata via `easykafka.MessageFromContext(ctx)`

---

### 4. ErrorStrategy (Public API - Interface)

**Purpose**: Pluggable strategy for handling message processing failures

**Interface**:
```go
type ErrorStrategy interface {
    // HandleError processes a handler failure
    // Returns nil to continue consumption, error to stop consumer
    HandleError(ctx context.Context, msg *Message, handlerErr error) error
    
    // Name returns strategy name for logging/debugging
    Name() string
}
```

**Built-in Implementations**:

#### 4.1 FailFastStrategy
- **Behavior**: Return error immediately, stop consumer
- **Use Case**: Critical processing where errors require manual intervention
- **State**: Stateless
- **Config**: None

#### 4.2 SkipStrategy
- **Behavior**: Log error, commit offset, continue with next message
- **Use Case**: Best-effort processing where message loss is acceptable
- **State**: Stateless
- **Config**: Optional custom logger

#### 4.3 RetryStrategy
- **Behavior**: Retry message processing with configurable backoff, then execute configured action after max attempts
- **Use Case**: Transient failures (network timeouts, temporary service unavailability)
- **State**: Retry attempt tracking per message (via message offset key)
- **Config**:
  - MaxAttempts (default: 3)
  - BackoffType: Fixed, Exponential, or Custom function
  - InitialDelay (default: 1 second)
  - MaxDelay (default: 30 seconds)
  - Multiplier (default: 2.0 for exponential)
  - OnMaxAttemptsExceeded: Action to take after all retries exhausted
    - **FailConsumer**: Stop consumer (default)
    - **SendToDLQ(topic)**: Write message to dead-letter queue and continue consumption

**Retry Attempt Tracking**:
```go
type RetryStrategy struct {
    maxAttempts         int
    backoff             BackoffFunc
    onMaxAttemptsAction MaxAttemptsAction  // New: configurable action
    dlqTopic            string             // New: only used if action is SendToDLQ
    attempts            map[int64]int      // offset -> attempt count
    mu                  sync.RWMutex
}

type MaxAttemptsAction int

const (
    FailConsumer MaxAttemptsAction = iota
    SendToDLQ
)
```

**DLQ Message Format** (when SendToDLQ action is configured):
```json
{
  "originalTopic": "orders",
  "originalPartition": 3,
  "originalOffset": 12345,
  "payload": "<base64 encoded original>",
  "error": "handler error message",
  "timestamp": "2026-02-09T10:30:00Z",
  "attemptCount": 3
}
```

#### 4.4 CircuitBreakerStrategy
- **Behavior**: Pause consumption after consecutive failure threshold
- **Use Case**: Protect downstream services from overload during incidents
- **State**: Failure counter, circuit state (closed/open/half-open)
- **Config**:
  - FailureThreshold (default: 10)
  - CooldownPeriod (default: 60 seconds)
  - HalfOpenMaxAttempts (default: 3)

**Circuit State Machine**:
```
Closed --[failures >= threshold]--> Open --[cooldown expired]--> HalfOpen
   ↑                                                                 |
   |                                                                 |
   +----[successes >= halfOpenAttempts]----------------------------+
   |                                                                 |
   +----[any failure]-----------------------------------------------+
```

**Thread Safety**:
- All strategy implementations must be thread-safe
- Internal state protected by mutex where needed
- Single strategy instance shared across all messages

---

### 5. Message (Internal)

**Purpose**: Internal representation of a Kafka message with metadata

**Structure**:
```go
type Message struct {
    // Core payload
    Value     []byte        // Message body (what handler receives)
    
    // Kafka metadata
    Topic     string        // Source topic
    Partition int32         // Source partition
    Offset    int64         // Message offset (unique within partition)
    Timestamp time.Time     // Message timestamp
    Headers   []Header      // Optional key-value headers
    
    // Internal tracking
    Key       []byte        // Message key (for partitioning)
    
    // Lifecycle
    Committed bool          // Whether offset has been committed
}

type Header struct {
    Key   string
    Value []byte
}
```

**Lifecycle**:
1. Created from confluent-kafka-go message in adapter
2. Passed to engine for dispatch
3. Payload extracted and passed to handler
4. If handler succeeds, marked for commit
5. Offset committed to Kafka
6. Message discarded

**Metadata Access**:
- By default, handler only receives `[]byte` payload
- Advanced users can access metadata via context values:
  ```go
  func handler(ctx context.Context, payload []byte) error {
      msg := easykafka.MessageFromContext(ctx)
      log.Info().
          Str("topic", msg.Topic).
          Int64("offset", msg.Offset).
          Msg("processing message")
      return processPayload(payload)
  }
  ```

**Memory Management**:
- Message payload backed by confluent-kafka-go buffer
- Must not be retained after handler returns (may be reused)
- If handler needs to keep payload, must copy the byte slice

---

### 6. Engine (Internal)

**Purpose**: Core message processing loop orchestrating polling, dispatch, and commit

**Responsibilities**:
- Poll messages from Kafka client
- Accumulate batches (if batch mode)
- Invoke handler with panic recovery
- Apply error strategy on failures
- Commit offsets after successful processing
- Handle graceful shutdown

**Structure**:
```go
type Engine struct {
    client       KafkaClient
    handler      Handler           // Or BatchHandler
    strategy     ErrorStrategy
    config       *Config
    
    // Batch mode state
    batchMode    bool
    accumulator  *BatchAccumulator
    
    // Lifecycle
    ctx          context.Context
    cancel       context.CancelFunc
    shutdownCh   chan struct{}
    doneCh       chan struct{}
    
    // Observability
    logger       zerolog.Logger
}
```

**Main Loop**:
```go
func (e *Engine) Run(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return e.gracefulShutdown()
            
        default:
            msg, err := e.client.Poll(e.config.PollTimeout)
            if err != nil {
                e.handlePollError(err)
                continue
            }
            if msg == nil {
                continue // Timeout, try again
            }
            
            if e.batchMode {
                if e.accumulator.Add(msg) {
                    e.processBatch()
                }
            } else {
                e.processMessage(msg)
            }
        }
    }
}
```

**Shutdown Sequence**:
1. Receive context cancellation signal
2. Stop polling new messages
3. If batch mode: process partial batch
4. Wait for in-flight handlers (with timeout)
5. Commit final offsets
6. Close Kafka client
7. Signal completion via doneCh

---

### 7. KafkaClient (Internal Adapter)

**Purpose**: Abstract confluent-kafka-go for testability

**Interface**:
```go
type KafkaClient interface {
    // Subscribe to topic
    Subscribe(topic string) error
    
    // Poll for next message (non-blocking with timeout)
    Poll(timeout time.Duration) (*Message, error)
    
    // Commit offset for message
    Commit(msg *Message) error
    
    // Close client and cleanup
    Close() error
}
```

**Production Implementation**:
```go
type ConfluentAdapter struct {
    consumer *kafka.Consumer
    logger   zerolog.Logger
}
```

**Mock Implementation** (for unit tests):
```go
type MockClient struct {
    messages chan *Message
    commits  []int64
    closed   bool
}
```

**Adapter Benefits**:
- Unit tests don't need real Kafka or testcontainers
- Can simulate specific error conditions easily
- Future-proof for supporting other Kafka clients

---

## Entity Relationships Diagram

```
┌──────────────────┐
│   User Code      │
└────────┬─────────┘
         │ provides Handler
         ↓
┌──────────────────────────────────────────────────┐
│              Consumer (Public)                   │
│  - Start()                                       │
│  - Shutdown()                                    │
└───┬──────────────────────────┬──────────────────┘
    │ has-one                  │ has-one
    │                          │
    ↓                          ↓
┌──────────────┐        ┌──────────────────┐
│   Config     │        │  ErrorStrategy   │ (interface)
│              │        │  - HandleError() │
└──────────────┘        └──────────────────┘
                               △
                               │ implements
                        ┌──────┴────────────────┬───────────┬─────────────┐
                        │                       │           │             │
                   ┌────┴─────┐          ┌─────┴────┐  ┌───┴────┐   ┌───┴────────┐
                   │FailFast  │          │   Skip   │  │ Retry  │   │    DLQ     │
                   └──────────┘          └──────────┘  └────────┘   └────────────┘

┌──────────────────────────────────────────────────┐
│          Engine (Internal)                       │
│  - Run()                                         │
│  - processMessage()                              │
│  - gracefulShutdown()                            │
└───┬──────────────────────────────────────────────┘
    │ uses
    ↓
┌──────────────────┐         ┌──────────────────┐
│  KafkaClient     │────────→│    Message       │
│  (Adapter)       │ produces│                  │
└──────────────────┘         └──────────────────┘
         △
         │ wraps
         │
┌────────┴─────────────────┐
│  confluent-kafka-go      │
│  (*kafka.Consumer)       │
└──────────────────────────┘
```

---

## Key Interactions & Flows

### Flow 1: Consumer Creation & Configuration

```
User Code
   │
   ├── easykafka.New(options...)
   │        │
   │        ├── Apply each Option function
   │        ├── Validate configuration
   │        ├── Create KafkaClient adapter
   │        ├── Initialize Engine
   │        └── Return Consumer
   │
   └── consumer.Start(ctx)
```

### Flow 2: Message Processing (Single-Message Mode)

```
Engine.Run()
   │
   ├── client.Poll(timeout) ────→ Get message from Kafka
   │        │
   │        └── Message received
   │
   ├── recovery.SafeCall(handler, msg.Value)
   │        │
   │        ├── Handler succeeds (nil)
   │        │     └── client.Commit(msg) ────→ Commit offset
   │        │
   │        └── Handler fails (error)
   │              └── strategy.HandleError(ctx, msg, err)
   │                     │
   │                     ├── Retry: wait & retry handler
   │                     ├── Skip: commit offset anyway
   │                     ├── DLQ: write to DLQ topic, commit
   │                     └── FailFast: return error, stop
   │
   └── Loop continues or exits
```

### Flow 3: Batch Processing Mode

```
Engine.Run()
   │
   ├── client.Poll(timeout)
   │        └── Message received
   │
   ├── accumulator.Add(msg)
   │        │
   │        ├── Batch size reached? ────YES───┐
   │        └── Timeout expired?   ────YES───┤
   │                                          │
   │                                          ↓
   ├── processBatch()
   │        │
   │        ├── handler([][]byte) ──→ Process batch
   │        │
   │        ├── Success: commit all offsets in batch
   │        │
   │        └── Failure: strategy.HandleError()
   │              └── Retry entire batch or skip entire batch
   │
   └── Loop continues
```

### Flow 4: Graceful Shutdown

```
User calls cancel() or sends SIGTERM
   │
   ↓
Context cancelled
   │
   ├── Engine detects ctx.Done()
   │        │
   │        ├── Stop polling new messages
   │        ├── Process partial batch (if any)
   │        ├── Wait for in-flight handlers (with timeout)
   │        ├── Commit final offsets
   │        └── client.Close()
   │
   └── consumer.Shutdown() completes
```

---

## State Management

### Consumer State
- **Immutable**: Configuration after creation
- **Mutable**: Running state, protected by mutex
- **Lifecycle**: Created → Started → Running → Stopped

### Engine State
- **Immutable**: Config, handler, strategy references
- **Mutable**: Batch accumulator buffer
- **Thread Safety**: Single goroutine owns engine loop

### Strategy State (varies by implementation)
- **Stateless**: FailFast, Skip
- **Stateful**: Retry (attempt tracking), CircuitBreaker (failure count)
- **Thread Safety**: Must be thread-safe (protected by mutex)

---

## Performance Considerations

### Memory Allocation
- Minimize allocations in hot path (engine loop)
- Reuse message buffers where possible
- Batch accumulator pre-allocated to batch size

### Goroutine Management
- One main goroutine for engine loop
- Handler execution synchronous (no goroutine per message by default)
- Advanced users can spawn goroutines inside handlers if needed

### Lock Contention
- Consumer state mutex only locked during Start/Shutdown
- Strategy state locks must be brief (avoid in hot path if possible)
- Use atomic operations where mutex is overkill

---

## Extensibility Points

### User-Provided Implementations

1. **Custom ErrorStrategy**:
   ```go
   type MyStrategy struct{}
   
   func (s *MyStrategy) HandleError(ctx context.Context, msg *Message, err error) error {
       // Custom logic
       return nil // Continue or error to stop
   }
   
   func (s *MyStrategy) Name() string {
       return "my-custom-strategy"
   }
   ```

2. **Custom Backoff Function** (for RetryStrategy):
   ```go
   func CustomBackoff(attempt int) time.Duration {
       return time.Duration(fibonacci(attempt)) * time.Second
   }
   
   strategy := easykafka.NewRetryStrategy(
       easykafka.WithCustomBackoff(CustomBackoff),
   )
   ```

3. **Message Metadata Access**:
   ```go
   func handler(ctx context.Context, payload []byte) error {
       msg := easykafka.MessageFromContext(ctx)
       // Access headers, offset, timestamp, etc.
   }
   ```

---

## Validation Rules Summary

| Entity | Validation | Error Message |
|--------|-----------|---------------|
| Config.Topic | Non-empty string | "topic cannot be empty" |
| Config.Brokers | At least one address | "at least one broker required" |
| Config.ConsumerGroup | Non-empty string | "consumer group cannot be empty" |
| Config.Handler | Non-nil function | "handler function required" |
| Config.BatchSize | > 0 if batch mode | "batch size must be positive" |
| Config.BatchTimeout | > 0 if batch mode | "batch timeout must be positive" |
| RetryStrategy.MaxAttempts | > 0 | "max attempts must be positive" |
| DLQStrategy.DLQTopic | Non-empty | "DLQ topic required" |
| CircuitBreaker.Threshold | > 0 | "failure threshold must be positive" |

All validation happens in Option functions, returning errors before any Kafka connection.

---

## Testing Implications

### Unit Test Boundaries
- Consumer: Mock KafkaClient and ErrorStrategy
- Engine: Mock KafkaClient, test with each strategy
- Strategies: Test each in isolation with mock messages

### Integration Test Scope
- End-to-end with real Kafka via testcontainers
- Verify offset commits in actual Kafka
- Test rebalancing by starting/stopping consumers
- Verify DLQ writes to real Kafka topic

### Property-Based Testing
- Invariant: Successful handler always commits offset
- Invariant: Failed handler never commits (unless Skip strategy)
- Invariant: At-least-once delivery (message redelivered on failure)
