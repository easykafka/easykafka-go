package metadata

import (
	"context"
	"time"

	"github.com/easykafka/easykafka-go/internal/types"
)

// WaitUntilRetryTime blocks until msg's retry time has passed. It returns nil
// at once if msg carries no retry time or the time is already past, and
// ctx.Err() if ctx is cancelled before the time arrives.
func WaitUntilRetryTime(ctx context.Context, msg *types.Message) error {
	due := GetRetryTime(msg)
	if due.IsZero() {
		return nil
	}

	wait := time.Until(due)
	if wait <= 0 {
		return nil
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
