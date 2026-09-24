package unit

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/engine"
	"github.com/easykafka/easykafka-go/internal/types"
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
		newTestMessage("topic", 0, 0, "msg-1"),
		newTestMessage("topic", 0, 1, "msg-2"),
		newTestMessage("topic", 0, 2, "msg-3"),
	}

	run := func(t *testing.T, autoCommit bool) []string {
		t.Helper()

		client := &recordingClient{autoCommit: autoCommit, messages: messages}
		handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

		eng := engine.NewEngine(client, handler, &mockStrategy{}, testLogger(), 10)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() { done <- eng.Start(ctx) }()

		require.Eventually(t, func() bool {
			return countCalls(client.calls(), "store") == len(messages)
		}, 2*time.Second, 10*time.Millisecond, "engine did not store every message")

		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("engine did not stop")
		}

		return client.calls()
	}

	t.Run("default commits after every message", func(t *testing.T) {
		calls := run(t, false)

		assert.Equal(t, len(messages), countCalls(calls, "store"))
		// One commit per message, plus the unconditional final one.
		assert.Equal(t, len(messages)+1, countCalls(calls, "commit"),
			"the default cadence commits after every message")
	})

	t.Run("interval mode commits only at shutdown", func(t *testing.T) {
		calls := run(t, true)

		// The correctness half is untouched: every message still stores its
		// offset, so the background committer has something true to publish.
		assert.Equal(t, len(messages), countCalls(calls, "store"),
			"storing must not depend on commit cadence")

		// The whole point: no per-message commits. The one that remains is the
		// unconditional commit on the way out, and it lands before the close.
		assert.Equal(t, 1, countCalls(calls, "commit"),
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
		newTestMessage("topic", 0, 0, "msg-1"),
		newTestMessage("topic", 0, 1, "msg-2"),
	}

	client := &recordingClient{autoCommit: true, messages: messages}
	batchHandler := func(ctx context.Context, batch *types.Batch) *types.Failure { return nil }

	// Batch size equal to the message count, so the batch dispatches on its own
	// and its commit is reached rather than skipped by the shutdown drop.
	eng := engine.NewBatchEngine(
		client, batchHandler, &mockStrategy{}, testLogger(), 10, len(messages), time.Minute,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Start(ctx) }()

	require.Eventually(t, func() bool {
		return countCalls(client.calls(), "store") > 0
	}, 2*time.Second, 10*time.Millisecond, "batch never dispatched")

	cancel()
	require.NoError(t, <-done)

	calls := client.calls()
	assert.Equal(t, 1, countCalls(calls, "commit"),
		"the per-batch commit must be skipped in interval mode, leaving only the final one")
	assert.Equal(t, "close", calls[len(calls)-1])
	assert.Equal(t, "commit", calls[len(calls)-2])
}

func countCalls(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

// guard against the helper above silently matching nothing if the event names
// in recordingClient are ever renamed.
func TestRecordingClientEventNames(t *testing.T) {
	client := &recordingClient{messages: []*types.Message{newTestMessage("topic", 0, 0, "m")}}
	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

	eng := engine.NewEngine(client, handler, &mockStrategy{}, testLogger(), 10)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, eng.Start(ctx))

	calls := client.calls()
	for _, name := range []string{"store", "commit", "close"} {
		assert.True(t, slices.Contains(calls, name), "recordingClient no longer records %q", name)
	}
}
