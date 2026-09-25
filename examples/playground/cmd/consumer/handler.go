package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/examples/playground/internal/script"
)

// handler carries out each message's payload script. One handler serves one
// consumer; name tells the log lines of the source and retry consumers apart.
type handler struct {
	name            string
	processingDelay time.Duration
	out             *printer
}

// resolved is a message together with the outcome its script gives for the
// current attempt, or the reason its payload could not be parsed.
type resolved struct {
	msg     easykafka.Message
	outcome script.Outcome
	err     error
}

// handle is the single-message handler.
func (h *handler) handle(ctx context.Context, _ []byte) *easykafka.Failure {
	msg, ok := easykafka.MessageFromContext(ctx)
	if !ok {
		return &easykafka.Failure{Err: errors.New("no message on the handler context")}
	}

	if err := easykafka.WaitUntilRetryTime(ctx, msg); err != nil {
		h.line(msg, "-", "interrupted while waiting for its retry time")
		return &easykafka.Failure{Err: err}
	}
	h.delay()

	r := h.resolve(*msg)
	if r.err == nil && r.outcome.Kind == script.Panic {
		h.line(&r.msg, r.outcome.Token, "panic")
		panic(fmt.Sprintf("scripted panic for key %s", r.msg.Key))
	}
	return h.carryOut(r)
}

// handleBatch is the batch handler.
func (h *handler) handleBatch(ctx context.Context, batch *easykafka.Batch) *easykafka.Failure {
	items := batch.Items()
	h.batchLine(items)

	for _, item := range items {
		msg := item.Message()
		if err := easykafka.WaitUntilRetryTime(ctx, &msg); err != nil {
			h.out.printf("%s [%s] batch interrupted while waiting for a retry time\n", now(), h.name)
			return &easykafka.Failure{Err: err}
		}
	}
	h.delay()

	// Resolve every item first: a batchko or a panic anywhere in the batch
	// decides the fate of all of it, so it has to be known before any item's
	// own verdict is recorded.
	all := make([]resolved, len(items))
	for i, item := range items {
		all[i] = h.resolve(item.Message())
	}

	for _, r := range all {
		if r.err != nil {
			continue
		}
		switch r.outcome.Kind {
		case script.BatchKo:
			for _, each := range all {
				h.line(&each.msg, tokenOf(each), fmt.Sprintf("batch failed (batchko on %s)", r.msg.Key))
			}
			return &easykafka.Failure{
				Err:  fmt.Errorf("scripted batch failure (batchko on %s)", r.msg.Key),
				Step: r.outcome.Step,
				Code: r.outcome.Code,
			}
		case script.Panic:
			for _, each := range all {
				h.line(&each.msg, tokenOf(each), fmt.Sprintf("batch failed (panic on %s)", r.msg.Key))
			}
			panic(fmt.Sprintf("scripted panic for key %s", r.msg.Key))
		default:
		}
	}

	for i, item := range items {
		if f := h.carryOut(all[i]); f != nil {
			item.Fail(*f)
		}
	}
	return nil
}

// resolve parses msg's script and picks the outcome for the attempt it is on.
func (h *handler) resolve(msg easykafka.Message) resolved {
	s, err := script.Parse(msg.Payload)
	if err != nil {
		return resolved{msg: msg, err: err}
	}
	return resolved{msg: msg, outcome: s.At(easykafka.GetRetryAttempt(&msg))}
}

// carryOut applies one message's outcome, logs it, and returns the failure to
// report, or nil for success. A panic is handled by the callers, because in
// batch mode it concerns the whole batch.
func (h *handler) carryOut(r resolved) *easykafka.Failure {
	if r.err != nil {
		h.line(&r.msg, "<malformed>", "perm ("+r.err.Error()+")")
		return &easykafka.Failure{Err: fmt.Errorf("%w: malformed payload: %v", easykafka.ErrPermanent, r.err)}
	}

	o := r.outcome
	switch o.Kind {
	case script.Ok:
		h.line(&r.msg, o.Token, "ok")
		return nil
	case script.Perm:
		h.line(&r.msg, o.Token, "perm")
		return &easykafka.Failure{
			Err:  fmt.Errorf("%w: scripted permanent failure", easykafka.ErrPermanent),
			Step: o.Step,
			Code: o.Code,
		}
	case script.Nil:
		h.line(&r.msg, o.Token, "nil (failure without an error)")
		return &easykafka.Failure{Step: o.Step, Code: o.Code}
	default: // script.Ko, and script.BatchKo outside batch mode
		result := "ko"
		if o.Kind == script.BatchKo {
			result = "ko (batchko acts as ko outside batch mode)"
		}
		h.line(&r.msg, o.Token, result)
		return &easykafka.Failure{Err: errors.New("scripted failure"), Step: o.Step, Code: o.Code}
	}
}

func (h *handler) delay() {
	if h.processingDelay > 0 {
		time.Sleep(h.processingDelay)
	}
}

// line prints one delivery: which consumer, the record's key and position, the
// attempt, step and code it arrived with, the token chosen and the result.
func (h *handler) line(msg *easykafka.Message, token, result string) {
	code := easykafka.GetErrorCode(msg)
	if code == "" {
		code = "-"
	}
	h.out.printf("%s [%s] key=%s topic=%s partition=%d offset=%d attempt=%d step=%d code=%s token=%s -> %s\n",
		now(), h.name, msg.Key, msg.Topic, msg.Partition, msg.Offset,
		easykafka.GetRetryAttempt(msg), easykafka.GetRetryStep(msg), code, token, result)
}

// batchLine prints the header line of a batch: its size and partitions.
func (h *handler) batchLine(items []*easykafka.BatchItem) {
	var partitions []int32
	for _, item := range items {
		if p := item.Message().Partition; !slices.Contains(partitions, p) {
			partitions = append(partitions, p)
		}
	}
	slices.Sort(partitions)
	h.out.printf("%s [%s] batch size=%d partitions=%v\n", now(), h.name, len(items), partitions)
}

func tokenOf(r resolved) string {
	if r.err != nil {
		return "<malformed>"
	}
	return r.outcome.Token
}

func now() string { return time.Now().Format("15:04:05.000") }

// printer serialises output lines, since the source and retry consumers print
// from their own goroutines.
type printer struct {
	mu sync.Mutex
}

func (p *printer) printf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprintf(os.Stdout, format, args...)
}
