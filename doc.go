// Package easykafka is the landing page of the EasyKafka module, a Kafka
// library for Go built on top of confluent-kafka-go. It exports nothing; the
// library is its two packages, one per side:
//
//   - [github.com/easykafka/easykafka-go/subscribe] reads a topic as a member of
//     a consumer group and hands every record to a handler. A record's offset is
//     stored only once the handler has handled it or an error strategy has
//     routed it: skipped, retried through a retry topic, or sent to a
//     dead-letter queue.
//   - [github.com/easykafka/easykafka-go/publish] writes records to Kafka and
//     confirms each one: a record is published once the broker has
//     acknowledged it, and a record that is not is reported.
//
// Both are laid out the same way: one public package per side, its internals
// under internal/subscribe or internal/publish, and one driver per side that
// alone talks to confluent-kafka-go.
package easykafka
