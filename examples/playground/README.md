# Playground

A local setup for trying easykafka by hand: a Kafka broker, a web UI to look inside it, and a
producer and consumer you drive from the command line. Every message's payload says how the
consumer should treat it — `ko/ko/ok` fails twice and then succeeds — so you can watch the retry
topic, the DLQ, batch routing and committed offsets as they happen.

> **Status: planned.** This README describes the playground as designed; the code is not written
> yet, so the commands below do not run.

## Quick start

From this directory:

```bash
docker compose up -d                          # Kafka, AKHQ, topics, and a consumer with default flags
docker compose logs -f consumer               # watch the consumer
docker compose run --rm producer ok ko/ok     # in another terminal: send two messages
```

Then open AKHQ at <http://localhost:8080> to see the records, their keys and headers, and the
consumer groups' offsets.

To run the consumer with other flags, stop the default one first — two consumers in the same group
would share the partitions between them:

```bash
docker compose stop consumer
docker compose run --rm consumer --mode batch --batch-size 10
```

The producer and consumer can also run on the host with `go run`, against the same broker on
`localhost:9092`. From the repository root:

```bash
go run ./examples/playground/cmd/consumer --mode batch
go run ./examples/playground/cmd/producer 9xok ko
```

When you are done, `docker compose down -v` removes everything, topics included.

## What is running

| Service | Purpose |
|---|---|
| `kafka` | Single-node broker (KRaft). Reachable as `kafka:29092` inside compose and `localhost:9092` from the host. |
| `kafka-init` | Creates the topics below, then exits. |
| `akhq` | Web UI at <http://localhost:8080>. |
| `consumer` | Runs the source and retry consumers; started by `up`. |
| `producer` | Sends messages; not started by `up`, run it with `docker compose run --rm producer …`. |

| Topic | Partitions | Why |
|---|---|---|
| `demo.orders` | 3 | Source. More than one partition, so batches mix partitions. |
| `demo.orders.retry` | 1 | Retry topic. One partition keeps retry records in due-time order. |
| `demo.orders.dlq` | 1 | Dead-letter topic. |

## Payload scripts

A message's payload is a **script**: one outcome per delivery, separated by `/`. On each delivery
the consumer reads the record's attempt count (`GetRetryAttempt`, 0 on the first delivery) and
carries out the outcome at that position. If the script is shorter than the attempts, **the last
outcome repeats** — so `ko` fails forever and ends in the DLQ, and `ok` alone is a plain success.

### Syntax at a glance

| Symbol | Meaning | Example |
|---|---|---|
| space | messages — the shell splits the command line, one message per argument | `ok ko` is two messages |
| `/` | the delivery attempts of one message; the whole script is that message's payload | `ko/ok` fails once, then succeeds |
| `Nx` in front | repeats a message N times | `9xok` is nine messages, each `ok` |
| `:` after a token | adds a step and a code to that one attempt | `ko:step=2,code=publish_failed` |

All four together:

```
producer 2xok ko:step=1/ok batchko/ok
         └─┬┘ └────┬─────┘ └───┬────┘
           │       │           └─ 1 message: fails its whole batch once, then succeeds
           │       └─ 1 message: fails once with step 1, then succeeds
           └─ 2 messages, script "ok"
```

Four messages in total.

### Outcomes

| Token | Handler does | Shows |
|---|---|---|
| `ok` | succeeds | commit |
| `ko` | fails the message with `Failure{Err}` | the retry ladder |
| `perm` | fails it with `ErrPermanent` | straight to the DLQ, attempt 1 |
| `panic` | panics | panic recovery; in batch mode the whole batch fails |
| `batchko` | fails the whole batch (batch mode; acts as `ko` in single mode) | every message in the batch routed under one failure |
| `nil` | fails it with a `Failure` that has no error | routed under `ErrUnspecified`, with a warning in the log |
| `bin` | *producer word, not a token* — see below | a malformed record: straight to the DLQ with its bytes unchanged |

**`batchko` in more detail.** A batch handler reports failure in one of two ways: per message, with
`item.Fail` — what `ko` does — or for the batch as a whole, by returning a `*Failure`, as it would
when the database is down and nothing in the batch can be processed. `batchko` asks for the second:
if any message in the current delivery carries it, every message in the batch goes to the error
strategy under one failure. There is no batch in single-message mode, so there it acts as `ko`.

**Steps and codes.** A failing token can carry a step and a code after a colon:

```
ko:step=1,code=write_failed/ko:step=2,code=publish_failed/ko/ok
```

They become `Failure.Step` and `Failure.Code`, so you can check the retry record's
`easykafka.retry.step` and `easykafka.error.code` headers in AKHQ. The consumer logs the step each
delivery arrives with: here the third delivery arrives with step 2, and so does the fourth, because
a bare `ko` carries the step forward while its code falls back to `HANDLER_ERROR`. This script fails
three times, so it needs `--max-attempts 4` or more to reach its `ok`.

**Malformed records and `bin`.** A payload that does not parse as a script is treated as malformed:
the handler fails it with `ErrPermanent`, so it goes straight to the DLQ. To send one on purpose,
give the producer the word `bin`: it sends raw bytes that are not valid UTF-8 instead of the text
`bin`, so the DLQ record shows binary bytes surviving unchanged. `bin` stands for a whole message —
it works with `Nx` (`3xbin`) and mixes with scripts (`producer ok bin ko/ok`) — but never for one
attempt inside a script: `ko/bin` is sent as text and fails to parse as a whole.

## Producer

```
producer [flags] <script> [<script> ...]
```

Each argument is one message; `<N>x<script>` sends N copies. Writing a whole batch in one command
keeps it in one batch, instead of the consumer's batch timeout splitting it between two commands.
The prefix is `x` rather than `*` because the shell treats an unquoted `*` as a pattern.

| Flag | Default | Meaning |
|---|---|---|
| `--brokers` | `localhost:9092` | |
| `--topic` | `demo.orders` | |
| `--key-prefix` | `msg` | Keys are `<prefix>-0001`, `-0002`, …, so you can follow a message by key across topics. |
| `--partition P` | any | Send every message to partition P. |

```bash
producer ok ko/ok ko/ko/ok ko/ko/ko perm      # five independent cases
producer 49xok ko                             # 49 good messages and one poison
producer ok bin ko/ok                         # a malformed record between two normal ones
```

It prints one line per record sent: key, partition, offset, payload.

## Consumer

Runs two easykafka consumers in one process — one on the source topic, one on the retry topic —
with the same handler and strategy settings. easykafka writes to the retry topic but never reads
it, so the playground does; running both in one process gives you one log to watch.

| Flag | Default | Meaning |
|---|---|---|
| `--brokers` | `localhost:9092` | |
| `--topic` | `demo.orders` | Retry and DLQ topics are `<topic>.retry` and `<topic>.dlq`. |
| `--group` | `demo` | Source consumer group; the retry consumer uses `<group>-retry`. |
| `--mode` | `single` | `single` or `batch`. |
| `--batch-size` | 10 | Batch mode only. |
| `--batch-timeout` | 2s | Batch mode only. |
| `--strategy` | `retry` | `retry`, `skip` or `fail-fast`. |
| `--max-attempts` | 3 | Retry strategy. |
| `--initial-delay` | 2s | Retry backoff; long enough to see a record sit in the retry topic. |
| `--max-delay` | 30s | Retry backoff cap. |
| `--no-retry-consumer` | off | Do not consume the retry topic, so you can watch records accumulate. |
| `--processing-delay` | 0 (off) | Block for this long on each message (single mode) or each batch (batch mode), like a slow database call. Slows things down enough to follow records and consumer lag in AKHQ. |

**Waiting for the retry time.** Before processing, the handler calls
`easykafka.WaitUntilRetryTime`, which holds a retry-topic record until its `easykafka.retry.time`.
It returns at once for a record without one, so the same handler serves both consumers. A record
that is not due yet is never failed back — that would use up an attempt without processing it.

**Processing delay.** Applied once the handler is ready to process — after the retry-time wait,
just before the outcome is carried out. It is a plain blocking sleep, not interrupted by Ctrl+C,
like a real blocking call. It changes timing only, never an outcome.

**One log line per delivery:**

```
[retry] key=msg-0003 topic=demo.orders.retry partition=0 offset=4 attempt=2 step=2 code=publish_failed token=ok -> ok
```

— which consumer, the key, the record's position, the attempt and step it arrived with, the code of
the previous failure, the token chosen, and what the handler did. In batch mode each batch also gets
a line with its size and partitions.

Ctrl+C (or `docker compose stop consumer`) stops it by cancelling the context, as described in the
library's [Stopping](../../README.md#-stopping) section.

## Walkthrough

### Single-message mode

Several messages in one command still make sense here: the space only means "send several
messages", and the consumer mode decides how they are handled. In single mode each message is
handled on its own, so one command can carry several independent cases —
`producer ok ko/ok ko/ko/ok perm` runs four at once, each following its own path. A failing message
does not hold the others up, and several messages on one partition show the committed offset moving
forward message by message. Batch-only tokens have nothing to act on: `batchko` acts as `ko`, and
`panic` fails only its own message.

With the defaults — retry strategy, 3 attempts:

| Send | You should see |
|---|---|
| `ok` | handled once, committed; nothing in retry or DLQ |
| `ko/ok` | one retry record, `attempt=1`; succeeds on the retry consumer |
| `ko/ko/ok` | two retry records, `attempt=1` then `2`; succeeds on the third delivery |
| `ko/ko/ko` or `ko` | two retry records, then the DLQ with `attempt=3` |
| `perm` | DLQ at once, `attempt=1`; nothing in retry |
| `bin` | DLQ at once; the body in AKHQ is the original bytes |
| `ko:step=1,code=write_failed/ko:step=2,code=publish_failed/ko/ok`, with `--max-attempts 4` | step and code change on each hop; the last delivery arrives with step 2 and `HANDLER_ERROR`, and succeeds |
| `nil` | treated as a failure with `ErrUnspecified`; a warning in the log |

### Batch mode

With `--mode batch --batch-size 10`:

| Send | You should see |
|---|---|
| `9xok ko` | only the `ko` message reaches the retry topic; the other nine commit |
| `9xok bin` | the malformed record goes straight to the DLQ, `attempt=1`; the other nine commit; nothing in retry |
| `9xok batchko/ok` | all ten go to the retry topic under one failure, each with `attempt=1`; on the retry consumer all ten succeed |
| `9xok panic/ok` | same as `batchko/ok`: a panic fails the whole batch |
| `--partition 0 5xok`, then `--partition 1 5xko` | both partitions' committed offsets advance in AKHQ once the failures are routed |

Note the `/ok` after `batchko` and `panic`. A bare `batchko` repeats on every delivery, and the
retry consumer runs the same handler, so every retry batch containing it fails whole again: healthy
messages that land in the same retry batch are dragged along and end in the DLQ with it. Worth
seeing once, as the cost of a batch-level failure.

### Other strategies

| Consumer flags | Send | You should see |
|---|---|---|
| `--strategy skip` | `ko` | logged and committed; nothing in retry or DLQ |
| `--strategy fail-fast --mode batch` | a batch with one `ko` | the consumer exits with the error; the committed offset in AKHQ does not move; restarting redelivers the whole batch |
| `--no-retry-consumer` | `ko/ok` | the record sits in `demo.orders.retry` and is never processed |
