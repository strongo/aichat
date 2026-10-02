package compose

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

// ---- helpers ----

// retryErr is an engine error that asks callers to wait.
type retryErr struct{ d time.Duration }

func (e retryErr) Error() string             { return "rate limited" }
func (e retryErr) RetryDelay() time.Duration { return e.d }

// gatedResult blocks until released (or its context is done), then returns a
// scripted result.
type gatedResult struct {
	*fake
	release chan struct{}
}

func newGatedResult(name string, d decision.Decision, ok bool, err error) *gatedResult {
	g := &gatedResult{release: make(chan struct{})}
	g.fake = &fake{name: name, decide: func(ctx context.Context, _ decision.Request) (decision.Decision, bool, error) {
		select {
		case <-g.release:
			return d, ok, err
		case <-ctx.Done():
			return decision.Decision{}, false, ctx.Err()
		}
	}}
	return g
}

func uncertainDecision(intent string) decision.Decision {
	d := answer(intent, 0.07)
	d.Calibrated = true
	d.Scores = map[string]float64{"m/i": 0.38, "m/j": 0.35, "x": 0.33}
	return d
}

type outcome struct {
	d   decision.Decision
	ok  bool
	rep decision.Report
	err error
}

// startHedged runs e.DecideTraced in the background.
func startDecide(e *Engine) <-chan outcome {
	ch := make(chan outcome, 1)
	go func() {
		d, ok, rep, err := e.DecideTraced(context.Background(), request())
		ch <- outcome{d, ok, rep, err}
	}()
	return ch
}

// fireHedge waits for the primary and the hedge timer, advances past the hedge
// budget and waits for the backup to start.
func fireHedge(t *testing.T, clk *fakeClock, after time.Duration, backup *fake) {
	t.Helper()
	clk.WaitTimers(t, 2) // the primary's timeout and the hedge budget
	clk.Advance(after)
	waitCalls(t, backup, 1)
}

// ---- Breaker: what counts as a failure ----

func TestBreaker_CallerFaultsNeverOpenIt(t *testing.T) {
	for name, err := range map[string]error{
		"invalid request": fmt.Errorf("engine: %w", decision.ErrInvalidRequest),
		"auth":            fmt.Errorf("engine: %w", decision.ErrAuth),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, WithBreakerThreshold(1))
			r.fail(err)
			for i := 0; i < 10; i++ {
				if got := r.call(t); !errors.Is(got, err) {
					t.Fatalf("call %d: %v", i, got)
				}
			}
			if r.b.State() != BreakerClosed {
				t.Fatalf("a caller fault opened the breaker: %v", r.b.State())
			}
		})
	}
}

func TestBreaker_RetryAfterIsAMinimumOpenTime(t *testing.T) {
	r := newRig(t, WithBreakerThreshold(2), WithBreakerCooldown(30*time.Second))
	r.fail(retryErr{90 * time.Second})
	_ = r.call(t)
	_ = r.call(t)
	if r.b.State() != BreakerOpen {
		t.Fatalf("state = %v", r.b.State())
	}
	r.clk.Advance(31 * time.Second) // past the cooldown, short of the Retry-After
	if err := r.call(t); !errors.Is(err, decision.ErrUnavailable) {
		t.Fatalf("Retry-After ignored: %v", err)
	}
	r.clk.Advance(60 * time.Second)
	r.p.decide = instant("jev", "i").decide
	if err := r.call(t); err != nil || r.b.State() != BreakerClosed {
		t.Fatalf("probe after Retry-After: err=%v state=%v", err, r.b.State())
	}
}

func TestBreaker_FailedProbeHonoursItsRetryAfterAndStaleRunsForgetTheirs(t *testing.T) {
	r := newRig(t, WithBreakerThreshold(2), WithBreakerCooldown(30*time.Second), WithBreakerWindow(10*time.Second))
	// A Retry-After from a failure run that expired must not lengthen a later opening.
	r.fail(retryErr{600 * time.Second})
	_ = r.call(t)
	r.clk.Advance(11 * time.Second)
	r.fail(errBoom)
	_ = r.call(t) // starts a new run (count 1)
	_ = r.call(t) // count 2: opens, for the plain cooldown
	if r.b.State() != BreakerOpen {
		t.Fatalf("state = %v", r.b.State())
	}
	r.clk.Advance(30 * time.Second)
	// The probe fails with a Retry-After of its own.
	r.fail(retryErr{120 * time.Second})
	if err := r.call(t); !errors.Is(err, retryErr{120 * time.Second}) || r.b.State() != BreakerOpen {
		t.Fatalf("probe: err=%v state=%v", err, r.b.State())
	}
	r.clk.Advance(31 * time.Second)
	if err := r.call(t); !errors.Is(err, decision.ErrUnavailable) {
		t.Fatalf("probe's Retry-After ignored: %v", err)
	}
	r.clk.Advance(90 * time.Second)
	r.p.decide = instant("jev", "i").decide
	if err := r.call(t); err != nil {
		t.Fatalf("after the probe's Retry-After: %v", err)
	}
}

func TestBreaker_ResultOfACallStartedBeforeOpeningIsIgnored(t *testing.T) {
	r := newRig(t, WithBreakerThreshold(1), WithBreakerCooldown(30*time.Second))
	release := make(chan struct{})
	var n atomic.Int32
	r.p.decide = func(context.Context, decision.Request) (decision.Decision, bool, error) {
		if n.Add(1) == 1 {
			<-release
			return decision.Decision{}, false, errBoom // late failure, then (below) late success
		}
		return decision.Decision{}, false, errBoom
	}
	late := make(chan error, 1)
	go func() { _, _, err := r.b.Decide(context.Background(), request()); late <- err }()
	// n, not the fake's call counter: the first call must be the one that blocks.
	waitFor(t, func() bool { return n.Load() >= 1 }, "the first call to reach the engine")
	_ = r.call(t) // opens the breaker
	if r.b.State() != BreakerOpen {
		t.Fatalf("state = %v", r.b.State())
	}
	r.clk.Advance(20 * time.Second)
	close(release)
	if err := <-late; !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	// The stale failure did not restart the cooldown: the probe is due at +30s.
	r.clk.Advance(10 * time.Second)
	r.p.decide = instant("jev", "i").decide
	if err := r.call(t); err != nil || r.b.State() != BreakerClosed {
		t.Fatalf("stale failure extended the cooldown: err=%v state=%v", err, r.b.State())
	}
}

func TestBreaker_LateSuccessCannotCloseAnOpenedBreaker(t *testing.T) {
	r := newRig(t, WithBreakerThreshold(1))
	release := make(chan struct{})
	var n atomic.Int32
	r.p.decide = func(context.Context, decision.Request) (decision.Decision, bool, error) {
		if n.Add(1) == 1 {
			<-release
			return answer("i", 0.9), true, nil
		}
		return decision.Decision{}, false, errBoom
	}
	late := make(chan error, 1)
	go func() { _, _, err := r.b.Decide(context.Background(), request()); late <- err }()
	// n, not the fake's call counter: the first call must be the one that blocks.
	waitFor(t, func() bool { return n.Load() >= 1 }, "the first call to reach the engine")
	_ = r.call(t)
	close(release)
	if err := <-late; err != nil {
		t.Fatal(err)
	}
	if r.b.State() != BreakerOpen {
		t.Fatalf("a late success from before the opening closed the breaker: %v", r.b.State())
	}
}

func TestBreaker_HungProbeIsBoundedAndTheBreakerReopens(t *testing.T) {
	noLeak(t)
	r := newRig(t, WithBreakerThreshold(1), WithBreakerCooldown(time.Second), WithBreakerProbeTimeout(5*time.Second))
	r.fail(errBoom)
	_ = r.call(t) // opens
	block := make(chan struct{})
	var healthy atomic.Bool
	r.p.decide = func(context.Context, decision.Request) (decision.Decision, bool, error) { // ignores its context
		if healthy.Load() {
			return answer("i", 0.9), true, nil
		}
		<-block
		return decision.Decision{}, false, errBoom
	}
	r.clk.Advance(time.Second)
	done := make(chan error, 1)
	go func() { _, _, err := r.b.Decide(context.Background(), request()); done <- err }()
	r.clk.WaitTimers(t, 1) // the probe's timeout
	if r.b.State() != BreakerHalfOpen {
		t.Fatalf("state = %v", r.b.State())
	}
	r.clk.Advance(5 * time.Second)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) || r.b.State() != BreakerOpen {
		t.Fatalf("hung probe: err=%v state=%v", err, r.b.State())
	}
	// The breaker is not stuck half-open: after the cooldown a new probe runs.
	healthy.Store(true)
	r.clk.Advance(time.Second)
	if err := r.call(t); err != nil || r.b.State() != BreakerClosed {
		t.Fatalf("next probe: err=%v state=%v", err, r.b.State())
	}
	close(block)
	waitFor(t, func() bool { return r.p.running.Load() == 0 }, "abandoned probe call to end")
}

func TestBreaker_ReturnsWhenTheContextIsDoneEvenIfTheEngineIgnoresIt(t *testing.T) {
	noLeak(t)
	r := newRig(t, WithBreakerThreshold(1))
	block := make(chan struct{})
	r.p.decide = func(context.Context, decision.Request) (decision.Decision, bool, error) {
		<-block
		return decision.Decision{}, false, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := r.b.Decide(ctx, request()); done <- err }()
	waitCalls(t, r.p, 1)
	cancel() // the caller gave up: neutral
	if err := <-done; !errors.Is(err, context.Canceled) || r.b.State() != BreakerClosed {
		t.Fatalf("err=%v state=%v", err, r.b.State())
	}
	close(block)
	waitFor(t, func() bool { return r.p.running.Load() == 0 }, "abandoned call to end")
}

func TestBreaker_NilEngine(t *testing.T) {
	b := NewBreaker(nil)
	if b.Name() != "<nil>" || b.DecisionTimeout() != 0 {
		t.Fatalf("name=%q timeout=%v", b.Name(), b.DecisionTimeout())
	}
	if _, _, err := b.Decide(context.Background(), request()); !errors.Is(err, ErrNoEngine) {
		t.Fatalf("decide: %v", err)
	}
	if _, err := b.Score(context.Background(), scoreRequest()); !errors.Is(err, decision.ErrUnsupported) {
		t.Fatalf("score: %v", err)
	}
}

// ---- Hedged: a hung primary opens its breaker ----

func TestHedged_HungPrimaryOpensItsBreaker(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	hang := cooperative("jev")
	b := NewBreaker(hang, WithBreakerClock(clk), WithBreakerThreshold(3))
	backup := instant("llm", "j")
	e := Hedged(b, backup, 100*time.Millisecond, WithClock(clk))
	for i := 1; i <= 3; i++ {
		ch := startDecide(e)
		clk.WaitTimers(t, 2)
		clk.Advance(100 * time.Millisecond)
		o := <-ch
		if o.err != nil || o.d.Intent.Value != "j" || !o.rep.HedgeFired {
			t.Fatalf("call %d: %+v", i, o)
		}
		if a := o.rep.Attempts[0]; a.Provider != "jev" || a.Outcome != decision.AttemptCancelled || a.Detail != "superseded by hedge" {
			t.Fatalf("call %d: primary attempt = %+v", i, a)
		}
		want := BreakerClosed
		if i == 3 {
			want = BreakerOpen
		}
		waitFor(t, func() bool { return b.State() == want }, fmt.Sprintf("breaker state %v after call %d", want, i))
	}
	// Open now: the primary fails fast and the backup starts at once.
	o := <-startDecide(e)
	if o.err != nil || !o.rep.FallbackFired || o.rep.HedgeFired {
		t.Fatalf("%+v", o)
	}
	if got := attemptOutcomes(o.rep); !reflect.DeepEqual(got, []string{"jev:unavailable", "llm:decided"}) {
		t.Fatalf("attempts = %v", got)
	}
}

// A Race loser is not slow, only slower: it never counts against a breaker.
func TestRace_LoserDoesNotOpenItsBreaker(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	b := NewBreaker(cooperative("jev"), WithBreakerClock(clk), WithBreakerThreshold(1))
	e := Race([]decision.Provider{b, instant("llm", "j")}, WithClock(clk))
	for i := 0; i < 3; i++ {
		d, _ := decideOK(t, e)
		if d.Intent.Value != "j" {
			t.Fatal(d)
		}
	}
	waitFor(t, func() bool { return b.inner.(*fake).running.Load() == 0 }, "cancelled racers to exit")
	if b.State() != BreakerClosed {
		t.Fatalf("state = %v", b.State())
	}
}

// ---- Hedged: one rule for abstain and uncertain ----

func TestHedged_AbstainFromThePrimaryStandsWhateverTheTiming(t *testing.T) {
	noLeak(t)
	const after = 100 * time.Millisecond
	// Primary answers (abstains) AFTER the hedge fired: the abstention stands and
	// the backup, still running, is cancelled -- exactly as when the primary
	// abstains before the hedge fires.
	clk := newFakeClock()
	primary := newGatedResult("jev", decision.Decision{}, false, nil)
	backup := newGated("llm", "j")
	ch := startDecide(Hedged(primary, backup, after, WithClock(clk)))
	fireHedge(t, clk, after, backup.fake)
	close(primary.release)
	o := <-ch
	if o.ok || o.err != nil || !o.rep.HedgeFired {
		t.Fatalf("%+v", o)
	}
	if got := attemptOutcomes(o.rep); !reflect.DeepEqual(got, []string{"jev:abstained", "llm:cancelled"}) {
		t.Fatalf("attempts = %v", got)
	}
	waitFor(t, func() bool { return backup.running.Load() == 0 }, "cancelled backup to exit")

	// A backup that already returned an uncertain answer does not change that.
	clk = newFakeClock()
	primary = newGatedResult("jev", decision.Decision{}, false, nil)
	backup2 := newGatedResult("llm", uncertainDecision("j"), true, nil)
	opts := []Option{WithClock(clk), WithPolicy(decision.NarrowingPolicy())}
	ch = startDecide(Hedged(primary, backup2, after, opts...))
	fireHedge(t, clk, after, backup2.fake)
	close(backup2.release)
	waitFor(t, func() bool { return backup2.running.Load() == 0 }, "backup to answer")
	// The engine has no hook to say its loop has taken the backup's answer, and
	// the primary must finish after that: give the (microsecond) hand-off ample time.
	time.Sleep(50 * time.Millisecond)
	close(primary.release)
	o = <-ch
	if o.ok || o.err != nil {
		t.Fatalf("%+v", o)
	}
	if got := attemptOutcomes(o.rep); !reflect.DeepEqual(got, []string{"jev:abstained", "llm:uncertain"}) {
		t.Fatalf("attempts = %v", got)
	}

	// With the opt-in, the same late abstention hands the call to the backup.
	clk = newFakeClock()
	primary = newGatedResult("jev", decision.Decision{}, false, nil)
	backup = newGated("llm", "j")
	ch = startDecide(Hedged(primary, backup, after, WithClock(clk), WithFallbackOn(OnAbstain)))
	fireHedge(t, clk, after, backup.fake)
	close(primary.release)
	waitFor(t, func() bool { return primary.running.Load() == 0 }, "primary to abstain")
	close(backup.release)
	o = <-ch
	if !o.ok || o.d.Intent.Value != "j" || o.rep.Engine != "llm" {
		t.Fatalf("opt-in: %+v", o)
	}
}

func TestHedged_UncertainFromThePrimaryStandsWhateverTheTiming(t *testing.T) {
	noLeak(t)
	const after = 100 * time.Millisecond
	pol := decision.NarrowingPolicy()
	clk := newFakeClock()
	primary := newGatedResult("jev", uncertainDecision("i"), true, nil)
	backup := newGated("llm", "j")
	ch := startDecide(Hedged(primary, backup, after, WithClock(clk), WithPolicy(pol)))
	fireHedge(t, clk, after, backup.fake)
	close(primary.release)
	o := <-ch
	if !o.ok || o.err != nil || o.d.Intent.Value != "i" || o.rep.Engine != "jev" {
		t.Fatalf("%+v", o)
	}
	if got := attemptOutcomes(o.rep); !reflect.DeepEqual(got, []string{"jev:uncertain", "llm:cancelled"}) {
		t.Fatalf("attempts = %v", got)
	}

	clk = newFakeClock()
	primary = newGatedResult("jev", uncertainDecision("i"), true, nil)
	backup = newGated("llm", "j")
	ch = startDecide(Hedged(primary, backup, after, WithClock(clk), WithPolicy(pol), WithFallbackOn(OnUncertain)))
	fireHedge(t, clk, after, backup.fake)
	close(primary.release)
	waitFor(t, func() bool { return primary.running.Load() == 0 }, "primary to answer")
	close(backup.release)
	o = <-ch
	if !o.ok || o.d.Intent.Value != "j" || o.rep.Engine != "llm" {
		t.Fatalf("opt-in: %+v", o)
	}
}

func TestHedged_UncertainScoreFromThePrimaryStands(t *testing.T) {
	noLeak(t)
	const after = 100 * time.Millisecond
	clk := newFakeClock()
	release := make(chan struct{})
	primary := &fake{name: "jev", score: func(ctx context.Context, _ decision.ScoreRequest) (decision.ScoreResult, error) {
		select {
		case <-release:
			return uncertainScore("jev"), nil
		case <-ctx.Done():
			return decision.ScoreResult{}, ctx.Err()
		}
	}}
	backup := &fake{name: "llm", score: func(ctx context.Context, _ decision.ScoreRequest) (decision.ScoreResult, error) {
		<-ctx.Done()
		return decision.ScoreResult{}, ctx.Err()
	}}
	e := Hedged(primary, backup, after, WithClock(clk), WithPolicy(decision.NarrowingPolicy()))
	type res struct {
		r   decision.ScoreResult
		rep decision.Report
		err error
	}
	ch := make(chan res, 1)
	go func() {
		r, rep, err := e.ScoreTraced(context.Background(), scoreRequest())
		ch <- res{r, rep, err}
	}()
	fireHedge(t, clk, after, backup)
	close(release)
	o := <-ch
	if o.err != nil || o.r.Engine != "jev" || !o.rep.HedgeFired {
		t.Fatalf("%+v", o)
	}
	if got := attemptOutcomes(o.rep); !reflect.DeepEqual(got, []string{"jev:uncertain", "llm:cancelled"}) {
		t.Fatalf("attempts = %v", got)
	}
}

// ---- classification, models, degenerate engines ----

func TestFallback_CallerFaultsAreNamedAndStillFailOver(t *testing.T) {
	noLeak(t)
	for outcome, err := range map[string]error{
		decision.AttemptAuth:     fmt.Errorf("jev: %w", decision.ErrAuth),
		decision.AttemptRejected: fmt.Errorf("jev: %w", decision.ErrInvalidRequest),
	} {
		d, rep := decideOK(t, Fallback(failing("jev", err), instant("llm", "j"), WithClock(newFakeClock())))
		if d.Intent.Value != "j" || !rep.FallbackFired {
			t.Fatalf("%s: %+v", outcome, rep)
		}
		if got := attemptOutcomes(rep); !reflect.DeepEqual(got, []string{"jev:" + outcome, "llm:decided"}) {
			t.Fatalf("attempts = %v", got)
		}
	}
}

func TestEngine_ReportsTheAnsweringModel(t *testing.T) {
	noLeak(t)
	p := &fake{name: "jev", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		d := answer("i", 0.9)
		d.Model = "jev-1.2.3"
		return d, true, nil
	}}
	d, rep := decideOK(t, Single(p, WithClock(newFakeClock())))
	if d.Model != "jev-1.2.3" || rep.Model != "jev-1.2.3" {
		t.Fatalf("d=%q rep=%q", d.Model, rep.Model)
	}
	res, rep, err := Single(scorer("jev", goodScore("jev"), nil), WithClock(newFakeClock())).ScoreTraced(context.Background(), scoreRequest())
	if err != nil || res.Model != "m-1" || rep.Model != "m-1" || res.Report.Model != "m-1" {
		t.Fatalf("score: %+v %+v %v", res, rep, err)
	}
}

func TestEngine_DegenerateConstructionsNeverPanic(t *testing.T) {
	ctx := context.Background()
	engines := map[string]*Engine{
		"race none":     Race(nil),
		"race empty":    Race([]decision.Provider{}),
		"single nil":    Single(nil),
		"fallback nil":  Fallback(nil, nil),
		"hedged nil":    Hedged(nil, instant("llm", "j"), time.Second),
		"race one nil":  Race([]decision.Provider{instant("llm", "j"), nil}),
		"zero engine":   {},
		"named by hand": Race(nil, WithName("custom")),
	}
	for name, e := range engines {
		t.Run(name, func(t *testing.T) {
			_ = e.Name()
			_ = e.DecisionTimeout()
			if _, ok, rep, err := e.DecideTraced(ctx, request()); ok || !errors.Is(err, ErrNoEngine) {
				t.Fatalf("decide: ok=%v err=%v rep=%+v", ok, err, rep)
			}
			if _, _, err := e.ScoreTraced(ctx, scoreRequest()); !errors.Is(err, ErrNoEngine) {
				t.Fatalf("score: %v", err)
			}
		})
	}
	if got := Race(nil).DecisionTimeout(); got != 0 {
		t.Fatalf("empty race timeout = %v", got)
	}
	if got := Single(nil).Name(); got != "single(<nil>)" {
		t.Fatalf("name = %q", got)
	}
}

func TestScore_UncertainDetailNamesTheReasonNotTheQuestion(t *testing.T) {
	noLeak(t)
	_, rep, err := Single(scorer("jev", uncertainScore("jev"), nil), WithClock(newFakeClock()), WithPolicy(decision.NarrowingPolicy())).
		ScoreTraced(context.Background(), scoreRequest())
	if err != nil || rep.Attempts[0].Outcome != decision.AttemptUncertain || rep.Attempts[0].Detail != decision.ReasonLowConfidence {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
}
