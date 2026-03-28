# Fix Implementation Plan: Data Race on `Engine.state`

## Problem Summary

`Engine.state` is a plain `engineState` (alias for `int`) field accessed concurrently without synchronization:
- **Read** in `runSingleLoop` (line 141) and `runBatchLoop` (line 206) — loop condition `for e.state == engineStateRunning`
- **Write** in `Stop` (line 343) — `e.state = engineStateStopping`

Go's race detector flags this as a data race because there is no happens-before guarantee between the two goroutines.

---

## Fix: Replace `engineState` field with `atomic.Int32`

### Step 1 — Change the `state` field in `Engine`

**File:** `internal/engine/engine.go`

Change:
```go
import (
    // existing imports...
)

type Engine struct {
    // ...
    state        engineState
    // ...
}

type engineState int
```

To:
```go
import (
    "sync/atomic"
    // existing imports...
)

type Engine struct {
    // ...
    state        atomic.Int32  // protected; use Load()/Store() everywhere
    // ...
}

type engineState = int32  // keep named alias so constants stay readable
```

> **Note:** `atomic.Int32` is a struct value and must not be copied. Since `Engine` is always used via pointer (`*Engine`), this is safe.

---

### Step 2 — Update the `engineState` constants

The constants must be typed as `int32` to match `atomic.Int32`:

```go
const (
    engineStateCreated  int32 = iota
    engineStateRunning
    engineStateStopping
    engineStateStopped
)
```

---

### Step 3 — Update `NewEngine` and `NewBatchEngine` constructors

Remove the explicit `state: engineStateCreated` initializer — `atomic.Int32` zero-value is `0`, which equals `engineStateCreated`. Both constructors become simpler:

```go
return &Engine{
    adapter:     adapter,
    handler:     handler,
    strategy:    strategy,
    logger:      logger,
    pollTimeout: pollTimeoutMs,
    // state zero-value == engineStateCreated, no explicit init needed
    done:        make(chan struct{}),
}
```

---

### Step 4 — Update every read of `e.state`

Replace all `e.state == X` comparisons with `e.state.Load() == X`:

| Location | Old | New |
|---|---|---|
| `Start` line 96 | `e.state != engineStateCreated` | `e.state.Load() != engineStateCreated` |
| `runSingleLoop` line 141 | `for e.state == engineStateRunning` | `for e.state.Load() == engineStateRunning` |
| `runSingleLoop` line 149 | `if e.state != engineStateRunning` | `if e.state.Load() != engineStateRunning` |
| `runBatchLoop` line 206 | `for e.state == engineStateRunning` | `for e.state.Load() == engineStateRunning` |
| `runBatchLoop` line 220 | `if e.state != engineStateRunning` | `if e.state.Load() != engineStateRunning` |
| `Stop` line 340 | `e.state != engineStateRunning` | `e.state.Load() != engineStateRunning` |

---

### Step 5 — Update every write to `e.state`

Replace all `e.state = X` assignments with `e.state.Store(X)`:

| Location | Old | New |
|---|---|---|
| `Start` line 100 | `e.state = engineStateRunning` | `e.state.Store(engineStateRunning)` |
| `Start` line 128 | `e.state = engineStateStopped` | `e.state.Store(engineStateStopped)` |
| `runSingleLoop` line 144 | `e.state = engineStateStopping` | `e.state.Store(engineStateStopping)` |
| `runSingleLoop` line 159 | `e.state = engineStateStopping` | `e.state.Store(engineStateStopping)` |
| `runSingleLoop` line 180 | `e.state = engineStateStopping` | `e.state.Store(engineStateStopping)` |
| `runBatchLoop` line 209 | `e.state = engineStateStopping` | `e.state.Store(engineStateStopping)` |
| `runBatchLoop` line 229 | `e.state = engineStateStopping` | `e.state.Store(engineStateStopping)` |
| `runBatchLoop` line 245 | `e.state = engineStateStopping` | `e.state.Store(engineStateStopping)` |
| `Stop` line 343 | `e.state = engineStateStopping` | `e.state.Store(engineStateStopping)` |

---

### Step 6 — Verify no other files reference `e.state` directly

Run:
```bash
grep -rn "\.state" internal/engine/
```

Confirm that all occurrences are updated and none remain as plain field accesses.

---

## Verification

```bash
# Run unit tests with race detector (same flag used in CI)
go test -race -v -count=1 -run TestShutdownEngineStopSignal ./tests/unit/...

# Run full unit test suite with race detector
go test -race -v -count=1 ./tests/unit/...

# Run integration tests to confirm no regressions
go test -v -count=1 -timeout 1000s ./tests/integration/...
```

---

## Why `atomic.Int32` and not a mutex

- `state` is a single integer with one-directional transitions (`Created → Running → Stopping → Stopped`). No compound read-modify-write operation needs to be atomic as a group; each individual access just needs to be safe.
- `atomic.Int32` is zero-allocation, branch-free, and requires no lock/unlock pairing.
- A `sync.RWMutex` would also be correct but adds boilerplate (`RLock`/`RUnlock` on every read, `Lock`/`Unlock` on every write) with no benefit for a single integer field.

---

## Files Changed


| File | Change |
|---|---|
| `internal/engine/engine.go` | All changes above (field type, constants, reads, writes) |

No other files require changes. The `engineState` type and constants are package-private and not used outside `internal/engine/`.