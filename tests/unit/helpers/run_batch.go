package helpers

import (
	"context"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/engine"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/rs/zerolog"
)

// RunBatch drives a batch engine over the client's messages as a single batch
// and returns the engine's result. The engine runs for 500ms, which is ample
// for a mock client that hands its messages over without delay.
func RunBatch(
	t *testing.T,
	client *MockKafkaClient,
	handler types.BatchHandler,
	strat types.ErrorStrategy,
	logger zerolog.Logger,
) error {

	t.Helper()
	eng := engine.NewBatchEngine(client, handler, strat, logger, 10, len(client.Messages), 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	return eng.Start(ctx)
}
