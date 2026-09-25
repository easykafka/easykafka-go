package unit

import (
	"context"
	"errors"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWaitUntilRetryTimeReturnsAtOnceWithoutRetryTime covers every message that
// is not waiting on anything: a first delivery with no retry header, one whose
// header does not parse, and a nil message. None of them may block.
func TestWaitUntilRetryTimeReturnsAtOnceWithoutRetryTime(t *testing.T) {
	cases := map[string]*easykafka.Message{
		"first delivery": {Topic: "orders", Headers: map[string]string{}},
		"no headers":     {Topic: "orders"},
		"unparseable":    {Topic: "orders.retry", Headers: map[string]string{easykafka.HeaderRetryTime: "soon"}},
		"nil message":    nil,
	}

	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			require.NoError(t, easykafka.WaitUntilRetryTime(context.Background(), msg))
			assert.Less(t, time.Since(start), 50*time.Millisecond)
		})
	}
}

// TestWaitUntilRetryTimeReturnsAtOnceWhenAlreadyDue verifies a record whose
// retry time has passed is processed straight away.
func TestWaitUntilRetryTimeReturnsAtOnceWhenAlreadyDue(t *testing.T) {
	msg := &easykafka.Message{
		Topic:   "orders.retry",
		Headers: map[string]string{easykafka.HeaderRetryTime: time.Now().Add(-time.Minute).Format(time.RFC3339)},
	}

	start := time.Now()
	require.NoError(t, easykafka.WaitUntilRetryTime(context.Background(), msg))
	assert.Less(t, time.Since(start), 50*time.Millisecond)
}

// TestWaitUntilRetryTimeWaitsForTheRetryTime builds the record's headers with
// the retry strategy's own header builder, so the test waits on exactly what
// the library writes.
//
// The header stores the retry time to the whole second. So the test first waits
// for the next whole second and sets the retry time exactly 3 seconds later:
// the header then holds it unchanged, and the expected wait is simply 3s.
func TestWaitUntilRetryTimeWaitsForTheRetryTime(t *testing.T) {
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	due := time.Now().Truncate(time.Second).Add(3 * time.Second)

	source := &types.Message{Topic: "orders", Headers: map[string]string{}}
	msg := &easykafka.Message{
		Topic:   "orders.retry",
		Headers: metadata.BuildRetryHeaders(source, 1, due, types.Failure{Err: errors.New("x")}),
	}

	start := time.Now()
	require.NoError(t, easykafka.WaitUntilRetryTime(context.Background(), msg))
	elapsed := time.Since(start)

	// Expect 2.9s <= elapsed < 3.5s: 3s plus margins for a late wake-up from
	// the sleep above and for timer/scheduling delay.
	assert.GreaterOrEqual(t, elapsed, 3*time.Second-100*time.Millisecond, "returned before the retry time")
	assert.Less(t, elapsed, 3*time.Second+500*time.Millisecond, "waited well past the retry time")
}

// TestWaitUntilRetryTimeStopsWhenCancelled verifies cancellation releases a
// waiting handler at once with the context's error, rather than holding
// shutdown until the record is due.
func TestWaitUntilRetryTimeStopsWhenCancelled(t *testing.T) {
	msg := &easykafka.Message{
		Topic:   "orders.retry",
		Headers: map[string]string{easykafka.HeaderRetryTime: time.Now().Add(time.Hour).Format(time.RFC3339)},
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	err := easykafka.WaitUntilRetryTime(ctx, msg)

	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), time.Second, "cancellation must not wait for the retry time")
}

// TestWaitUntilRetryTimeAlreadyCancelled verifies a context cancelled before
// the call returns its error without waiting, when the record is not yet due.
func TestWaitUntilRetryTimeAlreadyCancelled(t *testing.T) {
	msg := &easykafka.Message{
		Topic:   "orders.retry",
		Headers: map[string]string{easykafka.HeaderRetryTime: time.Now().Add(time.Hour).Format(time.RFC3339)},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, easykafka.WaitUntilRetryTime(ctx, msg), context.Canceled)
}
