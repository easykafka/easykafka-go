package publish

import "slices"

// Partitioner selects how librdkafka maps a record key to a partition. It
// applies to every topic the publisher writes.
//
// Two writers place a key on the same partition only if they use the same
// partitioner and the topic has the same partition count for both. Changing a
// topic's partitioner moves its keys to other partitions, so a topic must keep
// the partitioner its other writers use.
type Partitioner string

const (
	// PartitionerDefault is librdkafka's default: CRC32 of the key, and a
	// random partition for a null key.
	PartitionerDefault Partitioner = "consistent_random"

	// PartitionerJavaCompatible places non-null keys exactly as the Java
	// client's default partitioner does (murmur2), and picks a random
	// partition for a null key. Use it for topics that Java producers also
	// write.
	PartitionerJavaCompatible Partitioner = "murmur2_random"

	// PartitionerMurmur2 is murmur2 for every key. Unlike
	// PartitionerJavaCompatible it hashes a null key as empty input, so every
	// null-key record goes to one partition.
	PartitionerMurmur2 Partitioner = "murmur2"

	// PartitionerConsistent is CRC32 for every key, a null key included.
	PartitionerConsistent Partitioner = "consistent"

	// PartitionerRandom ignores the key and picks a random partition.
	PartitionerRandom Partitioner = "random"

	// PartitionerFNV1A is FNV-1a for every key, a null key included.
	PartitionerFNV1A Partitioner = "fnv1a"

	// PartitionerFNV1ARandom is FNV-1a of the key, and a random partition for a
	// null key.
	PartitionerFNV1ARandom Partitioner = "fnv1a_random"
)

// partitioners lists every value librdkafka accepts for "partitioner", in the
// version bundled with confluent-kafka-go v2.15.0.
var partitioners = []Partitioner{
	PartitionerDefault,
	PartitionerJavaCompatible,
	PartitionerMurmur2,
	PartitionerConsistent,
	PartitionerRandom,
	PartitionerFNV1A,
	PartitionerFNV1ARandom,
}

func (p Partitioner) valid() bool {
	return slices.Contains(partitioners, p)
}
