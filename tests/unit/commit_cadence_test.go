package unit

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/engine"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/easykafka/easykafka-go/tests/unit/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Commit cadence is opt-in: without WithAutoCommitEvery the engine commits after
// every message, and with it librdkafka's background committer owns timing and
// the per-message commits do nothing. The engine reads identically in both modes
// because the adapter, not the engine, knows which mode it is in — so these tests
// assert on the calls the adapter actually receives.
//
// What must hold in *both* modes, and is the correctness half: every handled
// message stores its offset, and shutdown commits unconditionally. Only the
// per-message commit is allowed to disappear.

// TestCommitCadence runs the same three messages through both modes and pins the
// difference to exactly one thing: whether a commit follows each store.
func TestCommitCadence(t *testing.T) {
	messages := []*types.Message{
		helpers.NewTestMessage("topic", 0, 0, "msg-1"),
		helpers.NewTestMessage("topic", 0, 1, "msg-2"),
		helpers.NewTestMessage("topic", 0, 2, "msg-3"),
	}

	run := func(t *testing.T, autoCommit bool) []string {
		t.Helper()

		client := &helpers.RecordingClient{AutoCommit: autoCommit, Messages: messages}
		handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

		eng := engine.NewEngine(client, handler, &helpers.MockStrategy{}, helpers.TestLogger(), 10)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() { done <- eng.Start(ctx) }()

		require.Eventually(t, func() bool {
			return helpers.CountCalls(client.Calls(), "store") == len(messages)
		}, 2*time.Second, 10*time.Millisecond, "engine did not store every message")

		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("engine did not stop")
		}

		return client.Calls()
	}

	t.Run("default commits after every message", func(t *testing.T) {
		calls := run(t, false)

		assert.Equal(t, len(messages), helpers.CountCalls(calls, "store"))
		// One commit per message, plus the unconditional final one.
		assert.Equal(t, len(messages)+1, helpers.CountCalls(calls, "commit"),
			"the default cadence commits after every message")
	})

	t.Run("interval mode commits only at shutdown", func(t *testing.T) {
		calls := run(t, true)

		// The correctness half is untouched: every message still stores its
		// offset, so the background committer has something true to publish.
		assert.Equal(t, len(messages), helpers.CountCalls(calls, "store"),
			"storing must not depend on commit cadence")

		// The whole point: no per-message commits. The one that remains is the
		// unconditional commit on the way out, and it lands before the close.
		assert.Equal(t, 1, helpers.CountCalls(calls, "commit"),
			"interval mode must leave commit timing to librdkafka")
		require.GreaterOrEqual(t, len(calls), 2)
		assert.Equal(t, "close", calls[len(calls)-1])
		assert.Equal(t, "commit", calls[len(calls)-2],
			"shutdown must commit even in interval mode, or a clean stop replays an interval")
	})
}

// TestCommitCadenceBatchMode is the batch-mode counterpart: dispatchBatch's
// commit is conditional in the same way, and the final one is not.
func TestCommitCadenceBatchMode(t *testing.T) {
	messages := []*types.Message{
		helpers.NewTestMessage("topic", 0, 0, "msg-1"),
		helpers.NewTestMessage("topic", 0, 1, "msg-2"),
	}

	client := &helpers.RecordingClient{AutoCommit: true, Messages: messages}
	batchHandler := func(ctx context.Context, batch *types.Batch) *types.Failure { return nil }

	// Batch size equal to the message count, so the batch dispatches on its own
	// and its commit is reached rather than skipped by the shutdown drop.
	eng := engine.NewBatchEngine(
		client, batchHandler, &helpers.MockStrategy{}, helpers.TestLogger(), 10, len(messages), time.Minute,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Start(ctx) }()

	require.Eventually(t, func() bool {
		return helpers.CountCalls(client.Calls(), "store") > 0
	}, 2*time.Second, 10*time.Millisecond, "batch never dispatched")

	cancel()
	require.NoError(t, <-done)

	calls := client.Calls()
	assert.Equal(t, 1, helpers.CountCalls(calls, "commit"),
		"the per-batch commit must be skipped in interval mode, leaving only the final one")
	assert.Equal(t, "close", calls[len(calls)-1])
	assert.Equal(t, "commit", calls[len(calls)-2])
}

// TestRecordingClientEventNames guards against helpers.CountCalls silently
// matching nothing if the event names in helpers.RecordingClient are ever renamed.
func TestRecordingClientEventNames(t *testing.T) {
	client := &helpers.RecordingClient{Messages: []*types.Message{helpers.NewTestMessage("topic", 0, 0, "m")}}
	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

	eng := engine.NewEngine(client, handler, &helpers.MockStrategy{}, helpers.TestLogger(), 10)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, eng.Start(ctx))

	calls := client.Calls()
	for _, name := range []string{"store", "commit", "close"} {
		assert.True(t, slices.Contains(calls, name), "helpers.RecordingClient no longer records %q", name)
	}
}
