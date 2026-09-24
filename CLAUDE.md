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
- `internal/kafka/` — wraps confluent-kafka-go consumer (`adapter.go`) and produces to retry/DLQ topics (`producer.go`)
- `internal/types/` — core interfaces: `Handler`, `BatchHandler`, `Batch`/`BatchItem`, `Failure`, `ErrorStrategy`, `Message`, `Initializable`, `LoggerAware`, `DeliveryError`/`DeliveryErrorFunc`
- `internal/metadata/` — message metadata via context decorators and header parsing

### What of `internal/metadata` is public, and what is deliberately not

`handler.go` re-exports the **read** side: `MessageFromContext`, the nine `Header*` key constants,
and `GetRetryAttempt` / `GetRetryTime` / `GetRetryStep` / `GetErrorCode` / `GetOriginalTopic`.

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

### Retry/DLQ writes are not confirmed — and what that means for the callback

`Produce` (`internal/kafka/producer.go`) passes a **nil delivery channel**, so it returns as soon as
the record is queued in librdkafka's local buffer. `RetryStrategy.HandleError` reads that nil as
success and returns nil, and the engine stores the source offset on the strength of it. **A retry or
DLQ write that never reaches the broker still advances the offset**, so the message is lost.

This is known and deliberate for now: confirming the write is scheduled with the greenfield producer
API rather than bolted onto the retry strategy. See `producer-durability-implementation-plan.md` in
`srm-specs` — and in particular its "Read this before designing the producer API" note, which
records that `kafka.Message.Opaque` correlates a delivery report back to its record **without** a
per-produce delivery channel, so confirmation need not block the producing goroutine at all.

`WithDeliveryErrorFunc(fn)` is the interim: it makes that loss visible so an application can log,
count and alert on it. **It reports a loss, it does not prevent one** — the offset has already
advanced when `fn` runs. Do not describe it as fixing the above, in code comments or docs.

Three things to preserve when touching this path:

- **`Produce` sets `Opaque: msg`.** librdkafka does not return headers on a delivery report, so the
  submitted `*types.ProduceMessage` is carried through as the opaque and preferred over the report's
  own copy in `DeliveryErrorFor`. Without it the callback cannot name the retry attempt or original
  topic. Needs no delivery channel; costs one client-side map entry per in-flight record.
- **`InvokeDeliveryError` recovers panics.** The callback runs inside the `range` over `Events()` on
  a single goroutine per producer; an unrecovered panic kills it and the producer then drains
  nothing for the life of the process — silently, since that goroutine was the only thing reporting
  failures. The same reason the callback must not block.
- **No `Retriable` field.** `kafka.Error.IsRetriable()` is only ever set by the transactional
  producer API, so on a delivery report it is always false. `DeliveryError.Code` carries the error
  kind instead, derived from `Code().String()`.

`DeliveryErrorFor` and `InvokeDeliveryError` are exported from `internal/kafka` purely so they are
reachable from `tests/`; `internal/` keeps them out of the public API.

### Testing approach
- `tests/unit/` — pure Go logic, no Kafka dependency (strategy behavior, batch buffer, shutdown logic, options validation, delivery-error mapping)
- `tests/integration/` — full Kafka via testcontainers-go (consumer basics, batch, retry/DLQ, fail-fast, graceful shutdown, rebalancing, reconnection, at-least-once semantics, delivery errors)

**All tests live under `tests/`** — there are no in-package `_test.go` files. Unexported logic is
therefore unreachable from tests, which is why some internals are exported within `internal/`.

To make a write fail deterministically in an integration test, create the target topic with
`max.message.bytes=1` (see `delivery_error_test.go`). The record then passes librdkafka's own
client-side size check — which would fail `Produce` synchronously and never produce a delivery
report — and is rejected by the broker instead, which is the path that generates one. Stopping the
broker does not work: `message.timeout.ms` is unset, so its 300 s default outlives any sane test.

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