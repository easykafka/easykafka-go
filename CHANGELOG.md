# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) — with the usual pre-1.0 caveat that the
public API may still change in a minor release.

## [0.1.0]

First release.

### Added

- **`Consumer`** — a handler-based Kafka consumer built on `confluent-kafka-go`. `New` takes functional
  options and returns a consumer whose lifecycle is `Created → Running → ShuttingDown → Stopped`;
  `Start` polls and dispatches until the context is cancelled, `Shutdown` drains in-flight work within
  a deadline. Polling, offset commits, rebalancing and reconnection are the library's job, so the
  caller writes only `func(ctx, payload) error`.
- **Batch mode** — `WithBatchHandler` delivers `[][]byte` instead of one payload. A batch is handed
  over when `WithBatchSize` is reached **or** `WithBatchTimeout` fires, whichever comes first, and its
  offsets are committed atomically.
- **Four error strategies**, selected with `WithErrorStrategy`: `Skip` (the default — log, commit,
  continue), `FailFast` (stop the consumer on the first error), `Retry` (Kafka-topic-based retry with
  exponential backoff and optional DLQ routing) and `CircuitBreaker` (retry plus pause/resume on
  consecutive failures). `ErrorStrategy` is a two-method interface, so a service can supply its own.
- **Retry configuration** — `WithRetryTopic`, `WithDLQTopic`, `WithMaxAttempts`, `WithInitialDelay`,
  `WithMaxDelay`, `WithBackoffMultiplier`, `WithCustomBackoff` for a caller-supplied schedule, and
  `WithFailedMessagePayloadEncoding` to choose between JSON and base64 for republished payloads.
- **Circuit-breaker configuration** — `WithFailureThreshold`, `WithCooldownPeriod`,
  `WithHalfOpenAttempts`, and `WithRetryOptions` to configure the retry behaviour it wraps.
- **Message metadata** — handlers that need more than the payload read topic, partition, offset,
  timestamp and headers from the `Message` carried on the context. Retried and dead-lettered records
  carry headers recording the attempt number, the retry time, and the original topic.
- **Twelve options**, from `WithTopic` to `WithKafkaConfig`. See the README's configuration reference.
- **Kafka config passthrough** — `WithKafkaConfig` forwards any librdkafka property. The three keys the
  library's semantics depend on (`bootstrap.servers`, `group.id`, `enable.auto.commit`) are rejected
  with the reason rather than silently ignored.

### Design decisions worth knowing

- **Auto-commit is off and offsets are committed by the library** once a handler returns nil, which is
  what makes delivery at-least-once. A handler that fails never advances the offset on its own; what
  happens next is the error strategy's decision.
- **Retry waits in Kafka, not in the handler.** A failed record is republished to a retry topic with
  its attempt count and due time in headers, so backoff never blocks the poll loop and never risks the
  consumer being evicted from its group for missing a heartbeat.
- **Error handling is pluggable, not built in.** The engine knows only that a handler returned an
  error; every decision after that belongs to an `ErrorStrategy`.

### Not in this release

These are named because a reader may come looking:

- **`CircuitBreaker` is experimental.** It works and is covered by tests, but the design is still being
  fine-tuned and it has had less exposure than the other three. Treat it as such in production; PRs
  are welcome.
- **There is no producer API.** This is a consumer library — it produces only to the retry and DLQ
  topics it owns. Use `confluent-kafka-go` directly to publish.
- **Compacted configuration topics are out of scope.** Reading one to its end into a typed map is a
  different pattern with no handlers, no retries and no offset commits; that is
  [easykafka-config-go](https://github.com/easykafka/easykafka-config-go).

[0.1.0]: https://github.com/easykafka/easykafka-go/releases/tag/v0.1.0
