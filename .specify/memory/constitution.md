<!--
SYNC IMPACT REPORT - Constitution v1.0.0
========================================
Version Change: Initial creation → 1.0.0
Rationale: Initial constitution for EasyKafka Go library

Principles Established:
  1. Simplicity First - Hide all Kafka technical complexity
  2. Metadata-Driven Configuration - All config via struct tags
  3. Minimal Handler Interface - Simple func([]byte) error signature
  4. Strategy Pattern Support - Pluggable consumption/error strategies
  5. Thin Wrapper Philosophy - Leverage confluent-kafka-go effectively

Sections Added:
  - Core Principles (5 principles)
  - Testing Requirements
  - Development Standards
  - Governance

Templates Status:
  ✅ plan-template.md - Reviewed, no updates needed (generic structure compatible)
  ✅ spec-template.md - Reviewed, no updates needed (user story format compatible)
  ✅ tasks-template.md - Reviewed, no updates needed (test-first approach compatible)

Follow-up TODOs:
  - None: All placeholders filled based on project requirements
  - Future: Add performance benchmarking requirements as library matures

Commit Message:
  docs: establish constitution v1.0.0 for EasyKafka Go library
-->

# EasyKafka Go Constitution

## Core Principles

### I. Simplicity First (NON-NEGOTIABLE)

The library MUST hide ALL technical complexity of Kafka consumption from application developers. Users write simple message handlers with signature `func([]byte) error` or `func([][]byte) error` for batch processing. The library handles:

- Connection management and health monitoring
- Consumer group coordination and rebalancing
- Offset management (commit/rollback strategies)
- Partition assignment and distribution
- Deserialization complexity (users receive raw bytes)
- Error recovery and retry mechanisms
- Graceful shutdown and cleanup

**Rationale**: Application developers should focus on business logic, not Kafka internals. If a user needs to understand consumer groups, offsets, or rebalancing to use this library, we have failed this principle.

**Test**: Any feature requiring users to reference Kafka documentation violates this principle.

---

### II. Metadata-Driven Configuration

ALL configuration MUST happen through Go struct tags, function options, or declarative metadata. NO configuration files, NO environment variable parsing in application code, NO builder patterns that expose Kafka-specific terminology.

Configuration categories:
- **Consumer behavior**: struct tags like `kafka:"topic=orders,consumer-group=processors"`
- **Error handling**: function options like `WithRetryStrategy(ExponentialBackoff(3))`
- **Consumption mode**: method selection (`Consume()` vs `ConsumeBatch()`)
- **Observability**: hooks and callbacks via functional options

**Rationale**: Metadata-driven design keeps configuration co-located with handlers, enables compile-time validation where possible, and maintains the simplicity principle by avoiding scattered configuration.

**Test**: If configuration requires more than one location (struct definition + function call), justify or refactor.

---

### III. Minimal Handler Interface

Handler signatures MUST remain minimal and idiomatic Go:

- Single message: `func([]byte) error`
- Batch: `func([][]byte) error`
- Optional context: `func(context.Context, []byte) error`

Handlers return `error` for failure cases. Library interprets errors according to configured error handling strategy (retry, skip, dead-letter, fail-fast).

**Extension points** (optional, via functional options):
- Message metadata access (headers, offset, timestamp) via context values
- Custom deserialization via middleware/interceptors

**Rationale**: Small interfaces are easy to implement, test, and compose. Generic `func([]byte) error` can be satisfied by any processing logic without tight coupling.

**Test**: Any handler requiring more than context + payload violates this principle.

---

### IV. Strategy Pattern Support

The library MUST support pluggable strategies for:

**Consumption Strategies**:
- Single-message processing (default)
- Batch processing with configurable batch size

**Error Handling Strategies**:
- Fail-fast: Stop on first error
- Skip: Log error and continue
- Retry: Fixed, exponential, or custom backoff
- Dead-letter: Route failures to DLQ topic
- Circuit-breaker: Pause consumption after threshold

Strategies are selected via functional options at consumer creation time, NOT hardcoded.

**Rationale**: Different use cases require different trade-offs (throughput vs. latency vs. reliability). Strategies enable users to adapt behavior without forking the library.

**Test**: Adding a new strategy should require implementing an interface, not modifying core logic.

---

### V. Thin Wrapper Philosophy

This library is a wrapper around [confluent-kafka-go](https://github.com/confluentinc/confluent-kafka-go), NOT a reimplementation. We MUST:

- Use confluent-kafka-go for all actual Kafka protocol operations
- Expose confluent-kafka-go configuration where needed (via passthrough options)
- Avoid duplicating features that confluent-kafka-go provides well
- Focus on ergonomics, not reinventing low-level primitives

**What we add**:
- Simplified API surface
- Metadata-driven configuration
- Strategy pattern implementations
- Observability hooks

**What we don't add**:
- Custom protocol implementations
- Alternative serialization formats (users work with `[]byte`)
- Kafka cluster management features

**Rationale**: Leveraging confluent-kafka-go ensures compatibility, performance, and maintainability. Our value is in the developer experience layer, not protocol handling.

**Test**: Any change that reimplements confluent-kafka-go functionality violates this principle.

---

## Testing Requirements

### Unit Testing

- **Coverage target**: >80% for core library logic (consumer lifecycle, strategy implementations, configuration parsing)
- **Mock Kafka**: Use interfaces and mocks for Kafka interactions in unit tests
- **Handler testing**: Provide test utilities for users to test their handlers in isolation

### Integration Testing

- **Test Kafka cluster**: Integration tests MUST run against a real Kafka cluster (testcontainers)
- **Scenarios**:
  - Consumer lifecycle: Start, consume, rebalance, shutdown
  - Error handling: Test each strategy with actual failures
  - Batch processing: Verify batch size/timeout behavior
  - Offset management: Verify commit/rollback semantics
- **No mocks**: Integration tests use real confluent-kafka-go and actual Kafka topics

### Compatibility Testing

- Test against multiple Kafka versions (at least 2.x and 3.x)
- Test with different consumer group configurations
- Performance benchmarks for throughput and latency

**Rationale**: As a wrapper library, correctness and compatibility are paramount. Unit tests verify logic, integration tests verify Kafka interaction semantics.

---

## Development Standards

### Dependencies

- **Primary**: `github.com/confluentinc/confluent-kafka-go` (required)
- **Testing**: `github.com/stretchr/testify`, `github.com/testcontainers/testcontainers-go` (dev only)
- **Minimize external dependencies**: Avoid unnecessary transitive dependencies

### API Stability

- **Versioning**: Follow semantic versioning (MAJOR.MINOR.PATCH)
- **Breaking changes**: Require MAJOR version bump and migration guide
- **Deprecation policy**: Mark deprecated APIs with `// Deprecated:` comments and provide alternatives; maintain for at least one MINOR version

### Documentation

- **Package-level**: Comprehensive package documentation with usage examples
- **Function-level**: Every exported function/type documented with Go doc comments
- **Examples**: Runnable examples in `example_test.go` files
- **README**: Quick start guide with common patterns

### Code Quality

- **Linting**: `golangci-lint` with strict settings enabled
- **Formatting**: `gofmt` and `goimports` enforced
- **Error handling**: Always return errors, never `panic()` in library code
- **Context propagation**: Accept `context.Context` where cancellation/timeout is needed

---

## Governance

This constitution supersedes all other development practices and guides for the EasyKafka Go project.

**Amendment Process**:
1. Propose change via GitHub issue with rationale
2. Discuss impact on existing code and user contracts
3. Update constitution with version bump (see versioning rules below)
4. Update dependent templates and documentation
5. Communicate changes to users (breaking changes require migration guide)

**Version Bump Rules**:
- **MAJOR**: Principle removal, redefinition, or new principle that invalidates existing code
- **MINOR**: New principle/section added or expanded guidance
- **PATCH**: Clarifications, wording improvements, non-semantic fixes

**Compliance**:
- All pull requests MUST align with constitution principles
- Code reviews MUST verify principle adherence
- Violations require either code changes OR justified principle amendment

**Review Cadence**: Constitution reviewed quarterly or when significant feature additions are planned.

**Version**: 1.0.0 | **Ratified**: 2026-02-07 | **Last Amended**: 2026-02-07
