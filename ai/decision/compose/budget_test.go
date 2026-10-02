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

var errTransient = errors.New("jev: HTTP 429 (rate limited)")

func decideOnce(p decision.Provider) (decision.Decision, bool, error) {
	return p.Decide(context.Background(), request())
}

func TestBudget_CapsCallsPerWindowAndNeverCallsTheEngineOnceSpent(t *testing.T) {
	clk := newFakeClock()
	inner := instant("llm", "i")
	b := NewBudget(inner, BudgetOptions{MaxCalls: 3, Per: time.Hour, Clock: clk})
	for i := 0; i < 3; i++ {
		if _, ok, err := decideOnce(b); !ok || err != nil {
			t.Fatalf("call %d: ok=%v err=%v", i, ok, err)
		}
	}
	var last error
	for i := 0; i < 5; i++ {
		d, ok, err := decideOnce(b)
		if ok || d.Actionable() || !errors.Is(err, decision.ErrBudget) || errors.Is(err, decision.ErrQuota) {
			t.Fatalf("over the cap: ok=%v err=%v", ok, err)
		}
		last = err
	}
	if inner.calls.Load() != 3 {
		t.Fatalf("the engine was called %d times, the cap is 3", inner.calls.Load())
	}
	if got := b.Stats(); got != (BudgetStats{Used: 3, Max: 3, Refused: 5}) {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(last.Error(), "3 calls used per 1h0m0s") {
		t.Fatalf("the error must say what the cap is: %v", last)
	}
	// A fixed window: one that has not finished does not reset, one that has does.
	clk.Advance(59 * time.Minute)
	if _, ok, _ := decideOnce(b); ok {
		t.Fatal("window not over")
	}
	clk.Advance(time.Minute)
	if _, ok, err := decideOnce(b); !ok || err != nil {
		t.Fatalf("a new window: ok=%v err=%v", ok, err)
	}
	if got := b.Stats(); got.Used != 1 || got.Refused != 6 {
		t.Fatalf("%+v", got)
	}
}

func TestBudget_ZeroPerNeverResets(t *testing.T) {
	clk := newFakeClock()
	b := NewBudget(instant("llm", "i"), BudgetOptions{MaxCalls: 1, Clock: clk})
	if _, ok, _ := decideOnce(b); !ok {
		t.Fatal("first call")
	}
	clk.Advance(1000 * time.Hour)
	_, _, err := decideOnce(b)
	if !errors.Is(err, decision.ErrBudget) || !strings.Contains(err.Error(), "never resets") {
		t.Fatalf("%v", err)
	}
}

func TestBudget_InvalidOptionsAreLoudNotSilent(t *testing.T) {
	for name, o := range map[string]BudgetOptions{
		"zero cap":     {MaxCalls: 0},
		"negative cap": {MaxCalls: -1, Per: time.Hour},
		"negative per": {MaxCalls: 1, Per: -time.Second},
	} {
		inner := instant("llm", "i")
		_, ok, err := decideOnce(NewBudget(inner, o))
		if ok || !errors.Is(err, decision.ErrMisconfigured) || inner.calls.Load() != 0 {
			t.Errorf("%s: ok=%v err=%v calls=%d", name, ok, err, inner.calls.Load())
		}
	}
}

func TestBudget_TransparentAndTolerant(t *testing.T) {
	// No engine: an error, never a panic.
	if _, ok, err := decideOnce(NewBudget(nil, BudgetOptions{MaxCalls: 1})); ok || !errors.Is(err, ErrNoEngine) {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if _, err := NewBudget(nil, BudgetOptions{MaxCalls: 1}).Score(context.Background(), decision.ScoreRequest{}); !errors.Is(err, ErrNoEngine) {
		t.Fatalf("%v", err)
	}
	// Name and timeout pass through; a plain provider reports no timeout.
	f := instant("jev", "i")
	f.timeout = 7 * time.Second
	b := NewBudget(f, BudgetOptions{MaxCalls: 1})
	if b.Name() != "jev" || b.DecisionTimeout() != 7*time.Second {
		t.Fatalf("%s %v", b.Name(), b.DecisionTimeout())
	}
	if got := NewBudget(noTimeout{instant("plain", "i")}, BudgetOptions{MaxCalls: 1}).DecisionTimeout(); got != 0 {
		t.Fatalf("%v", got)
	}
	// An engine that reports how it answered is passed through; one that does not
	// gets a report naming it.
	traced := NewBudget(Single(instant("a", "i")), BudgetOptions{MaxCalls: 2})
	if _, ok, rep, err := traced.DecideTraced(context.Background(), request()); !ok || err != nil || rep.Strategy != "single" || rep.Engine != "a" {
		t.Fatalf("ok=%v rep=%+v err=%v", ok, rep, err)
	}
	plain := NewBudget(noTimeout{instant("plain", "i")}, BudgetOptions{MaxCalls: 2})
	if _, ok, rep, err := plain.DecideTraced(context.Background(), request()); !ok || err != nil || rep.Engine != "plain" {
		t.Fatalf("ok=%v rep=%+v err=%v", ok, rep, err)
	}
	// An abstention and a failure cost a call too: the engine was billed.
	ab := abstaining("ab")
	bb := NewBudget(ab, BudgetOptions{MaxCalls: 1})
	if _, ok, err := decideOnce(bb); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if _, _, err := decideOnce(bb); !errors.Is(err, decision.ErrBudget) || ab.calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, ab.calls.Load())
	}
}

func TestBudget_ScoredQuestionsShareTheCapAndUnsupportedIsNotCounted(t *testing.T) {
	sreq := decision.ScoreRequest{Text: "t", Questions: []decision.Question{{ID: "q", Kind: decision.KindRelevance, Instructions: "x", Candidates: []decision.Candidate{{ID: "a"}}}}}
	f := &fake{name: "llm", score: func(context.Context, decision.ScoreRequest) (decision.ScoreResult, error) {
		return decision.ScoreResult{}, nil
	}, decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return answer("i", 0.9), true, nil
	}}
	b := NewBudget(f, BudgetOptions{MaxCalls: 2})
	if _, err := b.Score(context.Background(), sreq); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := decideOnce(b); !ok {
		t.Fatal("decide")
	}
	if _, err := b.Score(context.Background(), sreq); !errors.Is(err, decision.ErrBudget) {
		t.Fatalf("scores and decisions are one budget: %v", err)
	}
	// A provider that cannot score: unsupported, and nothing is spent.
	np := NewBudget(noTimeout{instant("plain", "i")}, BudgetOptions{MaxCalls: 1})
	if _, err := np.Score(context.Background(), sreq); !errors.Is(err, decision.ErrUnsupported) {
		t.Fatalf("%v", err)
	}
	if np.Stats().Used != 0 {
		t.Fatalf("%+v", np.Stats())
	}
}

// The probed hole, closed: the primary answers a transient error on every call (a
// 429 that is really a spent account), so with a plain Fallback every turn is a
// paid call. With a Budget on the backup the bill is bounded and the chain stops
// loudly, and a Breaker around the Budget neither opens on the refusal nor counts
// a call it did not let through.
func TestBudget_BoundsAPaidBackupWhenThePrimaryFailsTransientlyForever(t *testing.T) {
	const turns, cap = 50, 7
	run := func(wrap func(decision.Provider) decision.Provider) (paidCalls int, stopped, decided int, tr decision.Trace) {
		primary := failing("jev", errTransient)
		paid := instant("llm", "i")
		e := Fallback(NewBreaker(primary), NewBreaker(wrap(paid)))
		chain := decision.Chain{Providers: []decision.Provider{e}}
		for i := 0; i < turns; i++ {
			_, ok, t := chain.Decide(context.Background(), request())
			tr = t
			switch {
			case ok:
				decided++
			case t.StoppedBy != "":
				stopped++
			}
		}
		return int(paid.calls.Load()), stopped, decided, tr
	}
	// Without a budget: every turn is paid. This is the hole.
	if paid, stopped, decided, _ := run(func(p decision.Provider) decision.Provider { return p }); paid != turns || stopped != 0 || decided != turns {
		t.Fatalf("baseline: paid=%d stopped=%d decided=%d", paid, stopped, decided)
	}
	paid, stopped, decided, tr := run(func(p decision.Provider) decision.Provider {
		return NewBudget(p, BudgetOptions{MaxCalls: cap, Per: time.Hour})
	})
	if paid != cap || decided != cap || stopped != turns-cap {
		t.Fatalf("with a budget: paid=%d decided=%d stopped=%d", paid, decided, stopped)
	}
	if tr.StoppedBy != decision.AttemptBudget || !errors.Is(tr.Err(), decision.ErrBudget) {
		t.Fatalf("the last turn must stop loudly: %+v err=%v", tr, tr.Err())
	}
}

func TestBudget_BreakerIgnoresARefusalAndDoesNotCountCallsItBlocked(t *testing.T) {
	clk := newFakeClock()
	inner := instant("llm", "i")
	budget := NewBudget(inner, BudgetOptions{MaxCalls: 1, Clock: clk})
	br := NewBreaker(budget, WithBreakerClock(clk), WithBreakerThreshold(1))
	if _, ok, err := decideOnce(br); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := decideOnce(br); !errors.Is(err, decision.ErrBudget) {
			t.Fatalf("%v", err)
		}
	}
	if br.State() != BreakerClosed || br.Stats().Failures != 0 {
		t.Fatalf("a spent budget is not an engine fault: state=%v stats=%+v", br.State(), br.Stats())
	}
	// An open breaker never reaches the Budget: nothing is counted.
	failBr := NewBreaker(NewBudget(failing("x", errTransient), BudgetOptions{MaxCalls: 100}), WithBreakerClock(clk), WithBreakerThreshold(1))
	_, _, _ = decideOnce(failBr)
	for i := 0; i < 5; i++ {
		if _, _, err := decideOnce(failBr); !errors.Is(err, decision.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
	}
}

// A budget is a quota-class refusal for the strategies: no backup behind it
// unless OnQuota says so, and it is never swallowed by an abstention.
func TestBudget_IsQuotaClassForTheStrategies(t *testing.T) {
	noLeak(t)
	spent := func() *Budget {
		b := NewBudget(instant("jev", "i"), BudgetOptions{MaxCalls: 1})
		_, _, _ = decideOnce(b)
		return b
	}
	// A spent primary does not start the backup...
	backup := instant("llm", "i")
	_, ok, rep, err := Fallback(spent(), backup).DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, decision.ErrBudget) || backup.calls.Load() != 0 || rep.Attempts[0].Outcome != decision.AttemptBudget {
		t.Fatalf("ok=%v err=%v backupCalls=%d rep=%+v", ok, err, backup.calls.Load(), rep)
	}
	// ...and OnQuota, which is about an exhausted allowance, does not change that.
	_, ok, _, err = Fallback(spent(), backup, WithFallbackOn(OnQuota)).DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, decision.ErrBudget) || backup.calls.Load() != 0 {
		t.Fatalf("OnQuota must not fail over from a spent budget: ok=%v err=%v backupCalls=%d", ok, err, backup.calls.Load())
	}
	// A spent BACKUP behind a primary that failed: the call fails loudly.
	_, ok, _, err = Fallback(failing("jev", errTransient), spent()).DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, decision.ErrBudget) {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	// A backup that an abstention started and that is out of budget must not read
	// as "nobody decided".
	_, ok, rep, err = Fallback(abstaining("jev"), spent(), WithFallbackOn(OnAbstain)).DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, decision.ErrBudget) {
		t.Fatalf("an abstention swallowed the budget: ok=%v err=%v rep=%+v", ok, err, rep)
	}
	// A plain abstention still is one.
	if _, ok, _, err := Fallback(abstaining("jev"), instant("llm", "i")).DecideTraced(context.Background(), request()); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	// Race: any racer's spent budget ends the call, as a quota does.
	slow := newGated("slow", "i")
	_, ok, rep, err = Race([]decision.Provider{spent(), slow.fake}).DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, decision.ErrBudget) || !reflect.DeepEqual(attemptOutcomes(rep), []string{"jev:budget", "slow:cancelled"}) {
		t.Fatalf("ok=%v err=%v rep=%v", ok, err, attemptOutcomes(rep))
	}
	close(slow.release)
	// Hedged: a spent backup is just a failed backup; a primary that stands on its
	// abstention is not turned into an error by it.
	if _, ok, _, err := Hedged(abstaining("jev"), spent(), time.Hour, WithClock(newFakeClock())).DecideTraced(context.Background(), request()); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

// Quiet failures joined with a loud one are classified as the loud one.
func TestClassify_LoudConditionsWinOverQuietOnes(t *testing.T) {
	for want, err := range map[string]error{
		decision.AttemptQuota:         errors.Join(decision.ErrUnavailable, decision.ErrQuota),
		decision.AttemptBudget:        errors.Join(decision.ErrAuth, decision.ErrBudget),
		decision.AttemptMisconfigured: errors.Join(decision.ErrUnsupported, decision.ErrMisconfigured),
		decision.AttemptUnavailable:   errors.Join(decision.ErrUnavailable, decision.ErrAuth),
	} {
		if got, _ := classify(err, nil); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
}

// S2: what a provider writes in Outcome never makes a decision actionable, however
// it is wrapped; only a policy (or a chain) judging it does, and an inner engine's
// judgement survives the wrappers.
func TestEngines_ClaimedOutcomeIsNotActionableButAPolicysVerdictSurvivesWrappers(t *testing.T) {
	noLeak(t)
	for _, o := range []decision.Outcome{decision.OutcomeSelected, decision.OutcomeAccepted, decision.OutcomeDeterministic, decision.OutcomeFloor, decision.OutcomeSeveral} {
		liar := &fake{name: "liar", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
			d := answer("i", 0.2)
			d.Outcome = o
			return d, true, nil
		}}
		for name, p := range map[string]decision.Provider{
			"single":   Single(liar),
			"fallback": Fallback(failing("x", errTransient), liar),
			"breaker":  NewBreaker(liar),
			"budget":   NewBudget(liar, BudgetOptions{MaxCalls: 5}),
			"nested":   NewBreaker(Single(NewBudget(liar, BudgetOptions{MaxCalls: 5}))),
		} {
			if d, ok, err := decideOnce(p); !ok || err != nil || d.Actionable() {
				t.Errorf("%s/%s: ok=%v err=%v actionable=%v", name, o, ok, err, d.Actionable())
			}
		}
	}
	// With a policy the engine judges: a rule is actionable and stays so through wrappers.
	pol := decision.DurablePolicy()
	rule := &fake{name: "rule", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return decision.Deterministic(answer("i", 1)), true, nil
	}}
	judged := Single(rule, WithPolicy(pol))
	for name, p := range map[string]decision.Provider{
		"engine": judged, "breaker": NewBreaker(judged), "budget": NewBudget(judged, BudgetOptions{MaxCalls: 5}),
		"outer engine without a policy": Single(judged),
	} {
		if d, ok, _ := decideOnce(p); !ok || !d.Actionable() || d.Provenance() != decision.ProvenanceDeterministic {
			t.Errorf("%s: %+v", name, d)
		}
	}
	// A policy that refuses is stamped not actionable even on an answer that claims otherwise.
	liar := &fake{name: "liar", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		d := answer("i", 0.99)
		d.Outcome = decision.OutcomeSelected
		return d, true, nil
	}}
	if d, ok, _ := decideOnce(Single(liar, WithPolicy(pol))); !ok || d.Actionable() || d.Outcome != decision.OutcomeUnscored {
		t.Fatalf("%+v", d)
	}
}
