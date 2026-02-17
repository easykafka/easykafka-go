# Easy Kafka Consumer Library - Test Organization

## Overview

This directory contains comprehensive unit and integration tests for the Easy Kafka Consumer Library, organized following the Testing Pyramid with emphasis on realistic Kafka integration scenarios.

## Test Organization

```
tests/
├── unit/                 # Unit tests (~70% of tests)
│   ├── engine_dispatch_test.go
│   ├── options_validation_test.go
│   ├── strategy_basic_test.go
│   ├── strategy_retry_test.go
│   ├── batch_buffer_test.go
│   ├── shutdown_test.go
│   └── offset_manager_test.go
├── integration/          # Integration tests (~30% of tests)
│   ├── consumer_basic_test.go
│   ├── config_passthrough_test.go
│   ├── retry_dlq_test.go
│   ├── circuit_breaker_test.go
│   ├── batch_processing_test.go
│   ├── graceful_shutdown_test.go
│   ├── reconnection_test.go
│   ├── at_least_once_test.go
│   ├── rebalance_test.go
│   ├── kafka_test_helper.go
│   └── helpers/
│       └── testcontainers.go
```

## Unit Tests

Unit tests focus on individual components in isolation with mocked dependencies.

### Running Unit Tests

```bash
go test -v ./tests/unit -run "^TestUnit"
```

### Coverage

```bash
go test -cover ./tests/unit
```

## Integration Tests

Integration tests use `testcontainers-go` to spin up a real Kafka instance and verify end-to-end behavior.

### Prerequisites

- Docker (for Kafka container)
- Docker daemon running

### Running Integration Tests

```bash
go test -v ./tests/integration -run "^TestIntegration" -timeout 5m
```

### Test Isolation

- Each integration test uses unique topic names to avoid cross-test interference
- Kafka container is reused across tests for efficiency (via TestMain)
- Topics are created fresh for each test scenario

### Container Strategy

The test suite follows this container lifecycle pattern:

1. **TestMain**: Initializes a single Kafka container before all tests
2. **Test Helpers**: Provide topic creation, message production, consumption verification
3. **Cleanup**: Container stops after all integration tests complete

Example:

```go
var kafkaContainer testcontainers.Container
var brokerAddr string

func TestMain(m *testing.M) {
    ctx := context.Background()
    // Create container once  
    kafkaContainer, brokerAddr, err := setupKafkaContainer(ctx)
    if err != nil {
        log.Fatalf("failed to setup kafka: %v", err)
    }
    
    code := m.Run()
    
    // Cleanup
    kafkaContainer.Terminate(ctx)
    os.Exit(code)
}
```

## Test Helpers

### Kafka Test Helper (`kafka_test_helper.go`)

Provides utilities for integration tests:

- `setupKafkaContainer(ctx)`: Creates and configures a Kafka testcontainer
- `createTopic(ctx, brokerAddr, topic)`: Creates a Kafka topic
- `producMessages(ctx, brokerAddr, topic, messages)`: Produces messages to a topic
- `consumeMessages(ctx, brokerAddr, topic, groupID)`: Consumes and verifies messages

### Container Helpers (`helpers/testcontainers.go`)

Low-level testcontainers utilities:

- Image configuration with proper versions
- Readiness checks and container probes
- Network configuration for Docker environments

## Execution Requirements

### Supported Platforms

- Linux (primary)
- macOS (requires Docker Desktop)
- Windows (requires WSL2 + Docker Desktop)

### System Requirements

- Minimum 2GB RAM for Kafka container
- Docker socket access (`/var/run/docker.sock`)

## Test Naming Convention

- Unit tests: `TestUnit<Component><Scenario>` (e.g., `TestUnitEngineDispatch`)
- Integration tests: `TestIntegration<Feature><Scenario>` (e.g., `TestIntegrationConsumerBasic`)

## Coverage Goals

- **Overall Coverage**: ≥80%
- **Core Components** (consumer, engine, strategies): ≥85%
- **Integration Paths**: ≥70% (realistic scenario coverage)
- **Error Handling**: ≥80% (all error strategies tested)

## Running All Tests

```bash
# Unit tests only
go test -v ./tests/unit

# Integration tests only  
go test -v ./tests/integration -timeout 5m

# All tests with coverage
go test -v ./tests/... -timeout 5m -coverprofile=coverage.out
go tool cover -html coverage.out
```

## CI/CD Integration

In CI environments, ensure:

1. Docker is available and daemon is running
2. Tests run with `timeout 5m` to catch hanging tests
3. Coverage reports are generated post-run
4. Failed tests include full Kafka container logs for debugging
