package publishdriver

import "strings"

// managedKafkaKeys are librdkafka keys the publisher manages itself, each with
// the reason. Keys with a prefix in managedKafkaKeyPrefixes are managed too.
var managedKafkaKeys = map[string]string{
	"bootstrap.servers": "managed by WithBrokers",
	"acks":              "managed by WithAcksLeader; the default is acks=all",
	"request.required.acks": "managed by WithAcksLeader; the default is acks=all " +
		"(request.required.acks is another name for acks)",
	"enable.idempotence": "managed by the library: idempotence is on unless WithoutIdempotence or " +
		"WithAcksLeader is used",
	"enable.gapless.guarantee": "managed by the library and kept off: it turns a timed-out record into " +
		"a fatal error",
	"partitioner":         "managed by WithPartitioner",
	"message.timeout.ms":  "managed by WithDeliveryTimeout",
	"delivery.timeout.ms": "managed by WithDeliveryTimeout (delivery.timeout.ms is another name for message.timeout.ms)",
	"transactional.id":    "not supported: the publisher does not use transactions",
	"default.topic.config": "not supported: it would bypass WithAcksLeader, WithPartitioner and " +
		"WithDeliveryTimeout",
}

// managedKafkaKeyPrefixes are key prefixes the publisher manages, each with the
// reason.
var managedKafkaKeyPrefixes = []struct{ prefix, reason string }{
	{"go.", "managed by the library: confluent-kafka-go's own settings decide how delivery " +
		"reports reach the publisher"},
	// confluent-kafka-go moves a "{topic}." key into default.topic.config, so
	// "{topic}.acks" is default.topic.config's acks under another spelling.
	{"{topic}.", "not supported: confluent-kafka-go moves it into default.topic.config, which would " +
		"bypass WithAcksLeader, WithPartitioner and WithDeliveryTimeout; set the property without the prefix"},
}

// ManagedKeyReason returns why the publisher does not take key from a caller's
// Kafka config, if it does not. publish.WithKafkaConfig rejects such a key; the
// retry strategy, which hands the consumer's map to a publisher, drops it. One
// list serves both, so the two cannot drift apart.
func ManagedKeyReason(key string) (reason string, managed bool) {
	if reason, managed := managedKafkaKeys[key]; managed {
		return reason, true
	}
	for _, managed := range managedKafkaKeyPrefixes {
		if strings.HasPrefix(key, managed.prefix) {
			return managed.reason, true
		}
	}
	return "", false
}
