# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

EasyKafka is a handler-based Kafka consumer library for Go, built on top of `confluent-kafka-go`. It abstracts polling, offset management, rebalancing, and error recovery so developers write clean handler logic instead of Kafka infrastructure code.

## Commands

### Testing
```bash
# Unit tests
go test -v -count=1 ./tests/unit/...

# Integration tests (require Docker — uses testcontainers-go)
go test -v -count=1 -timeout 1000s ./tests/integration/...

# Human-readable integration test output
gotestsum --format testdox -- -count=1 -timeout 1000s ./tests/integration/...

# Coverage (note: -coverpkg=./... is required to instrument library packages, not just test packages)
go test -count=1 -timeout 1000s -coverprofile=coverage.out -covermode=atomic -coverpkg=./... ./tests/...

# Single test by name
go test -v -count=1 -run TestName ./tests/unit/...
```

### Build
```bash
go build ./...
go mod download
```

### Tooling & Linting
Dev tools are installed into `./bin` (gitignored) at pinned versions — never
globally, and never via brew. Run this once after cloning:
```bash
make install-tools        # golangci-lint + gotestsum into ./bin
make lint                 # ./bin/golangci-lint run
```

Version pins are single-sourced:
- `.golangci-lint-version` — read by both the `Makefile` and
  `.github/workflows/build-lint.yml` (via the action's `version-file` input),
  so local and CI always run the same linter build. Bump that one file.
- `GOTESTSUM_VERSION` in the `Makefile` — CI installs it via
  `make install-test-tools` and runs tests through `make test-unit` /
  `make test-integration`, so flags cannot drift between local and CI.

`.golangci.yml` is kept in sync with the other SRM Go repos (`srm-common`,
`srm-overask`, `srm-transactional-core`); `tests/` is excluded from the
churn-heavy linters (`mnd`, `lll`, `dupl`, `gocognit`, `gosec`, `errcheck`,
`unparam`). Note golangci-lint v2 requires `golangci-lint-action@v7` or newer.

## Architecture

### Public API (root package)
- `consumer.go` — `Consumer` interface. One method, `Start`, which blocks for the life of the consumer. Cancelling the context passed to it is the only way to stop; `Start` returns once the poll loop has exited, the final offsets are committed and the connection is closed.
- `options.go` — all configuration via functional options (`WithTopic`, `WithBrokers`, `WithHandler`, etc.)
- `handler.go` — re-exports types from internal packages for public consumption

### Internal packages
- `internal/engine/` — core polling loop (`engine.go`) and batch accumulation (`batch.go`). Supports both single-message and batch modes.
- `internal/kafka/` — wraps confluent-kafka-go consumer (`adapter.go`) and builds the retry publisher's Kafka config from the consumer's (`publisher_config.go`)
- `internal/types/` — core interfaces: `Handler`, `BatchHandler`, `Batch`/`BatchItem`, `Failure`, `ErrorStrategy`, `Message`, `Initializable`, `LoggerAware`
- `internal/metadata/` — message metadata via context decorators and header parsing

### What of `internal/metadata` is public, and what is deliberately not

`handler.go` re-exports the **read** side: `MessageFromContext`, the nine `Header*` key constants,
`GetRetryAttempt` / `GetRetryTime` / `GetRetryStep` / `GetErrorCode` / `GetOriginalTopic`, and
`WaitUntilRetryTime`, which waits on the retry time rather than returning it.

The **write** side stays internal on purpose. `WithMessage` is how the engine populates a handler
context, and `BuildRetryHeaders` / `BuildDLQHeaders` are the retry strategy's. Exporting either
would let an application plant a context value the engine then dispatches through, or mint headers
that lie to the library's own accessors about attempt counts. Keep the asymmetry.

Two things to preserve here:

- **The exported `Header*` constants spell their values out** rather than aliasing
  `metadata.Header*`, because `const X = metadata.X` renders in godoc as a reference into a package
  the reader cannot open — the wire string is then invisible on pkg.go.dev. The duplication is
  guarded by `TestHeaderKeysMatchInternal`, which fails if either side is renamed.
- **`MessageFromContext` returns `(nil, false)` in batch mode**, because `dispatchBatch` passes the
  raw loop context. That is documented in three places and pinned by
  `TestMessageFromContextIsEmptyInBatchMode`. Do not "fix" it by attaching one message of the batch.
  Nothing is missing: the per-message batch results in `srm-specs` `003-partial-batches` gave the
  batch handler a `*Batch`, and each item's `Message()` carries that message's metadata.

Tests reach the public symbols through `easykafka.` rather than `internal/metadata` wherever they
are exercising the export, so a regression in the re-export fails a test. Nothing in-module can
*prove* external reachability — `go doc .` is the real check.

### Handlers report a `*Failure`, and batch mode reports one per message

Both handler types return `*types.Failure` — never `error`, and `Failure` must not implement
`error`: a typed nil inside an `error` interface is non-nil and would read as a failure. A batch
handler records per-message verdicts with `item.Fail` on the `*Batch` it is given; the engine then
calls the strategy once per failed item, in batch order. A returned `*Failure` (or a panic) instead
routes every message in one call and discards the item verdicts.

Three things to preserve in `dispatchBatch`:

- **The store block stays below both strategy calls, and both return early on a strategy error.**
  The per-partition maximum is honest only because every message is resolved before it is stored.
  Stopping half-way through the walk leaves one message in neither the retry topic nor the DLQ;
  storing a higher offset from its partition would lose it. Nothing may defer a message's outcome
  past the store block.
- **Offsets come from the buffered `[]*Message`, not from the items.** `BatchItem.Message()` returns
  a copy for the same reason: a handler must not be able to move what the engine accounts against.
- **The engine fills in a nil `Failure.Err` with `ErrUnspecified`** (and warns), so no strategy has
  to defend against one.

In `internal/metadata`, of the library's headers only `easykafka.retry.step` and
`easykafka.error.code` are the handler's, through `Failure.Step` / `Failure.Code`, and they behave
oppositely when unset: the inbound step carries forward, the inbound code does not.
`easykafka.original.*` keep their inbound values on every hop. Retry and DLQ records are the same
shape — consumed key, consumed bytes, failure in headers — and there is no DLQ envelope.

### Error strategies (`strategy/` package — public)
Pluggable via `WithErrorStrategy()`. Three implementations:
- `skip.go` — logs error and continues (default)
- `fail_fast.go` — stops consumer immediately on first error
- `retry.go` — Kafka-based retry with exponential backoff, DLQ routing after max attempts or at
  once for a failure wrapping `ErrPermanent`

A strategy returning a non-nil error is fatal — the engine stops the poll loop and `Start` returns
it. That is the whole of the "stop consuming" capability; there is no pause/resume, and a
`CircuitBreaker` strategy that appeared to offer one was removed in 0.2.0 because it did not.

### Retry/DLQ writes are confirmed before the offset is stored

The retry strategy writes through one `publish.Publisher` (`acks=all`, idempotence on, 30 s delivery
timeout), with one writer per topic. `HandleError` returns nil only once every record of the call is
acknowledged; any failure fails it, and the engine stops without storing the source offset. A lost
write is a stopped consumer, not a lost message. `WithDeliveryErrorFunc` takes a
`publish.DeliveryErrorFunc` and reports a failure the consumer also stops on — never describe it as
reporting a loss.

Three things to preserve in `HandleError`:

- **Send every record first, then wait once.** Waiting per record would cost one broker round trip
  per message of a batch.
- **The wait uses `context.WithoutCancel(ctx)`.** A shutdown must not abandon a write about to be
  confirmed: that would fail `HandleError`, stop the consumer with a false error and duplicate the
  record. The delivery timeout bounds the wait.
- **A nil payload goes through `SendDelete`.** `publish.RawValue` refuses nil, and the record must
  keep the value it was consumed with.

`strategy.WithProducerFactory` is the unit tests' seam onto `helpers.FakePublishProducer`, as
`publish.WithProducerFactory` is the publisher's; its argument names `internal/` types, so no other
module can use it.

### Log codes

Notable log lines carry a stable code in the `ek_code` field (`logcode.Field`), with the message kept
as readable prose beside it. Codes are constants in `internal/logcode`: descriptive, `EK_`-prefixed,
one comment each saying what the event means and at what level it is logged. Never change a code
once released; reword the message instead. The codes are deliberately not exported: they are
documented strings, not API.

**A code added to `internal/logcode/logcode.go` must also get a row in the README's *Log codes*
table** — code, level, meaning, what to do — in the same change. Nothing checks this
automatically; the README table is the only place an operator finds what a code means.

### The retry publisher inherits the consumer's Kafka config

The consumer's `WithKafkaConfig` map reaches the strategy through `InitConfig.KafkaConfig`, and
`kafka.PublisherConfig` builds the publisher's config from it. Without that, the publisher would
connect without the consumer's security, SASL and TLS settings, and every retry and DLQ write would
fail.

Three things to preserve:

- **The filter is a deny-list of consumer-only keys, not an allowlist of shared ones.** A missed
  consumer-only key costs one librdkafka `CONFWARN` line at producer start. A missed shared key —
  `enable.ssl.certificate.verification`, say, which no `ssl.` prefix catches — would silently
  break every write. Keep adding to the deny-list; never replace it with an allowlist.
- **`go.*` keys are always dropped.** Some that a consumer accepts
  (`go.application.rebalance.enable`, `go.events.channel.enable`) make `kfk.NewProducer` fail with
  "No such configuration property".
- **Keys the publisher manages are dropped**, or `publish.WithKafkaConfig` would reject them and
  `Initialize` would fail. The list is `publishdriver.ManagedKeyReason`, the one
  `publish.WithKafkaConfig` checks too: never copy it.

### Testing approach
- `tests/unit/` — pure Go logic, no Kafka dependency (strategy behavior, batch buffer, shutdown logic, options validation, the publisher against a fake producer)
- `tests/integration/` — full Kafka via testcontainers-go (consumer basics, batch, retry/DLQ, fail-fast, graceful shutdown, rebalancing, reconnection, at-least-once semantics, delivery errors)

- `examples/playground/` — not a test: a producer and consumer for trying the library by hand,
  driven by payload scripts such as `ko/ko/ok`, against Kafka and AKHQ run by docker compose.
  `make playground` builds them into `bin/`. See its README. It is part of the module and linted with the full rule set, unlike `tests/`. Its
  `internal/script` package is unreachable from `tests/`, so it has no unit tests; the playground
  itself is how it is exercised.

**All tests live under `tests/`** — there are no in-package `_test.go` files. Unexported logic is
therefore unreachable from tests, which is why some internals are exported within `internal/`.

**Test files contain tests only.** Every helper — mocks, fakes, fixtures, shared setup — goes in
the helpers folder beside the tests that use it, in a file of its own, never inside a `_test.go`
file:

- `tests/unit/helpers/` — one file per mock (`mock_kafka_client.go`, `mock_strategy.go`,
  `fake_publish_producer.go`, …), `fixtures.go` for `NewTestMessage` / `TestLogger`, and one file per
  helper function (`run_batch.go`, `count_calls.go`, …).
- `tests/integration/helpers/` — the test cluster (`kafka_helper.go`), cluster setup such as
  `create_topic_rejecting_everything.go`, fakes such as `sync_buffer.go`, and consumer helpers
  (`run_until.go`, `wait_for_messages.go`, …).

A new helper gets a new file named after it, not a spot at the bottom of the test that first needed
it. The helpers are a separate package, so anything a test sets or reads must be exported: settings
as exported fields, observed state behind locking accessors (`Closed()`, `StoredOffsets()`), and
internal state — the mutex, indices, recorded calls — unexported. Files there are plain `.go`, not
`_test.go`, or the tests cannot import them.

To make a write fail deterministically in an integration test, create the target topic with
`max.message.bytes=1` (`cluster.CreateTopicRejectingEverything`). The record then passes
librdkafka's own client-side size check — which would fail `Produce` synchronously and never
produce a delivery report — and is rejected by the broker instead, which is the path that generates
one. Stopping the broker works less well: a record then waits out its delivery timeout, 30 s by
default.

### Key dependencies
- `confluent-kafka-go/v2` — underlying Kafka client
- `zerolog` — structured logging
- `testcontainers-go/modules/kafka` — integration test containers
- `testify` — test assertions

### CI/CD
GitHub Actions workflows in `.github/workflows/`:
- `unit-tests.yml` — runs unit tests with `-race` flag
- `integration-tests.yml` — runs integration tests with Docker/testcontainers
- `coverage.yml` — uploads coverage to Codecov
- `mirror-images.yml` — mirrors Kafka Docker image to GHCR (`confluentinc/cp-kafka:7.5.0`)

Integration tests in CI pull Kafka from `ghcr.io/<owner>/mirror-confluentinc-cp-kafka:7.5.0`.