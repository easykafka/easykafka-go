package sharedhelpers

// PublishBroker is a broker address where nothing listens (port 1), so publish
// unit tests can never reach a real cluster, even one running locally.
const PublishBroker = "localhost:1"
