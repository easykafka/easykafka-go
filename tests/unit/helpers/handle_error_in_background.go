package helpers

import (
	"context"

	"github.com/easykafka/easykafka-go/internal/types"
)

// HandleErrorInBackground calls strategy.HandleError on a goroutine of its own
// and returns a channel that receives its result, so a test can script the
// broker's reports while HandleError waits for them.
func HandleErrorInBackground(
	ctx context.Context, strategy types.ErrorStrategy, msgs []*types.Message, f types.Failure,
) <-chan error {

	result := make(chan error, 1)
	go func() { result <- strategy.HandleError(ctx, msgs, f) }()
	return result
}
