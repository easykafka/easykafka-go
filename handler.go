package easykafka

import (
	"github.com/easykafka/easykafka-go/internal/types"
)

// Handler is a re-export from internal/types
type Handler = types.Handler

// BatchHandler is a re-export from internal/types
type BatchHandler = types.BatchHandler

// ErrorStrategy is a re-export from internal/types
type ErrorStrategy = types.ErrorStrategy

// Message is a re-export from internal/types
type Message = types.Message

// PayloadEncoding is a re-export from internal/types
type PayloadEncoding = types.PayloadEncoding

const (
	PayloadEncodingJSON   = types.PayloadEncodingJSON
	PayloadEncodingBase64 = types.PayloadEncodingBase64
)
