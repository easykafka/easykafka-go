<!-- Mirror notice. Kept as plain HTML on purpose: it renders as a bordered box on
     both GitHub and GitLab, whereas GitHub's "> [!IMPORTANT]" alert syntax would
     show up as literal text on the mirror — the one place it needs to be read. -->
<table>
  <tr>
    <td>
      <h3>⚠️ &nbsp;Not on <code>github.com/easykafka</code>? You are reading a mirror.</h3>
      <p>
        This copy is <strong>read-only</strong> and may lag behind. Issues, pull requests, releases and CI
        all live at the source of truth:<br><br>
        👉 &nbsp;<a href="https://github.com/easykafka/easykafka-go"><strong>github.com/easykafka/easykafka-go</strong></a>
      </p>
    </td>
  </tr>
</table>

# 🔀 easykafka-go

[![Build & Lint](https://github.com/easykafka/easykafka-go/actions/workflows/build-lint.yml/badge.svg)](https://github.com/easykafka/easykafka-go/actions/workflows/build-lint.yml)
[![Unit Tests](https://github.com/easykafka/easykafka-go/actions/workflows/unit-tests.yml/badge.svg)](https://github.com/easykafka/easykafka-go/actions/workflows/unit-tests.yml)
[![Integration Tests](https://github.com/easykafka/easykafka-go/actions/workflows/integration-tests.yml/badge.svg)](https://github.com/easykafka/easykafka-go/actions/workflows/integration-tests.yml)
[![codecov](https://codecov.io/gh/easykafka/easykafka-go/branch/main/graph/badge.svg)](https://codecov.io/gh/easykafka/easykafka-go)
[![Go Reference](https://pkg.go.dev/badge/github.com/easykafka/easykafka-go.svg)](https://pkg.go.dev/github.com/easykafka/easykafka-go)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)

A minimal, handler-based Kafka consumer library for Go, built on top of
[confluent-kafka-go](https://github.com/confluentinc/confluent-kafka-go).

Write a function, point it at a topic, and let the library handle polling,
offset commits, rebalancing, and error recovery.

* **[Changelog](CHANGELOG.md)** — what each release contains, and what it deliberately does not.

## 💡 Why easykafka?

Great low-level Kafka clients already exist for Go, but they leave you to
wire up the same boilerplate every time: retry loops, dead-letter queues,
circuit breakers, graceful shutdown, batch accumulation, and offset
management. Business logic ends up tangled with infrastructure concerns.

**easykafka** was created to decouple message processing from error handling
and operational plumbing. You write a plain handler function; the library
provides high-level, composable blueprints — like retry-with-DLQ or
circuit-breaker strategies — so you can focus on *what* to do with a message
instead of *how* to survive when things go wrong.

## 🛠 Installation

```bash
go get github.com/easykafka/easykafka-go
```

Requires Go 1.27+ and a C toolchain for `librdkafka` (see the confluent-kafka-go
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

## 🛑 Stopping

**Cancelling the context passed to `Start` is the only way to stop a consumer.**
`Start` returns once that consumer's poll loop has exited, its final offsets are
committed and its connection is closed — so its return is the join point, and
there is nothing else to wait for.

```go
// The process context. Cancelling it is what stops the consumers.
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

signals := make(chan os.Signal, 1)
signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

go func() {
	<-signals
	cancel()                                 // this is what shuts the consumers down
}()

orders, err := easykafka.New(
	easykafka.WithTopic("orders"),
	easykafka.WithBrokers("localhost:9092"),
	easykafka.WithConsumerGroup("order-processors"),
	easykafka.WithHandler(processOrder),
)
if err != nil {
	log.Fatal(err)
}

payments, err := easykafka.New(
	easykafka.WithTopic("payments"),
	easykafka.WithBrokers("localhost:9092"),
	easykafka.WithConsumerGroup("payment-processors"),
	easykafka.WithHandler(processPayment),
)
if err != nil {
	log.Fatal(err)
}

consumers := map[string]easykafka.Consumer{
	"orders":   orders,
	"payments": payments,
}

// Start blocks for the life of a consumer, so each runs in its own goroutine.
var wg sync.WaitGroup
for name, c := range consumers {
	wg.Go(func() {                           // Go 1.25+: no Add/Done bookkeeping
		if err := c.Start(ctx); err != nil {
			log.Printf("consumer %s stopped: %v", name, err)
		}
	})
}

stopped := make(chan struct{})
go func() { wg.Wait(); close(stopped) }()    // makes Wait selectable

// ... serve traffic; the consumers poll and dispatch in the background ...

<-ctx.Done()                                 // a signal arrived and cancel ran

// Bound the wait below the pod's terminationGracePeriodSeconds, or SIGKILL
// lands first and this never runs.
select {
case <-stopped:
	log.Print("all consumers stopped")
case <-time.After(20 * time.Second):
	log.Print("consumers did not stop in time, exiting anyway")
}
```

Errors are logged where the consumer's name is in scope, so there is no results
channel and no correlation problem. Scaling from one consumer to several costs
four lines — the map, the `WaitGroup`, and the goroutine that makes `Wait`
selectable — and the shutdown block is unchanged.

**Two things worth knowing.** A message being handled when the context is
cancelled has its context cancelled with it, and whatever the handler returns
then goes to the error strategy like any other result: under `Skip` it is
written off, under `Retry` it is republished and burns an attempt. And a handler
that ignores its context holds `Start` open for as long as it keeps running —
Go cannot kill a goroutine, so no timeout the library held could change that.
Bounding the wait is the caller's job, because only the caller can decide to
exit. The real hard deadline is the orchestrator's.

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

| Strategy | Behaviour | Use Case | Production Readiness |
|---|---|---|---|
| **FailFast** | Stop consumer immediately | Critical processing, manual intervention | ✅ Stable |
| **Skip** | Log error, commit offset, continue | Best-effort / analytics pipelines | ✅ Stable |
| **Retry + DLQ** | Retry via Kafka topic with exponential backoff; route to DLQ after max attempts | Production systems with automatic recovery | ✅ Stable |
| **CircuitBreaker** | Retry + DLQ with pause/resume on consecutive failures | Protect downstream services during outages | ⚠️ Experimental — design fine-tuning & additional testing needed. PRs welcome! |

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

#### Observing failed retry and DLQ writes

A write to the retry or DLQ topic can fail — the broker rejects the record, or is unreachable.
Today the library does not wait for the broker to acknowledge that write before advancing the
source offset, so **a failed write loses the message**, and the only trace is a line on the
library's logger.

`WithDeliveryErrorFunc` makes that visible to your application, so you can log it in your own
format, count it and alert on it:

```go
retryStrategy, err := easykafka.NewRetryStrategy(
	easykafka.WithRetryTopic("orders.retry"),
	easykafka.WithDLQTopic("orders.dlq"),
	easykafka.WithDeliveryErrorFunc(func(de easykafka.DeliveryError) {
		lostWrites.WithLabelValues(de.Topic, de.Code).Inc()
		log.Error().Err(de.Err).
			Str("topic", de.Topic).
			Str("attempt", de.Headers["easykafka.retry.attempt"]).
			Msg("retry/DLQ write lost")
	}),
)
```

**It reports the loss, it does not prevent it.** The source offset has already advanced by the time
your function runs, and registering it changes nothing about that. Confirming the write before the
offset moves is a separate piece of work, scheduled with the producer API.

Two rules for the function you supply, because it runs on the producer's event goroutine: it must
not block — while it runs, no further delivery report is processed, including the failures it
exists to report — and it must be safe for concurrent use, since the retry and DLQ producers each
have their own goroutine. A panic is recovered and logged rather than allowed to kill the goroutine.

`de.Value` carries the record body, which for a DLQ write is the last copy that exists. Useful if
you want to spool it somewhere; usually the wrong thing to log wholesale.

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

## ⏱ Commit Cadence

By default the library commits an offset after every message, which is the
narrowest possible duplicate window and costs one round-trip to the group
coordinator per message. For higher throughput, hand commit timing to
librdkafka:

```go
easykafka.WithAutoCommitEvery(5 * time.Second)
```

A background thread then publishes the offset store on that interval. The
trade-off is the duplicate window: an abrupt exit replays up to `d` of
already-processed messages.

**Duplicates only, never loss.** The offset store advances only on messages that
were handled, so a committed offset can never run ahead of the work. Offsets are
still committed immediately on revocation and at shutdown, whatever the
interval, so a rebalance or a clean stop does not replay.

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
| `WithAutoCommitEvery(d)` | off | Commit on an interval instead of after every message |
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

Some keys are managed by the library and are rejected with an explanation rather
than silently ignored:

| Key | Why |
|---|---|
| `bootstrap.servers` | set by `WithBrokers` |
| `group.id` | set by `WithConsumerGroup` |
| `enable.auto.commit` | the library decides when offsets are published |
| `enable.auto.offset.store` | offsets are recorded only once a message has been processed |
| `partition.assignment.strategy` | rebalance handling requires an eager strategy |

The last two are what make at-least-once hold. Left to librdkafka, the offset
store advances the moment a message is polled — before your handler has seen it,
or at all in batch mode — so a rebalance would commit work that never happened.
And the rebalance handling assumes every partition is revoked at once, which a
cooperative strategy breaks.

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
gotestsum --format testdox -- -count=1 -timeout 1000s ./tests/integration/...
```
## 🔧 Built With

- [confluent-kafka-go](https://github.com/confluentinc/confluent-kafka-go) — Kafka client
- [zerolog](https://github.com/rs/zerolog) — Structured logging
- [testcontainers-go](https://github.com/testcontainers/testcontainers-go) — Integration test infrastructure

### 🤖 Built Using speckit

Spec driven development with AI (speckit) was used to generate the initial consumer implementation.

For details see:

- https://github.com/github/spec-kit
- https://www.youtube.com/watch?v=a9eR1xsfvHg
- https://github.blog/ai-and-ml/generative-ai/spec-driven-development-with-ai-get-started-with-a-new-open-source-toolkit/

## 📜 Releases

Version history is in [CHANGELOG.md](CHANGELOG.md); tagged releases appear on
[pkg.go.dev](https://pkg.go.dev/github.com/easykafka/easykafka-go). Pre-1.0, the public API may still
change in a minor release.

## 📄 Licence

MIT — see [MIT-LICENSE.md](MIT-LICENSE.md).