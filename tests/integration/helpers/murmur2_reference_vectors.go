package helpers

// Murmur2ReferenceVector is one key and the hash the Java client computes for
// it.
type Murmur2ReferenceVector struct {
	Key  string
	Hash int32
}

// Murmur2ReferenceVectors are Kafka's own murmur2 test vectors, from
// UtilsTest.testMurmur2 in the Java client
// (clients/src/test/java/org/apache/kafka/common/utils/UtilsTest.java).
func Murmur2ReferenceVectors() []Murmur2ReferenceVector {
	return []Murmur2ReferenceVector{
		{Key: "21", Hash: -973932308},
		{Key: "foobar", Hash: -790332482},
		{Key: "a-little-bit-long-string", Hash: -985981536},
		{Key: "a-little-bit-longer-string", Hash: -1486304829},
		{Key: "lkjh234lh9fiuh90y23oiuhsafujhadof229phr9h19h89h8", Hash: -58897971},
		{Key: "abc", Hash: 479470107},
	}
}
