# Data Race Analysis: TestShutdownEngineStopSignal

## Root Cause

`e.state` is a plain `int`-based field (`engineState int`) on the `Engine` struct — not protected by any mutex or atomic operation.

The race occurs between two goroutines:

- **Goroutine 97** (`runSingleLoop`, line 141): reads `e.state` in the loop condition `for e.state == engineStateRunning`
- **Goroutine 96** (`Stop`, line 343): writes `e.state = engineStateStopping`

These happen concurrently with no synchronization, which is a textbook data race.

## Explanation

The test calls `eng.Stop()` from the test goroutine while `eng.Start()` is running in a separate goroutine. `Stop()` writes to `e.state` at the same moment `runSingleLoop` reads it in the `for` loop condition. Go's race detector catches this because there's no happens-before guarantee between the two accesses.

## Suggested Fix

Replace the plain `engineState` field with `atomic.Int32` (from `sync/atomic`):

```go
import "sync/atomic"

type Engine struct {
    // ...
    state atomic.Int32  // replaces: state engineState
    // ...
}
```

Then replace all `e.state = X` and `e.state == X` with atomic load/store:

```go
// Read
e.state.Load() == int32(engineStateRunning)

// Write
e.state.Store(int32(engineStateStopping))
```

This makes all accesses to `state` safe across goroutines without needing a mutex. The `sync/atomic` package guarantees sequential consistency for these operations, which is all that's needed here since `state` transitions are one-directional.

Alternatively, you could protect `state` with a `sync.RWMutex` (read lock in the loop condition, write lock in `Stop`/`Start`), but `atomic.Int32` is simpler and more performant for a single integer field.