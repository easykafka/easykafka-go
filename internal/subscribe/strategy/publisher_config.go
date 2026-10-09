package strategy

import (
	"strings"

	"github.com/easykafka/easykafka-go/internal/publish/publishdriver"
)

// consumerOnlyPrefixes and consumerOnlyKeys name the librdkafka properties that
// apply to consumers only — scope "C" in librdkafka's CONFIGURATION.md. They are
// dropped when a producer inherits the consumer's configuration.
//
// This is a deny-list rather than an allowlist of shared keys on purpose. A
// consumer-only key missing from it reaches the producer, and librdkafka logs
// one CONFWARN line ("is a consumer property and will be ignored") and carries
// on. A shared key missing from an allowlist — a security, SASL or TLS setting —
// would be silently withheld, and every retry and DLQ write would fail.
var (
	consumerOnlyPrefixes = []string{"group.", "fetch.", "queued.", "auto.", "enable.auto.", "offset.store."}
	consumerOnlyKeys     = map[string]bool{
		"session.timeout.ms":            true,
		"heartbeat.interval.ms":         true,
		"max.poll.interval.ms":          true,
		"coordinator.query.interval.ms": true,
		"partition.assignment.strategy": true,
		"max.partition.fetch.bytes":     true,
		"isolation.level":               true,
		"consume.callback.max.messages": true,
		"enable.partition.eof":          true,
		"check.crcs":                    true,
	}
)

// PublisherConfig builds the Kafka config of the retry strategy's publisher
// from the consumer's WithKafkaConfig map, so the publisher connects the way
// the consumer does — with its security, SASL and TLS settings above all.
//
// Every key is carried over except:
//
//   - consumer-only keys, which a producer ignores anyway (see consumerOnlyKeys);
//   - "go." keys, confluent-kafka-go's own client options. Some that a consumer
//     accepts, such as go.application.rebalance.enable, make a producer fail to
//     start, and the publisher sets its own;
//   - keys the publisher manages (publishdriver.ManagedKeyReason): acks,
//     enable.idempotence, message.timeout.ms and the like.
//     publish.WithKafkaConfig rejects them, so leaving them in would fail
//     Initialize. A consumer map that holds one meant it for the consumer.
//
// The brokers are not set here: the publisher takes them from WithBrokers.
// kafkaConfig is only read; it also configures the consumer. The result is
// never nil.
//
// Exported within internal/ so that tests can reach it.
func PublisherConfig(kafkaConfig map[string]any) map[string]any {
	config := make(map[string]any, len(kafkaConfig))
	for key, value := range kafkaConfig {
		if strings.HasPrefix(key, "go.") || isConsumerOnly(key) {
			continue
		}
		if _, managed := publishdriver.ManagedKeyReason(key); managed {
			continue
		}
		config[key] = value
	}
	return config
}

// isConsumerOnly reports whether key is a consumer-only librdkafka property.
func isConsumerOnly(key string) bool {
	if consumerOnlyKeys[key] {
		return true
	}
	for _, prefix := range consumerOnlyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}
