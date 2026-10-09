package sharedhelpers

import (
	"context"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
)

// waitForTopicTimeout bounds waitForTopic.
const waitForTopicTimeout = 20 * time.Second

// waitForTopic returns once the broker admin talks to describes topic, and
// fails the test if it does not within waitForTopicTimeout.
//
// CreateTopics returns once the controller has created the topic, but each
// broker learns of it from the controller on its own, a moment later. Until
// then a metadata request answers that the topic is unknown: a test that
// creates a topic and at once asks about it, as Publisher.Ping does, would
// fail now and then.
func waitForTopic(ctx context.Context, t *testing.T, admin *kfk.AdminClient, topic string) {
	t.Helper()

	deadline := time.Now().Add(waitForTopicTimeout)
	for {
		known, err := describesTopic(ctx, admin, topic)
		if known {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("topic %s not visible in metadata after %s: %v", topic, waitForTopicTimeout, err)
		}
		// Give the broker a moment before asking again, rather than flooding
		// it with requests.
		time.Sleep(100 * time.Millisecond)
	}
}

// describesTopic asks once whether the broker knows topic. An error is
// returned for the failure message only: any answer but "known" is retried.
func describesTopic(ctx context.Context, admin *kfk.AdminClient, topic string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, describeAttemptTimeout)
	defer cancel()
	result, err := admin.DescribeTopics(ctx, kfk.NewTopicCollectionOfTopicNames([]string{topic}))
	if err != nil {
		return false, err
	}
	for _, description := range result.TopicDescriptions {
		if description.Error.Code() != kfk.ErrNoError {
			return false, description.Error
		}
	}
	return true, nil
}
