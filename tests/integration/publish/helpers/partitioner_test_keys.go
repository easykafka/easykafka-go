package helpers

// PartitionerTestKeys are the keys the partitioner tests place: Kafka's
// murmur2 reference keys, then the player-id-shaped ones.
func PartitionerTestKeys() []string {
	var keys []string
	for _, vector := range Murmur2ReferenceVectors() {
		keys = append(keys, vector.Key)
	}
	return append(keys, PlayerIDKeys()...)
}
