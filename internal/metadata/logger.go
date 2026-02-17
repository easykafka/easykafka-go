package metadata

// NoOpLogger is a logger that discards all messages.
type NoOpLogger struct{}

// Debug logs a debug message (no-op).
func (n *NoOpLogger) Debug(msg string, keyvals ...any) {}

// Info logs an info message (no-op).
func (n *NoOpLogger) Info(msg string, keyvals ...any) {}

// Warn logs a warning message (no-op).
func (n *NoOpLogger) Warn(msg string, keyvals ...any) {}

// Error logs an error message (no-op).
func (n *NoOpLogger) Error(msg string, keyvals ...any) {}

// NewNoOpLogger creates a new no-op logger instance.
func NewNoOpLogger() *NoOpLogger {
	return &NoOpLogger{}
}
