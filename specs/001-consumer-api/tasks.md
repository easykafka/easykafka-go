---

description: "Task list for Easy Kafka Consumer Library"
---

# Tasks: Easy Kafka Consumer Library

**Input**: Design documents from `/specs/001-consumer-api/`
**Prerequisites**: plan.md (required), spec.md (required for user stories), research.md, data-model.md, contracts/

**Tests**: Included because the specification requires comprehensive unit/integration coverage (SC-008).

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Project initialization and base structure

- [x] T001 Create consumer entrypoint stub in consumer.go
- [x] T002 [P] Create options scaffolding and config placeholder in options.go
- [x] T003 [P] Create handler type stubs in handler.go
- [x] T004 [P] Create ErrorStrategy interface stub in strategy/strategy.go
- [x] T005 [P] Create engine skeleton in internal/engine/engine.go
- [x] T006 [P] Create Kafka adapter skeleton in internal/kafka/adapter.go
- [x] T007 [P] Create metadata context helpers stub in internal/metadata/context.go
- [x] T008 [P] Create tests/README.md with test organization structure (supports Constitution mandate SC-008: comprehensive unit/integration test coverage). Document testing pyramid: unit tests in tests/unit/, integration tests in tests/integration/, test helpers in tests/integration/helpers/. Include setup instructions for testcontainers-go and Kafka test cluster initialization strategy.
- [x] T009 Create/update go.mod with required dependencies for all phases (enables FR-046: wrap confluent-kafka-go, and supports Constitution testing mandate). Add core dependencies: confluent-kafka-go/v2 (Kafka client), testify (assertions/mocking), testcontainers-go (integration test infrastructure), zerolog (structured logging). Document version constraints and rationale.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Core infrastructure that MUST be complete before ANY user story can be implemented

- [x] T010 Implement Config struct, defaults, and option validation in options.go
- [x] T011 Implement Consumer struct and New() wiring in consumer.go
- [x] T012 [P] Implement internal Message model and context injection in internal/engine/message.go
- [x] T013 [P] Implement MessageFromContext helper in internal/metadata/context.go
- [x] T014 [P] Implement Kafka adapter interface + confluent wrapper in internal/kafka/adapter.go
- [x] T015 Implement engine run loop scaffold (poll, dispatch hooks, stop signals) in internal/engine/engine.go
- [x] T016 [P] Implement default no-op logger in internal/metadata/logger.go

**Checkpoint**: Foundation ready - user story implementation can now begin

---

## Phase 3: User Story 1 - Simple Handler Registration (Priority: P1) 🎯 MVP

**Goal**: Basic consumer can run a single-message handler, commit offsets, and handle rebalancing.

**Independent Test**: Start consumer with handler, produce messages, verify handler receives payloads and commits offsets.

### Tests for User Story 1

- [x] T017 [P] [US1] Add unit test for engine dispatch/commit flow in tests/unit/engine_dispatch_test.go
- [x] T018 [P] [US1] Add integration test for basic consumption in tests/integration/consumer_basic_test.go

### Implementation for User Story 1

- [x] T019 [US1] Implement Start() to run engine and poll loop in consumer.go
- [x] T020 [US1] Implement handler invocation + panic recovery in internal/engine/engine.go
- [x] T021 [US1] Implement explicit offset commits on handler success in internal/kafka/adapter.go
- [x] T022 [US1] Implement rebalance handling (assign/revoke) in internal/kafka/adapter.go

---

## Phase 4: User Story 2 - Metadata-Driven Configuration (Priority: P1)

**Goal**: Configure consumer via functional options with validation and passthrough config.

**Independent Test**: Create consumers with valid/invalid options and verify validation and config passthrough.

### Tests for User Story 2

- [x] T023 [P] [US2] Add unit tests for option validation in tests/unit/options_validation_test.go
- [x] T024 [P] [US2] Add integration test for KafkaConfig passthrough in tests/integration/config_passthrough_test.go

### Implementation for User Story 2

- [x] T025 [US2] Implement required/optional options and validation errors in options.go
- [x] T026 [US2] Wire KafkaConfig passthrough into adapter config in internal/kafka/adapter.go
- [x] T027 [US2] Add consumer config defaults (poll/shutdown/logging) in options.go

---

## Phase 5: User Story 3 - Pluggable Error Handling Strategies (Priority: P1)

**Goal**: Provide fail-fast, skip, retry+DLQ, and circuit-breaker strategies.

**Independent Test**: Force handler failures and verify each strategy behavior (stop, skip, retry/DLQ, pause/resume).

### Tests for User Story 3

- [x] T028 [P] [US3] Add unit tests for fail-fast and skip strategies in tests/unit/strategy_basic_test.go
- [x] T029 [P] [US3] Add unit tests for retry headers/backoff in tests/unit/strategy_retry_test.go
- [x] T030 [P] [US3] Add integration test for retry + DLQ flow in tests/integration/retry_dlq_test.go
- [x] T031 [P] [US3] Add integration test for circuit breaker pause/resume in tests/integration/circuit_breaker_test.go

### Implementation for User Story 3

- [x] T032 [US3] Implement fail-fast and skip strategies in strategy/skip_failfast.go
- [x] T033 [US3] Implement retry strategy with retry/DLQ producers in strategy/retry.go
- [x] T034 [US3] Implement retry header encoding helpers in internal/metadata/headers.go
- [x] T035 [US3] Implement internal retry consumer for retry topic in internal/kafka/retry_consumer.go
- [x] T036 [US3] Implement circuit-breaker strategy state machine in strategy/circuit_breaker.go

---

## Phase 6: User Story 4 - Batch Processing for High Throughput (Priority: P2)

**Goal**: Provide batch handler support with size/timeout triggers and atomic commit.

**Independent Test**: Send multiple messages and verify batch size/timeout delivery with atomic offset commits.

### Tests for User Story 4

- [x] T037 [P] [US4] Add unit tests for batch buffer behavior in tests/unit/batch_buffer_test.go
- [x] T038 [P] [US4] Add integration test for batch size/timeout in tests/integration/batch_processing_test.go

### Implementation for User Story 4

- [x] T039 [US4] Implement batch buffer and timers in internal/engine/batch.go
- [x] T040 [US4] Wire batch mode execution and atomic commit in internal/engine/engine.go

---

## Phase 7: User Story 5 - Graceful Shutdown and Context Management (Priority: P2)

**Goal**: Support context cancellation, in-flight completion, and timeout-based shutdown.

**Independent Test**: Cancel context and verify stop-fetching, handler completion, and timeout behavior.

### Tests for User Story 5

- [x] T041 [P] [US5] Add unit tests for shutdown timeout logic in tests/unit/shutdown_test.go
- [x] T042 [P] [US5] Add integration test for graceful shutdown in tests/integration/graceful_shutdown_test.go

### Implementation for User Story 5

- [x] T043 [US5] Implement Shutdown() orchestration in consumer.go
- [x] T044 [US5] Implement engine shutdown coordination and context cancellation in internal/engine/shutdown.go

---

## Phase 8: Cross-Cutting Concerns & Requirements Coverage

**Purpose**: Implement connectivity, observability, and semantic guarantees across all modes

### FR-042: Automatic Reconnection on Broker Unavailability

- [x] T045 Add integration test for broker failure/recovery scenario in tests/integration/reconnection_test.go
- [x] T046 Implement reconnection backoff and retry logic in internal/kafka/adapter.go (wraps confluent-kafka-go's built-in reconnection)

### FR-043: At-Least-Once Delivery Validation

- [x] T047 Add end-to-end test verifying at-least-once semantics (messages may repeat, never lost) in tests/integration/at_least_once_test.go
- [x] T048 Add test cases for rebalance scenarios ensuring duplicate processing is acceptable in tests/integration/rebalance_test.go

### FR-044: Offset Commit Protection for Failed Messages

- [x] T049 Add unit tests for offset commit guard logic in tests/unit/offset_manager_test.go
- [x] T050 Implement offset commit prevention for failures (except skip strategy) in internal/engine/offset_manager.go

NOTE: we don't need this to be implemented. Error strategy commits the offsets if error handler succeeds. If error handler fails, engine stops.

### FR-045: Lifecycle & Error Logging

- [x] T051 [P] Add structured logging for startup/shutdown/rebalance events in consumer.go using zerolog
- [x] T052 [P] Add detailed error logging in strategy implementations (retry exhaustion, circuit breaker state changes) in strategy/*.go
- [x] T053 [P] Wire logger configuration option throughout consumer and engine in internal/engine/engine.go

### Phase 8 Polish Tasks

- [x] T054 [P] Add package-level documentation and examples in consumer.go
- [x] T055 [P] Update README with quickstart and strategy overview in README.md
- [x] T056 [P] Add integration test helper for Kafka container reuse in tests/integration/kafka_test_helper.go

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies - can start immediately
- **Foundational (Phase 2)**: Depends on Setup completion - BLOCKS all user stories
- **User Stories (Phase 3+)**: All depend on Foundational phase completion
- **Cross-Cutting Concerns (Phase 8a)**: Depends on Foundational + at least one user story for context
- **Polish (Phase 8b)**: Depends on all desired user stories being complete

### User Story Dependencies

- **US1 (P1)**: No dependencies beyond Foundational
- **US2 (P1)**: No dependencies beyond Foundational
- **US3 (P1)**: Depends on US1 for handler execution and commit flow
- **US4 (P2)**: Depends on US1 engine loop; integrates with US3 for error handling
- **US5 (P2)**: Depends on US1 engine lifecycle

### Cross-Cutting Requirement Coverage

- **FR-042 (Reconnection)**: T045-T046 test and implement broker reconnection via adapter
- **FR-043 (At-Least-Once)**: T047-T048 validate redelivery semantics across rebalance scenarios
- **FR-044 (Offset Guard)**: T049-T050 prevent offset commits on handler failure (except skip)
- **FR-045 (Lifecycle Logging)**: T051-T053 add structured logging across consumer, strategies, and engine

### Parallel Execution Examples

**US1**
- T017 and T018 can run in parallel (unit + integration tests)
- T020 and T021 can run in parallel (engine handler logic vs adapter commits)

**US2**
- T023 and T024 can run in parallel (unit + integration tests)
- T025 and T026 can run in parallel (options vs adapter config wiring)

**US3**
- T028, T029, T030, and T031 can run in parallel (tests)
- T032 and T034 can run in parallel (strategy vs header helpers)

**US4**
- T037 and T038 can run in parallel (unit + integration tests)
- T039 and T040 are sequential (buffer then engine wiring)

**US5**
- T041 and T042 can run in parallel (unit + integration tests)
- T043 and T044 are sequential (consumer shutdown then engine coordination)

**Cross-Cutting Concerns (Phase 8a)**
- T045, T047, T049, T051, T052, T053 can run in parallel (independent feature implementations)
- T046 and T050 can run in parallel (offset manager vs adapter reconnection logic)

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1: Setup
2. Complete Phase 2: Foundational
3. Complete Phase 3: User Story 1
4. Validate with T017 and T018 before proceeding

### Multi-Story Delivery

1. Deliver US1 + FR-043 validation (basic consumption + at-least-once guarantees)
2. Deliver US2 (configuration and validation)
3. Deliver US3 + FR-042/044/045 (error strategies + reconnection, offset guard, logging)
4. Deliver US4 (batch mode)
5. Deliver US5 (graceful shutdown)
6. Deliver Polish (documentation and test helpers)

---

## Notes

- **Total Tasks**: 56 implementation tasks across 8 phases
- **Requirement Coverage**: All 49 FRs now mapped to tasks (100% coverage)
  - User Story tasks: T001-T044
  - Cross-cutting requirements: T045-T053 (FR-042, 043, 044, 045)
  - Polish tasks: T054-T056
- Tasks are ordered by dependency; IDs reflect execution order.
- [P] tasks target separate files and can be parallelized safely.
- Each user story is independently testable via its integration test(s).
- Phase 8a (requirements) can overlap with phase 7 (last user story) for efficiency.

