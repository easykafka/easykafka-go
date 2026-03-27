# 📦 easykafka-go

A minimal, handler-based Kafka consumer library for Go, built on top of
[confluent-kafka-go](https://github.com/confluentinc/confluent-kafka-go).

Write a function, point it at a topic, and let the library handle polling,
offset commits, rebalancing, and error recovery.

## 🛠 Installation

```bash
go get github.com/easykafka/easykafka-go
```

Requires Go 1.19+ and a C toolchain for `librdkafka` (see the confluent-kafka-go
docs for platform-specific instructions).

## 🚀 Quick Start

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/easykafka/easykafka-go"
)

func main() {
	consumer, err := easykafka.New(
		easykafka.WithTopic("orders"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("order-processors"),
		easykafka.WithHandler(func(ctx context.Context, payload []byte) error {
			fmt.Printf("received: %s\n", payload)
			return nil
		}),
	)
	if err != nil {
		log.Fatal(err)
	}

	if err := consumer.Start(context.Background()); err != nil {
		log.Fatal(err)
	}
}
```

That's it — the consumer connects, polls messages, calls your handler, and
commits offsets on success.

## 🛑 Graceful Shutdown

Cancel the context or call `Shutdown` to let in-flight work complete:

```go
ctx, cancel := context.WithCancel(context.Background())

go func() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	cancel()
}()

if err := consumer.Start(ctx); err != nil {
	log.Fatal(err)
}
```

## 📦 Batch Processing

For high-throughput workloads, switch to batch mode:

```go
consumer, err := easykafka.New(
	easykafka.WithTopic("events"),
	easykafka.WithBrokers("localhost:9092"),
	easykafka.WithConsumerGroup("event-processors"),
	easykafka.WithBatchHandler(func(ctx context.Context, payloads [][]byte) error {
		return bulkInsert(ctx, payloads)
	}),
	easykafka.WithBatchSize(100),
	easykafka.WithBatchTimeout(5*time.Second),
)
```

Batches are delivered when the size limit is hit **or** the timeout fires,
whichever comes first. Offsets are committed atomically per batch.

## ⚡ Error Strategies

Pluggable strategies control what happens when a handler returns an error:

| Strategy | Behaviour | Use Case |
|---|---|---|
| **FailFast** | Stop consumer immediately | Critical processing, manual intervention |
| **Skip** | Log error, commit offset, continue | Best-effort / analytics pipelines |
| **Retry + DLQ** | Retry via Kafka topic with exponential backoff; route to DLQ after max attempts | Production systems with automatic recovery |
| **CircuitBreaker** | Retry + DLQ with pause/resume on consecutive failures | Protect downstream services during outages |

### Retry + DLQ

```go
retryStrategy, err := easykafka.NewRetryStrategy(
	easykafka.WithRetryTopic("orders.retry"),
	easykafka.WithDLQTopic("orders.dlq"),
	easykafka.WithMaxAttempts(3),
	easykafka.WithInitialDelay(1*time.Second),
	easykafka.WithMaxDelay(30*time.Second),
)
if err != nil {
	log.Fatal(err)
}

consumer, err := easykafka.New(
	easykafka.WithTopic("orders"),
	easykafka.WithBrokers("localhost:9092"),
	easykafka.WithConsumerGroup("order-processors"),
	easykafka.WithHandler(processOrder),
	easykafka.WithErrorStrategy(retryStrategy),
)
```

### Circuit Breaker

```go
cbStrategy, err := easykafka.NewCircuitBreakerStrategy(
	easykafka.WithFailureThreshold(5),
	easykafka.WithCooldownPeriod(30*time.Second),
	easykafka.WithHalfOpenAttempts(2),
	easykafka.WithRetryOptions(
		easykafka.WithRetryTopic("orders.retry"),
		easykafka.WithDLQTopic("orders.dlq"),
		easykafka.WithMaxAttempts(3),
	),
)
```

## ⚙️ Configuration Reference

### Required Options

| Option | Description |
|---|---|
| `WithTopic(topic)` | Kafka topic to consume from |
| `WithBrokers(addrs...)` | Broker addresses |
| `WithConsumerGroup(id)` | Consumer group ID |
| `WithHandler(fn)` or `WithBatchHandler(fn)` | Message processing function |

### Optional Options

| Option | Default | Description |
|---|---|---|
| `WithErrorStrategy(s)` | Skip | Error handling strategy |
| `WithBatchSize(n)` | 100 | Max messages per batch |
| `WithBatchTimeout(d)` | 5s | Partial-batch flush interval |
| `WithPollTimeout(d)` | 100ms | Kafka poll timeout |
| `WithShutdownTimeout(d)` | 30s | Graceful shutdown deadline |
| `WithLogger(l)` | no-op | Structured logger (zerolog) |
| `WithKafkaConfig(m)` | — | Passthrough to confluent-kafka-go |

### Kafka Config Passthrough

Low-level confluent-kafka-go settings can be passed through directly:

```go
easykafka.WithKafkaConfig(map[string]any{
	"session.timeout.ms": 6000,
	"auto.offset.reset":  "earliest",
})
```

Keys managed by the library (`bootstrap.servers`, `group.id`,
`enable.auto.commit`) cannot be overridden.

## 🧪 Testing

Unit and integration tests live under `tests/`:

```
tests/
├── unit/            # Pure logic tests, no Kafka dependency
└── integration/     # Require a real Kafka broker (testcontainers-go)
    └── helpers/     # Shared Kafka test cluster utilities
```

Run unit tests:

```bash
go test -v -count=1 ./tests/unit/...
```

Run integration tests (requires Docker):

```bash
go test -v -count=1 ./tests/integration/...
```

or (to get a more readable output):
```bash
go install gotest.tools/gotestsum@latest
gotestsum --format testdox -- -count=1 -timeout 600s ./tests/integration/...
```

format options:
* testdox
  * Human-readable test names with ✓/✗
* pkgname
  * One line per package + failures
* standard-verbose
  * Like -v but with a summary at the end
* dots
  * Minimal dots during run, failures at end


## 🔧 Built With

- [confluent-kafka-go](https://github.com/confluentinc/confluent-kafka-go) — Kafka client
- [zerolog](https://github.com/rs/zerolog) — Structured logging
- [testcontainers-go](https://github.com/testcontainers/testcontainers-go) — Integration test infrastructure

## 🤖 Built Using speckit

- https://github.com/github/spec-kit
- https://www.youtube.com/watch?v=a9eR1xsfvHg
- https://github.blog/ai-and-ml/generative-ai/spec-driven-development-with-ai-get-started-with-a-new-open-source-toolkit/