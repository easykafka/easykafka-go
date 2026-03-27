# EasyKafka Go - Quick Start Guide

**Feature**: Easy Kafka Consumer Library  
**Date**: 2026-02-17  
**For**: Developers getting started with the library

## Prerequisites

- Go 1.19 or later
- Running Kafka cluster

## Installation

```bash
go get github.com/yourusername/easykafka-go
```

## 5-Minute Quick Start

### 1. Simple Message Consumer

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/yourusername/easykafka-go"
)

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

	ctx := context.Background()
	if err := consumer.Start(ctx); err != nil {
		log.Fatal(err)
	}
}

func processOrder(ctx context.Context, payload []byte) error {
	fmt.Printf("processing order: %s\n", string(payload))
	return nil
}
```

### 2. Graceful Shutdown

```go
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/yourusername/easykafka-go"
)

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		cancel()
	}()

	if err := consumer.Start(ctx); err != nil {
		log.Fatal(err)
	}
}

func processOrder(ctx context.Context, payload []byte) error {
	return nil
}
```

### 3. Retry + DLQ

```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/yourusername/easykafka-go"
)

func main() {
	consumer, err := easykafka.New(
		easykafka.WithTopic("orders"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("order-processors"),
		easykafka.WithHandler(processOrder),
		easykafka.WithErrorStrategy(
			easykafka.Retry(
				easykafka.WithRetryTopic("orders.retry"),
				easykafka.WithDLQTopic("orders.dlq"),
				easykafka.WithMaxAttempts(3),
				easykafka.WithInitialDelay(1*time.Second),
				easykafka.WithMaxDelay(30*time.Second),
				easykafka.WithFailedMessagePayloadEncoding(easykafka.PayloadEncodingJSON),
			),
		),
	)
	if err != nil {
		log.Fatal(err)
	}

	if err := consumer.Start(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func processOrder(ctx context.Context, payload []byte) error {
	return nil
}
```

**Behavior**:
- Failures go to `orders.retry` with retry headers and attempt metadata.
- An internal retry consumer reprocesses messages after backoff.
- After max attempts, messages are written to `orders.dlq` and consumption continues.
```

**Payload Encoding Options**:
```go
// JSON encoding (default, human-readable)
easykafka.WithFailedMessagePayloadEncoding(easykafka.PayloadEncodingJSON)

// Base64 encoding (binary-safe for binary payloads)
easykafka.WithFailedMessagePayloadEncoding(easykafka.PayloadEncodingBase64)
```

---

### 5. Batch Processing for High Throughput

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

// Batch handler receives context and multiple messages at once
func processBatch(ctx context.Context, events [][]byte) error {
    fmt.Printf("Processing batch of %d events\n", len(events))
    
    // Process all events together (e.g., bulk insert to database)
    return bulkInsert(ctx, events)
}
```

**Performance**: Batch processing achieves ~3x higher throughput by reducing offset commit overhead.

**Trigger Conditions**: Batch is processed when:
- Batch size reached (100 messages), OR
- Timeout expired (5 seconds since first message in batch)

**Atomic Behavior**: 
- Batches are atomic units - if the batch handler returns an error, the entire batch is subject to the error strategy
- Retry strategy: all messages in the batch are re-processed together
- Skip strategy: all messages in the batch are skipped together
- No partial batch success/failure tracking

---

### 6. Skip Strategy (Best-Effort Processing)

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

### 7. Fail-Fast Strategy (Critical Processing)

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
func processOrder(ctx context.Context, orderData []byte) error {
    // Parse order
    order, err := parseOrder(orderData)
    if err != nil {
        // Return error → error strategy is applied
        return fmt.Errorf("invalid order format: %w", err)
    }
    
    // Save to database
    if err := saveOrder(ctx, order); err != nil {
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
| **Retry** (requires retry queue + DLQ) | Write to retry queue, retry with backoff, then send to DLQ and continue | Production systems needing automatic retry with failure investigation |
| **Skip** | Log and continue | Best-effort analytics, non-critical data |
| **FailFast** | Stop immediately | Critical processing requiring manual intervention |
| **CircuitBreaker** (requires retry queue + DLQ) | Same as Retry but pauses consumption during systemic failures | Protect downstream services during incidents |

**Default**: FailFast strategy (stop on first error). For production, configure Retry with retry queue and DLQ.

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

**Cause**: Handler is returning errors and using FailFast strategy (default).

**Solutions**:
1. Fix handler to return `nil` on success
2. Use Skip strategy for non-critical processing
3. Use Retry strategy with retry queue and DLQ to capture failures and continue:
   ```go
   easykafka.WithErrorStrategy(
       easykafka.Retry(
           easykafka.WithRetryTopic("orders.retry"),
           easykafka.WithDLQTopic("orders.dlq"),
           easykafka.WithMaxAttempts(3),
       ),
   )
   ```

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
