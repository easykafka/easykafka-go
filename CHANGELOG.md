# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) — with the usual pre-1.0 caveat that the
public API may still change in a minor release.

## [0.2.0]

### Added

- **`WithDeliveryErrorFunc(fn)`** — a retry-strategy option registering a function called for every
  retry or DLQ write that fails to reach the broker, so an application can log it in its own format,
  count it and alert on it. Until now such a failure produced one line on the library's own logger
  and nothing else: no counter, no hook, no way for the application to know it happened.

  **It reports a loss, it does not prevent one.** The library still treats a message as accounted
  for once the record is queued with the client rather than once the broker acknowledges it, so the
  source offset has already advanced when `fn` runs. Registering it does not change that. Confirming
  the write before the offset moves is separate work, scheduled with the producer API.

  `fn` receives a `DeliveryError` carrying the target topic, partition, key, value, the retry
  headers, the underlying error and a `Code` naming the kind of failure for use as a metric label.
  No confluent-kafka-go types appear in it, so callers need not import the Kafka client. `Value` is
  the record body — for a DLQ write the last copy of it that exists, which is why it is there, and
  usually the wrong thing to log wholesale.

  `fn` runs on the producer's event goroutine, so it must not block — while it runs no further
  delivery report is processed, including the failures it exists to report — and it must be safe for
  concurrent use, because the retry and DLQ producers each have their own goroutine. A panic is
  recovered and logged rather than allowed to kill that goroutine.

- **`WithAutoCommitEvery(d)`** — hands commit timing to librdkafka, which publishes the offset store
  on a background thread every `d`, instead of the library committing after every message. That
  removes one synchronous round-trip to the group coordinator per message, which was the throughput
  ceiling.

  Opt-in, so nothing changes for callers who do not set it: unset still means commit-per-message,
  the narrowest possible duplicate window. Setting an interval widens that window — an abrupt exit
  replays up to `d` of already-processed messages. **Duplicates only, never loss:** the offset store
  advances only on messages that were handled, so a committed offset can never run ahead of the
  work. Offsets are still committed immediately on revocation and at shutdown whatever the interval,
  so a rebalance or a clean stop does not replay.

  There is no default interval and no exported constant for one — not calling the option is the
  default. `auto.commit.interval.ms` joins `enable.auto.commit` as a managed key, both now derived
  from this option rather than hardcoded.

  Under an interval the commit happens off the poll loop, so failures arrive as
  `kafka.OffsetsCommitted` events rather than as return values. The adapter now handles that event
  and logs a failure at warning level; without it a coordinator rejecting every commit would be
  silent while the window grew.

### Removed

- **`Consumer.Shutdown` and `WithShutdownTimeout`.** **Breaking.** Cancelling the context passed to
  `Start` is now the only way to stop a consumer. Replace `consumer.Shutdown(ctx)` with cancelling
  that context, and wait for `Start` to return — it returns once the poll loop has exited, the final
  offsets are committed and the connection is closed, so there is nothing else to join on. The
  README's *Stopping* section has the pattern, including the shape for several consumers.

  The library had two stop mechanisms and they had drifted apart, and `Shutdown` was not the one
  being used: applications stop on SIGTERM by cancelling the context they passed to `Start`.
  `ShutdownTimeout` goes with it because it could not do anything — Go cannot kill a goroutine, so a
  handler that ignores its context cannot be stopped by any timeout the library holds. The caller,
  who *can* decide to give up and exit, is better placed to own that bound; the real hard deadline is
  the orchestrator's `terminationGracePeriodSeconds`.

- **`ConsumerState` and its constants.** Exported but unreadable — there was no accessor and no
  method took one — so nothing outside the library could observe or use them. The state they carried
  is now the single "already started" guard `Start` needs.

### Changed

- **A batch buffer that was never dispatched is dropped on shutdown, not flushed.** Its messages were
  polled but never stored, so they are re-read by whoever holds the partition next. Flushing them ran
  a bulk handler while the consumer was stopping, handed it a dead context, and let the error
  strategy advance offsets over work that never happened — written off under `Skip`, or republished
  under `Retry`, where every message burned an attempt it never earned.

- **A message being handled when the context is cancelled is failed, not rescued.** Its context is
  cancelled with the consumer's, and whatever the handler returns goes to the error strategy like any
  other result: the engine cannot tell "this failed" from "this was abandoned", and does not try.
  Under `Skip` that means a message interrupted by shutdown is skipped like any other failure. The
  alternative — handing handlers a context that survives cancellation — costs a second context, a
  deadline goroutine and a timeout the library cannot enforce anyway.

### Fixed

- **A context leak on every consumer that was not shut down via `Shutdown`** — which was all of them
  in practice. `Start` derived a cancellable context purely so `Shutdown` could cancel it, and never
  deferred the cancel, so the derived context held a reference on its parent until the parent
  finished. `go vet`'s lostcancel check missed it because the cancel func was stored in a field. The
  derived context is gone with `Shutdown`.

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

[0.2.0]: https://github.com/easykafka/easykafka-go/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/easykafka/easykafka-go/releases/tag/v0.1.0
