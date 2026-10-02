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
	// before letting one probe through (longer when a failure carried a
	// Retry-After; see decision.RetryDelay).
	DefaultBreakerCooldown = 30 * time.Second
	// DefaultBreakerProbeTimeout bounds the one probe call a half-open breaker
	// lets through, even when the caller's context has no deadline and the
	// engine ignores cancellation.
	DefaultBreakerProbeTimeout = 10 * time.Second
)

type breakerConfig struct {
	slowThreshold int
	threshold     int
	window        time.Duration
	cooldown      time.Duration
	probeTimeout  time.Duration
	clock         Clock
	onChange      func(engine string, from, to BreakerState)
}

// BreakerOption configures a Breaker.
type BreakerOption func(*breakerConfig)

// WithBreakerThreshold sets how many consecutive failures open the breaker.
func WithBreakerThreshold(n int) BreakerOption { return func(c *breakerConfig) { c.threshold = n } }

// WithBreakerSlowThreshold makes n consecutive "slow" calls open the breaker. A
// call is slow when a Hedged primary was still running when its backup answered
// (see ErrSuperseded): it missed its latency budget but did not fail. By default
// (0) slowness never opens the breaker, because a healthy engine whose p50 is
// above the hedge delay would otherwise be taken out of service by its own
// hedge. Set it for an engine that can hang without ever failing (the hedge then
// answers every call): the hung engine is paid for and cancelled on each call
// until the breaker opens. A call that completes in time resets the run. Either
// way, a primary that also exceeds its own timeout is a failure (see
// WithBreakerThreshold). Set the hedge delay above the primary's p99.
func WithBreakerSlowThreshold(n int) BreakerOption {
	return func(c *breakerConfig) { c.slowThreshold = n }
}

// WithBreakerWindow sets the span the consecutive failures must fall within.
func WithBreakerWindow(d time.Duration) BreakerOption { return func(c *breakerConfig) { c.window = d } }

// WithBreakerCooldown sets how long the breaker stays open.
func WithBreakerCooldown(d time.Duration) BreakerOption {
	return func(c *breakerConfig) { c.cooldown = d }
}

// WithBreakerProbeTimeout bounds the probe call: a probe that has not returned
// within d counts as a failure and the breaker goes back to open.
func WithBreakerProbeTimeout(d time.Duration) BreakerOption {
	return func(c *breakerConfig) { c.probeTimeout = d }
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
// engine-health failures it opens and the engine is not called at all: calls
// return decision.ErrUnavailable at once, which combinators and Chain record as
// the "unavailable" outcome. After the cooldown one probe call is let through;
// success closes the breaker, failure reopens it.
//
// What counts as a failure is the engine's health only: a transport error, a
// timeout (including a Hedged primary that outran its OWN timeout), a server
// error, and a rate limit or overload. These do NOT count: a request the engine
// rejected as invalid or an authentication failure (decision.ErrInvalidRequest,
// decision.ErrAuth: the caller's fault, which must not take a healthy engine out
// of service for everyone), an exhausted allowance (decision.ErrQuota), a
// misconfigured endpoint (decision.ErrMisconfigured), an unsupported operation,
// an abstention, an invalid answer, and a cancellation by the caller or by a race
// winner.
//
// A Hedged primary that is still running when its backup answers is "slow", a
// separate tally (Stats): it missed its latency budget, which says little about
// its health, so it does NOT open the breaker unless WithBreakerSlowThreshold is
// set. Set the hedge delay above the primary's p99.
//
// A failure that carries a retry delay (decision.RetryDelay, for example an HTTP
// Retry-After) makes the breaker stay open at least that long (capped by
// decision.MaxRetryDelay). A call that started before the breaker last opened is
// ignored when it finishes, so a late success cannot close a breaker that has
// since opened.
//
// The breaker does not trust the engine to honour its context: it returns as
// soon as the call's context is done, and bounds the half-open probe with
// WithBreakerProbeTimeout, so an engine that hangs cannot hold the breaker
// half-open. (The abandoned call may still run to completion on its own
// goroutine.)
//
// State is per instance and in memory.
type Breaker struct {
	inner decision.Provider
	cfg   breakerConfig

	mu         sync.Mutex
	state      BreakerState
	gen        uint64 // bumped each time the breaker opens
	failures   int
	slow       int // consecutive slow calls (a Hedged primary superseded by its backup)
	firstFail  time.Time
	openedAt   time.Time
	openFor    time.Duration
	retryFloor time.Duration // longest retry delay seen in the current failure run
	probing    bool
}

var (
	_ decision.Provider       = (*Breaker)(nil)
	_ decision.TracedProvider = (*Breaker)(nil)
	_ decision.ScoredProvider = (*Breaker)(nil)
)

// BreakerStats is a breaker's running tally.
type BreakerStats struct {
	// Failures is the current run of consecutive engine-health failures.
	Failures int
	// Slow is the current run of consecutive slow calls (see
	// WithBreakerSlowThreshold). A call that completes in time resets it.
	Slow int
}

// Stats returns the current failure and slow runs.
func (b *Breaker) Stats() BreakerStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BreakerStats{Failures: b.failures, Slow: b.slow}
}

// NewBreaker wraps p. A nil p gives a breaker whose calls fail with ErrNoEngine.
func NewBreaker(p decision.Provider, opts ...BreakerOption) *Breaker {
	cfg := breakerConfig{
		threshold: DefaultBreakerThreshold, window: DefaultBreakerWindow,
		cooldown: DefaultBreakerCooldown, probeTimeout: DefaultBreakerProbeTimeout,
		clock: SystemClock(),
	}
	for _, o := range opts {
		o(&cfg)
	}
	return &Breaker{inner: p, cfg: cfg}
}

// Name is the wrapped engine's name: the breaker is transparent in traces.
func (b *Breaker) Name() string { return providerName(b.inner) }

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
		b.cfg.onChange(b.Name(), from, to)
	}
}

// allow reports whether a call may proceed, whether it is the probe, and the
// generation it belongs to.
func (b *Breaker) allow() (ok, probe bool, gen uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	gen = b.gen
	switch b.state {
	case BreakerClosed:
		return true, false, gen
	case BreakerOpen:
		if b.cfg.clock.Now().Sub(b.openedAt) < b.openFor {
			return false, false, gen
		}
		b.setState(BreakerHalfOpen)
	}
	// Half-open: one probe at a time.
	if b.probing {
		return false, false, gen
	}
	b.probing = true
	return true, true, gen
}

type verdict int

const (
	verdictSuccess verdict = iota
	verdictFailure
	verdictNeutral
	verdictSlow
)

// judge classifies a call's error (see Breaker for the rules).
func judge(ctx context.Context, err error) verdict {
	cause := context.Cause(ctx)
	switch {
	case err == nil:
		return verdictSuccess
	case errors.Is(err, decision.ErrUnsupported), errors.Is(err, decision.ErrInvalidRequest), errors.Is(err, decision.ErrAuth),
		errors.Is(err, decision.ErrQuota), errors.Is(err, decision.ErrMisconfigured):
		return verdictNeutral
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(cause, context.DeadlineExceeded):
		return verdictFailure
	case errors.Is(cause, ErrSuperseded):
		return verdictSlow
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		return verdictNeutral
	default:
		return verdictFailure
	}
}

func (b *Breaker) record(v verdict, probe bool, gen uint64, retry time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if gen != b.gen {
		return // the call started before the breaker last opened: stale
	}
	if probe {
		b.probing = false
	}
	switch v {
	case verdictSlow:
		// Slowness is not failure: it leaves the failure run alone and opens the
		// breaker only when a slow threshold was asked for.
		b.slow++
		if b.cfg.slowThreshold > 0 && b.slow >= b.cfg.slowThreshold {
			b.open(b.cfg.clock.Now())
		}
	case verdictSuccess:
		b.failures, b.slow, b.retryFloor = 0, 0, 0
		b.setState(BreakerClosed)
	case verdictFailure:
		now := b.cfg.clock.Now()
		if probe {
			b.retryFloor = retry
			b.open(now)
			return
		}
		if b.failures == 0 || now.Sub(b.firstFail) > b.cfg.window {
			b.failures, b.firstFail, b.retryFloor = 0, now, 0
		}
		b.failures++
		b.retryFloor = max(b.retryFloor, retry)
		if b.failures >= b.cfg.threshold {
			b.open(now)
		}
	}
}

// open opens the breaker for the cooldown, or the longest retry delay seen,
// whichever is longer. Callers hold b.mu.
func (b *Breaker) open(now time.Time) {
	b.openedAt = now
	b.openFor = max(b.cfg.cooldown, b.retryFloor)
	b.retryFloor = 0
	b.slow = 0
	b.gen++
	b.setState(BreakerOpen)
}

func (b *Breaker) unavailable() error {
	return fmt.Errorf("%s: circuit breaker open: %w", b.Name(), decision.ErrUnavailable)
}

// callBreaker runs fn under b: admission, the call on its own goroutine so the
// breaker returns when ctx is done even if the engine does not, and the verdict.
func callBreaker[T any](b *Breaker, ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	ok, probe, gen := b.allow()
	if !ok {
		return zero, b.unavailable()
	}
	if probe {
		var release func()
		ctx, release = withTimeout(ctx, b.cfg.clock, b.cfg.probeTimeout)
		defer release()
	}
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1) // buffered: an abandoned call never blocks on send
	go func() {
		v, err := fn(ctx)
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		b.record(judge(ctx, r.err), probe, gen, decision.RetryDelay(r.err))
		return r.v, r.err
	case <-ctx.Done():
		err := fmt.Errorf("%s: %w", b.Name(), context.Cause(ctx))
		b.record(judge(ctx, err), probe, gen, 0)
		return zero, err
	}
}

type decideResult struct {
	d   decision.Decision
	ok  bool
	rep decision.Report
}

// Decide implements decision.Provider.
func (b *Breaker) Decide(ctx context.Context, req decision.Request) (decision.Decision, bool, error) {
	d, ok, _, err := b.DecideTraced(ctx, req)
	return d, ok, err
}

// DecideTraced implements decision.TracedProvider, transparently: when the wrapped
// engine reports how it answered (a combinator, or an engine that explains an
// abstention, as typesafe.Client does), the breaker passes that report on, so a
// circuit breaker around an engine never hides its attempts from a Chain's trace.
// For an engine that reports nothing the report is empty.
func (b *Breaker) DecideTraced(ctx context.Context, req decision.Request) (decision.Decision, bool, decision.Report, error) {
	if b.inner == nil {
		return decision.Decision{}, false, decision.Report{}, fmt.Errorf("%s: %w", b.Name(), ErrNoEngine)
	}
	r, err := callBreaker(b, ctx, func(ctx context.Context) (decideResult, error) {
		if tp, ok := b.inner.(decision.TracedProvider); ok {
			d, ok, rep, err := tp.DecideTraced(ctx, req)
			return decideResult{d, ok, rep}, err
		}
		d, ok, err := b.inner.Decide(ctx, req)
		return decideResult{d: d, ok: ok, rep: decision.Report{Engine: b.Name()}}, err
	})
	return r.d, r.ok, r.rep, err
}

// Score implements decision.ScoredProvider. A breaker with no engine returns
// ErrNoEngine, and when the wrapped engine is not a scored provider it returns
// decision.ErrUnsupported, neither touching the breaker.
func (b *Breaker) Score(ctx context.Context, req decision.ScoreRequest) (decision.ScoreResult, error) {
	if b.inner == nil {
		return decision.ScoreResult{}, fmt.Errorf("%s: %w", b.Name(), ErrNoEngine)
	}
	sp, isScorer := b.inner.(decision.ScoredProvider)
	if !isScorer {
		return decision.ScoreResult{}, fmt.Errorf("%s: %w", b.Name(), decision.ErrUnsupported)
	}
	return callBreaker(b, ctx, func(ctx context.Context) (decision.ScoreResult, error) {
		return sp.Score(ctx, req)
	})
}
