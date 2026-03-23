package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/easykafka/easykafka-go/tests/integration/helpers"
)

var (
	sharedCluster     *helpers.KafkaTestCluster
	sharedClusterOnce sync.Once
)

// SharedKafkaCluster returns a package-level Kafka container that is started
// once and reused across all integration tests in this package. This avoids the
// overhead of launching a new container for every test.
//
// The first call starts the container; subsequent calls return the same
// instance. The container is cleaned up automatically via t.Cleanup on the
// first caller's test, so it lives for the duration of the test suite.
//
// Tests that need an isolated broker (for example, reconnection tests that
// stop and restart the container) should call helpers.StartKafkaCluster
// directly instead of using this helper.
//
// Usage:
//
//	func TestMyFeature(t *testing.T) {
//	    if testing.Short() {
//	        t.Skip("skipping integration test in short mode")
//	    }
//	    cluster := SharedKafkaCluster(t)
//	    topic := helpers.UniqueTopicName(t, "my-feature")
//	    cluster.CreateTopic(context.Background(), t, topic, 1)
//	    // ... use cluster.Brokers, cluster.ProduceMessages, etc.
//	}
func SharedKafkaCluster(t *testing.T) *helpers.KafkaTestCluster {
	t.Helper()

	sharedClusterOnce.Do(func() {
		ctx := context.Background()
		sharedCluster = helpers.StartKafkaCluster(ctx, t)
		t.Cleanup(func() {
			sharedCluster.Stop(ctx, t)
		})
		t.Logf("shared kafka cluster started, brokers: %v", sharedCluster.Brokers)
	})

	return sharedCluster
}
