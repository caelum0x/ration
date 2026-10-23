package llm

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"
)

// ErrCircuitOpen is returned by CircuitBreaker.RunConversation instead of
// calling the wrapped Provider at all, once the breaker has tripped.
//
// docs/AGENT-DESIGN.md states plainly: "there is no confirmed... cap...
// no circuit breaker: a slow/erroring LLM provider stalls that tick's
// decision loop." internal/loop.Runner's own design already guarantees
// "LLM decides phrasing only" — policy.ComputeBridgeEnvelope's
// deterministic hard-rule result is computed and recorded
// (RecordAgentDecision) BEFORE the LLM is ever called, so the LLM's
// only real job per tick is choosing how much of that already-safe
// ceiling to actually execute and how to phrase it. callers that see
// ErrCircuitOpen should execute that same ceiling directly (see
// internal/loop.Runner.runFallbackDecision) rather than treating a
// tripped breaker as a reason to skip the tenant's decision entirely —
// a stuck LLM provider must never block or delay the money movement
// the hard rules already decided was safe.
var ErrCircuitOpen = errors.New("llm: circuit breaker open, provider calls short-circuited")

// BreakerState is one of CircuitBreaker's three states, exported for
// logging/tests.
type BreakerState string

const (
	// BreakerClosed is the normal state: every call is forwarded to the
	// wrapped Provider.
	BreakerClosed BreakerState = "closed"
	// BreakerOpen short-circuits every call with ErrCircuitOpen, calling
	// the wrapped Provider zero times, until Cooldown has elapsed since
	// the trip.
	BreakerOpen BreakerState = "open"
	// BreakerHalfOpen allows exactly the next call through as a trial:
	// success closes the breaker again, failure reopens it immediately
	// (not waiting for FailureThreshold more failures — one failed
	// recovery attempt is enough evidence the provider is still down).
	BreakerHalfOpen BreakerState = "half_open"
)

const (
	// defaultFailureThreshold is how many consecutive failures (within
	// defaultFailureWindow of each other) trip the breaker.
	defaultFailureThreshold = 3
	// defaultFailureWindow bounds how long a string of failures can be
	// spread out and still count as "consecutive" for tripping purposes
	// — a handful of failures spread across days is noise, not an
	// outage; a handful within minutes is an outage.
	defaultFailureWindow = 5 * time.Minute
	// defaultCooldown is how long the breaker stays fully Open (never
	// calling the provider at all) before allowing one HalfOpen trial
	// call through.
	defaultCooldown = 60 * time.Second
)

// CircuitBreaker wraps a Provider so a slow/erroring LLM provider can
// never stall the agent's decision loop beyond this breaker's own
// cooldown. Safe for concurrent use (guarded by mu), though
// internal/loop.Runner's own Tick processes deposits sequentially
// today — the lock exists for correctness regardless of caller
// concurrency, not because of a known concurrent caller.
//
// Tripping and recovery are both logged loudly (slog, at Warn/Info
// level respectively) per docs/AGENT-DESIGN.md's explicit ask — an
// operator watching logs must be able to see this happening, not just
// infer it from a stream of ErrCircuitOpen-caused fallback decisions.
type CircuitBreaker struct {
	provider Provider
	logger   *slog.Logger
	now      func() time.Time

	failureThreshold int
	failureWindow    time.Duration
	cooldown         time.Duration

	mu                  sync.Mutex
	state               BreakerState
	consecutiveFailures int
	lastFailureAt       time.Time
	openedAt            time.Time
}

// BreakerOption configures a CircuitBreaker at construction time.
type BreakerOption func(*CircuitBreaker)

// WithFailureThreshold overrides defaultFailureThreshold.
func WithFailureThreshold(n int) BreakerOption {
	return func(b *CircuitBreaker) {
		if n > 0 {
			b.failureThreshold = n
		}
	}
}

// WithFailureWindow overrides defaultFailureWindow.
func WithFailureWindow(d time.Duration) BreakerOption {
	return func(b *CircuitBreaker) {
		if d > 0 {
			b.failureWindow = d
		}
	}
}

// WithCooldown overrides defaultCooldown.
func WithCooldown(d time.Duration) BreakerOption {
	return func(b *CircuitBreaker) {
		if d > 0 {
			b.cooldown = d
		}
	}
}

// WithLogger overrides the default (slog.Default()) logger trip/
// recovery events are reported to.
func WithLogger(logger *slog.Logger) BreakerOption {
	return func(b *CircuitBreaker) {
		if logger != nil {
			b.logger = logger
		}
	}
}

// withNow overrides the clock. Test-only (unexported); production
// callers always get the real time.Now.
func withNow(now func() time.Time) BreakerOption {
	return func(b *CircuitBreaker) { b.now = now }
}

// NewCircuitBreaker wraps provider with a breaker using
// default{FailureThreshold,FailureWindow,Cooldown} unless overridden by
// opts.
func NewCircuitBreaker(provider Provider, opts ...BreakerOption) *CircuitBreaker {
	b := &CircuitBreaker{
		provider:         provider,
		logger:           slog.Default(),
		now:              time.Now,
		failureThreshold: defaultFailureThreshold,
		failureWindow:    defaultFailureWindow,
		cooldown:         defaultCooldown,
		state:            BreakerClosed,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// NewCircuitBreakerFromEnv builds a CircuitBreaker wrapping provider,
// reading RATION_AGENT_LLM_BREAKER_FAILURE_THRESHOLD,
// RATION_AGENT_LLM_BREAKER_FAILURE_WINDOW_SECONDS and
// RATION_AGENT_LLM_BREAKER_COOLDOWN_SECONDS (all optional, falling back
// to the same defaults NewCircuitBreaker uses when unset or
// unparseable) — the same env-driven construction convention
// internal/policy.LoadConfigFromEnv already uses for its own tunables.
func NewCircuitBreakerFromEnv(provider Provider, logger *slog.Logger) *CircuitBreaker {
	opts := []BreakerOption{WithLogger(logger)}

	if v := os.Getenv("RATION_AGENT_LLM_BREAKER_FAILURE_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts = append(opts, WithFailureThreshold(n))
		}
	}
	if v := os.Getenv("RATION_AGENT_LLM_BREAKER_FAILURE_WINDOW_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts = append(opts, WithFailureWindow(time.Duration(n)*time.Second))
		}
	}
	if v := os.Getenv("RATION_AGENT_LLM_BREAKER_COOLDOWN_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts = append(opts, WithCooldown(time.Duration(n)*time.Second))
		}
	}

	return NewCircuitBreaker(provider, opts...)
}

// State reports the breaker's current state, resolving an Open state
// whose cooldown has already elapsed into HalfOpen first — the same
// resolution RunConversation itself applies before deciding whether to
// call through. Exposed for logging/metrics/tests.
func (b *CircuitBreaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.resolveStateLocked()
}

// resolveStateLocked must be called with mu held. It transitions Open
// to HalfOpen once cooldown has elapsed, mutating b.state, and returns
// the resolved state.
func (b *CircuitBreaker) resolveStateLocked() BreakerState {
	if b.state == BreakerOpen && b.now().Sub(b.openedAt) >= b.cooldown {
		b.state = BreakerHalfOpen
	}
	return b.state
}

// RunConversation implements Provider. When the breaker is Open, this
// returns ErrCircuitOpen immediately, calling the wrapped Provider
// zero times — see the package doc comment on ErrCircuitOpen for why a
// caller should treat that as "execute the deterministic fallback",
// not as a generic failure to log and move on from.
func (b *CircuitBreaker) RunConversation(ctx context.Context, systemPrompt, userMessage string, tools []Tool, handler ToolHandler) (string, error) {
	b.mu.Lock()
	state := b.resolveStateLocked()
	if state == BreakerOpen {
		b.mu.Unlock()
		return "", ErrCircuitOpen
	}
	wasHalfOpen := state == BreakerHalfOpen
	b.mu.Unlock()

	result, err := b.provider.RunConversation(ctx, systemPrompt, userMessage, tools, handler)

	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.recordFailureLocked(wasHalfOpen, err)
	} else {
		b.recordSuccessLocked(wasHalfOpen)
	}
	return result, err
}

// recordFailureLocked must be called with mu held, immediately after a
// call to the wrapped Provider failed.
func (b *CircuitBreaker) recordFailureLocked(wasHalfOpen bool, callErr error) {
	now := b.now()

	if wasHalfOpen {
		// One failed recovery attempt is enough evidence the provider
		// is still down — reopen immediately rather than waiting to
		// re-accumulate failureThreshold more failures.
		b.state = BreakerOpen
		b.openedAt = now
		b.lastFailureAt = now
		b.logger.Warn("llm: circuit breaker recovery attempt failed, reopening",
			"cooldown", b.cooldown.String(), "error", callErr)
		return
	}

	if b.lastFailureAt.IsZero() || now.Sub(b.lastFailureAt) > b.failureWindow {
		// Either the first failure ever, or the previous failure is
		// old enough that this one doesn't continue the same streak —
		// start counting over.
		b.consecutiveFailures = 1
	} else {
		b.consecutiveFailures++
	}
	b.lastFailureAt = now

	if b.consecutiveFailures >= b.failureThreshold && b.state == BreakerClosed {
		b.state = BreakerOpen
		b.openedAt = now
		b.logger.Warn("llm: circuit breaker TRIPPED — short-circuiting to the hard-rule envelope until cooldown elapses",
			"consecutive_failures", b.consecutiveFailures,
			"failure_threshold", b.failureThreshold,
			"failure_window", b.failureWindow.String(),
			"cooldown", b.cooldown.String(),
			"error", callErr,
		)
	}
}

// recordSuccessLocked must be called with mu held, immediately after a
// call to the wrapped Provider succeeded.
func (b *CircuitBreaker) recordSuccessLocked(wasHalfOpen bool) {
	b.consecutiveFailures = 0
	if wasHalfOpen {
		b.state = BreakerClosed
		b.logger.Info("llm: circuit breaker recovered — provider call succeeded, resuming normal operation")
	}
}
