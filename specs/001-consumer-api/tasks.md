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

- [ ] T001 Create consumer entrypoint stub in consumer.go
- [ ] T002 [P] Create options scaffolding and config placeholder in options.go
- [ ] T003 [P] Create handler type stubs in handler.go
- [ ] T004 [P] Create ErrorStrategy interface stub in strategy/strategy.go
- [ ] T005 [P] Create engine skeleton in internal/engine/engine.go
- [ ] T006 [P] Create Kafka adapter skeleton in internal/kafka/adapter.go
- [ ] T007 [P] Create metadata context helpers stub in internal/metadata/context.go
- [ ] T008 [P] Add tests directory marker in tests/README.md
- [ ] T009 Add required dependencies and tooling notes in go.mod

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Core infrastructure that MUST be complete before ANY user story can be implemented

- [ ] T010 Implement Config struct, defaults, and option validation in options.go
- [ ] T011 Implement Consumer struct and New() wiring in consumer.go
- [ ] T012 [P] Implement internal Message model and context injection in internal/engine/message.go
- [ ] T013 [P] Implement MessageFromContext helper in internal/metadata/context.go
- [ ] T014 [P] Implement Kafka adapter interface + confluent wrapper in internal/kafka/adapter.go
- [ ] T015 Implement engine run loop scaffold (poll, dispatch hooks, stop signals) in internal/engine/engine.go
- [ ] T016 [P] Implement default no-op logger in internal/metadata/logger.go

**Checkpoint**: Foundation ready - user story implementation can now begin

---

## Phase 3: User Story 1 - Simple Handler Registration (Priority: P1) 🎯 MVP

**Goal**: Basic consumer can run a single-message handler, commit offsets, and handle rebalancing.

**Independent Test**: Start consumer with handler, produce messages, verify handler receives payloads and commits offsets.

### Tests for User Story 1

- [ ] T017 [P] [US1] Add unit test for engine dispatch/commit flow in tests/unit/engine_dispatch_test.go
- [ ] T018 [P] [US1] Add integration test for basic consumption in tests/integration/consumer_basic_test.go

### Implementation for User Story 1

- [ ] T019 [US1] Implement Start() to run engine and poll loop in consumer.go
- [ ] T020 [US1] Implement handler invocation + panic recovery in internal/engine/engine.go
- [ ] T021 [US1] Implement explicit offset commits on handler success in internal/kafka/adapter.go
- [ ] T022 [US1] Implement rebalance handling (assign/revoke) in internal/kafka/adapter.go

---

## Phase 4: User Story 2 - Metadata-Driven Configuration (Priority: P1)

**Goal**: Configure consumer via functional options with validation and passthrough config.

**Independent Test**: Create consumers with valid/invalid options and verify validation and config passthrough.

### Tests for User Story 2

- [ ] T023 [P] [US2] Add unit tests for option validation in tests/unit/options_validation_test.go
- [ ] T024 [P] [US2] Add integration test for KafkaConfig passthrough in tests/integration/config_passthrough_test.go

### Implementation for User Story 2

- [ ] T025 [US2] Implement required/optional options and validation errors in options.go
- [ ] T026 [US2] Wire KafkaConfig passthrough into adapter config in internal/kafka/adapter.go
- [ ] T027 [US2] Add consumer config defaults (poll/shutdown/logging) in options.go

---

## Phase 5: User Story 3 - Pluggable Error Handling Strategies (Priority: P1)

**Goal**: Provide fail-fast, skip, retry+DLQ, and circuit-breaker strategies.

**Independent Test**: Force handler failures and verify each strategy behavior (stop, skip, retry/DLQ, pause/resume).

### Tests for User Story 3

- [ ] T028 [P] [US3] Add unit tests for fail-fast and skip strategies in tests/unit/strategy_basic_test.go
- [ ] T029 [P] [US3] Add unit tests for retry headers/backoff in tests/unit/strategy_retry_test.go
- [ ] T030 [P] [US3] Add integration test for retry + DLQ flow in tests/integration/retry_dlq_test.go
- [ ] T031 [P] [US3] Add integration test for circuit breaker pause/resume in tests/integration/circuit_breaker_test.go

### Implementation for User Story 3

- [ ] T032 [US3] Implement fail-fast and skip strategies in strategy/skip_failfast.go
- [ ] T033 [US3] Implement retry strategy with retry/DLQ producers in strategy/retry.go
- [ ] T034 [US3] Implement retry header encoding helpers in internal/metadata/headers.go
- [ ] T035 [US3] Implement internal retry consumer for retry topic in internal/kafka/retry_consumer.go
- [ ] T036 [US3] Implement circuit-breaker strategy state machine in strategy/circuit_breaker.go

---

## Phase 6: User Story 4 - Batch Processing for High Throughput (Priority: P2)

**Goal**: Provide batch handler support with size/timeout triggers and atomic commit.

**Independent Test**: Send multiple messages and verify batch size/timeout delivery with atomic offset commits.

### Tests for User Story 4

- [ ] T037 [P] [US4] Add unit tests for batch buffer behavior in tests/unit/batch_buffer_test.go
- [ ] T038 [P] [US4] Add integration test for batch size/timeout in tests/integration/batch_processing_test.go

### Implementation for User Story 4

- [ ] T039 [US4] Implement batch buffer and timers in internal/engine/batch.go
- [ ] T040 [US4] Wire batch mode execution and atomic commit in internal/engine/engine.go

---

## Phase 7: User Story 5 - Graceful Shutdown and Context Management (Priority: P2)

**Goal**: Support context cancellation, in-flight completion, and timeout-based shutdown.

**Independent Test**: Cancel context and verify stop-fetching, handler completion, and timeout behavior.

### Tests for User Story 5

- [ ] T041 [P] [US5] Add unit tests for shutdown timeout logic in tests/unit/shutdown_test.go
- [ ] T042 [P] [US5] Add integration test for graceful shutdown in tests/integration/graceful_shutdown_test.go

### Implementation for User Story 5

- [ ] T043 [US5] Implement Shutdown() orchestration in consumer.go
- [ ] T044 [US5] Implement engine shutdown coordination and context cancellation in internal/engine/shutdown.go

---

## Phase 8: Polish & Cross-Cutting Concerns

**Purpose**: Improvements that affect multiple user stories

- [ ] T045 [P] Add package-level documentation and examples in consumer.go
- [ ] T046 [P] Update README with quickstart and strategy overview in README.md
- [ ] T047 [P] Add integration test helper for Kafka container reuse in tests/integration/kafka_test_helper.go

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies - can start immediately
- **Foundational (Phase 2)**: Depends on Setup completion - BLOCKS all user stories
- **User Stories (Phase 3+)**: All depend on Foundational phase completion
- **Polish (Final Phase)**: Depends on all desired user stories being complete

### User Story Dependencies

- **US1 (P1)**: No dependencies beyond Foundational
- **US2 (P1)**: No dependencies beyond Foundational
- **US3 (P1)**: Depends on US1 for handler execution and commit flow
- **US4 (P2)**: Depends on US1 engine loop; integrates with US3 for error handling
- **US5 (P2)**: Depends on US1 engine lifecycle

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

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1: Setup
2. Complete Phase 2: Foundational
3. Complete Phase 3: User Story 1
4. Validate with T017 and T018 before proceeding

### Incremental Delivery

1. Deliver US1 (basic consumption)
2. Deliver US2 (configuration and validation)
3. Deliver US3 (error strategies)
4. Deliver US4 (batch mode)
5. Deliver US5 (graceful shutdown)

---

## Notes

- Tasks are ordered by dependency; IDs reflect execution order.
- [P] tasks target separate files and can be parallelized safely.
- Each user story is independently testable via its integration test(s).
