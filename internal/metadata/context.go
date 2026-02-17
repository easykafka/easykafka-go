package metadata

import (
	"context"

	"github.com/easykafka/easykafka-go/internal/types"
)

// ContextKey is a type for context keys.
type ContextKey string

const messageKey ContextKey = "message"

// MessageFromContext extracts message metadata from a handler context.
// Returns nil and false if message is not found in context.
func MessageFromContext(ctx context.Context) (*types.Message, bool) {
	msg, ok := ctx.Value(messageKey).(*types.Message)
	return msg, ok
}

// WithMessage returns a new context with the message attached.
func WithMessage(ctx context.Context, msg *types.Message) context.Context {
	return context.WithValue(ctx, messageKey, msg)
}
