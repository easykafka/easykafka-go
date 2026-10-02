// Package publish writes typed records to Kafka and confirms every one of them.
//
// A Publisher owns one librdkafka producer. Topics are declared once with
// Topic, which fixes the key and value types and how they are encoded, and
// Bind turns a Topic into a typed Writer. Every record a Writer accepts is
// reported exactly once, through its delivery report: to a caller waiting on
// it, and to the function given to WithDeliveryErrorFunc when it fails.
//
// Defaults favour durability: acks=all, idempotence on, and a 30 s delivery
// timeout. WithAcksLeader, WithoutIdempotence and WithDeliveryTimeout change
// them.
package publish
