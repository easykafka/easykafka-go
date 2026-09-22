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
graceful shutdown, batch accumulation, and offset management. Business logic
ends up tangled with infrastructure concerns.

**easykafka** was created to decouple message processing from error handling
and operational plumbing. You write a plain handler function; the library
provides high-level, composable blueprints — like retry-with-DLQ — so you can
focus on *what* to do with a message instead of *how* to survive when things
go wrong.

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

## 🏷 Message Metadata

A handler receives the payload. When it needs more — which topic, which
partition, which offset, the timestamp, the headers — it reads the message off
the context:

```go
easykafka.WithHandler(func(ctx context.Context, payload []byte) error {
	msg, ok := easykafka.MessageFromContext(ctx)
	if !ok {
		return nil
	}

	log.Printf("%s[%d] offset %d at %s",
		msg.Topic, msg.Partition, msg.Offset, msg.Timestamp)
	return nil
})
```

**`ok` is false in batch mode.** A batch handler is given many messages at once
and its context carries none of them — there is no single message it could
describe. Per-message metadata for batches would mean changing `BatchHandler`'s
signature away from `[][]byte`; until then, batch handlers see payloads only.

There is no exported way to *put* a message on a context. Handlers read what the
library wrote; they cannot plant a value the library then dispatches through.

### Retry headers

Records the library republishes to the retry and DLQ topics carry headers
describing why they are there. The keys are exported as constants, and the
values are read with accessors:

```go
msg, _ := easykafka.MessageFromContext(ctx)

attempt := easykafka.GetRetryAttempt(msg)    // 0 on first delivery
origin  := easykafka.GetOriginalTopic(msg)   // "" if never retried
due     := easykafka.GetRetryTime(msg)       // zero Time if absent
```

This is what makes the retry topic consumable. **The library writes to the retry
topic but never reads from it** — it stamps each record with a due time and
republishes, and honouring that time is the application's job. A retry consumer
is an ordinary consumer subscribed to the retry topic, deciding from
`GetRetryTime` whether a record is ready:

```go
easykafka.New(
	easykafka.WithTopic("orders.retry"),
	easykafka.WithBrokers("localhost:9092"),
	easykafka.WithConsumerGroup("order-retries"),
	easykafka.WithHandler(func(ctx context.Context, payload []byte) error {
		msg, _ := easykafka.MessageFromContext(ctx)

		if due := easykafka.GetRetryTime(msg); time.Now().Before(due) {
			return fmt.Errorf("not due until %s", due) // let the strategy requeue it
		}
		return processOrder(ctx, payload)
	}),
)
```

The full set of header keys, for reading raw records or writing your own
tooling: `HeaderRetryAttempt`, `HeaderRetryTime`, `HeaderRetryStep`,
`HeaderErrorCode`, `HeaderErrorMessage`, `HeaderOriginalTopic`,
`HeaderOriginalPartition`, `HeaderOriginalOffset`, `HeaderFailedAt`.

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

| Strategy | Behaviour | Use Case |
|---|---|---|
| **Skip** | Log error, commit offset, continue | Best-effort / analytics pipelines (the default) |
| **FailFast** | Stop consumer immediately | Critical processing, manual intervention |
| **Retry + DLQ** | Retry via Kafka topic with exponential backoff; route to DLQ after max attempts | Production systems with automatic recovery |

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
			Str("attempt", de.Headers[easykafka.HeaderRetryAttempt]).
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

## 📜 Releases

Version history is in [CHANGELOG.md](CHANGELOG.md); tagged releases appear on
[pkg.go.dev](https://pkg.go.dev/github.com/easykafka/easykafka-go). Pre-1.0, the public API may still
change in a minor release.

## 📄 Licence

MIT — see [MIT-LICENSE.md](MIT-LICENSE.md).