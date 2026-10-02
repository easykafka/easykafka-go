package publish

// Writer publishes typed records to one topic. Safe for concurrent use.
type Writer[K, V any] struct {
	publisher *Publisher
	topic     Topic[K, V]
}

// Topic returns the name of the topic the writer publishes to.
func (w *Writer[K, V]) Topic() string {
	return w.topic.Name
}
