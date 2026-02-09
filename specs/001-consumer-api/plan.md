# Implementation Plan: Easy Kafka Consumer Library

**Branch**: `001-consumer-api` | **Date**: 2026-02-09 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `/specs/001-consumer-api/spec.md`

**Note**: This implementation plan follows the speckit workflow for Phase 0 (Research) and Phase 1 (Design & Contracts).

## Summary

Build a Go library that provides a simple, handler-based interface for consuming Kafka messages. Users write minimal handler functions (`func([]byte) error`) with metadata configuration, and the library manages all Kafka complexity including consumer groups, offset commits, error handling strategies (retry, skip, DLQ, fail-fast), and batch processing. The library wraps confluent-kafka-go to provide at-least-once delivery guarantees while hiding protocol details.

## Technical Context

**Language/Version**: Go 1.19+  
**Primary Dependencies**: 
  - `github.com/confluentinc/confluent-kafka-go/v2` (Kafka client, required)
  - `github.com/rs/zerolog` (structured logging)
  - `github.com/golang/mock` (mocking for unit tests)
  - `github.com/testcontainers/testcontainers-go` (integration testing with real Kafka)
  - `github.com/stretchr/testify` (test assertions)

**Storage**: N/A (library manages Kafka offsets only)  
**Testing**: 
  - Unit tests with `go test` + `gomock` for interfaces
  - Integration tests with `testcontainers-go` (no manual Docker Compose)
  - Target: >80% coverage

**Target Platform**: Any Go 1.19+ compatible platform (Linux, macOS, Windows)  
**Project Type**: Single Go library package  
**Performance Goals**: 
  - Single-message: 10,000+ msg/sec throughput
  - Batch mode: 3x throughput improvement over single-message
  - Startup time: <2 seconds to first message
  - Overhead: <5% compared to direct confluent-kafka-go usage

**Constraints**: 
  - At-least-once delivery semantics (no exactly-once in v1)
  - Single topic per consumer instance (no multi-topic in v1)
  - Handler functions must be stateless or manage own state
  - No custom partition assignment strategies in v1

**Scale/Scope**: 
  - Library API: ~10-15 public types/functions (minimal surface)
  - Internal implementation: ~2000-3000 LOC estimated
  - Support production workloads: 100k+ messages/sec with multiple consumer instances
  - Memory-efficient: bounded memory usage regardless of throughput

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

### Principle I: Simplicity First ✅ PASS

- **Check**: Users write `func([]byte) error` handlers only, no Kafka knowledge required
- **Status**: PASS - The plan wraps all Kafka complexity behind minimal handler interface
- **Evidence**: FR-001 through FR-006, FR-047, FR-048 specify handler-only interaction

### Principle II: Metadata-Driven Configuration ✅ PASS

- **Check**: No configuration files or environment variable parsing required
- **Status**: PASS - Configuration via struct tags and functional options only
- **Evidence**: FR-007, FR-008, FR-013 explicitly mandate metadata-driven approach

### Principle III: Minimal Handler Interface ✅ PASS

- **Check**: Handler signatures limited to `func([]byte) error` or batch/context variants
- **Status**: PASS - Exactly 4 handler signatures total (single, batch, each ±context)
- **Evidence**: FR-001 through FR-003 define complete handler signature set

### Principle IV: Strategy Pattern Support ✅ PASS

- **Check**: Pluggable error handling and consumption strategies
- **Status**: PASS - 5 error strategies defined as pluggable implementations
- **Evidence**: FR-019 through FR-023 define strategy interface with fail-fast, skip, retry, DLQ, circuit-breaker

### Principle V: Thin Wrapper Philosophy ✅ PASS

- **Check**: Library wraps confluent-kafka-go without reimplementing protocol
- **Status**: PASS - All Kafka operations delegated to confluent-kafka-go
- **Evidence**: FR-045, FR-046 mandate wrapper-only approach, no protocol reimplementation

### Testing Requirements ✅ PASS

- **Check**: >80% coverage, integration tests with real Kafka, compatibility testing
- **Status**: PASS - Plan includes testcontainers-go for real Kafka integration tests
- **Evidence**: Technical Context specifies testcontainers-go + unit/integration split

### Development Standards ✅ PASS

- **Check**: Semantic versioning, Go doc comments, linting, error handling
- **Status**: PASS - Go 1.19+ with standard tooling (gofmt, golangci-lint implied)
- **Evidence**: Technical Context + Go ecosystem defaults

**GATE RESULT**: ✅ ALL CHECKS PASS - Proceed to Phase 0

---

## Post-Design Constitution Re-Check

*Re-evaluated after Phase 1 (Design & Contracts) completion*

### Principle I: Simplicity First ✅ PASS

- **Design Check**: Public API has 8 functions for consumer creation + 4 handler types
- **Evidence**: [contracts/consumer-api.go](contracts/consumer-api.go) shows minimal surface
- **Verification**: User needs zero Kafka knowledge to use the library

### Principle II: Metadata-Driven Configuration ✅ PASS

- **Design Check**: 15 functional options, no config files required
- **Evidence**: All `WithXxx()` options in contracts file
- **Verification**: Configuration happens at consumer creation via options

### Principle III: Minimal Handler Interface ✅ PASS

- **Design Check**: Exactly 4 handler signatures (single, batch, each ±context)
- **Evidence**: Handler type definitions in contracts
- **Verification**: No Kafka types exposed to handlers

### Principle IV: Strategy Pattern Support ✅ PASS

- **Design Check**: ErrorStrategy interface with 5 implementations
- **Evidence**: [data-model.md](data-model.md) Section 4 defines strategy interface
- **Verification**: Users can implement custom strategies

### Principle V: Thin Wrapper Philosophy ✅ PASS

- **Design Check**: KafkaClient adapter wraps confluent-kafka-go
- **Evidence**: [data-model.md](data-model.md) Section 7 shows adapter pattern
- **Verification**: No Kafka protocol reimplementation

### Testing Requirements ✅ PASS

- **Design Check**: Integration tests with testcontainers-go, unit tests with gomock
- **Evidence**: [research.md](research.md) Section 5 documents testing approach
- **Verification**: Comprehensive test strategy defined

**FINAL GATE RESULT**: ✅ ALL CONSTITUTIONAL CHECKS PASS - Ready for Implementation

## Project Structure

### Documentation (this feature)

```text
specs/001-consumer-api/
├── plan.md              # This file (Phase 0-1 output)
├── research.md          # Phase 0: Technology decisions and best practices
├── data-model.md        # Phase 1: Core entities and relationships
├── quickstart.md        # Phase 1: Developer getting started guide
└── contracts/           # Phase 1: API contracts (Go interface definitions)
    └── consumer-api.go  # Public API contract
```

### Source Code (repository root)

```text
easykafka-go/
├── go.mod
├── go.sum
├── README.md
├── LICENSE
│
├── consumer.go          # Main Consumer type and public API
├── handler.go           # Handler function types and metadata
├── config.go            # Configuration types and builders
├── options.go           # Functional options pattern
│
├── strategy/            # Error handling strategies
│   ├── strategy.go      # ErrorStrategy interface
│   ├── retry.go         # Retry strategy implementation
│   ├── skip.go          # Skip strategy implementation
│   ├── dlq.go           # Dead-letter queue strategy
│   ├── failfast.go      # Fail-fast strategy
│   └── circuit.go       # Circuit-breaker strategy
│
├── internal/            # Internal implementation (not exported)
│   ├── consumer/        # Core consumption logic
│   │   ├── engine.go    # Message polling and dispatch
│   │   ├── batch.go     # Batch accumulation logic
│   │   └── lifecycle.go # Startup/shutdown management
│   ├── commit/          # Offset commit coordination
│   │   └── coordinator.go
│   └── recovery/        # Panic recovery and error handling
│       └── recovery.go
│
├── examples/            # Usage examples
│   ├── simple/          # Basic single-message consumer
│   ├── batch/           # Batch processing consumer
│   ├── retry/           # Retry strategy example
│   └── dlq/             # Dead-letter queue example
│
└── tests/               # Test organization
    ├── unit/            # Unit tests (co-located with source via _test.go)
    ├── integration/     # Integration tests with testcontainers
    │   ├── single_test.go
    │   ├── batch_test.go
    │   ├── strategies_test.go
    │   └── rebalance_test.go
    └── mocks/           # Generated mocks (gomock)
        └── mock_strategy.go
```

**Structure Decision**: Single Go library structure. This is a library package, not an application, so we use a flat structure for public API types (consumer.go, handler.go, config.go) with internal implementation details in `internal/` packages. The `strategy/` package is exported to allow users to implement custom strategies if needed. Test organization follows Go conventions with unit tests co-located (`*_test.go`) and integration tests in a separate directory.

**Rationale**: 
- Flat top-level structure keeps public API discoverable
- `internal/` prevents users from depending on implementation details
- `strategy/` package exportable for extensibility (Constitutional Principle IV)
- Examples provide copy-paste starting points (Constitutional testing requirements)
- Integration tests separated to allow running with `go test -tags=integration`

## Complexity Tracking

> **Fill ONLY if Constitution Check has violations that must be justified**

**Status**: No constitutional violations detected. All design decisions align with the five core principles:
1. Simplicity First - Minimal handler interface
2. Metadata-Driven - No config files
3. Minimal Interface - Four handler signatures total
4. Strategy Pattern - Pluggable error strategies
5. Thin Wrapper - Delegates to confluent-kafka-go

No complexity justification required.
