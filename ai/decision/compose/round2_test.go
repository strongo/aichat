package compose

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

// ---- slow is not failure ----

// A healthy primary that answers just after the hedge delay is answered for by
// its hedge on every call. It must not be taken out of service by that: the
// default breaker never opens on slowness.
func TestHedged_SlowButHealthyPrimaryNeverOpensTheDefaultBreaker(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	slow := cooperative("jev") // answers later than the hedge: here, never before the backup
	b := NewBreaker(slow, WithBreakerClock(clk))
	e := Hedged(b, instant("llm", "j"), 30*time.Millisecond, WithClock(clk))
	for i := 1; i <= 12; i++ {
		ch := startDecide(e)
		clk.WaitTimers(t, 2)
		clk.Advance(30 * time.Millisecond)
		if o := <-ch; o.err != nil || o.d.Intent.Value != "j" {
			t.Fatalf("call %d: %+v", i, o)
		}
		waitFor(t, func() bool { return b.Stats().Slow == i }, fmt.Sprintf("slow tally %d", i))
	}
	if b.State() != BreakerClosed || b.Stats().Failures != 0 {
		t.Fatalf("state=%v stats=%+v", b.State(), b.Stats())
	}
}

// A primary that outruns its OWN timeout is a plain failure, hedge or no hedge.
func TestHedged_PrimaryOutrunningItsOwnTimeoutIsAFailure(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	hang := cooperative("jev")
	hang.timeout = 500 * time.Millisecond
	b := NewBreaker(hang, WithBreakerClock(clk), WithBreakerThreshold(2))
	backup := cooperative("llm") // also hangs, so only timeouts end the call
	backup.timeout = 5 * time.Second
	e := Hedged(b, backup, 100*time.Millisecond, WithClock(clk))
	for i := 1; i <= 2; i++ {
		ch := startDecide(e)
		clk.WaitTimers(t, 2) // the primary's own timeout and the hedge budget
		clk.Advance(100 * time.Millisecond)
		waitCalls(t, backup, int32(i))
		clk.WaitTimers(t, 2)                // the primary's timeout and the backup's
		clk.Advance(400 * time.Millisecond) // the primary's own 500ms passes
		clk.Advance(5 * time.Second)        // and so does the backup's
		if o := <-ch; o.ok || o.err == nil {
			t.Fatalf("call %d: %+v", i, o)
		}
	}
	waitFor(t, func() bool { return b.State() == BreakerOpen }, "breaker open after two timeouts")
	if st := b.Stats(); st.Slow != 0 {
		t.Fatalf("a timeout is not slowness: %+v", st)
	}
}

// modal is an engine that either hangs until cancelled or answers at once,
// switched without touching the fake's fields (an abandoned call may still be
// running).
func modal(r *breakerRig) *atomic.Bool {
	var hang atomic.Bool
	r.p.decide = func(ctx context.Context, _ decision.Request) (decision.Decision, bool, error) {
		if hang.Load() {
			<-ctx.Done()
			return decision.Decision{}, false, ctx.Err()
		}
		return answer("i", 0.9), true, nil
	}
	return &hang
}

// superseded runs one call against a hanging engine and cancels it the way a
// successful hedge does.
func superseded(t *testing.T, b *Breaker) {
	t.Helper()
	f := b.inner.(*fake)
	before := f.calls.Load()
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := b.Decide(ctx, request()); done <- err }()
	waitFor(t, func() bool { return f.calls.Load() > before }, "call to start")
	cancel(ErrSuperseded)
	<-done
}

func TestBreaker_SlowRunIsResetByAnInTimeCallAndOpensAtTheThreshold(t *testing.T) {
	noLeak(t)
	r := newRig(t, WithBreakerSlowThreshold(3), WithBreakerThreshold(100))
	hang := modal(r)
	hang.Store(true)
	superseded(t, r.b)
	superseded(t, r.b)
	if st := r.b.Stats(); st.Slow != 2 || st.Failures != 0 || r.b.State() != BreakerClosed {
		t.Fatalf("stats=%+v state=%v", st, r.b.State())
	}
	// A call that completes in time resets the run.
	hang.Store(false)
	if err := r.call(t); err != nil {
		t.Fatal(err)
	}
	if st := r.b.Stats(); st.Slow != 0 {
		t.Fatalf("stats=%+v", st)
	}
	hang.Store(true)
	for i := 0; i < 3; i++ {
		superseded(t, r.b)
	}
	if r.b.State() != BreakerOpen || r.b.Stats().Slow != 0 {
		t.Fatalf("state=%v stats=%+v", r.b.State(), r.b.Stats())
	}
}

// A slow probe settles nothing: the breaker neither closes nor reopens, and the
// next call may probe again.
func TestBreaker_SlowProbeIsNeutral(t *testing.T) {
	noLeak(t)
	r := newRig(t, WithBreakerThreshold(1), WithBreakerCooldown(time.Second))
	r.fail(errBoom)
	_ = r.call(t) // opens
	r.clk.Advance(time.Second)
	hang := modal(r)
	hang.Store(true)
	superseded(t, r.b)
	if r.b.State() != BreakerHalfOpen {
		t.Fatalf("state = %v", r.b.State())
	}
	hang.Store(false)
	if err := r.call(t); err != nil || r.b.State() != BreakerClosed {
		t.Fatalf("err=%v state=%v", err, r.b.State())
	}
}

// ---- quota and misconfiguration ----

func TestBreaker_QuotaAndMisconfigurationNeverOpenIt(t *testing.T) {
	for name, err := range map[string]error{
		"quota":         fmt.Errorf("engine: %w", decision.ErrQuota),
		"misconfigured": fmt.Errorf("engine: %w", decision.ErrMisconfigured),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, WithBreakerThreshold(1))
			r.fail(err)
			for i := 0; i < 5; i++ {
				if got := r.call(t); !errors.Is(got, err) {
					t.Fatalf("call %d: %v", i, got)
				}
			}
			if r.b.State() != BreakerClosed {
				t.Fatalf("opened: %v", r.b.State())
			}
		})
	}
}

func TestFallback_QuotaIsSurfacedNotFailedOverUnlessAskedAndMisconfigurationNever(t *testing.T) {
	noLeak(t)
	quota := fmt.Errorf("jev: %w", decision.ErrQuota)
	for name, tc := range map[string]struct {
		err     error
		opts    []Option
		outcome string
		backup  bool
	}{
		"quota default":                 {quota, nil, decision.AttemptQuota, false},
		"quota with OnQuota":            {quota, []Option{WithFallbackOn(OnQuota)}, decision.AttemptQuota, true},
		"quota with abstain only":       {quota, []Option{WithFallbackOn(OnAbstain | OnUncertain)}, decision.AttemptQuota, false},
		"misconfigured default":         {fmt.Errorf("jev: %w", decision.ErrMisconfigured), nil, decision.AttemptMisconfigured, false},
		"misconfigured even w/ OnQuota": {fmt.Errorf("jev: %w", decision.ErrMisconfigured), []Option{WithFallbackOn(OnQuota | OnAbstain)}, decision.AttemptMisconfigured, false},
	} {
		t.Run(name, func(t *testing.T) {
			backup := instant("llm", "j")
			e := Fallback(failing("jev", tc.err), backup, append([]Option{WithClock(newFakeClock())}, tc.opts...)...)
			d, ok, rep, err := e.DecideTraced(context.Background(), request())
			if got := rep.Attempts[0].Outcome; got != tc.outcome {
				t.Fatalf("outcome = %q", got)
			}
			if tc.backup {
				if err != nil || !ok || d.Intent.Value != "j" || !rep.FallbackFired {
					t.Fatalf("ok=%v err=%v rep=%+v", ok, err, rep)
				}
				return
			}
			if ok || backup.calls.Load() != 0 || rep.FallbackFired || !errors.Is(err, tc.err) {
				t.Fatalf("the error must be surfaced, the backup untouched: ok=%v err=%v calls=%d", ok, err, backup.calls.Load())
			}
		})
	}
	// The same for scored questions.
	e := Fallback(scorer("jev", decision.ScoreResult{}, quota), scorer("llm", goodScore("llm"), nil), WithClock(newFakeClock()))
	if _, rep, err := e.ScoreTraced(context.Background(), scoreRequest()); !errors.Is(err, decision.ErrQuota) || rep.FallbackFired || rep.Attempts[0].Outcome != decision.AttemptQuota {
		t.Fatalf("score: err=%v rep=%+v", err, rep)
	}
}

// A Hedged primary refused for quota does not wake the backup either.
func TestHedged_QuotaFromThePrimaryStartsNoBackup(t *testing.T) {
	noLeak(t)
	backup := instant("llm", "j")
	e := Hedged(failing("jev", fmt.Errorf("jev: %w", decision.ErrQuota)), backup, time.Hour, WithClock(newFakeClock()))
	_, ok, rep, err := e.DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, decision.ErrQuota) || backup.calls.Load() != 0 || rep.HedgeFired || rep.FallbackFired {
		t.Fatalf("ok=%v err=%v rep=%+v", ok, err, rep)
	}
}

// ---- an engine's answer always carries its verdict ----

func calibratedDecision(top, second float64) decision.Decision {
	d := answer("i", 0.9)
	d.Calibrated = true
	d.Scores = map[string]float64{"m/i": top, "m/j": second}
	return d
}

func TestEngine_WithAPolicyEveryDecisionCarriesAnOutcome(t *testing.T) {
	noLeak(t)
	nar, dur := decision.NarrowingPolicy(), decision.DurablePolicy()
	llm := func(conf float64) decision.Provider {
		return &fake{name: "llm", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
			return answer("i", conf), true, nil
		}}
	}
	jev := func(d decision.Decision) decision.Provider {
		return &fake{name: "jev", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) { return d, true, nil }}
	}
	for name, tc := range map[string]struct {
		p          decision.Provider
		pol        decision.SelectionPolicy
		outcome    decision.Outcome
		actionable bool
	}{
		"calibrated clear":                {jev(calibratedDecision(0.95, 0.05)), nar, decision.OutcomeSelected, true},
		"calibrated uncertain (probed)":   {jev(uncertainDecision("i")), nar, decision.OutcomeUncertain, false},
		"narrowing accepts llm at .72":    {llm(0.72), nar, decision.OutcomeAccepted, true},
		"narrowing rejects llm at .6":     {llm(0.6), nar, decision.OutcomeUnscored, false},
		"durable never acts on llm (.72)": {llm(0.72), dur, decision.OutcomeUnscored, false},
	} {
		t.Run(name, func(t *testing.T) {
			e := Single(tc.p, WithClock(newFakeClock()), WithPolicy(tc.pol))
			d, ok, rep, err := e.DecideTraced(context.Background(), request())
			if err != nil || !ok {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			if d.Outcome != tc.outcome || d.Actionable() != tc.actionable {
				t.Fatalf("outcome=%q actionable=%v, want %q/%v", d.Outcome, d.Actionable(), tc.outcome, tc.actionable)
			}
			want := decision.AttemptDecided
			if !tc.actionable {
				want = decision.AttemptUncertain
			}
			if rep.Attempts[0].Outcome != want {
				t.Fatalf("attempt = %+v", rep.Attempts[0])
			}
		})
	}
	// Without a policy there is no bar to judge by: the answer passes through
	// unjudged and the Chain that owns the bar decides.
	d, ok, _, _ := Single(llm(0.72), WithClock(newFakeClock())).DecideTraced(context.Background(), request())
	if !ok || d.Outcome != "" {
		t.Fatalf("policy-less engine: %+v ok=%v", d, ok)
	}
}

// The probed bypass: a durable chain around an engine whose calibrated primary is
// down must not act on the LLM backup's self-reported 0.72.
func TestEngine_DurableChainDoesNotActOnTheBackupsSelfReportedConfidence(t *testing.T) {
	noLeak(t)
	dur := decision.DurablePolicy()
	e := Fallback(failing("jev", errBoom), &fake{name: "llm", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return answer("i", 0.72), true, nil
	}}, WithClock(newFakeClock()), WithPolicy(dur))
	d, ok, tr := decision.Chain{Providers: []decision.Provider{e}, Policy: &dur}.Decide(context.Background(), request())
	if ok || d.Outcome != "" {
		t.Fatalf("ok=%v d=%+v", ok, d)
	}
	if got := tr.Attempts; len(got) != 2 || got[1].Provider != "llm" || got[1].Outcome != decision.AttemptUncertain || got[1].Detail != "unscored: not_calibrated" {
		t.Fatalf("attempts = %+v", got)
	}
	// Asked to keep it, the chain returns it as non-actionable.
	d, ok, _ = decision.Chain{Providers: []decision.Provider{e}, Policy: &dur, KeepNonSelected: true}.Decide(context.Background(), request())
	if !ok || d.Actionable() || d.Outcome != decision.OutcomeUnscored {
		t.Fatalf("keep: ok=%v d=%+v", ok, d)
	}
}

// An engine with a policy inside a chain that forgot its policy: the engine's
// own uncertain verdict still stops the chain from acting.
func TestEngine_UncertainAnswerIsNotActedOnByAChainWithoutPolicy(t *testing.T) {
	noLeak(t)
	e := Single(&fake{name: "jev", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return uncertainDecision("i"), true, nil
	}}, WithClock(newFakeClock()), WithPolicy(decision.NarrowingPolicy()))
	d, ok, tr := decision.Chain{Providers: []decision.Provider{e}, MinConfidence: -1}.Decide(context.Background(), request())
	if ok || d.Outcome != "" || tr.Attempts[0].Outcome != decision.AttemptUncertain {
		t.Fatalf("ok=%v d=%+v tr=%+v", ok, d, tr)
	}
}

// ---- the request error names no ids and matches the sentinel ----

func TestEngine_InvalidScoreRequestMatchesTheSentinelAndQuotesNoIds(t *testing.T) {
	noLeak(t)
	e := Fallback(NewBreaker(scorer("jev", goodScore("jev"), nil)), NewBreaker(scorer("llm", goodScore("llm"), nil)), WithClock(newFakeClock()))
	_, err := e.Score(context.Background(), decision.ScoreRequest{Text: "t", Questions: []decision.Question{{
		ID: "q_private", Kind: decision.KindRelevance, Instructions: "x",
		Candidates: []decision.Candidate{{ID: "secret_table"}, {ID: "secret_table"}},
	}}})
	if !errors.Is(err, decision.ErrInvalidRequest) {
		t.Fatalf("err = %v", err)
	}
	for _, leak := range []string{"secret_table", "q_private"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("the error quotes %q: %v", leak, err)
		}
	}
}

// ---- per-attempt usage ----

func usageScore(engine string, in, out int) decision.ScoreResult {
	res := goodScore(engine)
	res.Usage = decision.Usage{InputTokens: in, OutputTokens: out}
	return res
}

func TestScore_EveryAnsweredAttemptCarriesItsOwnUsage(t *testing.T) {
	noLeak(t)
	// Fallback: the failed primary has no usage (unknown, nil); the backup has its own.
	e := Fallback(scorer("jev", decision.ScoreResult{}, errBoom), scorer("llm", usageScore("llm", 40, 7), nil), WithClock(newFakeClock()))
	res, rep, err := e.ScoreTraced(context.Background(), scoreRequest())
	if err != nil || rep.Attempts[0].Usage != nil || !reflect.DeepEqual(rep.Attempts[1].Usage, &decision.Usage{InputTokens: 40, OutputTokens: 7}) {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	if res.Usage != (decision.Usage{InputTokens: 40, OutputTokens: 7}) {
		t.Fatalf("usage = %+v", res.Usage)
	}
	// Uncertain primary then OnUncertain backup: both answered, both billed, each metered.
	e = Fallback(scorer("jev", func() decision.ScoreResult {
		r := uncertainScore("jev")
		r.Usage = decision.Usage{InputTokens: 11}
		return r
	}(), nil),
		scorer("llm", usageScore("llm", 40, 7), nil), WithClock(newFakeClock()), WithPolicy(decision.NarrowingPolicy()), WithFallbackOn(OnUncertain))
	_, rep, err = e.ScoreTraced(context.Background(), scoreRequest())
	if err != nil || rep.Attempts[0].Outcome != decision.AttemptUncertain || rep.Attempts[0].Usage == nil || rep.Attempts[0].Usage.InputTokens != 11 ||
		rep.Attempts[1].Usage == nil || rep.Attempts[1].Usage.InputTokens != 40 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	// An answer that reports none carries nil, not zero.
	_, rep, err = Single(scorer("jev", goodScore("jev"), nil), WithClock(newFakeClock())).ScoreTraced(context.Background(), scoreRequest())
	if err != nil || rep.Attempts[0].Usage != nil {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	// A traced inner engine's own per-attempt usage survives the outer judge.
	inner := Fallback(scorer("a", decision.ScoreResult{}, errBoom), scorer("b", usageScore("b", 5, 1), nil), WithClock(newFakeClock()), WithName("inner"))
	_, rep, err = Single(inner, WithClock(newFakeClock())).ScoreTraced(context.Background(), scoreRequest())
	if err != nil || len(rep.Attempts) != 2 || rep.Attempts[1].Usage == nil || rep.Attempts[1].Usage.InputTokens != 5 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
}
