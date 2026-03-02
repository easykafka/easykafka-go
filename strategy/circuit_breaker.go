package strategy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/rs/zerolog"
)

// CircuitState represents the state of a circuit breaker.
type CircuitState int

const (
	// CircuitClosed is normal operation — messages are processed.
	CircuitClosed CircuitState = iota
	// CircuitOpen means the circuit is tripped — messages are rejected.
	CircuitOpen
	// CircuitHalfOpen means testing recovery — cautiously processing messages.
	CircuitHalfOpen
)

// String returns a human-readable circuit state name.
func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// CircuitBreakerConfig holds configuration for the circuit breaker strategy.
type CircuitBreakerConfig struct {
	FailureThreshold int
	CooldownPeriod   time.Duration
	HalfOpenAttempts int
	RetryOpts        []RetryOption
}

// CircuitBreakerOption configures a circuit breaker strategy.
type CircuitBreakerOption func(*CircuitBreakerConfig) error

// WithFailureThreshold sets the consecutive failure count to trip the circuit. Default: 10.
func WithFailureThreshold(threshold int) CircuitBreakerOption {
	return func(c *CircuitBreakerConfig) error {
		if threshold <= 0 {
			return errors.New("failure threshold must be positive")
		}
		c.FailureThreshold = threshold
		return nil
	}
}

// WithCooldownPeriod sets how long the circuit stays open before testing recovery. Default: 60s.
func WithCooldownPeriod(period time.Duration) CircuitBreakerOption {
	return func(c *CircuitBreakerConfig) error {
		if period <= 0 {
			return errors.New("cooldown period must be positive")
		}
		c.CooldownPeriod = period
		return nil
	}
}

// WithHalfOpenAttempts sets how many successes are needed to close the circuit from half-open. Default: 3.
func WithHalfOpenAttempts(attempts int) CircuitBreakerOption {
	return func(c *CircuitBreakerConfig) error {
		if attempts <= 0 {
			return errors.New("half-open attempts must be positive")
		}
		c.HalfOpenAttempts = attempts
		return nil
	}
}

// WithRetryOptions configures the retry behavior used by the circuit breaker.
// The circuit breaker uses retry + DLQ for failed messages before tracking failure counts.
func WithRetryOptions(opts ...RetryOption) CircuitBreakerOption {
	return func(c *CircuitBreakerConfig) error {
		c.RetryOpts = opts
		return nil
	}
}

// CircuitBreakerStrategy pauses consumption after consecutive failures to protect downstream services.
// Uses retry + DLQ for individual message failures while tracking consecutive failure patterns.
// Only supported in single-message mode.
type CircuitBreakerStrategy struct {
	mu              sync.RWMutex
	state           CircuitState
	failureCount    int
	successCount    int
	lastStateChange time.Time
	config          CircuitBreakerConfig
	retryStrategy   *RetryStrategy // todo: there are no tests testing delegation to retry strategy. do we even want this?
	logger          zerolog.Logger
	// nowFunc is used for testing to mock time
	nowFunc func() time.Time
}

// NewCircuitBreakerStrategy creates a circuit breaker strategy.
func NewCircuitBreakerStrategy(opts ...CircuitBreakerOption) (*CircuitBreakerStrategy, error) {
	cfg := CircuitBreakerConfig{
		FailureThreshold: 10,
		CooldownPeriod:   60 * time.Second,
		HalfOpenAttempts: 3,
	}

	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, fmt.Errorf("invalid circuit breaker option: %w", err)
		}
	}

	// Create underlying retry strategy if retry options were provided
	var retrySt *RetryStrategy
	if len(cfg.RetryOpts) > 0 {
		var err error
		retrySt, err = NewRetryStrategy(cfg.RetryOpts...)
		if err != nil {
			return nil, fmt.Errorf("circuit breaker retry config: %w", err)
		}
	}

	return &CircuitBreakerStrategy{
		state:           CircuitClosed,
		config:          cfg,
		retryStrategy:   retrySt,
		lastStateChange: time.Now(),
		logger:          zerolog.Nop(),
		nowFunc:         time.Now,
	}, nil
}

// Initialize sets up the circuit breaker and its underlying retry strategy.
func (cb *CircuitBreakerStrategy) Initialize(config types.InitConfig) error {
	cb.logger = config.Logger
	if cb.retryStrategy != nil {
		return cb.retryStrategy.Initialize(config)
	}
	return nil
}

// Close shuts down the circuit breaker and its underlying retry strategy.
func (cb *CircuitBreakerStrategy) Close() error {
	if cb.retryStrategy != nil {
		return cb.retryStrategy.Close()
	}
	return nil
}

// HandleError implements the circuit breaker state machine.
// In Closed state: tracks failures, trips circuit at threshold.
// In Open state: rejects all messages, transitions to HalfOpen after cooldown.
// In HalfOpen state: allows test messages, closes on successive successes.
func (cb *CircuitBreakerStrategy) HandleError(ctx context.Context, msgs []*types.Message, handlerErr error) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := cb.nowFunc()

	switch cb.state {
	case CircuitClosed:
		return cb.handleClosed(ctx, msgs, handlerErr, now)
	case CircuitOpen:
		return cb.handleOpen(now)
	case CircuitHalfOpen:
		return cb.handleHalfOpen(ctx, msgs, handlerErr, now)
	}

	return nil
}

// OnSuccess is called by the engine when a handler succeeds. Used to track
// failure/success counts for the circuit breaker state machine.
// todo: seems like it is not called by engine as stated by comment above :-)
func (cb *CircuitBreakerStrategy) OnSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := cb.nowFunc()

	// todo: what about CircuitOpen? we should include it for completeness
	switch cb.state {
	case CircuitClosed:
		// Success resets the failure counter
		cb.failureCount = 0

	case CircuitHalfOpen:
		cb.successCount++
		cb.logger.Info().
			Int("successes", cb.successCount).
			Int("needed", cb.config.HalfOpenAttempts).
			Msg("half-open success")

		if cb.successCount >= cb.config.HalfOpenAttempts {
			cb.state = CircuitClosed
			cb.failureCount = 0
			cb.successCount = 0
			cb.lastStateChange = now
			cb.logger.Info().Msg("circuit breaker CLOSED — normal operation resumed")
		}
	}
}

// State returns the current circuit state (for testing/inspection).
func (cb *CircuitBreakerStrategy) State() CircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// Name returns the strategy name.
func (cb *CircuitBreakerStrategy) Name() string {
	return "circuit-breaker"
}

// handleClosed handles errors in the Closed (normal) state.
func (cb *CircuitBreakerStrategy) handleClosed(ctx context.Context, msgs []*types.Message, handlerErr error, now time.Time) error {
	cb.failureCount++
	cb.logger.Warn().
		Int("failures", cb.failureCount).
		Int("threshold", cb.config.FailureThreshold).
		Msg("handler failed (closed state)")

	// Delegate to retry strategy if available
	if cb.retryStrategy != nil {
		retryErr := cb.retryStrategy.HandleError(ctx, msgs, handlerErr)
		if retryErr != nil {
			return retryErr
		}
	}

	if cb.failureCount >= cb.config.FailureThreshold {
		cb.state = CircuitOpen
		cb.lastStateChange = now
		cb.logger.Error().
			Int("failures", cb.failureCount).
			Dur("cooldown", cb.config.CooldownPeriod).
			Msg("circuit breaker OPENED — pausing consumption")
		return fmt.Errorf("circuit breaker opened after %d consecutive failures", cb.failureCount)
	}

	return nil
}

// handleOpen handles errors in the Open state.
func (cb *CircuitBreakerStrategy) handleOpen(now time.Time) error {
	elapsed := now.Sub(cb.lastStateChange)

	if elapsed >= cb.config.CooldownPeriod {
		// Transition to half-open
		cb.state = CircuitHalfOpen
		cb.successCount = 0
		cb.lastStateChange = now
		cb.logger.Info().
			Dur("elapsed", elapsed).
			Msg("circuit breaker entering HALF-OPEN — testing recovery")
		// Allow this message through for testing
		return nil
	}

	remaining := cb.config.CooldownPeriod - elapsed
	cb.logger.Debug().
		Dur("remaining", remaining).
		Msg("circuit breaker OPEN — rejecting message")
	return fmt.Errorf("circuit breaker open, cooldown remaining: %v", remaining)
}

// handleHalfOpen handles errors in the HalfOpen state.
func (cb *CircuitBreakerStrategy) handleHalfOpen(ctx context.Context, msgs []*types.Message, handlerErr error, now time.Time) error {
	// Failure in half-open — downstream still broken, reopen circuit
	cb.state = CircuitOpen
	cb.failureCount = 0
	cb.lastStateChange = now
	cb.logger.Error().
		Err(handlerErr).
		Msg("circuit breaker reopened — downstream still failing")

	// Delegate to retry strategy for this failed message
	if cb.retryStrategy != nil {
		return cb.retryStrategy.HandleError(ctx, msgs, handlerErr)
	}

	return fmt.Errorf("circuit breaker reopened after half-open failure: %w", handlerErr)
}

// SetNowFunc sets the time function (for testing).
func (cb *CircuitBreakerStrategy) SetNowFunc(fn func() time.Time) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.nowFunc = fn
}
