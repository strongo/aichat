package compose

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

// BudgetOptions caps how many calls a Budget lets through.
type BudgetOptions struct {
	// MaxCalls is the most calls (decisions and scored questions, counted together,
	// one each) admitted per Per window. It must be above 0: a Budget with a smaller
	// value is misconfigured and every call fails with decision.ErrMisconfigured,
	// loud on purpose, instead of silently allowing everything or nothing.
	MaxCalls int
	// Per is the window the cap applies to. The window is fixed: it starts at the
	// first call and a call at or after Per later starts the next one, with the
	// count back at zero. 0 (the zero value) means the cap never resets for the
	// life of the Budget: a hard ceiling that only a restart lifts. It must not be
	// negative (misconfigured, as above).
	Per time.Duration
	// Clock drives the window (tests); nil is the real clock.
	Clock Clock
}

// BudgetStats is a Budget's running tally.
type BudgetStats struct {
	// Used is the number of calls admitted in the current window, Max the cap, and
	// Refused how many calls the Budget has refused since it was built.
	Used, Max, Refused int
}

// Budget caps how often an engine is called, and so what it can cost. It is the
// guard for a hole no provider's own errors can close: when the engine in front
// of a paid backup fails in a way that looks transient (a TypeSafe 429 does not
// tell a rate limit from an exhausted account, so it stays transient), a Fallback
// hands EVERY call to the backup, and nothing bounds the bill. Wrap the backup:
//
//	compose.Fallback(
//		compose.NewBreaker(typesafeEngine),
//		compose.NewBreaker(compose.NewBudget(llmEngine, compose.BudgetOptions{MaxCalls: 200, Per: time.Hour})),
//	)
//
// Put the Budget INSIDE the Breaker: a call the breaker refuses (open) never
// reaches the Budget and is not counted, so the cap measures real spend.
//
// Once the cap is used up the engine is not called: the call fails with an error
// matching decision.ErrBudget (outcome "budget"). That is a quota-class refusal,
// loud on purpose: a Breaker ignores it (it is not the engine's fault), a
// Fallback or Hedged does not start its backup for it unless OnQuota says so, and
// a decision.Chain stops at it by default (Chain.StopOnQuota, Trace.StoppedBy ==
// "budget") instead of handing the turn to the next, possibly paid, provider. A
// product that sees a stopped chain MUST NOT then call its paid main LLM as if
// nobody had decided.
//
// A call is counted when it is admitted, whatever it then does (answers, fails or
// abstains), because the engine is billed for it either way; one the engine does
// not support (a Budget around an engine that cannot score) is not counted.
// State is per instance and in memory: a cap per process, not per fleet.
type Budget struct {
	inner decision.Provider
	opts  BudgetOptions
	clock Clock

	mu      sync.Mutex
	start   time.Time
	used    int
	refused int
}

var (
	_ decision.Provider       = (*Budget)(nil)
	_ decision.TracedProvider = (*Budget)(nil)
	_ decision.ScoredProvider = (*Budget)(nil)
)

// NewBudget wraps p with the cap opts. A nil p gives a Budget whose calls fail
// with ErrNoEngine; invalid options are reported on the first call (constructors
// never panic).
func NewBudget(p decision.Provider, opts BudgetOptions) *Budget {
	clk := opts.Clock
	if clk == nil {
		clk = SystemClock()
	}
	return &Budget{inner: p, opts: opts, clock: clk}
}

// Name is the wrapped engine's name: the Budget is transparent in traces.
func (b *Budget) Name() string { return providerName(b.inner) }

// DecisionTimeout passes through the wrapped engine's timeout, if it has one.
func (b *Budget) DecisionTimeout() time.Duration {
	if dt, ok := b.inner.(interface{ DecisionTimeout() time.Duration }); ok {
		return dt.DecisionTimeout()
	}
	return 0
}

// Stats returns the current tally. After the window has passed and before the next
// call, Used still shows the finished window's count.
func (b *Budget) Stats() BudgetStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BudgetStats{Used: b.used, Max: b.opts.MaxCalls, Refused: b.refused}
}

// admit counts one call, or returns why it is not allowed.
func (b *Budget) admit() error {
	if b.opts.MaxCalls <= 0 || b.opts.Per < 0 {
		return fmt.Errorf("%s: budget: MaxCalls must be above 0 and Per not negative (got %d per %s): %w", b.Name(), b.opts.MaxCalls, b.opts.Per, decision.ErrMisconfigured)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	if b.opts.Per > 0 && b.used > 0 && now.Sub(b.start) >= b.opts.Per {
		b.used = 0
	}
	if b.used >= b.opts.MaxCalls {
		b.refused++
		return fmt.Errorf("%s: %w (%s)", b.Name(), decision.ErrBudget, b.describe())
	}
	if b.used == 0 {
		b.start = now
	}
	b.used++
	return nil
}

func (b *Budget) describe() string {
	if b.opts.Per == 0 {
		return fmt.Sprintf("%d calls used, the cap never resets", b.opts.MaxCalls)
	}
	return fmt.Sprintf("%d calls used per %s", b.opts.MaxCalls, b.opts.Per)
}

// Decide implements decision.Provider.
func (b *Budget) Decide(ctx context.Context, req decision.Request) (decision.Decision, bool, error) {
	d, ok, _, err := b.DecideTraced(ctx, req)
	return d, ok, err
}

// DecideTraced implements decision.TracedProvider, transparently: the wrapped
// engine's report is passed on.
func (b *Budget) DecideTraced(ctx context.Context, req decision.Request) (decision.Decision, bool, decision.Report, error) {
	if b.inner == nil {
		return decision.Decision{}, false, decision.Report{}, fmt.Errorf("%s: %w", b.Name(), ErrNoEngine)
	}
	if err := b.admit(); err != nil {
		return decision.Decision{}, false, decision.Report{}, err
	}
	if tp, ok := b.inner.(decision.TracedProvider); ok {
		return tp.DecideTraced(ctx, req)
	}
	d, ok, err := b.inner.Decide(ctx, req)
	return d, ok, decision.Report{Engine: b.Name()}, err
}

// Score implements decision.ScoredProvider. A Budget with no engine returns
// ErrNoEngine, and one around an engine that does not score returns
// decision.ErrUnsupported, neither counted.
func (b *Budget) Score(ctx context.Context, req decision.ScoreRequest) (decision.ScoreResult, error) {
	if b.inner == nil {
		return decision.ScoreResult{}, fmt.Errorf("%s: %w", b.Name(), ErrNoEngine)
	}
	sp, ok := b.inner.(decision.ScoredProvider)
	if !ok {
		return decision.ScoreResult{}, fmt.Errorf("%s: %w", b.Name(), decision.ErrUnsupported)
	}
	if err := b.admit(); err != nil {
		return decision.ScoreResult{}, err
	}
	return sp.Score(ctx, req)
}
