package compose

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

// Strategy names how an Engine combines its providers.
type Strategy string

const (
	StrategySingle   Strategy = "single"
	StrategyFallback Strategy = "fallback"
	StrategyHedged   Strategy = "hedged"
	StrategyRace     Strategy = "race"
)

// Trigger names an extra condition under which a failed-over strategy
// (Fallback, Hedged) starts its backup. Failures (error, timeout, an open
// breaker, an unsupported operation, an invalid answer, a request or credentials
// the engine refused) always start it. An exhausted allowance (OnQuota) and a
// misconfigured endpoint never do, unless OnQuota asks for the former. A spent
// budget (decision.ErrBudget, see NewBudget) never starts a backup either, and
// OnQuota does not change that. In Hedged and Race these conditions also end the
// call when they come from a leg that is already running (see Engine.halts).
type Trigger uint

const (
	// OnAbstain also starts the backup when the primary abstains.
	OnAbstain Trigger = 1 << iota
	// OnUncertain also starts the backup when the primary's calibrated answer is
	// judged uncertain by the engine's policy (see WithPolicy). Off by default:
	// an uncertain answer is information, not a failure.
	OnUncertain
	// OnQuota also starts the backup when the primary refuses because the
	// caller's allowance is exhausted (decision.ErrQuota). Off by default: the
	// backup is typically a paid engine, and quietly moving a metered caller's
	// traffic onto it is a spending decision, not a failover. Without it the quota
	// error is surfaced to the caller. A misconfigured endpoint
	// (decision.ErrMisconfigured) never starts a backup, with or without it.
	OnQuota
)

// DefaultHedgeAfter is the latency budget of a Hedged engine when configuration
// does not give one: a decision model answers in about a tenth of a second, so
// 600ms rarely fires and still bounds the wait when the primary is slow.
const DefaultHedgeAfter = 600 * time.Millisecond

// DefaultTimeout bounds an engine that does not report its own
// DecisionTimeout, matching decision.Chain's default.
const DefaultTimeout = 1500 * time.Millisecond

var (
	// ErrSuperseded is the cause (context.Cause) of the context of a Hedged
	// primary that was cancelled because its backup answered after the hedge
	// fired: the primary had not answered within the latency budget. A Breaker
	// tallies that as SLOW (BreakerStats.Slow), separately from failures, and
	// opens only if WithBreakerSlowThreshold asks for it: a healthy primary whose
	// latency sits above the hedge delay must not be taken out of service by its
	// own hedge. (A primary that also outruns its own timeout is a plain timeout
	// failure.) A Race loser is NOT superseded in this sense: all racers start
	// together, so losing a race says nothing about health or speed.
	ErrSuperseded = errors.New("compose: engine superseded by its hedge")
	// ErrNoEngine is returned by an engine or breaker built without a provider
	// to run.
	ErrNoEngine = errors.New("compose: no engine to run")
)

type config struct {
	name           string
	clock          Clock
	defaultTimeout time.Duration
	policy         *decision.SelectionPolicy
	also           Trigger
}

// Option configures an Engine.
type Option func(*config)

// WithName overrides the engine's default name.
func WithName(name string) Option { return func(c *config) { c.name = name } }

// WithClock replaces the real clock (tests).
func WithClock(clk Clock) Option { return func(c *config) { c.clock = clk } }

// WithDefaultTimeout sets the timeout for engines that do not report their own.
func WithDefaultTimeout(d time.Duration) Option { return func(c *config) { c.defaultTimeout = d } }

// WithPolicy lets the engine recognise an uncertain calibrated answer, which
// matters only together with WithFallbackOn(OnUncertain).
func WithPolicy(p decision.SelectionPolicy) Option { return func(c *config) { c.policy = &p } }

// WithFallbackOn adds triggers for starting the backup beyond plain failure.
func WithFallbackOn(t Trigger) Option { return func(c *config) { c.also |= t } }

// Engine runs one or more providers under a Strategy. Build one with Single,
// Fallback, Hedged or Race.
type Engine struct {
	cfg        config
	strategy   Strategy
	providers  []decision.Provider
	hedgeAfter time.Duration
}

var (
	_ decision.TracedProvider        = (*Engine)(nil)
	_ decision.TracedScorer          = (*Engine)(nil)
	_ decision.DeterministicProvider = (*Engine)(nil)
)

func newEngine(s Strategy, providers []decision.Provider, hedgeAfter time.Duration, opts []Option) *Engine {
	cfg := config{clock: SystemClock(), defaultTimeout: DefaultTimeout}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.name == "" {
		names := make([]string, len(providers))
		for i, p := range providers {
			names[i] = providerName(p)
		}
		cfg.name = string(s) + "(" + strings.Join(names, ",") + ")"
	}
	return &Engine{cfg: cfg, strategy: s, providers: providers, hedgeAfter: hedgeAfter}
}

// providerName is p's name, "<nil>" for a missing provider.
func providerName(p decision.Provider) string {
	if p == nil {
		return "<nil>"
	}
	return p.Name()
}

// check reports an engine that cannot run: a zero value (not built by a
// constructor), no providers, or a nil provider. Constructors never panic; the
// first call returns this error instead.
func (e *Engine) check() error {
	if e.cfg.clock == nil || len(e.providers) == 0 {
		return fmt.Errorf("compose: %s: %w (engine has no providers)", e.cfg.name, ErrNoEngine)
	}
	for _, p := range e.providers {
		if p == nil {
			return fmt.Errorf("compose: %s: %w (a provider is nil)", e.cfg.name, ErrNoEngine)
		}
	}
	return nil
}

// Single runs one provider. It exists so every decision, even one with no
// backup, reports through the same Report shape and per-engine timeout.
func Single(p decision.Provider, opts ...Option) *Engine {
	return newEngine(StrategySingle, []decision.Provider{p}, 0, opts)
}

// Fallback runs primary and, only if it fails (error, timeout, open breaker,
// invalid answer; see Trigger for opt-ins), backup. One call in the normal case.
func Fallback(primary, backup decision.Provider, opts ...Option) *Engine {
	return newEngine(StrategyFallback, []decision.Provider{primary, backup}, 0, opts)
}

// Hedged starts primary; if it has not answered after the latency budget it
// also starts backup, and the first valid answer wins while the other is
// cancelled. A primary that fails outright starts the backup at once. One call
// in the normal case, two only when the primary is slow or down.
func Hedged(primary, backup decision.Provider, after time.Duration, opts ...Option) *Engine {
	return newEngine(StrategyHedged, []decision.Provider{primary, backup}, after, opts)
}

// Race starts every provider at once; the first valid answer wins and the rest
// are cancelled. Every engine is paid on every call.
func Race(providers []decision.Provider, opts ...Option) *Engine {
	return newEngine(StrategyRace, providers, 0, opts)
}

// Name implements decision.Provider.
func (e *Engine) Name() string { return e.cfg.name }

// Strategy returns the engine's strategy.
func (e *Engine) Strategy() Strategy { return e.strategy }

// IsDeterministic implements decision.DeterministicProvider: true only when the
// engine has providers and every one of them is deterministic, so wrapping rules
// in an engine does not hide them from aiconfig's ordering check.
func (e *Engine) IsDeterministic() bool {
	return len(e.providers) > 0 && allDeterministic(e.providers...)
}

// allDeterministic reports whether every provider is a deterministic one.
func allDeterministic(ps ...decision.Provider) bool {
	for _, p := range ps {
		if dp, ok := p.(decision.DeterministicProvider); !ok || !dp.IsDeterministic() {
			return false
		}
	}
	return true
}

// DecisionTimeout implements the optional interface decision.Chain honours: the
// longest this engine can take, so a hedged backup is not starved by what is
// left of a chain's default.
func (e *Engine) DecisionTimeout() time.Duration {
	if len(e.providers) == 0 {
		return 0
	}
	ts := make([]time.Duration, len(e.providers))
	for i, p := range e.providers {
		ts[i] = e.legTimeout(p)
	}
	switch e.strategy {
	case StrategyFallback:
		var sum time.Duration
		for _, t := range ts {
			sum += t
		}
		return sum
	case StrategyHedged:
		return max(ts[0], e.hedgeAfter+ts[1])
	default: // single, race
		return slicesMax(ts)
	}
}

func slicesMax(ts []time.Duration) time.Duration {
	m := ts[0]
	for _, t := range ts[1:] {
		m = max(m, t)
	}
	return m
}

func (e *Engine) legTimeout(p any) time.Duration {
	if dt, ok := p.(interface{ DecisionTimeout() time.Duration }); ok {
		if v := dt.DecisionTimeout(); v > 0 {
			return v
		}
	}
	return e.cfg.defaultTimeout
}

func (e *Engine) role(i int) string {
	switch {
	case e.strategy == StrategyRace:
		return "racer"
	case i == 0:
		return "primary"
	default:
		return "backup"
	}
}

// triggers reports whether a leg that ended with outcome starts the next leg.
func (e *Engine) triggers(outcome string) bool {
	switch outcome {
	case decision.AttemptAbstained:
		return e.cfg.also&OnAbstain != 0
	case decision.AttemptUncertain:
		return e.cfg.also&OnUncertain != 0
	case decision.AttemptCancelled:
		return false // the caller gave up: nothing to fail over to
	case decision.AttemptQuota:
		return e.cfg.also&OnQuota != 0
	case decision.AttemptBudget, decision.AttemptMisconfigured:
		// A spent budget is a cap the caller set to keep a leg from being billed: a
		// backup never takes over for it, whatever OnQuota says (OnQuota is only about
		// the allowance of an engine the backup stands in for). A misconfigured
		// endpoint must be fixed, and a backup would hide it.
		return false
	default: // error, timeout, unavailable, unsupported, invalid, rejected, auth
		return true
	}
}

// halts reports whether a leg that ended with outcome ends the whole call, in the
// strategies that run legs concurrently (Hedged, Race): an exhausted allowance
// (unless OnQuota chose to fail over), a spent budget and a misconfigured endpoint. Without it a
// backup that was already started, by the hedge timer or because a race starts
// everyone, would answer and quietly take over the metered caller's traffic.
func (e *Engine) halts(outcome string) bool {
	switch outcome {
	case decision.AttemptQuota:
		return e.cfg.also&OnQuota == 0
	case decision.AttemptBudget, decision.AttemptMisconfigured:
		return true
	}
	return false
}

// ---- Provider (decisions) ----

// Decide implements decision.Provider.
func (e *Engine) Decide(ctx context.Context, req decision.Request) (decision.Decision, bool, error) {
	d, ok, _, err := e.DecideTraced(ctx, req)
	return d, ok, err
}

type decided struct{ d decision.Decision }

// DecideTraced implements decision.TracedProvider.
func (e *Engine) DecideTraced(ctx context.Context, req decision.Request) (decision.Decision, bool, decision.Report, error) {
	if err := e.check(); err != nil {
		return decision.Decision{}, false, decision.Report{Strategy: string(e.strategy)}, err
	}
	legs := make([]leg[decided], len(e.providers))
	for i, p := range e.providers {
		legs[i] = leg[decided]{
			name: p.Name(), role: e.role(i), timeout: e.legTimeout(p),
			call: func(ctx context.Context) (decided, bool, *decision.Report, error) {
				if tp, ok := p.(decision.TracedProvider); ok {
					d, ok, rep, err := tp.DecideTraced(ctx, req)
					return decided{d}, ok, &rep, err
				}
				d, ok, err := p.Decide(ctx, req)
				return decided{d}, ok, nil, err
			},
		}
	}
	judge := func(v decided) (string, string) {
		if err := decision.Validate(v.d, req.Taxonomy); err != nil {
			return decision.AttemptInvalid, decision.InvalidDetail(err)
		}
		if e.cfg.policy != nil {
			if sel := e.cfg.policy.EvaluateDecision(v.d); sel.Outcome == decision.OutcomeInvalid {
				return decision.AttemptInvalid, sel.Detail()
			} else if !sel.Actionable() {
				return decision.AttemptUncertain, sel.Detail()
			}
		}
		return decision.AttemptDecided, ""
	}
	out := run(ctx, e, legs, judge)
	rep := out.report(e)
	if v, ok := out.value(); ok {
		rep.Model = v.d.Model
		if e.cfg.policy != nil {
			// With a policy every answer carries its stamped verdict, so a direct caller
			// of the engine can tell an accepted answer from one that was only
			// returned because an uncertain answer is an answer (Actionable). Without
			// one nothing judged the answer: its verdict stays whatever the provider
			// behind it stamped (an inner engine's), never what a provider wrote.
			v.d, _ = e.cfg.policy.JudgeDecision(v.d)
		}
		return v.d, true, rep, nil
	}
	if out.abstained() && !out.halted && !out.refused(e) {
		return decision.Decision{}, false, rep, nil
	}
	return decision.Decision{}, false, rep, out.failure(e)
}

// ---- ScoredProvider ----

// Score implements decision.ScoredProvider. Providers that are not scored
// providers are recorded as "unsupported" and skipped over.
func (e *Engine) Score(ctx context.Context, req decision.ScoreRequest) (decision.ScoreResult, error) {
	res, _, err := e.ScoreTraced(ctx, req)
	return res, err
}

// ScoreTraced implements decision.TracedScorer.
func (e *Engine) ScoreTraced(ctx context.Context, req decision.ScoreRequest) (decision.ScoreResult, decision.Report, error) {
	if err := e.check(); err != nil {
		return decision.ScoreResult{}, decision.Report{Strategy: string(e.strategy)}, err
	}
	if err := decision.ValidateScoreRequest(req); err != nil {
		return decision.ScoreResult{}, decision.Report{Strategy: string(e.strategy)}, fmt.Errorf("compose: %s: %w", e.cfg.name, err)
	}
	legs := make([]leg[decision.ScoreResult], len(e.providers))
	for i, p := range e.providers {
		legs[i] = leg[decision.ScoreResult]{
			name: p.Name(), role: e.role(i), timeout: e.legTimeout(p), usage: scoreUsage,
			call: func(ctx context.Context) (decision.ScoreResult, bool, *decision.Report, error) {
				switch sp := p.(type) {
				case decision.TracedScorer:
					res, rep, err := sp.ScoreTraced(ctx, req)
					return res, true, &rep, err
				case decision.ScoredProvider:
					res, err := sp.Score(ctx, req)
					return res, true, nil, err
				default:
					return decision.ScoreResult{}, false, nil, fmt.Errorf("%s: %w", p.Name(), decision.ErrUnsupported)
				}
			},
		}
	}
	judge := func(res decision.ScoreResult) (string, string) {
		if err := decision.ValidateScoreResult(req, res); err != nil {
			return decision.AttemptInvalid, decision.InvalidDetail(err)
		}
		if e.cfg.policy != nil {
			// In question order, so the verdict never depends on map iteration.
			for _, q := range req.Questions {
				if a := res.Answers[q.ID]; a.Calibrated {
					if sel := e.cfg.policy.Evaluate(a); sel.Outcome == decision.OutcomeUncertain {
						return decision.AttemptUncertain, sel.Reason
					}
				}
			}
		}
		return decision.AttemptDecided, ""
	}
	out := run(ctx, e, legs, judge)
	rep := out.report(e)
	if v, ok := out.value(); ok {
		v.Engine = rep.Engine
		rep.Model = v.Model
		v.Report = &rep
		return v, rep, nil
	}
	return decision.ScoreResult{}, rep, out.failure(e)
}

// scoreUsage is what an answered score call consumed, nil when the engine
// reported nothing.
func scoreUsage(res decision.ScoreResult) *decision.Usage {
	if res.Usage == (decision.Usage{}) {
		return nil
	}
	u := res.Usage
	return &u
}

// ---- generic runner ----

// leg is one provider invocation: call returns the value, whether it answered
// (false = abstained), the provider's own report when it is itself traced, and
// an error.
type leg[T any] struct {
	name    string
	role    string
	timeout time.Duration
	call    func(ctx context.Context) (T, bool, *decision.Report, error)
	// usage, when set, reads what an answer consumed, for the attempt record.
	usage func(T) *decision.Usage
}

type legResult[T any] struct {
	role     string
	val      T
	answered bool
	outcome  string
	detail   string
	err      error
	latency  time.Duration
	engine   string
	attempts []decision.Attempt
}

func (r legResult[T]) accepted() bool { return r.outcome == decision.AttemptDecided }

// runLeg runs one leg under its own timeout and classifies the result. It
// returns when the provider returns OR the leg's timeout fires OR ctx is
// cancelled, whichever is first, so a provider that ignores its context cannot
// hold the caller past its timeout.
func runLeg[T any](ctx context.Context, e *Engine, l leg[T], judge func(T) (string, string)) legResult[T] {
	start := e.cfg.clock.Now() // before the timer exists, so latency never undercounts
	lctx, release := withTimeout(ctx, e.cfg.clock, l.timeout)
	defer release()

	type raw struct {
		val      T
		answered bool
		rep      *decision.Report
		err      error
	}
	done := make(chan raw, 1) // buffered: a late provider never blocks on send
	go func() {
		v, answered, rep, err := l.call(lctx)
		done <- raw{v, answered, rep, err}
	}()

	res := legResult[T]{role: l.role, engine: l.name}
	var rep *decision.Report
	select {
	case r := <-done:
		res.val, res.answered, rep, res.err = r.val, r.answered, r.rep, r.err
	case <-lctx.Done():
		res.err = context.Cause(lctx)
	}
	res.latency = e.cfg.clock.Now().Sub(start)

	cause := context.Cause(lctx) // nil unless lctx is done
	switch {
	case res.err != nil:
		res.outcome, res.detail = classify(res.err, cause)
	case !res.answered:
		res.outcome = decision.AttemptAbstained
	default:
		res.outcome, res.detail = judge(res.val)
	}

	a := decision.Attempt{Provider: l.name, Outcome: res.outcome, Detail: res.detail, Latency: res.latency, Role: l.role}
	if l.usage != nil && res.err == nil && res.answered {
		a.Usage = l.usage(res.val)
	}
	if rep != nil {
		if rep.Engine != "" {
			res.engine = rep.Engine
		}
		res.attempts = decision.MergeReport(*rep, a, res.err == nil && res.answered)
	} else {
		res.attempts = []decision.Attempt{a}
	}
	return res
}

// classify maps a leg error to an attempt outcome. cause is context.Cause of
// the leg's context: DeadlineExceeded when the leg timed out (or its parent's
// deadline passed), another non-nil value when it was cancelled.
func classify(err, cause error) (outcome, detail string) {
	// The conditions that must stay loud come first: an engine error that joins
	// several legs' errors must not have them hidden behind a quieter one.
	switch {
	case errors.Is(err, decision.ErrQuota):
		return decision.AttemptQuota, err.Error()
	case errors.Is(err, decision.ErrBudget):
		return decision.AttemptBudget, err.Error()
	case errors.Is(err, decision.ErrMisconfigured):
		return decision.AttemptMisconfigured, err.Error()
	case errors.Is(err, decision.ErrUnavailable):
		return decision.AttemptUnavailable, err.Error()
	case errors.Is(err, decision.ErrUnsupported):
		return decision.AttemptUnsupported, err.Error()
	case errors.Is(err, decision.ErrAuth):
		return decision.AttemptAuth, err.Error()
	case errors.Is(err, decision.ErrInvalidRequest):
		return decision.AttemptRejected, err.Error()
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(cause, context.DeadlineExceeded):
		return decision.AttemptTimeout, err.Error()
	case errors.Is(err, context.Canceled) && cause != nil:
		return decision.AttemptCancelled, ""
	default:
		return decision.AttemptError, err.Error()
	}
}

// runOut is what a strategy produced.
type runOut[T any] struct {
	results   []legResult[T] // started legs, in start order
	winner    int            // index into results of the accepted answer, -1 if none
	answerIdx int            // index into results of the answer returned (the winner, else an uncertain one), -1 if none
	fallback  bool
	hedge     bool
	// primaryStands is true when a Hedged primary answered with an abstention or
	// an uncertain answer that does not trigger the backup: that answer is the
	// result, whatever the backup was doing.
	primaryStands bool
	// halted is true when a started leg reported an exhausted allowance or a
	// misconfigured endpoint that the engine does not fail over (see halts): the
	// other legs were cancelled and no answer is returned, only the failure.
	halted bool
}

// pickAnswer chooses the answer an engine returns: the accepted one, else the
// first uncertain one (an answer, just not a clear one), else none.
func (o runOut[T]) pickAnswer() int {
	if o.winner >= 0 {
		return o.winner
	}
	if o.halted {
		return -1
	}
	if o.primaryStands {
		if o.results[0].outcome == decision.AttemptUncertain {
			return 0
		}
		return -1 // an abstention
	}
	for i, r := range o.results {
		if r.outcome == decision.AttemptUncertain {
			return i
		}
	}
	return -1
}

func (o runOut[T]) value() (T, bool) {
	if o.answerIdx < 0 {
		var zero T
		return zero, false
	}
	return o.results[o.answerIdx].val, true
}

func (o runOut[T]) abstained() bool {
	for _, r := range o.results {
		if r.outcome == decision.AttemptAbstained {
			return true
		}
	}
	return false
}

// refused reports whether a started leg ended in a refusal this engine does not
// absorb (see halts): an exhausted allowance, a spent budget or a misconfigured
// endpoint. Neither an abstention nor an uncertain answer elsewhere in the call may
// swallow it: a backup that an abstention (OnAbstain) or an uncertain answer
// (OnUncertain) started and that was out of budget would otherwise read as "nobody
// decided" or as the primary's answer, which a product answers with its paid
// main-LLM path while the chain never stops. A spent budget counts even when a
// Hedged primary stands on its own abstention or uncertain answer; the other
// refusals do not, because that primary's answer is then the call's.
func (o runOut[T]) refused(e *Engine) bool {
	for _, r := range o.results {
		if r.outcome == decision.AttemptBudget || (!o.primaryStands && e.halts(r.outcome)) {
			return true
		}
	}
	return false
}

// failure is the error returned when no engine answered: every failed leg,
// joined, each still matchable with errors.Is.
func (o runOut[T]) failure(e *Engine) error {
	var errs []error
	for _, r := range o.results {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("%s: %s: %w", r.engine, r.outcome, r.err))
		} else {
			errs = append(errs, fmt.Errorf("%s: %s: %s", r.engine, r.outcome, r.detail))
		}
	}
	return fmt.Errorf("compose: %s: no engine answered: %w", e.cfg.name, errors.Join(errs...))
}

func (o runOut[T]) report(e *Engine) decision.Report {
	rep := decision.Report{Strategy: string(e.strategy), FallbackFired: o.fallback, HedgeFired: o.hedge}
	for _, r := range o.results {
		rep.Attempts = append(rep.Attempts, r.attempts...)
	}
	if o.answerIdx >= 0 {
		rep.Engine = o.results[o.answerIdx].engine
	}
	return rep
}

// run executes the engine's strategy over legs.
func run[T any](ctx context.Context, e *Engine, legs []leg[T], judge func(T) (string, string)) runOut[T] {
	var out runOut[T]
	if e.strategy == StrategyHedged || e.strategy == StrategyRace {
		out = runConcurrent(ctx, e, legs, judge)
	} else {
		out = runSequential(ctx, e, legs, judge)
	}
	out.answerIdx = out.pickAnswer()
	if out.winner < 0 && out.refused(e) {
		// A refusal the engine does not absorb (a spent budget, an exhausted allowance,
		// a misconfigured endpoint) is the result, even when another leg answered
		// uncertain or abstained: a caller that got the uncertain answer instead would
		// never learn that the backup was refused, and would keep calling it.
		out.answerIdx = -1
	}
	return out
}

// runSequential serves Single and Fallback: legs run one after another, and the
// next starts only when the previous failed in a way that triggers it.
func runSequential[T any](ctx context.Context, e *Engine, legs []leg[T], judge func(T) (string, string)) runOut[T] {
	out := runOut[T]{winner: -1}
	for i, l := range legs {
		r := runLeg(ctx, e, l, judge)
		out.results = append(out.results, r)
		if r.accepted() {
			out.winner = len(out.results) - 1
			break
		}
		if !e.triggers(r.outcome) || i == len(legs)-1 {
			break
		}
		out.fallback = true
	}
	return out
}

type indexed[T any] struct {
	idx int
	res legResult[T]
}

// runConcurrent serves Hedged and Race. Each leg runs under runLeg in its own
// goroutine, the first accepted answer wins, and every other leg is cancelled
// through the shared context. Cancelled legs are recorded but not waited for.
func runConcurrent[T any](ctx context.Context, e *Engine, legs []leg[T], judge func(T) (string, string)) runOut[T] {
	rctx, cancelAll := context.WithCancelCause(ctx)
	defer cancelAll(nil)

	results := make([]*legResult[T], len(legs))
	started := make([]bool, len(legs))
	startedAt := make([]time.Time, len(legs))
	ch := make(chan indexed[T], len(legs)) // buffered: finished legs never block
	running := 0
	startLeg := func(i int) {
		started[i], startedAt[i] = true, e.cfg.clock.Now()
		running++
		go func() { ch <- indexed[T]{i, runLeg(rctx, e, legs[i], judge)} }()
	}

	out := runOut[T]{winner: -1}
	hedgeC := make(chan struct{}) // closed by the hedge timer, which fires at most once
	if e.strategy == StrategyRace {
		for i := range legs {
			startLeg(i)
		}
	} else {
		startLeg(0)
		timer := e.cfg.clock.AfterFunc(e.hedgeAfter, func() { close(hedgeC) })
		defer timer.Stop()
	}

	winnerLeg := -1
	for running > 0 && winnerLeg < 0 && !out.primaryStands && !out.halted {
		select {
		case <-hedgeC:
			hedgeC = nil // a nil channel never fires again
			if !started[1] {
				out.hedge = true
				startLeg(1)
			}
		case r := <-ch:
			running--
			rr := r.res
			results[r.idx] = &rr
			switch {
			case rr.accepted():
				winnerLeg = r.idx
			case e.halts(rr.outcome) && (e.strategy == StrategyRace || r.idx == 0):
				// A race pays every engine on every call, so one engine's refusal
				// of the allowance ends the call. A hedge's backup is only the
				// primary's stand-in: the primary's refusal ends it, the backup's
				// own is just a failed backup.
				out.halted = true
			case e.strategy == StrategyHedged && r.idx == 0 && e.triggers(rr.outcome):
				if !started[1] {
					out.fallback = true
					startLeg(1)
				}
			case e.strategy == StrategyHedged && r.idx == 0 && (rr.outcome == decision.AttemptAbstained || rr.outcome == decision.AttemptUncertain):
				out.primaryStands = true
			}
		}
	}
	// A primary that is still running when its hedge answered did not answer
	// within the latency budget: cancel it with ErrSuperseded so a Breaker around
	// it counts that as a failure instead of a neutral cancellation.
	cause := context.Canceled
	superseded := e.strategy == StrategyHedged && winnerLeg == 1 && out.hedge && results[0] == nil
	if superseded {
		cause = ErrSuperseded
	}
	cancelAll(cause)

	// Record every started leg in leg order; one still running is a loser.
	for i := range legs {
		if i == winnerLeg {
			out.winner = len(out.results)
		}
		switch {
		case results[i] != nil:
			out.results = append(out.results, *results[i])
		case started[i]:
			lat := e.cfg.clock.Now().Sub(startedAt[i])
			a := decision.Attempt{Provider: legs[i].name, Outcome: decision.AttemptCancelled, Latency: lat, Role: legs[i].role}
			if superseded && i == 0 {
				a.Detail = "superseded by hedge"
			}
			out.results = append(out.results, legResult[T]{
				role: legs[i].role, outcome: decision.AttemptCancelled, latency: lat,
				engine: legs[i].name, attempts: []decision.Attempt{a},
			})
		}
	}
	return out
}
