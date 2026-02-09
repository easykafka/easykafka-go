# EasyKafka Go - Quick Start Guide

**Feature**: Easy Kafka Consumer Library  
**Date**: 2026-02-09  
**For**: Developers getting started with the library

## Prerequisites

- Go 1.19 or later
- Running Kafka cluster (for testing, see [Local Development](#local-development))
- Basic understanding of Go functions and error handling

**NO Kafka knowledge required!** This library hides all Kafka complexity.

## Installation

```bash
go get github.com/yourusername/easykafka-go
```

## 5-Minute Quick Start

### 1. Simple Message Consumer (Bare Minimum)

```go
package main

import (
    "context"
    "fmt"
    "log"
    
    "github.com/yourusername/easykafka-go"
)

func main() {
    // Create consumer with minimal configuration
    consumer, err := easykafka.New(
        easykafka.WithTopic("orders"),
        easykafka.WithBrokers("localhost:9092"),
        easykafka.WithConsumerGroup("order-processors"),
        easykafka.WithHandler(processOrder),
    )
    if err != nil {
        log.Fatal(err)
    }
    
    // Start consuming (blocks until context cancelled)
    ctx := context.Background()
    if err := consumer.Start(ctx); err != nil {
        log.Fatal(err)
    }
}

// Handler: just a function that receives bytes and returns an error
func processOrder(orderData []byte) error {
    fmt.Printf("Processing order: %s\n", string(orderData))
    // Your business logic here
    return nil
}
```

**That's it!** The library handles:
- ✅ Connecting to Kafka
- ✅ Joining consumer group
- ✅ Polling for messages
- ✅ Committing offsets
- ✅ Rebalancing partitions
- ✅ Error recovery

---

## Common Patterns

### 2. With Graceful Shutdown

```go
func main() {
    consumer, err := easykafka.New(
        easykafka.WithTopic("orders"),
        easykafka.WithBrokers("localhost:9092"),
        easykafka.WithConsumerGroup("order-processors"),
        easykafka.WithHandler(processOrder),
    )
    if err != nil {
        log.Fatal(err)
    }
    
    // Create cancellable context
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    
    // Handle SIGINT/SIGTERM for graceful shutdown
    go func() {
        sigCh := make(chan os.Signal, 1)
        signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
        <-sigCh
        
        log.Println("Shutting down gracefully...")
        cancel() // Triggers graceful shutdown
    }()
    
    // Start consuming
    if err := consumer.Start(ctx); err != nil {
        log.Fatal(err)
    }
}
```

---

### 3. With Context-Aware Handler

```go
func main() {
    consumer, err := easykafka.New(
        easykafka.WithTopic("orders"),
        easykafka.WithBrokers("localhost:9092"),
        easykafka.WithConsumerGroup("order-processors"),
        easykafka.WithHandlerContext(processOrderWithContext),
    )
    if err != nil {
        log.Fatal(err)
    }
    
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    
    if err := consumer.Start(ctx); err != nil {
        log.Fatal(err)
    }
}

// Handler can check context cancellation for long operations
func processOrderWithContext(ctx context.Context, orderData []byte) error {
    // Access message metadata
    msg := easykafka.MessageFromContext(ctx)
    fmt.Printf("Processing offset %d from partition %d\n", msg.Offset, msg.Partition)
    
    // Your business logic
    return processWithTimeout(ctx, orderData)
}
```

---

### 4. With Retry Strategy

```go
func main() {
    consumer, err := easykafka.New(
        easykafka.WithTopic("orders"),
        easykafka.WithBrokers("localhost:9092"),
        easykafka.WithConsumerGroup("order-processors"),
        easykafka.WithHandler(processOrder),
        
        // Retry failed messages, then stop consumer
        easykafka.WithErrorStrategy(
            easykafka.Retry(
                easykafka.WithMaxAttempts(5),
                easykafka.WithInitialDelay(1 * time.Second),
                easykafka.WithMaxDelay(30 * time.Second),
                easykafka.WithOnMaxAttemptsExceeded(easykafka.FailConsumer),
            ),
        ),
    )
    if err != nil {
        log.Fatal(err)
    }
    
    // ... rest of the code
}
```

**Effect**: If `processOrder` returns an error, the library will:
1. Wait 1 second, retry
2. Wait 2 seconds, retry
3. Wait 4 seconds, retry
4. Wait 8 seconds, retry
5. Wait 16 seconds, retry (capped at 30s)
6. After 5 attempts, consumer stops

---

### 5. With Retry + Dead-Letter Queue

```go
func main() {
    consumer, err := easykafka.New(
        easykafka.WithTopic("orders"),
        easykafka.WithBrokers("localhost:9092"),
        easykafka.WithConsumerGroup("order-processors"),
        easykafka.WithHandler(processOrder),
        
        // Retry failed messages, then send to DLQ and continue
        easykafka.WithErrorStrategy(
            easykafka.Retry(
                easykafka.WithMaxAttempts(3),
                easykafka.WithInitialDelay(1 * time.Second),
                easykafka.WithOnMaxAttemptsExceeded(easykafka.SendToDLQ("orders-dlq")),
            ),
        ),
    )
    if err != nil {
        log.Fatal(err)
    }
    
    // ... rest of the code
}
```

**Effect**: If `processOrder` fails 3 times:
- Message is written to `orders-dlq` topic with error metadata
- Original message offset is committed
- Processing continues with next message

**DLQ Message Format**:
```json
{
  "originalTopic": "orders",
  "originalPartition": 3,
  "originalOffset": 12345,
  "payload": "<base64 encoded>",
  "error": "database connection failed",
  "timestamp": "2026-02-09T10:30:00Z",
  "attemptCount": 3
}
```

---

### 6. Batch Processing for High Throughput

```go
func main() {
    consumer, err := easykafka.New(
        easykafka.WithTopic("events"),
        easykafka.WithBrokers("localhost:9092"),
        easykafka.WithConsumerGroup("event-processors"),
        
        // Batch handler processes multiple messages together
        easykafka.WithBatchHandler(processBatch),
        easykafka.WithBatchSize(100),           // Up to 100 messages per batch
        easykafka.WithBatchTimeout(5 * time.Second), // Or every 5 seconds
    )
    if err != nil {
        log.Fatal(err)
    }
    
    ctx := context.Background()
    if err := consumer.Start(ctx); err != nil {
        log.Fatal(err)
    }
}

// Batch handler receives multiple messages at once
func processBatch(events [][]byte) error {
    fmt.Printf("Processing batch of %d events\n", len(events))
    
    // Process all events together (e.g., bulk insert to database)
    return bulkInsert(events)
}
```

**Performance**: Batch processing achieves ~3x higher throughput by reducing offset commit overhead.

**Trigger Conditions**: Batch is processed when:
- Batch size reached (100 messages), OR
- Timeout expired (5 seconds since first message in batch)

---

### 7. Skip Strategy (Best-Effort Processing)

```go
func main() {
    consumer, err := easykafka.New(
        easykafka.WithTopic("analytics"),
        easykafka.WithBrokers("localhost:9092"),
        easykafka.WithConsumerGroup("analytics-processors"),
        easykafka.WithHandler(processEvent),
        
        // Skip failed messages, continue processing
        easykafka.WithErrorStrategy(easykafka.Skip()),
    )
    if err != nil {
        log.Fatal(err)
    }
    
    // ... rest of the code
}
```

**Effect**: If `processEvent` returns an error:
- Error is logged
- Offset is committed anyway (message skipped)
- Processing continues with next message

**Use Case**: Analytics ingestion where some data loss is acceptable.

---

### 8. Fail-Fast Strategy (Critical Processing)

```go
func main() {
    consumer, err := easykafka.New(
        easykafka.WithTopic("payments"),
        easykafka.WithBrokers("localhost:9092"),
        easykafka.WithConsumerGroup("payment-processors"),
        easykafka.WithHandler(processPayment),
        
        // Stop immediately on any error
        easykafka.WithErrorStrategy(easykafka.FailFast()),
    )
    if err != nil {
        log.Fatal(err)
    }
    
    // ... rest of the code
}
```

**Effect**: If `processPayment` returns an error:
- Consumer stops immediately
- Error is returned from `Start()`
- Requires manual intervention to fix and restart

**Use Case**: Financial transactions where errors need immediate attention.

---

## Error Handling Guide

### Handler Error Behavior

```go
func processOrder(orderData []byte) error {
    // Parse order
    order, err := parseOrder(orderData)
    if err != nil {
        // Return error → error strategy is applied
        return fmt.Errorf("invalid order format: %w", err)
    }
    
    // Save to database
    if err := saveOrder(order); err != nil {
        // Return error → error strategy is applied
        return fmt.Errorf("database error: %w", err)
    }
    
    // Success → offset is committed, next message consumed
    return nil
}
```

### Choosing an Error Strategy

| Strategy | When Handler Fails | Use Case |
|----------|-------------------|----------|
| **Retry + FailConsumer** (default) | Retry with backoff, then stop | Transient failures where eventual stop is acceptable |
| **Retry + SendToDLQ** | Retry, then send to DLQ and continue | Production systems needing error investigation without stopping |
| **Skip** | Log and continue | Best-effort analytics, non-critical data |
| **FailFast** | Stop immediately | Critical processing (payments, orders) |
| **CircuitBreaker** | Pause consumption | Protect downstream during incidents |

**Default**: Retry with 3 attempts, exponential backoff, then stop consumer.

---

## Configuration Reference

### Required Options

```go
easykafka.WithTopic("my-topic")           // Kafka topic to consume
easykafka.WithBrokers("localhost:9092")   // Kafka broker addresses
easykafka.WithConsumerGroup("my-group")   // Consumer group ID
easykafka.WithHandler(myHandler)          // Message handler function
```

### Optional Options

```go
// Error handling
easykafka.WithErrorStrategy(strategy)

// Batch processing
easykafka.WithBatchSize(100)
easykafka.WithBatchTimeout(5 * time.Second)

// Performance tuning
easykafka.WithPollTimeout(100 * time.Millisecond)
easykafka.WithShutdownTimeout(30 * time.Second)

// Observability
easykafka.WithLogger(logger)

// Advanced Kafka configuration
easykafka.WithKafkaConfig(map[string]any{
    "session.timeout.ms": 6000,
    "auto.offset.reset": "earliest",
})
```

---

## Local Development

### Start Kafka with Docker

```bash
# docker-compose.yml
version: '3'
services:
  zookeeper:
    image: confluentinc/cp-zookeeper:latest
    environment:
      ZOOKEEPER_CLIENT_PORT: 2181
  
  kafka:
    image: confluentinc/cp-kafka:latest
    depends_on:
      - zookeeper
    ports:
      - "9092:9092"
    environment:
      KAFKA_ZOOKEEPER_CONNECT: zookeeper:2181
      KAFKA_ADVERTISED_LISTENERS: PLAINTEXT://localhost:9092
      KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR: 1
```

```bash
docker-compose up -d
```

### Create Test Topic

```bash
docker exec -it <kafka-container-id> kafka-topics \
  --create \
  --topic orders \
  --bootstrap-server localhost:9092 \
  --partitions 3 \
  --replication-factor 1
```

### Produce Test Messages

```bash
docker exec -it <kafka-container-id> kafka-console-producer \
  --topic orders \
  --bootstrap-server localhost:9092

# Type messages (one per line):
{"orderId": "123", "amount": 99.99}
{"orderId": "124", "amount": 149.99}
```

---

## Testing Your Handler

### Unit Test (Mock Message)

```go
func TestProcessOrder(t *testing.T) {
    // Test with sample message
    orderJSON := []byte(`{"orderId": "123", "amount": 99.99}`)
    
    err := processOrder(orderJSON)
    
    assert.NoError(t, err)
    // ... verify side effects
}
```

### Integration Test (Real Kafka)

```go
import (
    "github.com/testcontainers/testcontainers-go"
    "github.com/testcontainers/testcontainers-go/modules/kafka"
)

func TestConsumerIntegration(t *testing.T) {
    ctx := context.Background()
    
    // Start Kafka container
    kafkaContainer, err := kafka.RunContainer(ctx)
    require.NoError(t, err)
    defer kafkaContainer.Terminate(ctx)
    
    brokers, err := kafkaContainer.Brokers(ctx)
    require.NoError(t, err)
    
    // Create consumer
    received := make(chan string, 1)
    consumer, err := easykafka.New(
        easykafka.WithTopic("test-topic"),
        easykafka.WithBrokers(brokers...),
        easykafka.WithConsumerGroup("test-group"),
        easykafka.WithHandler(func(msg []byte) error {
            received <- string(msg)
            return nil
        }),
    )
    require.NoError(t, err)
    
    // Produce test message
    produceMessage(t, brokers, "test-topic", "hello world")
    
    // Start consumer
    go consumer.Start(ctx)
    
    // Verify message received
    select {
    case msg := <-received:
        assert.Equal(t, "hello world", msg)
    case <-time.After(10 * time.Second):
        t.Fatal("timeout waiting for message")
    }
}
```

---

## Troubleshooting

### Consumer Not Receiving Messages

**Symptom**: Consumer starts but no messages are processed.

**Checklist**:
1. ✅ Kafka cluster is running and accessible
2. ✅ Topic exists (check with `kafka-topics --list`)
3. ✅ Messages exist in topic (check with `kafka-console-consumer`)
4. ✅ Consumer group ID is unique (different groups don't interfere)
5. ✅ Broker address is correct (localhost:9092, not 127.0.0.1:9092 if advertised differently)

**Debug**:
```go
// Enable debug logging
logger := zerolog.New(os.Stdout).Level(zerolog.DebugLevel)
easykafka.WithLogger(logger)
```

---

### Consumer Stops on Errors

**Symptom**: Consumer processes a few messages then stops.

**Cause**: Handler is returning errors and using FailFast or Retry strategy (default).

**Solutions**:
1. Fix handler to return `nil` on success
2. Use Skip strategy for non-critical processing
3. Use DeadLetter strategy to capture failures and continue

---

### Slow Consumption

**Symptom**: Consumer can't keep up with message production rate.

**Solutions**:
1. **Use batch processing**: 3x throughput improvement
   ```go
   easykafka.WithBatchHandler(processBatch)
   easykafka.WithBatchSize(100)
   ```

2. **Scale horizontally**: Add more consumer instances with same group ID
   ```go
   // Start multiple processes with same consumer group
   easykafka.WithConsumerGroup("processors") // Same group ID
   ```

3. **Optimize handler**: Profile and optimize your handler code

4. **Increase partitions**: More partitions = more parallelism
   ```bash
   kafka-topics --alter --topic orders --partitions 10
   ```

---

### Offset Reset Issues

**Symptom**: Consumer reads old messages or skips messages on restart.

**Cause**: Offset commit behavior.

**Control via Kafka config**:
```go
easykafka.WithKafkaConfig(map[string]any{
    // Start from earliest available message if no committed offset
    "auto.offset.reset": "earliest",
    
    // Or start from latest (skip old messages)
    "auto.offset.reset": "latest",
})
```

---

## Next Steps

- **Production Checklist**: [See deployment guide](deployment.md)
- **Performance Tuning**: [See performance guide](performance.md)
- **API Reference**: [See full API docs](https://pkg.go.dev/...)
- **Examples**: [See examples directory](../examples/)

---

## Getting Help

- **Issues**: [GitHub Issues](https://github.com/yourusername/easykafka-go/issues)
- **Discussions**: [GitHub Discussions](https://github.com/yourusername/easykafka-go/discussions)
- **Contributing**: [Contributing Guide](../CONTRIBUTING.md)

---

## License

MIT License - see [LICENSE](../LICENSE) file for details.
