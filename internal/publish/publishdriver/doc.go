// Package publishdriver is the publish package's only contact with
// confluent-kafka-go: it wraps one librdkafka producer behind the Producer
// interface.
//
// It builds the librdkafka configuration (ConfigMap), produces records with a
// token that comes back on each record's delivery report, translates
// confluent's events into its own sealed Event types (Report, ClientError) with
// errors as KafkaError flags, and exposes flush, purge and a topic metadata
// lookup.
//
// The publisher depends on the interface and these types only, so:
//
//   - confluent types never reach the public API;
//   - the publisher's logic is unit-tested against a fake Producer, with no
//     broker.
//
// The architecture test guards the boundary: on the publish side only this
// package imports confluent-kafka-go, and this package never imports publish.
package publishdriver
