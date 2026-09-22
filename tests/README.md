# 🧪 Easy Kafka Consumer Library - Test Organization

## 📋 Overview

This directory contains comprehensive unit and integration tests for the Easy Kafka Consumer Library, organized following the Testing Pyramid with emphasis on realistic Kafka integration scenarios.

## 📂 Test Organization

```
tests/
├── unit/                 # Unit tests (~70% of tests)
│   ├── batch_buffer_test.go
│   ├── commit_cadence_test.go
│   ├── engine_dispatch_test.go
│   ├── offset_store_test.go
│   ├── options_validation_test.go
│   ├── producer_delivery_test.go
│   ├── shutdown_test.go
│   ├── strategy_basic_test.go
│   └── strategy_retry_test.go
│
├── integration/          # Integration tests (~30% of tests)
│   ├── at_least_once_test.go
│   ├── batch_processing_test.go
│   ├── commit_cadence_test.go
│   ├── config_passthrough_test.go
│   ├── consumer_basic_test.go
│   ├── delivery_error_test.go
│   ├── fail_fast_test.go
│   ├── graceful_shutdown_test.go
│   ├── offset_semantics_test.go
│   ├── rebalance_test.go
│   ├── reconnection_test.go
│   ├── retry_dlq_test.go
│   ├── revoked_store_test.go
│   └── helpers/
│       └── kafka_helper.go
```

## 🔬 Unit Tests

Unit tests focus on individual components in isolation with mocked dependencies.

### Running Unit Tests

```bash
go test -v ./tests/unit -run "^TestUnit"
```

### Coverage

```bash
go test -cover ./tests/unit
```

## 🔗 Integration Tests

Integration tests use `testcontainers-go` to spin up a real Kafka instance and verify end-to-end behavior.

### Prerequisites

- Docker (for Kafka container)
- Docker daemon running

### Running Integration Tests

```bash
go test -v ./tests/integration -run "^TestIntegration" -timeout 5m
```

or (to get a more readable output):
```bash
go install gotest.tools/gotestsum@latest
gotestsum --format testdox -- -count=1 -timeout 1000s ./tests/integration/...
```

format options:
* testdox
    * Human-readable test names with ✓/✗
* pkgname
    * One line per package + failures
* standard-verbose
    * Like -v but with a summary at the end
* dots
    * Minimal dots during run, failures at end

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

## 🧰 Test Helpers

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

## 💻 Execution Requirements

### Supported Platforms

- Linux (primary)
- macOS (requires Docker Desktop)
- Windows (requires WSL2 + Docker Desktop)

### System Requirements

- Minimum 2GB RAM for Kafka container
- Docker socket access (`/var/run/docker.sock`)

## 📝 Test Naming Convention

- Unit tests: `TestUnit<Component><Scenario>` (e.g., `TestUnitEngineDispatch`)
- Integration tests: `TestIntegration<Feature><Scenario>` (e.g., `TestIntegrationConsumerBasic`)

## 🎯 Coverage Goals

- **Overall Coverage**: ≥80%
- **Core Components** (consumer, engine, strategies): ≥85%
- **Integration Paths**: ≥70% (realistic scenario coverage)
- **Error Handling**: ≥80% (all error strategies tested)

## ▶️ Running All Tests

```bash
# Unit tests only
go test -v ./tests/unit

# Integration tests only  
go test -v ./tests/integration -timeout 5m

# All tests with coverage (-coverpkg=./... instruments all library packages)
go test -v -coverpkg=./... ./tests/... -timeout 5m -coverprofile=coverage.out
go tool cover -html coverage.out
```

## 🔄 CI/CD Integration

In CI environments, ensure:

1. Docker is available and daemon is running
2. Tests run with `timeout 5m` to catch hanging tests
3. Coverage reports are generated post-run
4. Failed tests include full Kafka container logs for debugging
