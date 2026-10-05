// Package publish writes typed records to Kafka and confirms every one of them.
//
// From the top down:
//
//   - Publisher: created by New, owns one librdkafka producer and the settings
//     that belong to it: brokers, acks, idempotence, partitioner, delivery
//     timeout, logger and callbacks. A service that needs two partitioners
//     creates two publishers. It also checks the cluster and the bound topics
//     with Ping.
//   - Topic[K, V]: a declaration of one Kafka topic: its name, how to encode a
//     key of type K and a value of type V into bytes (StringKey, JSONValue and
//     the other encoders cover the common cases), and headers added to every
//     record.
//   - Writer[K, V]: what Publisher.Bind returns for a Topic. It writes that
//     topic's records with typed keys and values, through its publisher's
//     producer: Publish and Delete wait for the broker, Send and SendDelete
//     return at once. One publisher can have any number of writers.
//   - Delivery: what Send and SendDelete return, the outcome of one record. It
//     resolves exactly once, from the record's delivery report, whether or not
//     anyone waits on it; Wait, or WaitAll for several, waits for it.
//   - DeliveryError: why a record was not acknowledged. It is what a failed
//     wait returns, and what the function given to WithDeliveryErrorFunc
//     receives, for every failure.
//
// Defaults favour durability: acks=all, idempotence on, and a 30 s delivery
// timeout. WithAcksLeader, WithoutIdempotence and WithDeliveryTimeout change
// them.
package publish
