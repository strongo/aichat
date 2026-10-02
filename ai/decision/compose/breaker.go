package compose

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

// BreakerState is a circuit breaker's state.
type BreakerState int

const (
	// BreakerClosed: calls pass through.
	BreakerClosed BreakerState = iota
	// BreakerOpen: the engine is considered down; calls fail fast with
	// decision.ErrUnavailable until the cooldown passes.
	BreakerOpen
	// BreakerHalfOpen: the cooldown passed; exactly one probe call is let
	// through to find out whether the engine is back.
	BreakerHalfOpen
)

func (s BreakerState) String() string {
	switch s {
	case BreakerClosed:
		return "closed"
	case BreakerOpen:
		return "open"
	default:
		return "half-open"
	}
}

// Documented defaults of a Breaker.
const (
	// DefaultBreakerThreshold consecutive failures within DefaultBreakerWindow
	// open the breaker.
	DefaultBreakerThreshold = 5
	DefaultBreakerWindow    = 30 * time.Second
	// DefaultBreakerCooldown is how long an open breaker answers "unavailable"
	// before letting one probe through.
	DefaultBreakerCooldown = 30 * time.Second
)

type breakerConfig struct {
	threshold int
	window    time.Duration
	cooldown  time.Duration
	clock     Clock
	onChange  func(engine string, from, to BreakerState)
}

// BreakerOption configures a Breaker.
type BreakerOption func(*breakerConfig)

// WithBreakerThreshold sets how many consecutive failures open the breaker.
func WithBreakerThreshold(n int) BreakerOption { return func(c *breakerConfig) { c.threshold = n } }

// WithBreakerWindow sets the span the consecutive failures must fall within.
func WithBreakerWindow(d time.Duration) BreakerOption { return func(c *breakerConfig) { c.window = d } }

// WithBreakerCooldown sets how long the breaker stays open.
func WithBreakerCooldown(d time.Duration) BreakerOption {
	return func(c *breakerConfig) { c.cooldown = d }
}

// WithBreakerClock replaces the real clock (tests).
func WithBreakerClock(clk Clock) BreakerOption { return func(c *breakerConfig) { c.clock = clk } }

// WithBreakerOnChange registers a callback for every state change, so a host can
// log and count them. It runs synchronously while the breaker is locked: keep it
// short and do not call back into the breaker.
func WithBreakerOnChange(f func(engine string, from, to BreakerState)) BreakerOption {
	return func(c *breakerConfig) { c.onChange = f }
}

// Breaker is a circuit breaker around one engine. After a run of consecutive
// failures (errors and timeouts, not cancellations or abstentions) it opens and
// the engine is not called at all: calls return decision.ErrUnavailable at once,
// which combinators and Chain record as the "unavailable" outcome. After the
// cooldown one probe call is let through; success closes the breaker, failure
// reopens it.
//
// State is per instance and in memory.
type Breaker struct {
	inner decision.Provider
	cfg   breakerConfig

	mu        sync.Mutex
	state     BreakerState
	failures  int
	firstFail time.Time
	openedAt  time.Time
	probing   bool
}

var (
	_ decision.Provider       = (*Breaker)(nil)
	_ decision.ScoredProvider = (*Breaker)(nil)
)

// NewBreaker wraps p.
func NewBreaker(p decision.Provider, opts ...BreakerOption) *Breaker {
	cfg := breakerConfig{
		threshold: DefaultBreakerThreshold, window: DefaultBreakerWindow,
		cooldown: DefaultBreakerCooldown, clock: SystemClock(),
	}
	for _, o := range opts {
		o(&cfg)
	}
	return &Breaker{inner: p, cfg: cfg}
}

// Name is the wrapped engine's name: the breaker is transparent in traces.
func (b *Breaker) Name() string { return b.inner.Name() }

// DecisionTimeout passes through the wrapped engine's timeout, if it has one.
func (b *Breaker) DecisionTimeout() time.Duration {
	if dt, ok := b.inner.(interface{ DecisionTimeout() time.Duration }); ok {
		return dt.DecisionTimeout()
	}
	return 0
}

// State returns the current state (an open breaker whose cooldown has passed
// still reports open until the next call takes the probe).
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func (b *Breaker) setState(to BreakerState) {
	from := b.state
	if from == to {
		return
	}
	b.state = to
	if b.cfg.onChange != nil {
		b.cfg.onChange(b.inner.Name(), from, to)
	}
}

// allow reports whether a call may proceed, and whether it is the probe.
func (b *Breaker) allow() (ok, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerClosed:
		return true, false
	case BreakerOpen:
		if b.cfg.clock.Now().Sub(b.openedAt) < b.cfg.cooldown {
			return false, false
		}
		b.setState(BreakerHalfOpen)
	}
	// Half-open: one probe at a time.
	if b.probing {
		return false, false
	}
	b.probing = true
	return true, true
}

type verdict int

const (
	verdictSuccess verdict = iota
	verdictFailure
	verdictNeutral
)

// judge classifies a call's error. A timeout (deadline, whether of the
// combinator's per-engine timeout or the caller's) is a failure; a cancellation
// (a hedge or race loser, a caller that gave up) and an unsupported operation
// say nothing about the engine's health.
func judge(ctx context.Context, err error) verdict {
	switch {
	case err == nil:
		return verdictSuccess
	case errors.Is(err, decision.ErrUnsupported):
		return verdictNeutral
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(context.Cause(ctx), context.DeadlineExceeded):
		return verdictFailure
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		return verdictNeutral
	default:
		return verdictFailure
	}
}

func (b *Breaker) record(v verdict, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		b.probing = false
	}
	switch v {
	case verdictSuccess:
		b.failures = 0
		b.setState(BreakerClosed)
	case verdictFailure:
		now := b.cfg.clock.Now()
		if probe {
			b.openedAt = now
			b.setState(BreakerOpen)
			return
		}
		if b.failures == 0 || now.Sub(b.firstFail) > b.cfg.window {
			b.failures, b.firstFail = 0, now
		}
		b.failures++
		if b.failures >= b.cfg.threshold {
			b.openedAt = now
			b.setState(BreakerOpen)
		}
	}
}

func (b *Breaker) unavailable() error {
	return fmt.Errorf("%s: circuit breaker open: %w", b.inner.Name(), decision.ErrUnavailable)
}

// Decide implements decision.Provider.
func (b *Breaker) Decide(ctx context.Context, req decision.Request) (decision.Decision, bool, error) {
	ok, probe := b.allow()
	if !ok {
		return decision.Decision{}, false, b.unavailable()
	}
	d, answered, err := b.inner.Decide(ctx, req)
	b.record(judge(ctx, err), probe)
	return d, answered, err
}

// Score implements decision.ScoredProvider. When the wrapped engine is not a
// scored provider it returns decision.ErrUnsupported without touching the
// breaker.
func (b *Breaker) Score(ctx context.Context, req decision.ScoreRequest) (decision.ScoreResult, error) {
	sp, isScorer := b.inner.(decision.ScoredProvider)
	if !isScorer {
		return decision.ScoreResult{}, fmt.Errorf("%s: %w", b.inner.Name(), decision.ErrUnsupported)
	}
	ok, probe := b.allow()
	if !ok {
		return decision.ScoreResult{}, b.unavailable()
	}
	res, err := sp.Score(ctx, req)
	b.record(judge(ctx, err), probe)
	return res, err
}
