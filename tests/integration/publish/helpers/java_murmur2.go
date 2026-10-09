package helpers

// JavaMurmur2 is a port of the Java client's Utils.murmur2
// (org.apache.kafka.common.utils.Utils), the hash its default partitioner
// applies to a non-null key.
//
// It exists so the partitioner tests compare librdkafka against the Java
// client's function rather than against librdkafka itself. Java's int
// arithmetic wraps, as uint32 does here; its >>> is the logical shift a uint32
// shift is.
func JavaMurmur2(data []byte) int32 {
	const (
		seed = 0x9747b28c
		m    = 0x5bd1e995
		r    = 24
	)

	length := len(data)
	h := uint32(seed) ^ uint32(length)

	for index := 0; index+4 <= length; index += 4 {
		k := uint32(data[index]) | uint32(data[index+1])<<8 | uint32(data[index+2])<<16 | uint32(data[index+3])<<24
		k *= m
		k ^= k >> r
		k *= m
		h *= m
		h ^= k
	}

	// The tail, as Java's switch falls through from case 3 to case 1.
	tail := length &^ 3
	switch length % 4 {
	case 3:
		h ^= uint32(data[tail+2]) << 16
		fallthrough
	case 2:
		h ^= uint32(data[tail+1]) << 8
		fallthrough
	case 1:
		h ^= uint32(data[tail])
		h *= m
	}

	h ^= h >> 13
	h *= m
	h ^= h >> 15
	return int32(h)
}

// JavaPartition is the partition the Java client's default partitioner picks
// for a non-null key: toPositive(murmur2(key)) % partitions, where toPositive
// clears the sign bit.
func JavaPartition(key []byte, partitions int) int32 {
	return (JavaMurmur2(key) & 0x7fffffff) % int32(partitions)
}
