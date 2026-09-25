// Package script parses a playground message payload: one outcome per
// delivery attempt, separated by "/", such as "ko/ko/ok".
package script

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kind is what the handler does with a message on one delivery.
type Kind string

const (
	Ok      Kind = "ok"      // succeed
	Ko      Kind = "ko"      // fail the message
	Perm    Kind = "perm"    // fail it with ErrPermanent
	Panic   Kind = "panic"   // panic
	BatchKo Kind = "batchko" // fail the whole batch; acts as Ko in single-message mode
	Nil     Kind = "nil"     // fail it with a Failure that has no error
)

// Outcome is one attempt's instruction: what to do and, for a failure, the step
// and code to report.
type Outcome struct {
	Kind  Kind
	Step  int32
	Code  string
	Token string // the token as written, for logging
}

// Script is a message's outcomes in delivery order.
type Script []Outcome

// Parse reads a payload into a Script. A payload that is not valid UTF-8, is
// empty, or contains an unknown token or a bad modifier is an error: the
// consumer treats it as a malformed record.
func Parse(payload []byte) (Script, error) {
	if !utf8.Valid(payload) {
		return nil, errors.New("payload is not valid UTF-8")
	}
	text := strings.TrimSpace(string(payload))
	if text == "" {
		return nil, errors.New("payload is empty")
	}

	tokens := strings.Split(text, "/")
	s := make(Script, 0, len(tokens))
	for i, token := range tokens {
		outcome, err := parseToken(token)
		if err != nil {
			return nil, fmt.Errorf("token %d %q: %w", i+1, token, err)
		}
		s = append(s, outcome)
	}
	return s, nil
}

// At returns the outcome for the given attempt, counted from 0. Past the end of
// the script, the last outcome repeats.
func (s Script) At(attempt int) Outcome {
	if attempt >= len(s) {
		return s[len(s)-1]
	}
	return s[attempt]
}

// parseToken reads one token: a kind, optionally followed by ":step=N,code=C".
func parseToken(token string) (Outcome, error) {
	name, modifiers, hasModifiers := strings.Cut(token, ":")
	outcome := Outcome{Kind: Kind(name), Token: token}

	switch outcome.Kind {
	case Ok, Panic:
		if hasModifiers {
			return Outcome{}, fmt.Errorf("%q takes no step or code", name)
		}
		return outcome, nil
	case Ko, Perm, BatchKo, Nil:
	default:
		return Outcome{}, fmt.Errorf("unknown token %q", name)
	}

	if !hasModifiers {
		return outcome, nil
	}
	for modifier := range strings.SplitSeq(modifiers, ",") {
		key, value, ok := strings.Cut(modifier, "=")
		if !ok || value == "" {
			return Outcome{}, fmt.Errorf("modifier %q is not key=value", modifier)
		}
		switch key {
		case "step":
			step, err := strconv.ParseInt(value, 10, 32)
			if err != nil || step < 1 {
				return Outcome{}, fmt.Errorf("step %q is not a positive number", value)
			}
			outcome.Step = int32(step)
		case "code":
			outcome.Code = value
		default:
			return Outcome{}, fmt.Errorf("unknown modifier %q", key)
		}
	}
	return outcome, nil
}
