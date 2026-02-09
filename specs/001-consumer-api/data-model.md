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
    RetryQueueMode bool               // Default: false (set true for retry queue consumer)
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
- Batches are atomic units for handler execution: BatchHandler processes all messages together
- If BatchHandler returns error, error strategy receives entire batch but behavior depends on strategy:
  - **Skip**: All messages in batch are skipped together (all offsets committed)
  - **FailFast**: Consumer stops immediately (atomic)
  - **Retry**: Each message in batch is retried individually (see RetryStrategy details)
  - **CircuitBreaker**: Treats batch as single failure unit for circuit state
- Handler success: all offsets in batch are committed together
- Handler failure: strategy determines granularity (batch-level or message-level)

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
    // msgs contains 1 message in single-message mode, N messages in batch mode
    // In batch mode, all messages in the slice failed together atomically
    // Returns nil to continue consumption, error to stop consumer
    HandleError(ctx context.Context, msgs []*Message, handlerErr error) error
    
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
- **Batch Handling**: In batch mode, stops consumer if any batch fails (receives all messages from failed batch)

#### 4.2 SkipStrategy
- **Behavior**: Log error, commit offsets, continue processing
- **Use Case**: Best-effort processing where message loss is acceptable
- **State**: Stateless
- **Config**: Optional custom logger
- **Batch Handling**: In batch mode, skips entire batch (commits all offsets in batch)

#### 4.3 RetryStrategy
- **Behavior**: Retry message processing with configurable backoff using a dedicated Kafka retry queue, then execute configured action after max attempts
- **Use Case**: Transient failures (network timeouts, temporary service unavailability)
- **State**: Stateless - retry attempt tracking stored in Kafka message headers
- **Batch Handling**: In batch mode, each message in the failed batch is written to retry queue individually with its own retry metadata
- **Config**:
  - RetryTopic (required) - Dedicated Kafka topic for retry queue
  - MaxAttempts (default: 3)
  - BackoffType: Fixed, Exponential, or Custom function
  - InitialDelay (default: 1 second)
  - MaxDelay (default: 30 seconds)
  - Multiplier (default: 2.0 for exponential)
  - OnMaxAttemptsExceeded: Action to take after all retries exhausted
    - **FailConsumer**: Stop consumer (default)
    - **SendToDLQ(topic)**: Write message to dead-letter queue and continue consumption

**Kafka-Based Retry Queue Architecture**:
```go
type RetryStrategy struct {
    maxAttempts         int
    backoff             BackoffFunc
    retryTopic          string            // Dedicated Kafka retry queue topic
    retryProducer       KafkaProducer     // Producer for writing to retry queue
    onMaxAttemptsAction MaxAttemptsAction // Configurable action after max attempts
    dlqTopic            string            // Only used if action is SendToDLQ
    dlqProducer         KafkaProducer     // Producer for DLQ
    logger              zerolog.Logger
}

type MaxAttemptsAction int

const (
    FailConsumer MaxAttemptsAction = iota
    SendToDLQ
)
```

**Why Kafka-Based Retry Queue?**

Instead of storing retry attempts in memory (which causes memory leaks and state loss on consumer restart), retry metadata is stored in Kafka message headers:

**Benefits**:
1. **Stateless Strategy**: No in-memory maps or state management needed
2. **Survives Restarts**: Retry state persists across consumer restarts
3. **No Memory Leaks**: No unbounded map growth issues
4. **Distributed**: Multiple retry consumers can process retry queue
5. **Visibility**: Retry queue is a real Kafka topic - can be monitored, inspected, replayed
6. **Dedicated Processing**: Separate consumer for retry queue with different configuration

**Architecture Overview**:
```
Main Consumer (Topic: "orders")
        │
        ├─ Message succeeds ──→ Commit offset
        │
        └─ Message fails (handler error)
                 ↓
           RetryStrategy.HandleError()
                 ↓
           Read retry headers (if present)
                 ↓
           Increment RetryAttempt
                 ↓
      ┌──────────┴──────────┐
      │                     │
   < MaxAttempts        >= MaxAttempts
      │                     │
      ↓                     ↓
  Write to         Execute OnMaxAttemptsAction
  Retry Queue       - FailConsumer: stop
  with headers      - SendToDLQ: write to DLQ

Retry Consumer (Topic: "orders.retry")
        │
        ├─ Poll message from retry queue
        ├─ Check RetryTime header
        │     └─ If not elapsed: Skip (re-poll later)
        │     └─ If elapsed: Process message
        ├─ Call original handler
        │
        ├─ Success ──→ Commit offset (message done)
        │
        └─ Failure ──→ Back to RetryStrategy.HandleError()
                       (increments attempt, writes back to retry queue)
```

**Retry Message Headers**:

When a message is written to the retry queue, these headers are added/updated:

```go
type RetryHeaders struct {
    RetryAttempt    int       // Current attempt number (1, 2, 3, ...)
    RetryTime       time.Time // Timestamp when message can be retried
    ErrorCode       string    // Error code or type (e.g., "TIMEOUT", "DB_ERROR")
    ErrorMessage    string    // Full error message for debugging
    OriginalTopic   string    // Source topic where message came from
    OriginalPartition int32   // Source partition
    OriginalOffset    int64   // Source offset
    FailedAt        time.Time // When the failure occurred
}
```

**Header Format in Kafka**:
```
Headers:
  "easykafka.retry.attempt"    = "2"
  "easykafka.retry.time"       = "2026-02-09T10:35:00Z"
  "easykafka.error.code"       = "TIMEOUT"
  "easykafka.error.message"    = "context deadline exceeded"
  "easykafka.original.topic"   = "orders"
  "easykafka.original.partition" = "3"
  "easykafka.original.offset"  = "12345"
  "easykafka.failed.at"        = "2026-02-09T10:30:00Z"
```

**How Retry Tracking Works**:

**Flow Diagram**:
```
Main Consumer: Handler fails → HandleError() called with msgs []*Message
                                         ↓
                   FOR EACH message in msgs (1 in single mode, N in batch mode):
                                         ↓
                           Read retry headers from message
                                         ↓
                   ┌──────────────────────────────────────┐
                   │                                      │
                   ↓                                      ↓
          Headers NOT present                    Headers present
          (first failure)                     (retry attempt)
                   │                                      │
                   └─→ RetryAttempt = 1                   ├─→ RetryAttempt = existing + 1
                                         ↓
                   Check: RetryAttempt < maxAttempts?
                                         ↓
                   ┌─────────────────────┴──────────────────┐
                   │                                        │
                   ↓                                        ↓
                 YES                                       NO
           (retry again)                          (max attempts reached)
                   │                                        │
                   ├─→ Calculate backoff delay              ├─→ Execute onMaxAttemptsAction
                   │   delay = backoff(RetryAttempt)        │     - FailConsumer: log & return error
                   │                                        │     - SendToDLQ: write to DLQ topic
                   ├─→ RetryTime = now + delay              │
                   │                                        └─→ Commit offset (done with message)
                   ├─→ Add/update retry headers
                   │
                   └─→ Write message to retry queue topic
                       (with original payload + retry headers)

                   END FOR EACH
                                         ↓
                           Commit offsets for all processed messages
                           (they're now in retry queue or DLQ)
```

**Pseudocode Implementation**:

```go
func (r *RetryStrategy) HandleError(ctx context.Context, msgs []*Message, handlerErr error) error {
    // In batch mode, msgs contains all messages from failed batch
    // We process each message individually and write to retry queue
    
    for _, msg := range msgs {
        // Step 1: Read retry metadata from message headers (if exists)
        retryAttempt := r.getRetryAttemptFromHeaders(msg)
        retryAttempt++ // Increment for this failure
        
        r.logger.Warn().
            Str("topic", msg.Topic).
            Int32("partition", msg.Partition).
            Int64("offset", msg.Offset).
            Int("attempt", retryAttempt).
            Int("maxAttempts", r.maxAttempts).
            Err(handlerErr).
            Msg("handler failed for message")
        
        // Step 2: Check if max attempts reached for THIS message
        if retryAttempt >= r.maxAttempts {
            // Max attempts exhausted - execute configured action
            r.logger.Error().
                Str("topic", msg.Topic).
                Int64("offset", msg.Offset).
                Int("attempts", retryAttempt).
                Msg("max retry attempts reached")
            
            switch r.onMaxAttemptsAction {
            case FailConsumer:
                r.logger.Error().Msg("max attempts reached, stopping consumer")
                return fmt.Errorf("max retry attempts exceeded: %w", handlerErr)
                
            case SendToDLQ:
                // Send THIS individual message to DLQ
                if err := r.sendMessageToDLQ(ctx, msg, handlerErr, retryAttempt); err != nil {
                    r.logger.Error().Err(err).Msg("failed to send message to DLQ")
                    return fmt.Errorf("DLQ write failed: %w", err)
                }
                r.logger.Info().Msg("message sent to DLQ")
                // Continue to next message (this one is handled)
            }
        } else {
            // Not at max attempts yet - write to retry queue
            if err := r.sendMessageToRetryQueue(ctx, msg, handlerErr, retryAttempt); err != nil {
                r.logger.Error().Err(err).Msg("failed to send message to retry queue")
                return fmt.Errorf("retry queue write failed: %w", err)
            }
            r.logger.Info().
                Int("attempt", retryAttempt).
                Str("retryTopic", r.retryTopic).
                Msg("message sent to retry queue")
        }
    }
    
    // All messages either in retry queue or DLQ - continue consumption
    return nil
}

func (r *RetryStrategy) getRetryAttemptFromHeaders(msg *Message) int {
    // Look for "easykafka.retry.attempt" header
    for _, h := range msg.Headers {
        if h.Key == "easykafka.retry.attempt" {
            attempt, _ := strconv.Atoi(string(h.Value))
            return attempt
        }
    }
    return 0 // First failure - no retry headers yet
}

func (r *RetryStrategy) sendMessageToRetryQueue(ctx context.Context, msg *Message, handlerErr error, attemptCount int) error {
    // Calculate backoff delay based on attempt count
    delay := r.backoff(attemptCount)
    retryTime := time.Now().Add(delay)
    
    // Extract error code (simplified - could be more sophisticated)
    errorCode := "HANDLER_ERROR"
    if errors.Is(handlerErr, context.DeadlineExceeded) {
        errorCode = "TIMEOUT"
    }
    
    // Create/update retry headers
    retryHeaders := []Header{
        {Key: "easykafka.retry.attempt", Value: []byte(strconv.Itoa(attemptCount))},
        {Key: "easykafka.retry.time", Value: []byte(retryTime.Format(time.RFC3339))},
        {Key: "easykafka.error.code", Value: []byte(errorCode)},
        {Key: "easykafka.error.message", Value: []byte(handlerErr.Error())},
        {Key: "easykafka.original.topic", Value: []byte(msg.Topic)},
        {Key: "easykafka.original.partition", Value: []byte(strconv.Itoa(int(msg.Partition)))},
        {Key: "easykafka.original.offset", Value: []byte(strconv.FormatInt(msg.Offset, 10))},
        {Key: "easykafka.failed.at", Value: []byte(time.Now().Format(time.RFC3339))},
    }
    
    // Preserve original message headers (application-level headers)
    retryHeaders = append(retryHeaders, msg.Headers...)
    
    // Write to retry queue with updated headers
    retryMessage := &Message{
        Topic:   r.retryTopic,
        Key:     msg.Key,
        Value:   msg.Value, // Original payload unchanged
        Headers: retryHeaders,
    }
    
    return r.retryProducer.Produce(ctx, retryMessage)
}

func (r *RetryStrategy) sendMessageToDLQ(ctx context.Context, msg *Message, handlerErr error, attemptCount int) error {
    // Send individual message to DLQ (similar format as before)
    dlqMessage := map[string]interface{}{
        "originalTopic":     msg.Topic,
        "originalPartition": msg.Partition,
        "originalOffset":    msg.Offset,
        "payload":           base64.StdEncoding.EncodeToString(msg.Value),
        "error":             handlerErr.Error(),
        "timestamp":         time.Now().Format(time.RFC3339),
        "attemptCount":      attemptCount,
    }
    
    dlqPayload, _ := json.Marshal(dlqMessage)
    dlqMsg := &Message{
        Topic: r.dlqTopic,
        Value: dlqPayload,
    }
    return r.dlqProducer.Produce(ctx, dlqMsg)
}
```

**Retry Consumer Implementation**:

A **separate consumer** is needed to process the retry queue:

```go
// User creates TWO consumers:

// 1. Main consumer for original topic
mainConsumer := easykafka.New(
    easykafka.WithTopic("orders"),
    easykafka.WithHandler(processOrder),
    easykafka.WithRetryStrategy(
        easykafka.NewRetryStrategy(
            easykafka.WithRetryTopic("orders.retry"),
            easykafka.WithMaxRetries(3),
        ),
    ),
)

// 2. Retry consumer for retry topic
retryConsumer := easykafka.New(
    easykafka.WithTopic("orders.retry"),
    easykafka.WithHandler(processOrder), // SAME handler
    easykafka.WithRetryStrategy(
        easykafka.NewRetryStrategy(
            easykafka.WithRetryTopic("orders.retry"), // Same retry queue
            easykafka.WithMaxRetries(3),
        ),
    ),
    easykafka.WithRetryQueueMode(true), // Special mode for retry processing
)
```

**Retry Queue Processing Logic**:

When `WithRetryQueueMode(true)` is set, the engine adds a pre-processing step:

```go
func (e *Engine) processMessage(msg *Message) {
    if e.config.RetryQueueMode {
        // Check if retry time has elapsed
        retryTime := e.getRetryTimeFromHeaders(msg)
        if retryTime.After(time.Now()) {
            // Too early to retry - skip this message (don't commit offset)
            e.logger.Debug().
                Time("retryTime", retryTime).
                Msg("retry time not elapsed, skipping message")
            return // Message will be re-polled later
        }
    }
    
    // Normal processing continues...
    e.callHandler(msg)
}
```

**Example Trace (Single-Message Mode)**:

```
Time | Event                                      | Retry Queue State       | Action
-----|--------------------------------------------|-----------------------|---------------------------
0s   | Main: Message offset 100 fails            | -                     | Write to retry queue (attempt=1, retryTime=0s+1s)
0s   | Main: Commit offset 100                   | 1 message in queue    | Done with main topic
1s   | Retry: Poll message (retryTime=1s)        | Processing            | Time elapsed, call handler
1s   | Retry: Handler fails again                | -                     | Write back to retry queue (attempt=2, retryTime=1s+2s)
1s   | Retry: Commit offset                      | 1 message in queue    | Done with this iteration
3s   | Retry: Poll message (retryTime=3s)        | Processing            | Time elapsed, call handler
3s   | Retry: Handler fails again                | -                     | Write back to retry queue (attempt=3, retryTime=3s+4s)
7s   | Retry: Poll message (retryTime=7s)        | Processing            | Time elapsed, call handler
7s   | Retry: Handler fails (attempt=3)          | -                     | Max attempts! Send to DLQ
7s   | Retry: Commit offset                      | Empty                 | Message in DLQ
```

**Example Trace (Batch Mode)**:

```
Time | Event                                  | Retry Queue State             | Action
-----|----------------------------------------|-------------------------------|---------------------------
0s   | Main: Batch [100-109] fails (10 msgs) | -                             | Write 10 messages to retry queue
0s   | Main: Commit offsets 100-109           | 10 messages (attempt=1)       | Done with main topic
1s   | Retry: Poll 10 messages                | Processing all                | All retry times elapsed
1s   | Retry: Batch fails again               | -                             | Write 10 back (attempt=2)
3s   | Retry: Poll 10 messages                | Processing all                | All retry times elapsed
3s   | Retry: Batch fails again               | -                             | Write 10 back (attempt=3)
7s   | Retry: Poll 10 messages                | Processing all                | All retry times elapsed
7s   | Retry: Batch fails (attempt=3)         | -                             | Max attempts! Send 10 to DLQ
```

**Key Architecture Points**:

1. **No In-Memory State**: RetryStrategy is completely stateless - all state in Kafka headers
2. **Two Consumers Required**: One for main topic, one for retry topic (can use same handler function)
3. **Graceful Backoff**: RetryTime header prevents premature retries without blocking
4. **Survives Restarts**: Consumer can restart anytime - retry state persists in Kafka
5. **Single Topic per Consumer**: Each consumer handles one topic (main OR retry), not both
6. **Independent Scaling**: Main and retry consumers can scale independently

**DLQ Message Format** (when SendToDLQ action is configured):

Same format as before - individual messages:
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
- **Behavior**: Temporarily pause message processing after consecutive failures to protect downstream services
- **Use Case**: Protect downstream services from overload during incidents (database down, API unavailable, etc.)
- **State**: Failure counter, success counter, circuit state (closed/open/half-open), last state change timestamp
- **Batch Handling**: In batch mode, treats each batch failure as a single circuit breaker event (one batch failure = one failure count increment), but does NOT retry messages individually like RetryStrategy
- **Config**:
  - FailureThreshold (default: 10) - consecutive failures before opening circuit
  - CooldownPeriod (default: 60 seconds) - how long to wait in Open state before trying again
  - HalfOpenMaxAttempts (default: 3) - successful messages needed in HalfOpen to close circuit

**Circuit States Explained**:

1. **Closed (Normal Operation)**:
   - Messages are processed normally
   - Handler called for every message
   - On success: Reset failure counter to 0
   - On failure: Increment failure counter
   - Transition: When `failureCounter >= FailureThreshold` → Open

2. **Open (Circuit Tripped)**:
   - **Messages are NOT processed** - handler is NOT called
   - All messages immediately FAIL (HandleError returns error to stop consumer)
   - This gives downstream services time to recover
   - Timestamp recorded when entering Open state
   - Transition: After `CooldownPeriod` elapses → HalfOpen

3. **HalfOpen (Testing Recovery)**:
   - Allow LIMITED number of messages through to test if downstream recovered
   - Process messages one at a time (no parallel processing)
   - Track success counter (starts at 0)
   - On success: Increment success counter
     - If `successCounter >= HalfOpenMaxAttempts` → Closed (recovery confirmed)
   - On failure: Immediately → Open (downstream still broken, restart cooldown)

**Circuit State Machine**:
```
     ┌─────────────────────────────────────────────────────────┐
     │                                                           │
     │                CLOSED (Normal)                           │
     │  - Process all messages                                  │
     │  - Track consecutive failures                            │
     │                                                           │
     └───────────────────┬───────────────────────────────────────┘
                         │
                         │ 10 consecutive failures
                         │ (FailureThreshold reached)
                         ↓
     ┌─────────────────────────────────────────────────────────┐
     │                                                           │
     │                 OPEN (Circuit Tripped)                   │
     │  - Reject all messages (handler NOT called)              │
     │  - Wait for CooldownPeriod (60s)                         │
     │  - No processing = downstream can recover                │
     │                                                           │
     └───────────────────┬───────────────────────────────────────┘
                         │
                         │ 60 seconds elapsed
                         │ (CooldownPeriod expired)
                         ↓
     ┌─────────────────────────────────────────────────────────┐
     │                                                           │
     │              HALF-OPEN (Testing)                         │
     │  - Try processing messages cautiously                    │
     │  - Track successes (need 3 to close)                     │
     │                                                           │
     └─────┬──────────────────────────────────────────┬─────────┘
           │                                          │
           │ 3 consecutive successes                  │ ANY failure
           │ (recovery confirmed)                     │ (still broken)
           │                                          │
           ↓                                          ↓
       Back to CLOSED                            Back to OPEN
   (reset all counters)                    (restart cooldown)
```

**Detailed State Transition Logic**:

```go
type CircuitBreakerStrategy struct {
    state            CircuitState  // Closed, Open, HalfOpen
    failureCount     int           // Consecutive failures in Closed state
    successCount     int           // Consecutive successes in HalfOpen state
    lastStateChange  time.Time     // When did we last change state
    failureThreshold int           // Config: failures to trip circuit
    cooldownPeriod   time.Duration // Config: how long to wait in Open
    halfOpenAttempts int           // Config: successes needed to close
    mu               sync.RWMutex
}

func (cb *CircuitBreakerStrategy) HandleError(ctx context.Context, msgs []*Message, handlerErr error) error {
    cb.mu.Lock()
    defer cb.mu.Unlock()
    
    // Note: msgs contains 1 message in single mode, N messages in batch mode
    // Circuit breaker treats each call as one failure/success regardless of batch size
    
    switch cb.state {
    case Closed:
        // Normal operation - handler was called and failed
        cb.failureCount++
        log.Warn().Int("failures", cb.failureCount).Msg("handler failed")
        
        if cb.failureCount >= cb.failureThreshold {
            // Too many failures - trip the circuit
            cb.state = Open
            cb.lastStateChange = time.Now()
            log.Error().Msg("Circuit breaker OPENED - pausing consumption")
            return fmt.Errorf("circuit breaker opened after %d failures", cb.failureCount)
        }
        
        // Not at threshold yet - retry this message (or use retry strategy)
        return nil // Continue processing
        
    case Open:
        // Circuit is open - check if cooldown period has elapsed
        if time.Since(cb.lastStateChange) >= cb.cooldownPeriod {
            // Cooldown completed - try testing recovery
            cb.state = HalfOpen
            cb.successCount = 0
            cb.lastStateChange = time.Now()
            log.Info().Msg("Circuit breaker entering HALF-OPEN - testing recovery")
            // Fall through to HalfOpen handling
        } else {
            // Still cooling down - reject this message
            log.Debug().Msg("Circuit breaker OPEN - rejecting message")
            return fmt.Errorf("circuit breaker open, waiting for cooldown")
        }
        
    case HalfOpen:
        // We're testing if downstream recovered - handler was called
        if handlerErr == nil {
            // Success! Increment success counter
            cb.successCount++
            log.Info().Int("successes", cb.successCount).Msg("half-open success")
            
            if cb.successCount >= cb.halfOpenAttempts {
                // Enough successes - downstream is healthy again
                cb.state = Closed
                cb.failureCount = 0
                cb.lastStateChange = time.Now()
                log.Info().Msg("Circuit breaker CLOSED - normal operation resumed")
            }
            return nil // Continue processing
            
        } else {
            // Failure in half-open - downstream still broken
            cb.state = Open
            cb.failureCount = 0
            cb.lastStateChange = time.Now()
            log.Error().Msg("Circuit breaker reopened - downstream still failing")
            return fmt.Errorf("circuit breaker reopened")
        }
    }
    
    return nil
}
```

**Key Differences from Retry Strategy**:
- **Retry**: Retries the SAME message multiple times (transient failures)
- **Circuit Breaker**: Stops processing ALL messages temporarily (systemic failures)
- **Retry**: Helps with occasional glitches
- **Circuit Breaker**: Protects during outages/incidents

**Example Scenario**:
```
Time | Event                           | State      | Action
-----|----------------------------------|------------|---------------------------
0s   | Processing normally             | Closed     | Process all messages
10s  | Database connection fails       | Closed     | failureCount = 1
11s  | Still failing                   | Closed     | failureCount = 2
...  | ...                             | Closed     | ...
20s  | 10th consecutive failure        | Closed→Open| Stop processing, start cooldown
21s  | New message arrives             | Open       | Reject (don't call handler)
...  | (60 seconds pass)               | Open       | ...
80s  | Cooldown expired                | Open→Half  | Try next message
81s  | Message succeeds!               | HalfOpen   | successCount = 1
82s  | Another success                 | HalfOpen   | successCount = 2
83s  | Third success                   | Half→Close | Resume normal operation
84s  | Back to normal                  | Closed     | Process all messages
```

**Consumer Behavior in Each State**:
- **Closed**: Consumer runs normally, offset commits after successful processing
- **Open**: Consumer STOPS with error (forces manual intervention or restart)
- **HalfOpen**: Consumer cautiously processes messages, commits on success

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
                        ┌──────┴────────────────┬───────────┬──────────────┐
                        │                       │           │              │
                   ┌────┴─────┐          ┌─────┴────┐  ┌───┴────┐   ┌────┴────────┐
                   │FailFast  │          │   Skip   │  │ Retry  │   │CircuitBreaker│
                   └──────────┘          └──────────┘  └────┬───┘   └─────────────┘
                                                             │
                                                             │ has-one
                                                             ↓
                                                      ┌──────────────┐
                                                      │KafkaProducer │
                                                      │ (Retry Queue)│
                                                      └──────┬───────┘
                                                             │ writes to
                                                             ↓
                                                      ┌──────────────┐
                                                      │ Retry Queue  │
                                                      │ (Kafka Topic)│
                                                      └──────┬───────┘
                                                             │ consumed by
                                                             ↓
                                                      ┌──────────────┐
                                                      │Retry Consumer│
                                                      │(Separate)    │
                                                      └──────────────┘

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
   ├── If RetryQueueMode: Check retry headers
   │        │
   │        ├── RetryTime not elapsed? ────→ Skip message (re-poll later)
   │        └── RetryTime elapsed ────→ Continue processing
   │
   ├── recovery.SafeCall(handler, msg.Value)
   │        │
   │        ├── Handler succeeds (nil)
   │        │     └── client.Commit(msg) ────→ Commit offset
   │        │
   │        └── Handler fails (error)
   │              └── strategy.HandleError(ctx, []*Message{msg}, err)
   │                     │                      └─ Single message wrapped in slice
   │                     ├── Retry: write to retry queue with headers, commit offset
   │                     ├── Skip: commit offset anyway
   │                     ├── DLQ: write to DLQ topic, commit offset
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
   │        ├── handler([][]byte) ──→ Process batch atomically
   │        │
   │        ├── Success: commit all offsets in batch
   │        │
   │        └── Failure: strategy.HandleError(ctx, batchMsgs, err)
   │              │                         └─ All messages from batch as slice
   │              │
   │              ├── Skip: commit all offsets (batch skipped)
   │              ├── FailFast: stop consumer (batch atomic)
   │              ├── CircuitBreaker: increment failure count (batch atomic)
   │              └── Retry: write each message to retry queue individually
   │                    ├── Each message gets retry headers (attempt, time, error)
   │                    ├── Commit all offsets (messages now in retry queue)
   │                    └── Retry consumer processes them later
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
- **Stateless**: FailFast, Skip, Retry (with Kafka queue)
- **Stateful**: CircuitBreaker (failure count, circuit state)
- **Thread Safety**: Must be thread-safe (protected by mutex where needed)

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
- RetryStrategy is stateless (no locks needed - uses Kafka for state)
- CircuitBreaker state locks must be brief (avoid in hot path if possible)
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
| RetryStrategy.RetryTopic | Non-empty string | "retry topic required for retry strategy" |
| RetryStrategy.DLQTopic | Non-empty if SendToDLQ action | "DLQ topic required when using SendToDLQ" |
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
