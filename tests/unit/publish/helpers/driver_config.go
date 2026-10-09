package helpers

import (
	"time"

	"github.com/easykafka/easykafka-go/internal/publish/publishdriver"
	"github.com/easykafka/easykafka-go/tests/unit/sharedhelpers"
)

// PublishDriverConfig returns a valid driver configuration for brokers where
// nothing listens.
func PublishDriverConfig() publishdriver.Config {
	return publishdriver.Config{
		Brokers:         []string{sharedhelpers.PublishBroker, "localhost:2"},
		KafkaConfig:     map[string]any{"linger.ms": 1, "client.id": "invoices"},
		Idempotence:     true,
		Partitioner:     "murmur2_random",
		DeliveryTimeout: 30 * time.Second,
	}
}
