package unit

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/internal/engine"
	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/easykafka/easykafka-go/strategy"
	"github.com/easykafka/easykafka-go/tests/unit/helpers"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Partial batches: each message carries its own verdict
// =============================================================================

// TestPartialBatchOneFailureRoutesOneMessage is the gap this change closes: one
// poison message in a batch reaches the strategy alone, with its own error.
func TestPartialBatchOneFailureRoutesOneMessage(t *testing.T) {
	messages := make([]*types.Message, 50)
	for i := range messages {
		messages[i] = helpers.NewTestMessage("topic", 0, int64(i), "ok")
	}
	messages[17].Payload = []byte("poison")
	poisonErr := errors.New("cannot process poison")

	handler := func(_ context.Context, batch *types.Batch) *types.Failure {
		for _, item := range batch.Items() {
			if string(item.Message().Payload) == "poison" {
				item.Fail(types.Failure{Err: poisonErr})
			}
		}
		return nil
	}

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{}
	require.NoError(t, helpers.RunBatch(t, client, handler, strat, helpers.TestLogger()))

	calls := strat.HandleCalls()
	require.Len(t, calls, 1, "exactly one strategy call for one failed message")
	require.Len(t, calls[0].Msgs, 1)
	assert.Equal(t, int64(17), calls[0].Msgs[0].Offset)
	assert.Equal(t, poisonErr, calls[0].HandlerErr)
}

// TestPartialBatchResolvedFailuresStoreMaximum asserts, rather than assumes,
// that per-message routing leaves the offset path alone: once the strategy has
// resolved every failure, each partition stores its maximum — including a
// partition whose highest message is the one that failed.
func TestPartialBatchResolvedFailuresStoreMaximum(t *testing.T) {
	messages := []*types.Message{
		helpers.NewTestMessage("topic", 0, 10, "ok"),
		helpers.NewTestMessage("topic", 1, 20, "fail"),
		helpers.NewTestMessage("topic", 0, 11, "ok"),
		helpers.NewTestMessage("topic", 1, 21, "fail"),
	}

	handler := func(_ context.Context, batch *types.Batch) *types.Failure {
		for _, item := range batch.Items() {
			if string(item.Message().Payload) == "fail" {
				item.Fail(types.Failure{Err: errors.New("boom")})
			}
		}
		return nil
	}

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{}
	require.NoError(t, helpers.RunBatch(t, client, handler, strat, helpers.TestLogger()))

	assert.Len(t, strat.HandleCalls(), 2)
	assert.Equal(t, map[int32]int64{0: 11, 1: 21}, helpers.StoredByPartition(client.StoredOffsets()))
}

// TestPartialBatchStrategyErrorStoresNothing verifies that a strategy error
// part-way through the walk stops dispatch there and stores nothing at all —
// not even for a partition that had no failure.
func TestPartialBatchStrategyErrorStoresNothing(t *testing.T) {
	messages := []*types.Message{
		helpers.NewTestMessage("topic", 0, 0, "fail-a"),
		helpers.NewTestMessage("topic", 1, 0, "ok"), // partition 1 never fails
		helpers.NewTestMessage("topic", 0, 1, "fail-b"),
		helpers.NewTestMessage("topic", 0, 2, "fail-c"),
	}

	handler := func(_ context.Context, batch *types.Batch) *types.Failure {
		for _, item := range batch.Items() {
			if string(item.Message().Payload) != "ok" {
				item.Fail(types.Failure{Err: errors.New("boom")})
			}
		}
		return nil
	}

	client := &helpers.MockKafkaClient{Messages: messages}
	// The strategy fails on the second failed message (fail-b): fail-a is already
	// routed, fail-c is never offered.
	strat := &helpers.StopAtStrategy{StopAt: 2}
	err := helpers.RunBatch(t, client, handler, strat, helpers.TestLogger())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "error strategy")

	calls := strat.Calls()
	require.Len(t, calls, 2, "dispatch must stop at the first strategy error")
	assert.Equal(t, "fail-a", string(calls[0].Msgs[0].Payload))
	assert.Equal(t, "fail-b", string(calls[1].Msgs[0].Payload))
	assert.Empty(t, client.StoredOffsets(), "no partition may store an offset")
}

// TestPartialBatchWholeFailureDiscardsMarks verifies a returned *Failure routes
// every message under it in one call, ignoring verdicts already recorded.
func TestPartialBatchWholeFailureDiscardsMarks(t *testing.T) {
	messages := []*types.Message{
		helpers.NewTestMessage("topic", 0, 0, "a"),
		helpers.NewTestMessage("topic", 0, 1, "b"),
		helpers.NewTestMessage("topic", 0, 2, "c"),
	}
	dbDown := errors.New("database unreachable")

	handler := func(_ context.Context, batch *types.Batch) *types.Failure {
		batch.Items()[0].Fail(types.Failure{Err: errors.New("item error"), Code: "item"})
		return &types.Failure{Err: dbDown, Step: 3, Code: "db_down"}
	}

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{}
	require.NoError(t, helpers.RunBatch(t, client, handler, strat, helpers.TestLogger()))

	// One call, not three: all three messages go together under the one
	// batch-level Failure.
	calls := strat.HandleCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"a", "b", "c"}, helpers.PayloadsOf(calls[0].Msgs))
	assert.Equal(t, types.Failure{Err: dbDown, Step: 3, Code: "db_down"}, calls[0].Failure)

	// a's own mark ("item") is discarded, not routed alongside the batch failure.
	assert.NotEqual(t, "item", calls[0].Failure.Code)
	assert.Equal(t, map[int32]int64{0: 2}, helpers.StoredByPartition(client.StoredOffsets()))
}

// TestPartialBatchPanicVoidsBatch verifies a panic behaves exactly like a
// returned failure: every message, one call, marks discarded, no step or code.
func TestPartialBatchPanicVoidsBatch(t *testing.T) {
	messages := []*types.Message{
		helpers.NewTestMessage("topic", 0, 0, "a"),
		helpers.NewTestMessage("topic", 0, 1, "b"),
	}

	handler := func(_ context.Context, batch *types.Batch) *types.Failure {
		batch.Items()[0].Fail(types.Failure{Err: errors.New("item error"), Step: 1, Code: "item"})
		panic("half-way through")
	}

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{}
	require.NoError(t, helpers.RunBatch(t, client, handler, strat, helpers.TestLogger()))

	calls := strat.HandleCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"a", "b"}, helpers.PayloadsOf(calls[0].Msgs))
	assert.Contains(t, calls[0].HandlerErr.Error(), "handler panic: half-way through")
	assert.Zero(t, calls[0].Failure.Step)
	assert.Empty(t, calls[0].Failure.Code)
	assert.Equal(t, map[int32]int64{0: 1}, helpers.StoredByPartition(client.StoredOffsets()))
}

// TestPartialBatchFailureWithoutErrorStillRoutes verifies a Failure with no Err
// is routed under ErrUnspecified, with a warning naming the offset, and the
// offset advances as for any other resolved failure. The batch-level
// counterpart is [TestWholeBatchFailureWithoutErrorStillRoutes].
func TestPartialBatchFailureWithoutErrorStillRoutes(t *testing.T) {
	messages := []*types.Message{
		helpers.NewTestMessage("topic", 0, 0, "a"),
		helpers.NewTestMessage("topic", 0, 1, "b"),
	}

	handler := func(_ context.Context, batch *types.Batch) *types.Failure {
		batch.Items()[1].Fail(types.Failure{})
		return nil
	}

	var logs bytes.Buffer
	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{}
	require.NoError(t, helpers.RunBatch(t, client, handler, strat, zerolog.New(&logs)))

	calls := strat.HandleCalls()
	require.Len(t, calls, 1)
	// The sentinel the engine substitutes for the missing error.
	require.ErrorIs(t, calls[0].HandlerErr, types.ErrUnspecified)
	// The same sentinel through the public re-export an application compares
	// against, so a broken re-export fails here too.
	require.ErrorIs(t, calls[0].HandlerErr, easykafka.ErrUnspecified)
	assert.Equal(t, map[int32]int64{0: 1}, helpers.StoredByPartition(client.StoredOffsets()))

	assert.Contains(t, logs.String(), `"level":"warn"`)
	assert.Contains(t, logs.String(), "without an error")
	assert.Contains(t, logs.String(), `"offset":1`)
}

// TestWholeBatchFailureWithoutErrorStillRoutes is the batch-level counterpart of
// [TestPartialBatchFailureWithoutErrorStillRoutes]: a whole-batch Failure with no
// Err is routed under ErrUnspecified too.
func TestWholeBatchFailureWithoutErrorStillRoutes(t *testing.T) {
	messages := []*types.Message{helpers.NewTestMessage("topic", 0, 0, "a")}

	handler := func(_ context.Context, _ *types.Batch) *types.Failure {
		return &types.Failure{}
	}

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{}
	require.NoError(t, helpers.RunBatch(t, client, handler, strat, helpers.TestLogger()))

	calls := strat.HandleCalls()
	require.Len(t, calls, 1)
	assert.ErrorIs(t, calls[0].HandlerErr, types.ErrUnspecified)
}

// TestPartialBatchFailingTwiceKeepsLast verifies the last verdict wins.
func TestPartialBatchFailingTwiceKeepsLast(t *testing.T) {
	messages := []*types.Message{helpers.NewTestMessage("topic", 0, 0, "a")}
	final := errors.New("refined")

	handler := func(_ context.Context, batch *types.Batch) *types.Failure {
		item := batch.Items()[0]
		item.Fail(types.Failure{Err: errors.New("first"), Code: "first"})
		item.Fail(types.Failure{Err: final, Code: "second"})
		return nil
	}

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{}
	require.NoError(t, helpers.RunBatch(t, client, handler, strat, helpers.TestLogger()))

	calls := strat.HandleCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, final, calls[0].HandlerErr)
	assert.Equal(t, "second", calls[0].Failure.Code)
}

// TestPartialBatchFailuresInBatchOrder verifies failures reach the strategy in
// batch order, partitions interleaved as polled, with nothing regrouped.
func TestPartialBatchFailuresInBatchOrder(t *testing.T) {
	messages := []*types.Message{
		helpers.NewTestMessage("topic", 1, 5, "p1-5"),
		helpers.NewTestMessage("topic", 0, 9, "p0-9"),
		helpers.NewTestMessage("topic", 1, 6, "p1-6"),
		helpers.NewTestMessage("topic", 2, 1, "p2-1"),
		helpers.NewTestMessage("topic", 0, 10, "p0-10"),
	}

	handler := func(_ context.Context, batch *types.Batch) *types.Failure {
		for _, item := range batch.Items() {
			item.Fail(types.Failure{Err: errors.New("x")})
		}
		return nil
	}

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{}
	require.NoError(t, helpers.RunBatch(t, client, handler, strat, helpers.TestLogger()))

	calls := strat.HandleCalls()
	require.Len(t, calls, len(messages), "one strategy call per failed message")

	var order []string
	for _, c := range calls {
		require.Len(t, c.Msgs, 1, "each strategy call carries a single message")
		order = append(order, string(c.Msgs[0].Payload))
	}
	// Equal, not ElementsMatch: the order must match the batch exactly, which is
	// the point of this test.
	assert.Equal(t, helpers.PayloadsOf(messages), order)
}

// TestPartialBatchMessageCopyCannotMoveOffsets verifies a handler that mutates
// the Message it was given cannot change which offset is committed, nor what
// the strategy is told about the message's position.
func TestPartialBatchMessageCopyCannotMoveOffsets(t *testing.T) {
	messages := []*types.Message{
		helpers.NewTestMessage("topic", 0, 3, "a"),
		helpers.NewTestMessage("topic", 0, 4, "b"),
	}

	handler := func(_ context.Context, batch *types.Batch) *types.Failure {
		for _, item := range batch.Items() {
			msg := item.Message()
			msg.Offset = 1_000_000
			msg.Partition = 99
			msg.Topic = "elsewhere"
		}
		batch.Items()[0].Fail(types.Failure{Err: errors.New("x")})
		return nil
	}

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{}
	require.NoError(t, helpers.RunBatch(t, client, handler, strat, helpers.TestLogger()))

	// Full records, not StoredByPartition: the handler also tampers with the
	// topic, which that helper drops, and exactly one store must happen.
	assert.Equal(t, []helpers.StoreRecord{{Topic: "topic", Partition: 0, Offset: 4}}, client.StoredOffsets())
	calls := strat.HandleCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(3), calls[0].Msgs[0].Offset)
	assert.Equal(t, "topic", calls[0].Msgs[0].Topic)
}

// TestPartialBatchConcurrentFailIsRaceFree fails distinct items from several
// goroutines. It only means something under -race, which CI runs.
//
// A batch handler may process its items in parallel. Fail is a plain write to
// that one item's own field, so distinct items share nothing and need no lock.
// The handler must keep to two rules: never fail the same item from two
// goroutines, and join them all before returning — the engine reads the
// verdicts as soon as the handler returns, and a verdict still being written
// could be missed, committing that message as a success. Routing order and
// offsets are unaffected: the engine walks the items in batch order afterwards.
func TestPartialBatchConcurrentFailIsRaceFree(t *testing.T) {
	messages := make([]*types.Message, 64)
	for i := range messages {
		messages[i] = helpers.NewTestMessage("topic", int32(i%4), int64(i), "m")
	}

	handler := func(_ context.Context, batch *types.Batch) *types.Failure {
		var wg sync.WaitGroup
		for i, item := range batch.Items() {
			if i%2 == 0 {
				continue
			}
			wg.Add(1)
			go func(item *types.BatchItem) {
				defer wg.Done()
				item.Fail(types.Failure{Err: errors.New("odd")})
			}(item)
		}
		wg.Wait() // joining before return is the handler's obligation
		return nil
	}

	client := &helpers.MockKafkaClient{Messages: messages}
	strat := &helpers.MockStrategy{}
	require.NoError(t, helpers.RunBatch(t, client, handler, strat, helpers.TestLogger()))

	assert.Len(t, strat.HandleCalls(), 32)
}

// TestBatchRangeOverItemsTakesEffect checks that Items returns pointers, so the
// usual handler loop — range over Items, call Fail — records the verdict on
// the batch's own item.
//
// The handler always gets the batch itself, not a copy. The problem is the
// copy made by the range loop: `for _, item := range s` copies each element
// of s into the loop variable item.
//
//   - Items returns []*BatchItem, so item is a copy of a pointer. It still
//     points at the batch's own item, and item.Fail writes there.
//   - If Items returned []BatchItem instead, item would be a copy of the whole
//     struct. item.Fail would still compile and run — Go takes &item for the
//     pointer receiver — but it would change only the loop variable. The
//     batch's element would stay unchanged, the engine would see no failure,
//     and the message would be committed as a success. There would be no
//     error and no warning.
//
// Indexing in place (items[i].Fail) works in both cases; only the usual range
// form breaks. Returning pointers means the obvious way to write a handler is
// also the correct one. This test fails if Items ever stops returning them.
//
// It also shows the intended way to test a batch handler: NewBatch, call the
// handler directly, check Failed on each item — no broker.
func TestBatchRangeOverItemsTakesEffect(t *testing.T) {
	handler := func(_ context.Context, batch *easykafka.Batch) *easykafka.Failure {
		for _, item := range batch.Items() {
			if string(item.Message().Payload) == "bad" {
				item.Fail(easykafka.Failure{Err: errors.New("bad payload")})
			}
		}
		return nil
	}

	batch := easykafka.NewBatch([]easykafka.Message{
		{Offset: 0, Payload: []byte("good")},
		{Offset: 1, Payload: []byte("bad")},
		{Offset: 2, Payload: []byte("good")},
	})
	require.Nil(t, handler(context.Background(), batch))

	require.Equal(t, 3, batch.Len())
	assert.Nil(t, batch.Items()[0].Failed())
	require.NotNil(t, batch.Items()[1].Failed())
	require.EqualError(t, batch.Items()[1].Failed().Err, "bad payload")
	assert.Nil(t, batch.Items()[2].Failed())
}

// TestBatchItemCarriesKeyAndHeaders verifies a batch handler sees the whole
// message, not just the payload.
func TestBatchItemCarriesKeyAndHeaders(t *testing.T) {
	msg := helpers.NewTestMessage("topic", 2, 7, "p")
	msg.Key = []byte("slip-1")
	msg.Headers[metadata.HeaderRetryStep] = "2"

	var seen types.Message
	handler := func(_ context.Context, batch *types.Batch) *types.Failure {
		seen = batch.Items()[0].Message()
		return nil
	}

	client := &helpers.MockKafkaClient{Messages: []*types.Message{msg}}
	require.NoError(t, helpers.RunBatch(t, client, handler, &helpers.MockStrategy{}, helpers.TestLogger()))

	assert.Equal(t, []byte("slip-1"), seen.Key)
	assert.Equal(t, int32(2), easykafka.GetRetryStep(&seen))
	assert.Equal(t, int32(2), seen.Partition)
	assert.Equal(t, int64(7), seen.Offset)
}

// TestNilFailurePointerIsSuccess pins the reason handlers return *Failure
// rather than error: a typed nil left unset on the success path is success.
func TestNilFailurePointerIsSuccess(t *testing.T) {
	single := func(_ context.Context, payload []byte) *types.Failure {
		var f *types.Failure // set only on the failure path
		if string(payload) == "bad" {
			f = &types.Failure{Err: errors.New("bad")}
		}
		return f
	}
	batchHandler := func(_ context.Context, _ *types.Batch) *types.Failure {
		var f *types.Failure
		return f
	}

	t.Run("single", func(t *testing.T) {
		client := &helpers.MockKafkaClient{Messages: []*types.Message{helpers.NewTestMessage("topic", 0, 0, "good")}}
		strat := &helpers.MockStrategy{}
		eng := engine.NewEngine(client, single, strat, helpers.TestLogger(), 10)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		require.NoError(t, eng.Start(ctx))
		assert.Empty(t, strat.HandleCalls())
	})

	t.Run("batch", func(t *testing.T) {
		client := &helpers.MockKafkaClient{Messages: []*types.Message{helpers.NewTestMessage("topic", 0, 0, "good")}}
		strat := &helpers.MockStrategy{}
		require.NoError(t, helpers.RunBatch(t, client, batchHandler, strat, helpers.TestLogger()))
		assert.Empty(t, strat.HandleCalls())
	})
}

// TestSingleAndBatchHandlersWriteStepAndCodeAlike verifies a single-message
// handler returning a Failure with a step and a code produces the same headers
// as a batch handler recording that Failure with item.Fail.
func TestSingleAndBatchHandlersWriteStepAndCodeAlike(t *testing.T) {
	failure := types.Failure{Err: errors.New("publish failed"), Step: 2, Code: "x"}
	newRetry := func() (*strategy.RetryStrategy, *helpers.MockProducer) {
		retryProd := &helpers.MockProducer{}
		cfg := strategy.RetryConfig{
			RetryTopic: "t.retry", DLQTopic: "t.dlq", MaxAttempts: 3,
			InitialDelay: time.Second, MaxDelay: time.Second, Multiplier: 1,
		}
		return strategy.NewRetryStrategyWithProducers(cfg, retryProd, &helpers.MockProducer{}, zerolog.Nop()), retryProd
	}

	// Single-message mode.
	singleStrat, singleProd := newRetry()
	single := func(_ context.Context, _ []byte) *types.Failure {
		f := failure
		return &f
	}
	client := &helpers.MockKafkaClient{Messages: []*types.Message{helpers.NewTestMessage("topic", 0, 0, "m")}}
	eng := engine.NewEngine(client, single, singleStrat, helpers.TestLogger(), 10)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, eng.Start(ctx))

	// Batch mode.
	batchStrat, batchProd := newRetry()
	batchHandler := func(_ context.Context, batch *types.Batch) *types.Failure {
		batch.Items()[0].Fail(failure)
		return nil
	}
	client = &helpers.MockKafkaClient{Messages: []*types.Message{helpers.NewTestMessage("topic", 0, 0, "m")}}
	require.NoError(t, helpers.RunBatch(t, client, batchHandler, batchStrat, helpers.TestLogger()))

	for name, prod := range map[string]*helpers.MockProducer{"single": singleProd, "batch": batchProd} {
		t.Run(name, func(t *testing.T) {
			produced := prod.Messages()
			require.Len(t, produced, 1)
			assert.Equal(t, "2", produced[0].Headers[metadata.HeaderRetryStep])
			assert.Equal(t, "x", produced[0].Headers[metadata.HeaderErrorCode])
			assert.Equal(t, "publish failed", produced[0].Headers[metadata.HeaderErrorMessage])
		})
	}
}
