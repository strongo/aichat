package compose

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

var errBoom = errors.New("boom")

func decideOK(t *testing.T, e *Engine) (decision.Decision, decision.Report) {
	t.Helper()
	d, ok, rep, err := e.DecideTraced(context.Background(), request())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v report=%+v", ok, err, rep)
	}
	return d, rep
}

func TestSingle_Decides(t *testing.T) {
	noLeak(t)
	e := Single(instant("jev", "i"), WithClock(newFakeClock()))
	d, rep := decideOK(t, e)
	if d.Intent.Value != "i" || rep.Strategy != "single" || rep.Engine != "jev" || rep.FallbackFired || rep.HedgeFired {
		t.Fatalf("d=%+v rep=%+v", d, rep)
	}
	if got := rep.Attempts[0]; got.Provider != "jev" || got.Outcome != decision.AttemptDecided || got.Role != "primary" {
		t.Fatalf("attempt = %+v", got)
	}
	if e.Name() != "single(jev)" || e.Strategy() != StrategySingle {
		t.Fatalf("name=%q strategy=%q", e.Name(), e.Strategy())
	}
	// Decide (untraced) delegates.
	if d2, ok, err := e.Decide(context.Background(), request()); err != nil || !ok || d2.Intent.Value != "i" {
		t.Fatalf("Decide: %v %v %v", d2, ok, err)
	}
}

func TestEngine_WithNameAndDefaultTimeout(t *testing.T) {
	p := noTimeout{instant("jev", "i")}
	e := Single(p, WithName("primary-engine"), WithDefaultTimeout(250*time.Millisecond))
	if e.Name() != "primary-engine" || e.DecisionTimeout() != 250*time.Millisecond {
		t.Fatalf("name=%q timeout=%v", e.Name(), e.DecisionTimeout())
	}
	if Single(p).DecisionTimeout() != DefaultTimeout {
		t.Fatal("default timeout not applied")
	}
}

func TestSingle_ErrorAbstainAndInvalid(t *testing.T) {
	noLeak(t)
	_, ok, rep, err := Single(failing("jev", errBoom)).DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, errBoom) || rep.Attempts[0].Outcome != decision.AttemptError {
		t.Fatalf("error: ok=%v err=%v rep=%+v", ok, err, rep)
	}
	_, ok, rep, err = Single(abstaining("jev")).DecideTraced(context.Background(), request())
	if ok || err != nil || rep.Attempts[0].Outcome != decision.AttemptAbstained || rep.Engine != "" {
		t.Fatalf("abstain: ok=%v err=%v rep=%+v", ok, err, rep)
	}
	bad := &fake{name: "jev", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return answer("nope", 0.9), true, nil
	}}
	_, ok, rep, err = Single(bad).DecideTraced(context.Background(), request())
	if ok || err == nil || rep.Attempts[0].Outcome != decision.AttemptInvalid || rep.Attempts[0].Detail == "" {
		t.Fatalf("invalid: ok=%v err=%v rep=%+v", ok, err, rep)
	}
}

func TestSingle_TimeoutCooperativeAndRude(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()

	p := cooperative("jev")
	p.timeout = 800 * time.Millisecond
	e := Single(p, WithClock(clk))
	type res struct {
		rep decision.Report
		err error
	}
	done := make(chan res, 1)
	go func() {
		_, _, rep, err := e.DecideTraced(context.Background(), request())
		done <- res{rep, err}
	}()
	clk.WaitTimers(t, 1)
	clk.Advance(799 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("returned before the timeout")
	case <-time.After(20 * time.Millisecond):
	}
	clk.Advance(time.Millisecond)
	r := <-done
	if !errors.Is(r.err, context.DeadlineExceeded) || r.rep.Attempts[0].Outcome != decision.AttemptTimeout {
		t.Fatalf("err=%v rep=%+v", r.err, r.rep)
	}
	if r.rep.Attempts[0].Latency != 800*time.Millisecond {
		t.Fatalf("latency = %v", r.rep.Attempts[0].Latency)
	}

	// A provider that ignores its context still cannot hold the caller past its timeout.
	release := make(chan struct{})
	rude := &fake{name: "rude", timeout: time.Second, decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		<-release
		return decision.Decision{}, false, nil
	}}
	e = Single(rude, WithClock(clk))
	go func() {
		_, _, rep, err := e.DecideTraced(context.Background(), request())
		done <- res{rep, err}
	}()
	clk.WaitTimers(t, 1)
	clk.Advance(time.Second)
	r = <-done
	if r.rep.Attempts[0].Outcome != decision.AttemptTimeout {
		t.Fatalf("rude: %+v", r.rep)
	}
	close(release) // let the rude provider's goroutine finish so noLeak passes
}

func TestFallback_PrimaryHealthyNeverCallsBackup(t *testing.T) {
	noLeak(t)
	primary, backup := instant("jev", "i"), instant("llm", "j")
	d, rep := decideOK(t, Fallback(primary, backup, WithClock(newFakeClock())))
	// Two engines that would disagree: the primary's answer is used.
	if d.Intent.Value != "i" || backup.calls.Load() != 0 || rep.FallbackFired || len(rep.Attempts) != 1 {
		t.Fatalf("d=%+v backupCalls=%d rep=%+v", d, backup.calls.Load(), rep)
	}
}

func TestFallback_FailureKindsStartBackup(t *testing.T) {
	noLeak(t)
	invalid := &fake{name: "jev", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return answer("nope", 0.9), true, nil
	}}
	cases := map[string]struct {
		primary *fake
		outcome string
	}{
		"error":       {failing("jev", errBoom), decision.AttemptError},
		"unavailable": {failing("jev", decision.ErrUnavailable), decision.AttemptUnavailable},
		"invalid":     {invalid, decision.AttemptInvalid},
		"unsupported": {failing("jev", decision.ErrUnsupported), decision.AttemptUnsupported},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d, rep := decideOK(t, Fallback(tc.primary, instant("llm", "j"), WithClock(newFakeClock())))
			if d.Intent.Value != "j" || !rep.FallbackFired || rep.HedgeFired || rep.Engine != "llm" {
				t.Fatalf("d=%+v rep=%+v", d, rep)
			}
			want := []string{"jev:" + tc.outcome, "llm:decided"}
			if got := attemptOutcomes(rep); !reflect.DeepEqual(got, want) {
				t.Fatalf("attempts = %v want %v", got, want)
			}
			if rep.Attempts[1].Role != "backup" {
				t.Fatalf("role = %q", rep.Attempts[1].Role)
			}
		})
	}
}

func TestFallback_TimeoutStartsBackup(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	primary := cooperative("jev")
	primary.timeout = time.Second
	e := Fallback(primary, instant("llm", "j"), WithClock(clk))
	type res struct {
		d   decision.Decision
		rep decision.Report
	}
	done := make(chan res, 1)
	go func() {
		d, _, rep, _ := e.DecideTraced(context.Background(), request())
		done <- res{d, rep}
	}()
	clk.WaitTimers(t, 1)
	clk.Advance(time.Second)
	r := <-done
	if r.d.Intent.Value != "j" || !r.rep.FallbackFired || attemptOutcomes(r.rep)[0] != "jev:timeout" {
		t.Fatalf("%+v", r)
	}
}

func TestFallback_AbstainAndUncertainDoNotTriggerByDefault(t *testing.T) {
	noLeak(t)
	backup := instant("llm", "j")
	_, ok, rep, err := Fallback(abstaining("jev"), backup, WithClock(newFakeClock())).DecideTraced(context.Background(), request())
	if ok || err != nil || backup.calls.Load() != 0 || rep.FallbackFired {
		t.Fatalf("abstain: ok=%v err=%v backup=%d", ok, err, backup.calls.Load())
	}

	uncertain := &fake{name: "jev", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		d := answer("i", 0.07)
		d.Calibrated = true
		d.Scores = map[string]float64{"m/i": 0.38, "m/j": 0.35, "x": 0.33}
		return d, true, nil
	}}
	e := Fallback(uncertain, backup, WithClock(newFakeClock()), WithPolicy(decision.NarrowingPolicy()))
	d, ok, rep, err := e.DecideTraced(context.Background(), request())
	if !ok || err != nil || backup.calls.Load() != 0 || rep.Engine != "jev" || d.Intent.Value != "i" {
		t.Fatalf("uncertain: ok=%v err=%v backup=%d rep=%+v", ok, err, backup.calls.Load(), rep)
	}
	if rep.Attempts[0].Outcome != decision.AttemptUncertain {
		t.Fatalf("attempt = %+v", rep.Attempts[0])
	}
}

func TestFallback_OptInTriggers(t *testing.T) {
	noLeak(t)
	d, rep := decideOK(t, Fallback(abstaining("jev"), instant("llm", "j"), WithClock(newFakeClock()), WithFallbackOn(OnAbstain)))
	if d.Intent.Value != "j" || !rep.FallbackFired {
		t.Fatalf("abstain opt-in: %+v %+v", d, rep)
	}

	uncertain := &fake{name: "jev", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		d := answer("i", 0.07)
		d.Calibrated = true
		d.Scores = map[string]float64{"m/i": 0.38, "m/j": 0.35, "x": 0.33}
		return d, true, nil
	}}
	opts := []Option{WithClock(newFakeClock()), WithPolicy(decision.NarrowingPolicy()), WithFallbackOn(OnUncertain)}
	d, rep = decideOK(t, Fallback(uncertain, instant("llm", "j"), opts...))
	if d.Intent.Value != "j" || !rep.FallbackFired {
		t.Fatalf("uncertain opt-in: %+v %+v", d, rep)
	}
	// Backup also fails: the primary's uncertain answer is still returned.
	d, rep = decideOK(t, Fallback(uncertain, failing("llm", errBoom), opts...))
	if d.Intent.Value != "i" || rep.Engine != "jev" || !rep.FallbackFired {
		t.Fatalf("uncertain, backup down: %+v %+v", d, rep)
	}
}

func TestFallback_BothFailJoinsErrors(t *testing.T) {
	noLeak(t)
	e := Fallback(failing("jev", errBoom), failing("llm", decision.ErrUnavailable), WithClock(newFakeClock()))
	_, ok, rep, err := e.DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, errBoom) || !errors.Is(err, decision.ErrUnavailable) {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !strings.Contains(err.Error(), "jev: error") || !strings.Contains(err.Error(), "llm: unavailable") || rep.Engine != "" {
		t.Fatalf("err=%q rep=%+v", err, rep)
	}
}

func TestFallback_InvalidWithoutErrorIsDescribed(t *testing.T) {
	noLeak(t)
	bad := &fake{name: "jev", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return answer("nope", 0.9), true, nil
	}}
	_, ok, _, err := Fallback(bad, bad, WithClock(newFakeClock())).DecideTraced(context.Background(), request())
	if ok || err == nil || !strings.Contains(err.Error(), "jev: invalid: ") {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestHedged_FastPrimaryNeverStartsBackup(t *testing.T) {
	noLeak(t)
	backup := instant("llm", "j")
	d, rep := decideOK(t, Hedged(instant("jev", "i"), backup, 600*time.Millisecond, WithClock(newFakeClock())))
	if d.Intent.Value != "i" || backup.calls.Load() != 0 || rep.HedgeFired || rep.FallbackFired || rep.Strategy != "hedged" {
		t.Fatalf("d=%+v backup=%d rep=%+v", d, backup.calls.Load(), rep)
	}
}

func TestHedged_SlowPrimaryHedgesAndLoserIsCancelled(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	primary := newGated("jev", "i")
	primary.timeout = 2 * time.Second
	backup := newGated("llm", "j")
	backup.timeout = 3 * time.Second
	e := Hedged(primary, backup, 600*time.Millisecond, WithClock(clk))

	type res struct {
		d   decision.Decision
		rep decision.Report
		err error
	}
	done := make(chan res, 1)
	go func() {
		d, _, rep, err := e.DecideTraced(context.Background(), request())
		done <- res{d, rep, err}
	}()
	waitCalls(t, primary.fake, 1)
	clk.WaitTimers(t, 2) // the primary's timeout and the hedge budget
	if backup.calls.Load() != 0 {
		t.Fatal("backup started before the latency budget")
	}
	clk.Advance(600 * time.Millisecond)
	waitCalls(t, backup.fake, 1)
	close(backup.release)
	r := <-done
	if r.err != nil || r.d.Intent.Value != "j" || !r.rep.HedgeFired || r.rep.FallbackFired || r.rep.Engine != "llm" {
		t.Fatalf("%+v", r)
	}
	if got := attemptOutcomes(r.rep); !reflect.DeepEqual(got, []string{"jev:cancelled", "llm:decided"}) {
		t.Fatalf("attempts = %v", got)
	}
	if lat := r.rep.Attempts[0].Latency; lat != 600*time.Millisecond {
		t.Fatalf("loser latency = %v", lat)
	}
	waitFor(t, func() bool { return primary.running.Load() == 0 }, "cancelled primary to exit")
}

func TestHedged_PrimaryAnswersAfterHedgeStarted(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	primary, backup := newGated("jev", "i"), newGated("llm", "j")
	e := Hedged(primary, backup, 100*time.Millisecond, WithClock(clk))
	done := make(chan decision.Report, 1)
	go func() {
		_, _, rep, _ := e.DecideTraced(context.Background(), request())
		done <- rep
	}()
	clk.WaitTimers(t, 2)
	clk.Advance(100 * time.Millisecond)
	waitCalls(t, backup.fake, 1)
	close(primary.release)
	rep := <-done
	if rep.Engine != "jev" || !rep.HedgeFired {
		t.Fatalf("%+v", rep)
	}
	if got := attemptOutcomes(rep); !reflect.DeepEqual(got, []string{"jev:decided", "llm:cancelled"}) {
		t.Fatalf("attempts = %v", got)
	}
}

func TestHedged_PrimaryFailureStartsBackupAtOnce(t *testing.T) {
	noLeak(t)
	backup := instant("llm", "j")
	// The fake clock never advances: the backup must not wait for the hedge budget.
	d, rep := decideOK(t, Hedged(failing("jev", errBoom), backup, time.Hour, WithClock(newFakeClock())))
	if d.Intent.Value != "j" || !rep.FallbackFired || rep.HedgeFired {
		t.Fatalf("d=%+v rep=%+v", d, rep)
	}
	if got := attemptOutcomes(rep); !reflect.DeepEqual(got, []string{"jev:error", "llm:decided"}) {
		t.Fatalf("attempts = %v", got)
	}
}

func TestHedged_PrimaryFailsAfterHedgeStartedBackupStillWins(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	failLate := &fake{name: "jev", decide: nil}
	release := make(chan struct{})
	failLate.decide = func(ctx context.Context, _ decision.Request) (decision.Decision, bool, error) {
		select {
		case <-release:
			return decision.Decision{}, false, errBoom
		case <-ctx.Done():
			return decision.Decision{}, false, ctx.Err()
		}
	}
	backup := newGated("llm", "j")
	e := Hedged(failLate, backup, 50*time.Millisecond, WithClock(clk))
	done := make(chan decision.Report, 1)
	go func() {
		_, _, rep, _ := e.DecideTraced(context.Background(), request())
		done <- rep
	}()
	clk.WaitTimers(t, 2)
	clk.Advance(50 * time.Millisecond)
	waitCalls(t, backup.fake, 1)
	close(release) // primary fails while the hedge is already running: no second backup start
	waitFor(t, func() bool { return failLate.running.Load() == 0 }, "primary to fail")
	close(backup.release)
	rep := <-done
	if backup.calls.Load() != 1 || rep.Engine != "llm" || !rep.HedgeFired || rep.FallbackFired {
		t.Fatalf("backupCalls=%d rep=%+v", backup.calls.Load(), rep)
	}
}

func TestHedged_AbstainDoesNotStartBackupAndStopsTimer(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	backup := instant("llm", "j")
	_, ok, rep, err := Hedged(abstaining("jev"), backup, time.Hour, WithClock(clk)).DecideTraced(context.Background(), request())
	if ok || err != nil || backup.calls.Load() != 0 || rep.HedgeFired {
		t.Fatalf("ok=%v err=%v rep=%+v", ok, err, rep)
	}
	clk.WaitTimers(t, 0)
}

func TestHedged_BothFail(t *testing.T) {
	noLeak(t)
	_, ok, rep, err := Hedged(failing("jev", errBoom), failing("llm", errBoom), time.Hour, WithClock(newFakeClock())).
		DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, errBoom) || !rep.FallbackFired || len(rep.Attempts) != 2 {
		t.Fatalf("ok=%v err=%v rep=%+v", ok, err, rep)
	}
}

func TestRace_FirstValidAnswerWinsAndLosersAreCancelled(t *testing.T) {
	noLeak(t)
	a, b, c := cooperative("a"), newGated("b", "j"), cooperative("c")
	e := Race([]decision.Provider{a, b, c}, WithClock(newFakeClock()))
	done := make(chan decision.Report, 1)
	go func() {
		_, _, rep, _ := e.DecideTraced(context.Background(), request())
		done <- rep
	}()
	waitCalls(t, a, 1)
	waitCalls(t, c, 1)
	waitCalls(t, b.fake, 1)
	close(b.release)
	rep := <-done
	if rep.Engine != "b" || rep.Strategy != "race" || rep.FallbackFired || rep.HedgeFired {
		t.Fatalf("%+v", rep)
	}
	if got := attemptOutcomes(rep); !reflect.DeepEqual(got, []string{"a:cancelled", "b:decided", "c:cancelled"}) {
		t.Fatalf("attempts = %v", got)
	}
	if rep.Attempts[0].Role != "racer" {
		t.Fatalf("role = %q", rep.Attempts[0].Role)
	}
	waitFor(t, func() bool { return a.running.Load() == 0 && c.running.Load() == 0 }, "losers to exit")
}

func TestRace_InvalidAnswerLosesToValid(t *testing.T) {
	noLeak(t)
	bad := &fake{name: "bad", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return answer("nope", 0.9), true, nil
	}}
	slowGood := newGated("good", "j")
	e := Race([]decision.Provider{bad, slowGood}, WithClock(newFakeClock()))
	done := make(chan decision.Report, 1)
	go func() {
		_, _, rep, _ := e.DecideTraced(context.Background(), request())
		done <- rep
	}()
	waitCalls(t, bad, 1)
	waitCalls(t, slowGood.fake, 1)
	close(slowGood.release)
	rep := <-done
	if rep.Engine != "good" || rep.Attempts[0].Outcome != decision.AttemptInvalid {
		t.Fatalf("%+v", rep)
	}
}

func TestRace_AllFailOrAbstain(t *testing.T) {
	noLeak(t)
	_, ok, _, err := Race([]decision.Provider{failing("a", errBoom), failing("b", errBoom)}, WithClock(newFakeClock())).
		DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, errBoom) {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	_, ok, _, err = Race([]decision.Provider{abstaining("a"), abstaining("b")}, WithClock(newFakeClock())).
		DecideTraced(context.Background(), request())
	if ok || err != nil {
		t.Fatalf("abstain: ok=%v err=%v", ok, err)
	}
}

func TestEngine_ParentCancellationStopsEverythingWithoutLeaks(t *testing.T) {
	noLeak(t)
	a, b := cooperative("a"), cooperative("b")
	e := Race([]decision.Provider{a, b}, WithClock(newFakeClock()))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, _, err := e.DecideTraced(ctx, request())
		done <- err
	}()
	waitCalls(t, a, 1)
	waitCalls(t, b, 1)
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	waitFor(t, func() bool { return a.running.Load() == 0 && b.running.Load() == 0 }, "providers to exit")
}

func TestEngine_DecisionTimeoutPerStrategy(t *testing.T) {
	a := &fake{name: "a", timeout: time.Second}
	b := &fake{name: "b", timeout: 3 * time.Second}
	c := &fake{name: "c", timeout: 2 * time.Second}
	if got := Single(a).DecisionTimeout(); got != time.Second {
		t.Errorf("single = %v", got)
	}
	if got := Fallback(a, b).DecisionTimeout(); got != 4*time.Second {
		t.Errorf("fallback = %v", got)
	}
	if got := Hedged(a, b, 600*time.Millisecond).DecisionTimeout(); got != 3600*time.Millisecond {
		t.Errorf("hedged (backup dominates) = %v", got)
	}
	if got := Hedged(b, a, 100*time.Millisecond).DecisionTimeout(); got != 3*time.Second {
		t.Errorf("hedged (primary dominates) = %v", got)
	}
	if got := Race([]decision.Provider{a, b, c}).DecisionTimeout(); got != 3*time.Second {
		t.Errorf("race = %v", got)
	}
}

func TestEngine_NestsInChainAndInsideAnotherEngine(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	inner := Hedged(failing("jev", errBoom), instant("llm", "j"), time.Hour, WithClock(clk), WithName("inner"))
	outer := Fallback(inner, instant("rules", "i"), WithClock(clk), WithName("outer"))

	d, ok, tr := decision.Chain{Providers: []decision.Provider{outer}}.Decide(context.Background(), request())
	if !ok || d.Intent.Value != "j" {
		t.Fatalf("ok=%v d=%+v", ok, d)
	}
	// The trace lists the leaf engines, not the wrappers.
	var got []string
	for _, a := range tr.Attempts {
		got = append(got, a.Provider+":"+a.Outcome)
	}
	if !reflect.DeepEqual(got, []string{"jev:error", "llm:decided"}) {
		t.Fatalf("attempts = %v", got)
	}
	if tr.DecidedBy != "outer" || tr.Engine != "llm" || tr.Strategy != "fallback" || tr.HedgeFired || tr.FallbackFired {
		t.Fatalf("trace = %+v", tr)
	}
}

func TestEngine_ChainRejectionOfWinnerIsRecorded(t *testing.T) {
	noLeak(t)
	low := &fake{name: "jev", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return answer("i", 0.2), true, nil // valid, but under the chain's 0.7 floor
	}}
	_, ok, tr := decision.Chain{Providers: []decision.Provider{Single(low, WithClock(newFakeClock()))}}.Decide(context.Background(), request())
	if ok || tr.Attempts[0].Outcome != decision.AttemptLowConfidence {
		t.Fatalf("ok=%v trace=%+v", ok, tr)
	}
}

func TestEngine_BreakerOpenIsRecordedAsUnavailableInChain(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	jev := NewBreaker(failing("jev", errBoom), WithBreakerClock(clk), WithBreakerThreshold(1))
	e := Fallback(jev, instant("llm", "j"), WithClock(clk))
	chain := decision.Chain{Providers: []decision.Provider{e}}
	chain.Decide(context.Background(), request()) // opens the breaker
	_, ok, tr := chain.Decide(context.Background(), request())
	if !ok || tr.Attempts[0].Outcome != decision.AttemptUnavailable || tr.Engine != "llm" || !tr.FallbackFired {
		t.Fatalf("ok=%v trace=%+v", ok, tr)
	}
}

func TestFallback_CancelledCallerDoesNotStartBackup(t *testing.T) {
	noLeak(t)
	backup := instant("llm", "j")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok, rep, err := Fallback(cooperative("jev"), backup, WithClock(newFakeClock())).DecideTraced(ctx, request())
	if ok || !errors.Is(err, context.Canceled) || backup.calls.Load() != 0 || rep.FallbackFired {
		t.Fatalf("ok=%v err=%v backup=%d rep=%+v", ok, err, backup.calls.Load(), rep)
	}
}
