# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) — with the usual pre-1.0 caveat that the
public API may still change in a minor release.

## [0.2.0]

### Added

- **Per-message verdicts in batch mode.** A batch handler is now given a `*Batch` — the polled
  messages, each paired with a verdict — and records each failed message with `item.Fail`. The
  engine routes every failure to the error strategy on its own, with its own error, its own attempt
  count and its own retry-vs-DLQ decision. One poison message in a batch of 500 used to send all 500
  to the retry topic under its error, each burning an attempt it never earned; now it sends one.

  ```go
  easykafka.WithBatchHandler(func(ctx context.Context, batch *easykafka.Batch) *easykafka.Failure {
      for _, item := range batch.Items() {
          if err := process(item.Message().Payload); err != nil {
              item.Fail(easykafka.Failure{Err: err})
          }
      }
      return nil
  })
  ```

  Returning a `*Failure` still fails the batch as a whole — every message is routed under it and any
  per-item verdicts are discarded — for a handler whose failure is not about any one message (the
  database is down). A panic does the same, as before.

  Offsets are unchanged: each partition still stores its highest offset in the batch, which stays
  honest because every message is resolved — succeeded, or written off by the strategy — before the
  offsets are stored. If the strategy returns an error part-way through, dispatch stops there and
  nothing in the batch is stored, so the whole batch is redelivered on restart; messages already
  routed may then be routed twice, which is within at-least-once.

  Items are in poll order: partitions interleaved as delivered, offsets ascending within each.
  `item.Message()` returns a copy, so a handler cannot move the offsets the engine accounts against;
  the payload and headers are still shared and must be treated as read-only. Distinct items may be
  failed from different goroutines, provided they are joined before the handler returns.

  `NewBatch(msgs)` builds a batch without a broker, so a batch handler can be unit-tested: run it,
  then check `item.Failed()` on each item.

- **`Failure`**, what both handlers now return and what `item.Fail` takes. Beside the required `Err`
  it carries two optional values the handler owns:

  - `Step` — a resume point, written to `easykafka.retry.step` and read back with `GetRetryStep`.
    Left at 0, the step the record arrived with carries forward, so a resume point is never
    silently lost.
  - `Code` — a domain error code, written to `easykafka.error.code` and read back with the new
    `GetErrorCode`. Left empty, the library writes `HANDLER_ERROR`; the code the record arrived with
    is *not* carried forward, because it described a different failure.

  A `Failure` with no `Err` is still routed, under `ErrUnspecified`, with a warning naming the
  offset — no strategy ever sees a nil error. `Failure` deliberately does not implement `error`, so a
  typed nil cannot end up inside an interface and read as a failure.

- **`ErrPermanent`.** Wrap it into a failure's error — `fmt.Errorf("%w: unmarshal: %v",
  easykafka.ErrPermanent, err)` — and the retry strategy sends the message straight to the DLQ on
  its first failure instead of walking the retry ladder. For a failure no reprocessing can fix: a
  malformed record, an unknown schema version. It must be `%w`; `%v` compiles and silently drops the
  marker. `Skip` and `FailFast` ignore it.

- **`Message.Key`**, populated from the consumed record. Until now the key was dropped in `Poll`, so
  nothing downstream — handler included — could see it.

- **A playground** in `examples/playground`: a producer and a consumer for trying the library by
  hand, built into `bin/` by `make playground`, with Kafka and the AKHQ web UI run by docker
  compose. Each message's payload is a script — `ko/ko/ok` fails twice, then succeeds — so the
  retry topic, the DLQ, batch routing and committed offsets can be watched as they happen. Its
  README walks through the scenarios.

- **`WaitUntilRetryTime(ctx, msg)`**, for consuming a retry topic. It blocks until the record's
  retry time, returns at once for a record with none — a first delivery — or one already due, and
  returns `ctx.Err()` if the context is cancelled first. One handler can therefore call it
  unconditionally and serve both the source topic and the retry topic. It replaces the README's
  earlier advice to fail a record back while it is not yet due, which counted every early look as
  an attempt and could send a record to the DLQ unprocessed.

  The wait runs on the polling goroutine, so nothing is fetched meanwhile, and a wait longer than
  `max.poll.interval.ms` (300 s by default) gets the consumer removed from its group. The retry
  time is stored to the second, so the wait may end up to a second early.

- **`MessageFromContext(ctx)`, the nine retry header keys and the four `Get*` accessors** are now
  part of the public API. The 0.1.0 notes advertised message metadata as shipped — "handlers that
  need more than the payload read topic, partition, offset, timestamp and headers from the `Message`
  carried on the context" — but the accessor lived in `internal/`, so no consumer of the library
  could reach it. The `Message` type was exported with nothing that returns one. That is now fixed
  rather than re-promised.

  **`ok` is false in batch mode.** A batch handler is given many messages at once and its context
  carries none of them, so there is no single message to describe. Attaching one element of the
  batch would be worse than attaching none: the accessor would report true with metadata describing
  an arbitrary record. Nor is anything missing: a batch handler reads each message's metadata from
  `item.Message()` (see *Per-message verdicts in batch mode*, below).

  `HeaderRetryAttempt` and its eight siblings, plus `GetRetryAttempt`, `GetRetryTime`,
  `GetRetryStep` and `GetOriginalTopic`, go out with it. These are what make the retry topic
  consumable: the library republishes a failed record with a due time and never reads it back, so
  the waiting half is the application's to write, and until now the headers it needed were
  unreachable. The README has the pattern.

  Exporting the keys freezes them as API, which they effectively already were — a retry topic
  outlives the deployment that wrote to it.

  The write side stays internal. There is no exported `WithMessage`, and no `BuildRetryHeaders`: a
  handler that could mint either would be able to plant a context value the engine then dispatches
  through, or lie to the library's own accessors about attempt counts.

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

- **`WithFailedMessagePayloadEncoding`, `PayloadEncoding`, `PayloadEncodingJSON` and
  `PayloadEncodingBase64`.** **Breaking.** They existed only to fit binary payloads into the DLQ's
  JSON envelope, which is gone (see *Changed*); a DLQ record now carries the consumed bytes as they
  came, so there is nothing to encode.

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

- **The `CircuitBreaker` error strategy.** **Breaking.** `NewCircuitBreakerStrategy` is gone, along
  with `WithFailureThreshold`, `WithCooldownPeriod`, `WithHalfOpenAttempts` and `WithRetryOptions`.
  Three strategies remain: `Skip`, `FailFast` and `Retry`.

  It did not do what its name said. When the breaker tripped it *returned an error*, and the engine
  treats any non-nil strategy error as fatal — so it stopped the consumer rather than pausing it,
  which made `CircuitOpen` and `CircuitHalfOpen` unreachable in real operation and the cooldown,
  half-open probing and recovery logic dead code. Separately, the engine never called `OnSuccess`,
  so `WithFailureThreshold(n)` counted the *n*th failure ever rather than the *n*th consecutive one.

  **There is no replacement, and a pause/resume capability is not planned.** A caller reaching for
  the breaker to stop on repeated failures wants `FailFast`, which is what the breaker actually did.
  Making it real would need pause/resume on the consumer and a way for a strategy to ask for it —
  machinery nothing else in the library wants. No deprecation cycle: there was no correct usage to
  migrate, so a release spent keeping the misleading name would have bought nothing.

  This removes the library's last experimental surface; everything shipped is now supported.

- **`ConsumerState` and its constants.** Exported but unreadable — there was no accessor and no
  method took one — so nothing outside the library could observe or use them. The state they carried
  is now the single "already started" guard `Start` needs.

### Changed

- **Both handlers return `*Failure` instead of `error`.** **Breaking.** nil still means success.

  ```go
  // before
  func(ctx context.Context, payload []byte) error    { return err }
  func(ctx context.Context, payloads [][]byte) error { return err }

  // after
  func(ctx context.Context, payload []byte) *easykafka.Failure { return &easykafka.Failure{Err: err} }
  func(ctx context.Context, batch *easykafka.Batch) *easykafka.Failure {
      for _, item := range batch.Items() { /* item.Fail(...) on the ones that failed */ }
      return nil
  }
  ```

  A batch handler that only ever had one error for the whole batch migrates by wrapping it —
  `return &easykafka.Failure{Err: err}` — and keeps its all-or-nothing behaviour.

- **`ErrorStrategy.HandleError` takes a `Failure` instead of an error.** **Breaking** for anyone
  implementing a strategy outside the library. It is how the step and the code reach the strategy.
  In batch mode `msgs` now holds a single message for a per-item failure, and every message of the
  batch only for a whole-batch failure or a panic.

- **A DLQ record is the original message.** **Breaking** for anything reading a DLQ topic. The body
  is the consumed bytes and the key is the consumed key — exactly like a retry record — and every
  fact about the failure is in headers. The JSON envelope is gone; each of its fields was already a
  header:

  | Envelope field | Header |
  |---|---|
  | `originalTopic`, `originalPartition`, `originalOffset` | `easykafka.original.topic`, `.partition`, `.offset` |
  | `error` | `easykafka.error.message` |
  | `attemptCount` | `easykafka.retry.attempt` |
  | `timestamp` | `easykafka.failed.at` |
  | `payload` | the body itself |

  The envelope's default path, `string(payload)`, silently replaced invalid UTF-8 — irreversible for
  a protobuf or Avro record. Replaying a dead-lettered message is now a re-publish of its body and
  key; drop the `easykafka.*` headers when doing so, or it arrives already at its last attempt.

- **Retry and DLQ records carry the consumed key.** Retry records used to be written without one,
  for round-robin partitioning. A keyed record can be found by key in a Kafka UI or with `kcat`; the
  cost is that a hot key on the source topic makes a hot retry partition too.

- **`easykafka.retry.step` is written from `Failure.Step`,** carrying the inbound value forward when
  the handler reports none, and is omitted when there is no step at all. Previously nothing could set
  it and it was always `"0"`. DLQ records now carry it too.

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

- **`easykafka.original.partition` and `.offset` survive every hop.** Only the topic used to: the
  partition and offset were overwritten with the current record's on each republish, so after a
  second hop they named a position on the retry topic paired with the source topic's name. They now
  keep the inbound values, as the topic always did.

- **A context leak on every consumer that was not shut down via `Shutdown`** — which was all of them
  in practice. `Start` derived a cancellable context purely so `Shutdown` could cancel it, and never
  deferred the cancel, so the derived context held a reference on its parent until the parent
  finished. `go vet`'s lostcancel check missed it because the cancel func was stored in a field. The
  derived context is gone with `Shutdown`.

- **The package doc named the wrong default error strategy.** It listed `FailFast` as the default;
  the default is and always was `Skip`, for both single-message and batch mode.

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
