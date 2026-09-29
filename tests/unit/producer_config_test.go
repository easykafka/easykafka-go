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
// Retry/DLQ producers inherit the consumer's Kafka config
//
// The two plumbing tests below need no broker. They give the config a value
// librdkafka rejects when a client is created — security.protocol "bogus" — so
// an error naming it proves the map reached the retry strategy's producers.
// =============================================================================

// TestRetryStrategyInitializeUsesKafkaConfig is the strategy side of the
// plumbing: the map handed to Initialize must reach the producers it creates.
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

	require.Error(t, err, "the producers must be built from the consumer's Kafka config")
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

// =============================================================================
// kafka.ProducerConfig
// =============================================================================

// TestProducerConfigCarriesConnectionSettings verifies the settings a producer
// needs to reach a secured cluster are passed through unchanged — including
// enable.ssl.certificate.verification, whose name an "ssl." allowlist would miss.
func TestProducerConfigCarriesConnectionSettings(t *testing.T) {
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

	cfg, err := kafka.ProducerConfig([]string{"broker:9092"}, shared)
	require.NoError(t, err)

	for key, want := range shared {
		assert.Equal(t, want, (*cfg)[key], "%s must reach the producer", key)
	}
}

// TestProducerConfigDropsConsumerOnlyKeys verifies consumer-only keys — every
// exact key on the list, and one from each prefix family — and all "go." keys
// are left out. The keys are spelled out here rather than read from the
// package, so the test checks the list instead of mirroring it.
func TestProducerConfigDropsConsumerOnlyKeys(t *testing.T) {
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

	cfg, err := kafka.ProducerConfig([]string{"broker:9092"}, dropped)
	require.NoError(t, err)

	for key := range dropped {
		assert.NotContains(t, *cfg, key, "%s must not reach the producer", key)
	}
}

// TestProducerConfigLibraryKeysWin verifies the caller's map cannot change the
// brokers the producer connects to or weaken acks=all.
func TestProducerConfigLibraryKeysWin(t *testing.T) {
	cfg, err := kafka.ProducerConfig(
		[]string{"a:9092", "b:9092"},
		map[string]any{"acks": "0", "bootstrap.servers": "elsewhere:9092"},
	)
	require.NoError(t, err)

	assert.Equal(t, "a:9092,b:9092", (*cfg)["bootstrap.servers"])
	assert.Equal(t, "all", (*cfg)["acks"])
}

// TestProducerConfigLeavesCallerMapAlone verifies the caller's map is only
// read: it also configures the consumer, and both producers are built from it.
func TestProducerConfigLeavesCallerMapAlone(t *testing.T) {
	original := map[string]any{
		"security.protocol":  "SASL_SSL",
		"session.timeout.ms": 30000,
		"acks":               "0",
	}
	snapshot := maps.Clone(original)

	_, err := kafka.ProducerConfig([]string{"broker:9092"}, original)
	require.NoError(t, err)

	assert.Equal(t, snapshot, original)
}

// TestProducerConfigWithoutKafkaConfig verifies a consumer with no
// WithKafkaConfig still gets a working producer config.
func TestProducerConfigWithoutKafkaConfig(t *testing.T) {
	cfg, err := kafka.ProducerConfig([]string{"broker:9092"}, nil)
	require.NoError(t, err)

	assert.Len(t, *cfg, 2, "only the library's own keys")
	assert.Equal(t, "broker:9092", (*cfg)["bootstrap.servers"])
	assert.Equal(t, "all", (*cfg)["acks"])
}
