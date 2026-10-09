# 🧪 Tests

Every test of the module lives here; there are no in-package `_test.go` files. The tests are split
first by what they need, then by the side of the library they cover:

```
tests/
├── unit/                   no Docker; make test-unit runs ./tests/unit/... with -race
│   ├── subscribe/          package subscribe_test: the poll loop, options, strategies, metadata, batches
│   │   └── helpers/        fake consumers, fake strategies, retry-strategy builders, fixtures
│   ├── publish/            package publish_test: publisher, writer, delivery, close, driver
│   │   └── helpers/        fake publishers, publish fixtures, report helpers
│   ├── sharedhelpers/      helpers both sides' unit tests use
│   └── architecture/       package architecture_test: the guard on package boundaries
│
└── integration/            Docker; make test-integration runs ./tests/integration/...
    ├── subscribe/          package subscribe_test: the subscriber against a real broker
    │   └── helpers/        RunUntil, WaitForMessages, ConsumptionRecorder, NewRetryStrategy
    ├── publish/            package publish_test: the publisher against real brokers
    │   └── helpers/        invoices, partitioner vectors, throughput report, warm-up
    └── sharedhelpers/      the clusters, topic setup, shared fakes and loads
```

## Where a helper goes

Test files contain tests only. Every helper — fake, fixture, shared setup — goes in a file of its
own, named after it, in the lowest folder that covers every test using it:

- one side's `helpers/` if only that side's tests use it;
- `sharedhelpers/` if a test of each side uses it today — not because it might one day. Moving one
  later is cheap.

`sharedhelpers` never imports a side's `helpers`, and a side's `helpers` never imports the other
side's. At a call site the package says where a helper comes from: `helpers.NewTestMessage(…)` from
the test's own side, `sharedhelpers.TestLogger()` from both.

Helpers are a separate package from the tests, so anything a test sets or reads is exported. Files
there are plain `.go`, not `_test.go`, or the tests could not import them.

Two test files keep their helpers beside the tests, on purpose, because they are complex enough that
one place to read serves a reviewer better: the architecture guard
(`unit/architecture/architecture_test.go`) and the subscriber's broker-outage test
(`integration/subscribe/broker_outage_test.go`). No other test file may do so.

Scripted stand-ins are named `Fake` + what they stand in for: `FakeConsumer` and its variants for the
subscriber's driver, `FakeProducer` for the publisher's, `FakeStrategy` for an error strategy.

## Unit tests

```bash
make test-unit                                   # what CI runs
go test -count=1 -race ./tests/unit/...
go test -count=1 -run TestName ./tests/unit/...  # one test
```

The subscriber's tests drive its public entry point, `subscribe.New`, over a fake consumer through
the `WithConsumerFactory` seam (`helpers.NewSubscriberOnFake`); the publisher's do the same through
`publish.WithProducerFactory` and a `FakeProducer`.

## Integration tests

They use `testcontainers-go` to run Kafka in Docker. Each test binary starts one shared broker on
first use (`sharedhelpers.SharedCluster`) and reuses it; a test that stops or restarts brokers takes
a `DedicatedCluster` or a `NewThreeBrokerCluster` of its own. Every test creates its topics under
unique names (`sharedhelpers.UniqueTopicName`), so tests sharing the broker do not interfere.

```bash
make test-integration                            # what CI runs
go test -count=1 -p 1 -timeout 1000s ./tests/integration/...
```

`-p 1` runs one package at a time — `subscribe`, then `publish` — each still running its tests in
parallel. Each package is its own test binary with its own clusters, so running both at once would
start both sides' brokers together; on a CI runner that is more load than the suite needs.
`make coverage` and the Code Coverage workflow run with `-p 1` too.

In CI the Kafka image is pulled from the GHCR mirror named by `KAFKA_IMAGE`.

## Coverage

```bash
make coverage    # unit + integration; needs Docker
```

`-coverpkg=./...` is what instruments the library's packages: without it only the test packages,
which hold no library code, would be measured.
