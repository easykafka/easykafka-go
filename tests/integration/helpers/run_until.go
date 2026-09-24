package helpers

import (
	"context"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
)

// RunUntil starts consumer and returns a stop function that cancels it and
// returns what Start returned. It fails the test if the consumer has not
// stopped within 30 seconds of the cancel.
func RunUntil(ctx context.Context, t *testing.T, consumer easykafka.Consumer) func() error {
	t.Helper()
	consumerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- consumer.Start(consumerCtx) }()
	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			t.Fatal("consumer did not stop after context cancellation")
			return nil
		}
	}
}
