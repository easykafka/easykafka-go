package helpers

// UnreachableBroker is a broker address where nothing listens (port 1), so
// every connection is refused at once. A client starts normally against it,
// since librdkafka does not connect at construction, accepts records into its
// queue, and can never send them.
const UnreachableBroker = "localhost:1"
