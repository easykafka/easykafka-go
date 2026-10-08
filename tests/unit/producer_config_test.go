package unit

import (
	"context"
	"maps"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/internal/kafka"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/easykafka/easykafka-go/strategy"
	"github.com/easykafka/easykafka-go/tests/unit/helpers"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// The retry publisher inherits the consumer's Kafka config
//
// The two plumbing tests below need no broker. They give the config a value
// librdkafka rejects when a client is created — security.protocol "bogus" — so
// an error naming it proves the map reached the retry strategy's publisher.
// =============================================================================

// TestRetryStrategyInitializeUsesKafkaConfig is the strategy side of the
// plumbing: the map handed to Initialize must reach the publisher it creates.
func TestRetryStrategyInitializeUsesKafkaConfig(t *testing.T) {
	s, err := strategy.NewRetryStrategy(
		strategy.WithRetryTopic("orders.retry"),
		strategy.WithDLQTopic("orders.dlq"),
	)
	require.NoError(t, err)

	err = s.Initialize(types.InitConfig{
		Brokers:     []string{"localhost:1"},
		Logger:      zerolog.Nop(),
		KafkaConfig: map[string]any{"security.protocol": "bogus"},
	})
	t.Cleanup(func() { _ = s.Close() })

	require.Error(t, err, "the publisher must be built from the consumer's Kafka config")
	assert.Contains(t, err.Error(), "security.protocol")
}

// TestConsumerPassesKafkaConfigToRetryStrategy is the consumer side: a map set
// with WithKafkaConfig must travel through InitConfig to the strategy. The
// strategy is initialized before the Kafka adapter is created, so failing with
// "failed to initialize error strategy" — rather than later, when the consumer
// itself connects — shows the map got there.
func TestConsumerPassesKafkaConfigToRetryStrategy(t *testing.T) {
	retry, err := easykafka.NewRetryStrategy(
		easykafka.WithRetryTopic("orders.retry"),
		easykafka.WithDLQTopic("orders.dlq"),
	)
	require.NoError(t, err)

	consumer, err := easykafka.New(
		easykafka.WithTopic("orders"),
		easykafka.WithBrokers("localhost:1"),
		easykafka.WithConsumerGroup("orders-group"),
		easykafka.WithHandler(helpers.NoopHandler),
		easykafka.WithErrorStrategy(retry),
		easykafka.WithKafkaConfig(map[string]any{"security.protocol": "bogus"}),
	)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = consumer.Start(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to initialize error strategy")
	assert.Contains(t, err.Error(), "security.protocol")
}

// TestRetryStrategyInitializeDropsPublisherManagedKeys verifies a consumer map
// holding keys the publisher manages still initializes the strategy:
// publish.WithKafkaConfig would reject them, so the strategy drops them, and
// the publisher keeps its own acks, idempotence and delivery timeout.
func TestRetryStrategyInitializeDropsPublisherManagedKeys(t *testing.T) {
	// Not scripted: nothing is sent, the fake only records the config the publisher builds it with.
	fake := helpers.NewFakePublishProducer()
	t.Cleanup(fake.Close)
	s, err := strategy.NewRetryStrategy(
		strategy.WithRetryTopic("orders.retry"),
		strategy.WithDLQTopic("orders.dlq"),
		strategy.WithProducerFactory(fake.Factory()),
	)
	require.NoError(t, err)

	err = s.Initialize(types.InitConfig{
		Brokers: []string{"broker:9092"}, // kept: reaches the publisher through publish.WithBrokers
		Logger:  zerolog.Nop(),
		KafkaConfig: map[string]any{
			"acks":               "0",              // dropped: publisher-managed, it keeps acks=all
			"enable.idempotence": false,            // dropped: publisher-managed, it keeps idempotence on
			"message.timeout.ms": 300000,           // dropped: publisher-managed, it keeps its 30 s delivery timeout
			"{topic}.acks":       "1",              // dropped: publisher-managed prefix, would bypass acks=all
			"client.id":          "orders-service", // kept: a shared key the publisher does not manage
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	config := fake.Config()
	assert.Equal(t, map[string]any{"client.id": "orders-service"}, config.KafkaConfig)
	assert.Equal(t, []string{"broker:9092"}, config.Brokers)
	assert.False(t, config.AcksLeader, "acks=all")
	assert.True(t, config.Idempotence)
	assert.Equal(t, 30*time.Second, config.DeliveryTimeout)
}

// =============================================================================
// kafka.PublisherConfig
// =============================================================================

// TestPublisherConfigCarriesConnectionSettings verifies the settings a producer
// needs to reach a secured cluster are passed through unchanged — including
// enable.ssl.certificate.verification, whose name an "ssl." allowlist would miss.
func TestPublisherConfigCarriesConnectionSettings(t *testing.T) {
	shared := map[string]any{
		"security.protocol":                   "SASL_SSL",
		"sasl.mechanisms":                     "PLAIN",
		"sasl.username":                       "orders-svc",
		"sasl.password":                       "secret",
		"ssl.ca.location":                     "/etc/kafka/ca.pem",
		"enable.ssl.certificate.verification": false,
		"client.id":                           "orders-service",
		"socket.keepalive.enable":             true,
	}

	assert.Equal(t, shared, kafka.PublisherConfig(shared))
}

// TestPublisherConfigDropsConsumerOnlyKeys verifies consumer-only keys — every
// exact key on the list, and one from each prefix family — and all "go." keys
// are left out. The keys are spelled out here rather than read from the
// package, so the test checks the list instead of mirroring it.
func TestPublisherConfigDropsConsumerOnlyKeys(t *testing.T) {
	dropped := map[string]any{
		// exact keys
		"session.timeout.ms":            30000,
		"heartbeat.interval.ms":         3000,
		"max.poll.interval.ms":          600000,
		"coordinator.query.interval.ms": 600000,
		"partition.assignment.strategy": "range",
		"max.partition.fetch.bytes":     1048576,
		"isolation.level":               "read_committed",
		"consume.callback.max.messages": 0,
		"enable.partition.eof":          true,
		"check.crcs":                    true,
		// one per prefix family
		"group.instance.id":   "instance-1",
		"fetch.min.bytes":     1,
		"queued.min.messages": 100000,
		"auto.offset.reset":   "latest",
		"enable.auto.commit":  true,
		"offset.store.method": "broker",
		// confluent-kafka-go client options
		"go.application.rebalance.enable": true,
		"go.events.channel.enable":        true,
		"go.logs.channel.enable":          true,
	}

	assert.Empty(t, kafka.PublisherConfig(dropped))
}

// TestPublisherConfigDropsPublisherManagedKeys verifies every key
// publish.WithKafkaConfig would reject is left out, so a consumer map holding
// one cannot fail Initialize. Spelled out, as above.
func TestPublisherConfigDropsPublisherManagedKeys(t *testing.T) {
	managed := map[string]any{
		"bootstrap.servers":        "elsewhere:9092",
		"acks":                     "0",
		"request.required.acks":    "1",
		"enable.idempotence":       false,
		"enable.gapless.guarantee": true,
		"partitioner":              "random",
		"message.timeout.ms":       300000,
		"delivery.timeout.ms":      300000,
		"transactional.id":         "orders-tx",
		"default.topic.config":     map[string]any{"acks": "1"},
		"{topic}.acks":             "1",
	}

	assert.Empty(t, kafka.PublisherConfig(managed))
}

// TestPublisherConfigLeavesCallerMapAlone verifies the caller's map is only
// read: it also configures the consumer.
func TestPublisherConfigLeavesCallerMapAlone(t *testing.T) {
	original := map[string]any{
		"security.protocol":  "SASL_SSL",
		"session.timeout.ms": 30000,
		"acks":               "0",
	}
	snapshot := maps.Clone(original)

	assert.Equal(t, map[string]any{"security.protocol": "SASL_SSL"}, kafka.PublisherConfig(original))
	assert.Equal(t, snapshot, original)
}

// TestPublisherConfigWithoutKafkaConfig verifies a consumer with no
// WithKafkaConfig still gets a config publish.WithKafkaConfig accepts: empty,
// never nil.
func TestPublisherConfigWithoutKafkaConfig(t *testing.T) {
	config := kafka.PublisherConfig(nil)
	require.NotNil(t, config)
	assert.Empty(t, config)
}
