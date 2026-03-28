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
gotestsum --format testdox -- -count=1 -timeout 600s ./tests/integration/...

# Coverage
go test -count=1 -timeout 1000s -coverprofile=coverage.out -covermode=atomic ./tests/...

# Single test by name
go test -v -count=1 -run TestName ./tests/unit/...
```

### Build
```bash
go build ./...
go mod download
```

## Architecture

### Public API (root package)
- `consumer.go` — `Consumer` interface + lifecycle management: `Created → Running → ShuttingDown → Stopped`
- `options.go` — all configuration via functional options (`WithTopic`, `WithBrokers`, `WithHandler`, etc.)
- `handler.go` — re-exports types from internal packages for public consumption

### Internal packages
- `internal/engine/` — core polling loop (`engine.go`) and batch accumulation (`batch.go`). Supports both single-message and batch modes.
- `internal/kafka/` — wraps confluent-kafka-go consumer (`adapter.go`) and produces to retry/DLQ topics (`producer.go`)
- `internal/types/` — core interfaces: `Handler`, `BatchHandler`, `ErrorStrategy`, `Message`, `Initializable`, `LoggerAware`
- `internal/metadata/` — message metadata via context decorators and header parsing

### Error strategies (`strategy/` package — public)
Pluggable via `WithErrorStrategy()`. Four implementations:
- `skip.go` — logs error and continues (default)
- `fail_fast.go` — stops consumer immediately on first error
- `retry.go` — Kafka-based retry with exponential backoff, optional DLQ routing
- `circuit_breaker.go` — wraps retry with pause/resume on consecutive failures (**experimental**)

### Testing approach
- `tests/unit/` — pure Go logic, no Kafka dependency (strategy behavior, batch buffer, shutdown logic, options validation)
- `tests/integration/` — full Kafka via testcontainers-go (consumer basics, batch, retry/DLQ, circuit breaker, graceful shutdown, rebalancing, reconnection, at-least-once semantics)

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