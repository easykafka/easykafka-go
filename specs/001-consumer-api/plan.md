# Implementation Plan: Easy Kafka Consumer Library

**Branch**: `001-consumer-api` | **Date**: 2026-02-17 | **Spec**: [specs/001-consumer-api/spec.md](specs/001-consumer-api/spec.md)
**Input**: Feature specification from [specs/001-consumer-api/spec.md](specs/001-consumer-api/spec.md)

## Summary

Deliver a Go library that wraps `confluent-kafka-go` to provide a minimal handler-based consumer API with metadata-driven configuration, pluggable error strategies (retry + DLQ + circuit breaker), batch mode, and graceful shutdown. The technical approach uses explicit offset commits after handler success, a Kafka retry topic with an internal retry consumer, and circuit-breaker state machine to pause/resume consumption.

## Technical Context

**Language/Version**: Go 1.19+  
**Primary Dependencies**: `confluent-kafka-go` v2.x, Go stdlib  
**Storage**: N/A  
**Testing**: `go test`, `testify`, `testcontainers-go` (integration)  
**Target Platform**: Linux/macOS/Windows (where `librdkafka` is supported)  
**Project Type**: Single Go library  
**Performance Goals**: 10k msg/s single-message throughput; batch mode 3x throughput  
**Constraints**: <5% overhead vs direct `confluent-kafka-go`; at-least-once semantics  
**Scale/Scope**: Single-topic consumer per instance; one library module

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

- **Simplicity First**: PASS. Public API is handler + functional options; Kafka internals hidden.
- **Metadata-Driven Configuration**: PASS. Functional options only; no config files/env parsing.
- **Minimal Handler Interface**: PASS. `func(context.Context, []byte) error` and batch equivalent only.
- **Strategy Pattern Support**: PASS. Error strategies are pluggable via interface and options.
- **Thin Wrapper Philosophy**: PASS. All Kafka operations delegated to `confluent-kafka-go`.

## Project Structure

### Documentation (this feature)

```text
specs/001-consumer-api/
├── plan.md              # This file (/speckit.plan command output)
├── research.md          # Phase 0 output (/speckit.plan command)
├── data-model.md        # Phase 1 output (/speckit.plan command)
├── quickstart.md        # Phase 1 output (/speckit.plan command)
├── contracts/           # Phase 1 output (/speckit.plan command)
└── tasks.md             # Phase 2 output (/speckit.tasks command - NOT created by /speckit.plan)
```

### Source Code (repository root)

```text
.
├── consumer.go            # Public API (New, Consumer interface)
├── options.go             # Functional options and validation
├── handler.go             # Handler and batch handler types
├── strategy/
│   ├── strategy.go         # ErrorStrategy interface
│   ├── retry.go            # Retry + DLQ strategy
│   ├── circuit_breaker.go  # Circuit breaker strategy
│   └── skip_failfast.go    # Skip and fail-fast strategies
├── internal/
│   ├── engine/             # Poll loop, dispatching, batch assembly
│   ├── kafka/              # Confluent adapter and retry consumer
│   └── metadata/           # Context helpers and header encoding
└── tests/
    ├── unit/
    └── integration/
```

**Structure Decision**: Single Go library at repo root with internal subpackages for poll loop, Kafka adapter, and metadata encoding. Public API stays in root package.

## Constitution Check (Post-Design)

- **Simplicity First**: PASS. Contracts expose only handler + options.
- **Metadata-Driven Configuration**: PASS. No configuration files or env parsing.
- **Minimal Handler Interface**: PASS. Context + payload only.
- **Strategy Pattern Support**: PASS. Error strategies remain pluggable.
- **Thin Wrapper Philosophy**: PASS. Kafka interactions remain in adapter layer.

## Complexity Tracking

None.
