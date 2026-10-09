package helpers

// StoredByPartition maps each partition to the last offset stored for it.
func StoredByPartition(stored []StoreRecord) map[int32]int64 {
	out := make(map[int32]int64)
	for _, s := range stored {
		out[s.Partition] = s.Offset
	}
	return out
}
