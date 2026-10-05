package publishdriver

import (
	"fmt"
	"strings"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
)

// Config describes one producer. The publish package builds it from its
// options, which have already validated it and rejected every key set below
// from KafkaConfig.
type Config struct {
	Brokers []string
	// KafkaConfig is passed through to librdkafka. It is only read.
	KafkaConfig map[string]any
	// AcksLeader selects acks=1; otherwise acks=all.
	AcksLeader      bool
	Idempotence     bool
	Partitioner     string
	DeliveryTimeout time.Duration
}

// ConfigMap builds the librdkafka configuration, in this order:
//
//  1. the caller's KafkaConfig, copied;
//  2. bootstrap.servers, acks, enable.idempotence, partitioner and
//     message.timeout.ms, each always set, so librdkafka's defaults never
//     decide them;
//  3. go.delivery.reports=true, so every report reaches Events(), and
//     go.delivery.report.fields=none: the token carries the record, so
//     confluent need not copy every key and value back from C.
//
// Exported so it can be tested without a broker; internal/ keeps it out of the
// library's public API.
func ConfigMap(config Config) (*kfk.ConfigMap, error) {
	configMap := &kfk.ConfigMap{}
	for key, value := range config.KafkaConfig {
		if err := configMap.SetKey(key, value); err != nil {
			return nil, fmt.Errorf("setting kafka config key %q: %w", key, err)
		}
	}

	acks := "all"
	if config.AcksLeader {
		acks = "1"
	}
	managed := []struct {
		key   string
		value kfk.ConfigValue
	}{
		{"bootstrap.servers", strings.Join(config.Brokers, ",")},
		{"acks", acks},
		{"enable.idempotence", config.Idempotence},
		{"partitioner", config.Partitioner},
		{"message.timeout.ms", int(config.DeliveryTimeout.Milliseconds())},
		{"go.delivery.reports", true},
		{"go.delivery.report.fields", "none"},
	}
	for _, setting := range managed {
		if err := configMap.SetKey(setting.key, setting.value); err != nil {
			return nil, fmt.Errorf("setting kafka config key %q: %w", setting.key, err)
		}
	}
	return configMap, nil
}
