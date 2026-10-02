package compose

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

var errQuota = fmt.Errorf("jev: allowance used up: %w", decision.ErrQuota)

// quotaAfter answers a quota refusal once released (or fails with its context).
func quotaAfter(name string, err error) (*fake, chan struct{}) {
	release := make(chan struct{})
	return &fake{name: name, decide: func(ctx context.Context, _ decision.Request) (decision.Decision, bool, error) {
		select {
		case <-release:
			return decision.Decision{}, false, err
		case <-ctx.Done():
			return decision.Decision{}, false, ctx.Err()
		}
	}}, release
}

// ---- m5: quota and misconfiguration end a Hedged or Race call too ----

// The probed leak: the hedge fired (the primary was slow), then the primary
// refused with an exhausted allowance. The paid backup must not answer in its
// place.
func TestHedged_QuotaFromThePrimaryAfterTheHedgeFiredEndsTheCall(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	after := 100 * time.Millisecond
	primary, refuse := quotaAfter("jev", errQuota)
	backup := newGated("llm", "i")
	ch := startDecide(Hedged(primary, backup.fake, after, WithClock(clk)))
	fireHedge(t, clk, after, backup.fake)
	close(refuse)
	o := <-ch
	if o.ok || !errors.Is(o.err, decision.ErrQuota) || !o.rep.HedgeFired {
		t.Fatalf("%+v", o)
	}
	if got := attemptOutcomes(o.rep); !reflect.DeepEqual(got, []string{"jev:quota", "llm:cancelled"}) {
		t.Fatalf("attempts = %v", got)
	}
	// Even if the backup would have answered, it was cancelled and never accepted.
	close(backup.release)
	waitFor(t, func() bool { return backup.running.Load() == 0 }, "cancelled backup to exit")
}

func TestHedged_QuotaWithOnQuotaLetsTheBackupAnswer(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	after := 100 * time.Millisecond
	primary, refuse := quotaAfter("jev", errQuota)
	backup := newGated("llm", "i")
	ch := startDecide(Hedged(primary, backup.fake, after, WithClock(clk), WithFallbackOn(OnQuota)))
	fireHedge(t, clk, after, backup.fake)
	close(refuse)
	close(backup.release)
	o := <-ch
	if !o.ok || o.err != nil || o.rep.Engine != "llm" {
		t.Fatalf("%+v", o)
	}
}

// Before the hedge fires nothing changed: a quota refusal is the answer.
func TestHedged_QuotaBeforeTheHedgeFiresIsSurfaced(t *testing.T) {
	noLeak(t)
	backup := instant("llm", "i")
	d, ok, _, err := Hedged(failing("jev", errQuota), backup, time.Hour, WithClock(newFakeClock())).DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, decision.ErrQuota) || backup.calls.Load() != 0 || d.Actionable() {
		t.Fatalf("ok=%v err=%v backupCalls=%d", ok, err, backup.calls.Load())
	}
}

// A backup that is refused is just a failed backup: the primary may still answer.
func TestHedged_QuotaFromTheBackupDoesNotEndTheCall(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	after := 100 * time.Millisecond
	primary := newGated("jev", "i")
	backup, refuse := quotaAfter("llm", errQuota)
	ch := startDecide(Hedged(primary.fake, backup, after, WithClock(clk)))
	fireHedge(t, clk, after, backup)
	close(refuse)
	waitFor(t, func() bool { return backup.running.Load() == 0 }, "refused backup to return")
	close(primary.release)
	o := <-ch
	if !o.ok || o.rep.Engine != "jev" {
		t.Fatalf("%+v", o)
	}
}

func TestRace_QuotaFromAnyRacerEndsTheCallAndCancelsTheOthers(t *testing.T) {
	noLeak(t)
	slow := newGated("slow", "i")
	d, ok, rep, err := Race([]decision.Provider{failing("jev", errQuota), slow.fake}).DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, decision.ErrQuota) || d.Actionable() {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got := attemptOutcomes(rep); !reflect.DeepEqual(got, []string{"jev:quota", "slow:cancelled"}) {
		t.Fatalf("attempts = %v", got)
	}
	close(slow.release)
}

func TestRace_MisconfiguredEndsTheCallEvenWithOnQuota(t *testing.T) {
	noLeak(t)
	mis := fmt.Errorf("jev: %w", decision.ErrMisconfigured)
	slow := newGated("slow", "i")
	_, ok, rep, err := Race([]decision.Provider{failing("jev", mis), slow.fake}, WithFallbackOn(OnQuota)).DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, decision.ErrMisconfigured) || rep.Attempts[0].Outcome != decision.AttemptMisconfigured {
		t.Fatalf("ok=%v err=%v rep=%+v", ok, err, rep)
	}
	close(slow.release)
}

func TestRace_QuotaWithOnQuotaLetsTheOthersAnswer(t *testing.T) {
	noLeak(t)
	_, ok, rep, err := Race([]decision.Provider{failing("jev", errQuota), instant("llm", "i")}, WithFallbackOn(OnQuota)).DecideTraced(context.Background(), request())
	if !ok || err != nil || rep.Engine != "llm" {
		t.Fatalf("ok=%v err=%v rep=%+v", ok, err, rep)
	}
}

// A halted call has no answer, even when another racer had already abstained or
// answered uncertain: the refusal is the loud outcome.
func TestRace_HaltedCallIsAnErrorNotAnAbstentionOrUncertainAnswer(t *testing.T) {
	noLeak(t)
	_, ok, _, err := Race([]decision.Provider{abstaining("a"), failing("jev", errQuota)}).DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, decision.ErrQuota) {
		t.Fatalf("abstain + quota: ok=%v err=%v", ok, err)
	}
	// Uncertain first (it is processed before the gated refusal), then quota.
	unc := &fake{name: "unc", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return uncertainDecision("i"), true, nil
	}}
	late, refuse := quotaAfter("jev", errQuota)
	ch := startDecide(Race([]decision.Provider{unc, late}, WithPolicy(decision.NarrowingPolicy())))
	waitCalls(t, unc, 1)
	waitFor(t, func() bool { return unc.running.Load() == 0 }, "the uncertain racer to finish")
	time.Sleep(20 * time.Millisecond) // let the engine take the uncertain answer into account
	close(refuse)
	o := <-ch
	if o.ok || !errors.Is(o.err, decision.ErrQuota) || o.d.Outcome != "" {
		t.Fatalf("%+v", o)
	}
}

func TestRace_QuotaOnAScoredCallEndsItToo(t *testing.T) {
	noLeak(t)
	jev := &fake{name: "jev", score: func(context.Context, decision.ScoreRequest) (decision.ScoreResult, error) {
		return decision.ScoreResult{}, errQuota
	}}
	slow := &fake{name: "slow", score: func(ctx context.Context, _ decision.ScoreRequest) (decision.ScoreResult, error) {
		<-ctx.Done()
		return decision.ScoreResult{}, ctx.Err()
	}}
	_, _, err := Race([]decision.Provider{jev, slow}).ScoreTraced(context.Background(), scoreRequest())
	if !errors.Is(err, decision.ErrQuota) {
		t.Fatalf("err=%v", err)
	}
}

// ---- M2: engines return unjudged decisions ----

func TestEngineWithoutAPolicyReturnsAnUnjudgedNonActionableDecision(t *testing.T) {
	for name, e := range map[string]*Engine{
		"single":   Single(instant("llm", "i")),
		"fallback": Fallback(failing("jev", errors.New("down")), instant("llm", "i")),
		"breaker":  Single(NewBreaker(instant("llm", "i"))),
	} {
		d, ok, err := e.Decide(context.Background(), request())
		if err != nil || !ok || d.Outcome != "" || d.Actionable() {
			t.Errorf("%s: d=%+v ok=%v err=%v", name, d, ok, err)
		}
	}
}

func TestEngineWithAPolicyJudgesDeterministicAndContradictoryAnswers(t *testing.T) {
	pol := decision.DurablePolicy()
	rule := &fake{name: "rules", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return decision.Deterministic(answer("i", 1)), true, nil
	}}
	d, ok, err := Fallback(rule, instant("llm", "i"), WithPolicy(pol)).Decide(context.Background(), request())
	if err != nil || !ok || d.Outcome != decision.OutcomeDeterministic || !d.Actionable() {
		t.Fatalf("d=%+v ok=%v err=%v", d, ok, err)
	}
	// A calibrated decision that contradicts its own scores is an invalid answer.
	bad := answer("i", 0.95)
	bad.Calibrated, bad.Scores = true, map[string]float64{"m/j": 0.9, "m/i": 0.1}
	jev := &fake{name: "jev", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) { return bad, true, nil }}
	d, ok, rep, err := Single(jev, WithPolicy(pol)).DecideTraced(context.Background(), request())
	if ok || err == nil || d.Actionable() || rep.Attempts[0].Outcome != decision.AttemptInvalid || rep.Attempts[0].Detail != "invalid: decision_not_top" {
		t.Fatalf("d=%+v ok=%v err=%v rep=%+v", d, ok, err, rep)
	}
}

// ---- Breaker is transparent for traced providers ----

type tracedStub struct {
	name string
	rep  decision.Report
	err  error
}

func (s tracedStub) Name() string { return s.name }
func (s tracedStub) Decide(ctx context.Context, r decision.Request) (decision.Decision, bool, error) {
	d, ok, _, err := s.DecideTraced(ctx, r)
	return d, ok, err
}
func (s tracedStub) DecideTraced(context.Context, decision.Request) (decision.Decision, bool, decision.Report, error) {
	return decision.Decision{}, false, s.rep, s.err
}

func TestBreaker_PassesTheWrappedEnginesReportOn(t *testing.T) {
	rep := decision.Report{Engine: "jev", Attempts: []decision.Attempt{{Provider: "jev", Outcome: decision.AttemptAbstained, Detail: "interaction_gap"}}}
	_, ok, got, err := NewBreaker(tracedStub{name: "jev", rep: rep}).DecideTraced(context.Background(), request())
	if ok || err != nil || !reflect.DeepEqual(got, rep) {
		t.Fatalf("ok=%v err=%v rep=%+v", ok, err, got)
	}
	// A plain engine reports only who it is.
	_, _, got, _ = NewBreaker(instant("llm", "i")).DecideTraced(context.Background(), request())
	if got.Engine != "llm" || len(got.Attempts) != 0 {
		t.Fatalf("%+v", got)
	}
	// No engine.
	_, _, _, err = NewBreaker(nil).DecideTraced(context.Background(), request())
	if !errors.Is(err, ErrNoEngine) {
		t.Fatalf("%v", err)
	}
	// The abstention detail reaches a Chain through breaker and engine alike.
	c := decision.Chain{Providers: []decision.Provider{Single(NewBreaker(tracedStub{name: "jev", rep: rep}))}}
	_, ok, tr := c.Decide(context.Background(), request())
	if ok || len(tr.Attempts) != 1 || tr.Attempts[0].Detail != "interaction_gap" {
		t.Fatalf("ok=%v tr=%+v", ok, tr)
	}
}
