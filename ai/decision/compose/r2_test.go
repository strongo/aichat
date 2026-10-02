package compose

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

// uncertainPrimary answers a calibrated decision a narrowing policy judges uncertain.
func uncertainPrimary() *fake {
	d := answer("i", 0.6)
	d.Calibrated, d.Scores = true, map[string]float64{"m/i": 0.55, "m/j": 0.45}
	return &fake{name: "jev", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) { return d, true, nil }}
}

// R2 major: a spent backup budget was swallowed when the primary's uncertain answer,
// or an abstention with OnQuota set, was returned instead, so the chain never
// stopped and went on to the next provider on every turn.
func TestEngines_ASpentBackupBudgetStopsTheChainWhateverTheStrategyOrTrigger(t *testing.T) {
	const turns, cap = 50, 3
	nar := decision.NarrowingPolicy()
	type mk func(primary, backup decision.Provider, opts ...Option) *Engine
	strategies := map[string]mk{
		"fallback": func(p, b decision.Provider, o ...Option) *Engine { return Fallback(p, b, o...) },
		"hedged":   func(p, b decision.Provider, o ...Option) *Engine { return Hedged(p, b, time.Hour, o...) },
		"race":     func(p, b decision.Provider, o ...Option) *Engine { return Race([]decision.Provider{p, b}, o...) },
	}
	primaries := map[string]struct {
		primary func() *fake
		opts    []Option
	}{
		"uncertain + OnUncertain":       {uncertainPrimary, []Option{WithPolicy(nar), WithFallbackOn(OnUncertain)}},
		"uncertain + OnUncertain|Quota": {uncertainPrimary, []Option{WithPolicy(nar), WithFallbackOn(OnUncertain | OnQuota)}},
		"abstain + OnAbstain":           {func() *fake { return abstaining("jev") }, []Option{WithFallbackOn(OnAbstain)}},
		"abstain + OnAbstain|OnQuota":   {func() *fake { return abstaining("jev") }, []Option{WithFallbackOn(OnAbstain | OnQuota)}},
		"transient failure":             {func() *fake { return failing("jev", errTransient) }, nil},
	}
	for sn, strategy := range strategies {
		for pn, pc := range primaries {
			t.Run(sn+"/"+pn, func(t *testing.T) {
				llm := instant("llm", "i")
				var next atomic.Int64
				nextProvider := &fake{name: "cloud-decider", decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
					next.Add(1)
					return decision.Decision{}, false, nil
				}}
				budget := NewBudget(llm, BudgetOptions{MaxCalls: cap})
				engine := strategy(pc.primary(), NewBreaker(budget), pc.opts...)
				chain := decision.Chain{Providers: []decision.Provider{engine, nextProvider}}
				var decided, stopped int
				var last decision.Trace
				for i := 0; i < turns; i++ {
					_, ok, tr := chain.Decide(context.Background(), request())
					last = tr
					switch {
					case ok:
						decided++
					case tr.StoppedBy != "":
						stopped++
					}
				}
				if llm.calls.Load() != cap || decided != cap || stopped != turns-cap || next.Load() != 0 {
					t.Fatalf("backup calls=%d decided=%d stopped=%d next-provider calls=%d (want %d, %d, %d, 0)",
						llm.calls.Load(), decided, stopped, next.Load(), cap, cap, turns-cap)
				}
				if last.StoppedBy != decision.AttemptBudget || !errors.Is(last.Err(), decision.ErrBudget) {
					t.Fatalf("%+v err=%v", last, last.Err())
				}
			})
		}
	}
}

// An uncertain primary followed by a backup that is refused for any reason the engine
// does not absorb is a failure, not the primary's uncertain answer.
func TestEngines_AnyRefusalOfALaterLegBeatsAnEarlierUncertainAnswer(t *testing.T) {
	nar := decision.NarrowingPolicy()
	for name, refusal := range map[string]error{"quota": errQuota, "budget": decision.ErrBudget, "misconfigured": decision.ErrMisconfigured} {
		_, ok, _, err := Fallback(uncertainPrimary(), failing("llm", refusal), WithPolicy(nar), WithFallbackOn(OnUncertain)).DecideTraced(context.Background(), request())
		if ok || !errors.Is(err, refusal) {
			t.Errorf("%s: ok=%v err=%v", name, ok, err)
		}
	}
	// With OnQuota the quota is the caller's chosen failover, so the uncertain answer
	// of the primary stands only when the backup was not refused for something else.
	d, ok, _, err := Fallback(uncertainPrimary(), instant("llm", "i"), WithPolicy(nar), WithFallbackOn(OnUncertain)).DecideTraced(context.Background(), request())
	if !ok || err != nil || !d.Actionable() {
		t.Fatalf("an accepted backup answer is the result: ok=%v err=%v", ok, err)
	}
}

// A scored call: the same rule through ScoreTraced.
func TestEngines_ASpentBudgetBeatsAnUncertainScoredAnswer(t *testing.T) {
	nar := decision.NarrowingPolicy()
	sreq := decision.ScoreRequest{Text: "t", Questions: []decision.Question{{ID: "q", Kind: decision.KindChoice, Instructions: "x", Candidates: []decision.Candidate{{ID: "a"}, {ID: "b"}}}}}
	uncertain := &fake{name: "jev", score: func(context.Context, decision.ScoreRequest) (decision.ScoreResult, error) {
		return decision.ScoreResult{Answers: map[string]decision.Answer{"q": {
			QuestionID: "q", Kind: decision.KindChoice, Calibrated: true, HasConfidence: true, Confidence: 0.1,
			Scores: []decision.Score{{ID: "a", Probability: 0.5}, {ID: "b", Probability: 0.5}},
		}}}, nil
	}}
	spent := NewBudget(&fake{name: "llm", score: func(context.Context, decision.ScoreRequest) (decision.ScoreResult, error) {
		return decision.ScoreResult{}, nil
	}}, BudgetOptions{MaxCalls: 1})
	_, _ = spent.Score(context.Background(), sreq) // spends the one call
	_, err := Fallback(uncertain, spent, WithPolicy(nar), WithFallbackOn(OnUncertain)).Score(context.Background(), sreq)
	if !errors.Is(err, decision.ErrBudget) {
		t.Fatalf("%v", err)
	}
}

// OnQuota is about an exhausted allowance: it never lets a backup take over from a
// spent budget, in any strategy.
func TestEngines_OnQuotaDoesNotFailOverFromASpentBudget(t *testing.T) {
	noLeak(t)
	spent := func() *Budget {
		b := NewBudget(instant("jev", "i"), BudgetOptions{MaxCalls: 1})
		_, _, _ = decideOnce(b)
		return b
	}
	backup := instant("llm", "i")
	for name, e := range map[string]*Engine{
		"fallback": Fallback(spent(), backup, WithFallbackOn(OnQuota)),
		"hedged":   Hedged(spent(), backup, time.Hour, WithFallbackOn(OnQuota), WithClock(newFakeClock())),
		"race":     Race([]decision.Provider{spent(), newGated("slow", "i").fake}, WithFallbackOn(OnQuota)),
	} {
		if _, ok, _, err := e.DecideTraced(context.Background(), request()); ok || !errors.Is(err, decision.ErrBudget) {
			t.Errorf("%s: ok=%v err=%v", name, ok, err)
		}
	}
	if backup.calls.Load() != 0 {
		t.Fatalf("the backup took over from a spent budget %d times", backup.calls.Load())
	}
}

// m-f: wrapping rules does not hide that they are rules.
func TestWrappersForwardIsDeterministic(t *testing.T) {
	rules := detProvider{instant("rules", "i")}
	plain := instant("llm", "i")
	cases := map[string]struct {
		p    decision.DeterministicProvider
		want bool
	}{
		"single(rules)":                  {Single(rules), true},
		"fallback(rules, rules)":         {Fallback(rules, rules), true},
		"fallback(rules, llm)":           {Fallback(rules, plain), false},
		"single(llm)":                    {Single(plain), false},
		"breaker(rules)":                 {NewBreaker(rules), true},
		"breaker(llm)":                   {NewBreaker(plain), false},
		"breaker(nil)":                   {NewBreaker(nil), false},
		"budget(rules)":                  {NewBudget(rules, BudgetOptions{MaxCalls: 1}), true},
		"budget(llm)":                    {NewBudget(plain, BudgetOptions{MaxCalls: 1}), false},
		"breaker(budget(single(rules)))": {NewBreaker(NewBudget(Single(rules), BudgetOptions{MaxCalls: 1})), true},
		"engine with no providers":       {Race(nil), false},
		"a nil provider":                 {Single(nil), false},
	}
	for name, tc := range cases {
		if got := tc.p.IsDeterministic(); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// detProvider is a deterministic provider (as rules.Provider is).
type detProvider struct{ *fake }

func (detProvider) IsDeterministic() bool { return true }
