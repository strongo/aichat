package decision

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func scoredDecision(conf float64, calibrated bool, scores map[string]float64) Decision {
	d := decided("calendar", "show", conf)
	d.Scores, d.Calibrated = scores, calibrated
	return d
}

func chainOf(policy *SelectionPolicy, ps ...Provider) Chain {
	return Chain{Providers: ps, Policy: policy}
}

func constProvider(name string, d Decision) Provider {
	return providerFunc{name: name, fn: func(context.Context, Request) (Decision, bool, error) { return d, true, nil }}
}

func TestChain_PolicySelectedStopsTheChain(t *testing.T) {
	pol := NarrowingPolicy()
	d, ok, tr := chainOf(&pol, constProvider("jev", scoredDecision(0.9, true, map[string]float64{"calendar/show": 0.9, "calendar/create": 0.1}))).Decide(context.Background(), req())
	if !ok || d.Outcome != OutcomeSelected || !d.Actionable() {
		t.Fatalf("ok=%v outcome=%q", ok, d.Outcome)
	}
	if tr.Outcome != OutcomeSelected || !tr.Calibrated || tr.DecidedBy != "jev" || tr.Engine != "jev" {
		t.Fatalf("trace = %+v", tr)
	}
	if got := tr.Attempts[0]; got.Outcome != AttemptDecided || got.Detail != "selected" {
		t.Fatalf("attempt = %+v", got)
	}
}

// 0.38/0.35/0.33 is not a clear answer: by default the chain records it as
// "uncertain" and falls through to the next provider rather than returning a
// decision a caller could act on by accident.
func TestChain_PolicyUncertainFallsThrough(t *testing.T) {
	pol := NarrowingPolicy()
	uncertain := scoredDecision(0.07, true, map[string]float64{"calendar/show": 0.38, "calendar/create": 0.35, "x": 0.33})
	good := scoredDecision(0.9, true, map[string]float64{"calendar/show": 0.9, "y": 0.1})
	d, ok, tr := chainOf(&pol, constProvider("a", uncertain), constProvider("b", good)).Decide(context.Background(), req())
	if !ok || tr.DecidedBy != "b" || d.Outcome != OutcomeSelected {
		t.Fatalf("ok=%v tr=%+v", ok, tr)
	}
	if got := tr.Attempts[0]; got.Outcome != AttemptUncertain || got.Detail != "uncertain: low_confidence" {
		t.Fatalf("attempt = %+v", got)
	}
	// Alone, an uncertain answer leaves the chain undecided.
	d, ok, tr = chainOf(&pol, constProvider("a", uncertain)).Decide(context.Background(), req())
	if ok || d.Outcome != "" || tr.DecidedBy != "" {
		t.Fatalf("ok=%v d=%+v tr=%+v", ok, d, tr)
	}
}

func TestChain_KeepNonSelectedReturnsTheAnswerButItIsNotActionable(t *testing.T) {
	pol := NarrowingPolicy()
	uncertain := scoredDecision(0.07, true, map[string]float64{"calendar/show": 0.38, "calendar/create": 0.35, "x": 0.33})
	c := Chain{Providers: []Provider{constProvider("jev", uncertain)}, Policy: &pol, KeepNonSelected: true}
	d, ok, tr := c.Decide(context.Background(), req())
	if !ok || d.Outcome != OutcomeUncertain || d.Actionable() || tr.Outcome != OutcomeUncertain {
		t.Fatalf("ok=%v d=%+v tr=%+v", ok, d, tr)
	}
	if got := tr.Attempts[0]; got.Outcome != AttemptDecided || got.Detail != "uncertain: low_confidence" {
		t.Fatalf("attempt = %+v", got)
	}
}

func TestDecision_Actionable(t *testing.T) {
	want := map[Outcome]bool{OutcomeSelected: true, OutcomeSeveral: true, OutcomeAccepted: true, OutcomeDeterministic: true, OutcomeFloor: true,
		"": false, OutcomeUnscored: false, OutcomeUncertain: false, OutcomeNone: false, OutcomeInvalid: false, "Selected": false}
	for outcome, w := range want {
		if got := (Decision{}).stamped(outcome).Actionable(); got != w {
			t.Errorf("Decision %q: %v", outcome, got)
		}
		// One rule: a Selection with the same verdict agrees.
		if got := (Selection{Outcome: outcome}).Actionable(); got != w {
			t.Errorf("Selection %q: %v", outcome, got)
		}
	}
	// Nothing is actionable by omission: not the zero value, not a decision a
	// provider returned with no verdict.
	if (Decision{}).Actionable() || decided("calendar", "show", 1).Actionable() {
		t.Fatal("an unjudged decision must not be actionable")
	}
}

func TestChain_NilPolicyKeepsLegacyBehaviour(t *testing.T) {
	d := scoredDecision(0.07, true, map[string]float64{"calendar/show": 0.38})
	got, ok, tr := chainOf(nil, constProvider("jev", d)).Decide(context.Background(), req())
	if ok || got.Outcome != "" || tr.Attempts[0].Outcome != AttemptLowConfidence {
		t.Fatalf("ok=%v d=%+v tr=%+v", ok, got, tr)
	}
}

// Under a policy, an uncalibrated decision is accepted only by the policy's
// explicit AcceptUncalibratedAt bar: narrowing opts in at 0.70, durable never.
func TestChain_PolicyUncalibratedNeedsTheExplicitOptIn(t *testing.T) {
	nar, dur := NarrowingPolicy(), DurablePolicy()
	llm := func(conf float64) Provider { return constProvider("llm", decided("calendar", "show", conf)) }

	d, ok, tr := chainOf(&nar, llm(0.95)).Decide(context.Background(), req())
	if !ok || d.Outcome != OutcomeAccepted || !d.Actionable() || d.Calibrated || tr.Outcome != OutcomeAccepted || tr.Calibrated {
		t.Fatalf("narrowing 0.95: ok=%v d=%+v tr=%+v", ok, d, tr)
	}
	if got := tr.Attempts[0]; got.Outcome != AttemptDecided || got.Detail != "accepted: accepted_uncalibrated" {
		t.Fatalf("attempt = %+v", got)
	}
	if _, ok, tr := chainOf(&nar, llm(0.72)).Decide(context.Background(), req()); !ok || tr.Outcome != OutcomeAccepted {
		t.Fatalf("narrowing 0.72 clears its 0.70 bar: ok=%v tr=%+v", ok, tr)
	}
	// Below the bar: unscored, not actionable, falls through.
	if _, ok, tr := chainOf(&nar, llm(0.6)).Decide(context.Background(), req()); ok || tr.Attempts[0].Outcome != AttemptUncertain || tr.Attempts[0].Detail != "unscored: low_confidence" {
		t.Fatalf("narrowing 0.6: ok=%v tr=%+v", ok, tr)
	}
	// The probed bypass: a durable chain whose calibrated engine is down must NOT
	// act on an LLM's self-reported 0.72 (the durable bar is 0.90, calibrated).
	d, ok, tr = chainOf(&dur, llm(0.72)).Decide(context.Background(), req())
	if ok || d.Outcome != "" || tr.Attempts[0].Outcome != AttemptUncertain || tr.Attempts[0].Detail != "unscored: not_calibrated" {
		t.Fatalf("durable 0.72: ok=%v d=%+v tr=%+v", ok, d, tr)
	}
	if _, ok, _ := chainOf(&dur, llm(0.99)).Decide(context.Background(), req()); ok {
		t.Fatal("a durable chain never acts on an uncalibrated decision")
	}
	// KeepNonSelected returns it, and it is not actionable.
	keep := Chain{Providers: []Provider{llm(0.72)}, Policy: &dur, KeepNonSelected: true}
	d, ok, tr = keep.Decide(context.Background(), req())
	if !ok || d.Outcome != OutcomeUnscored || d.Actionable() || tr.Outcome != OutcomeUnscored {
		t.Fatalf("keep: ok=%v d=%+v tr=%+v", ok, d, tr)
	}
	// A calibrated claim with no Scores cannot be judged by probabilities either.
	cal := decided("calendar", "show", 0.72)
	cal.Calibrated = true
	if _, ok, tr := chainOf(&dur, constProvider("jev", cal)).Decide(context.Background(), req()); ok || tr.Attempts[0].Detail != "unscored: no_scores" {
		t.Fatalf("calibrated without scores: ok=%v tr=%+v", ok, tr)
	}
	// The explicit opt-in is a policy value.
	custom := dur
	custom.AcceptUncalibratedAt = 0.8
	if _, ok, _ := chainOf(&custom, llm(0.85)).Decide(context.Background(), req()); !ok {
		t.Fatal("a durable policy that opts in at 0.8 accepts 0.85")
	}
	if _, ok, _ := chainOf(&custom, llm(0.75)).Decide(context.Background(), req()); ok {
		t.Fatal("... and rejects 0.75")
	}
}

// With a policy the policy's bar replaces the chain's MinConfidence floor, and a
// score-bearing uncalibrated decision is judged by self-reported confidence only.
func TestChain_PolicyDoesNotThresholdUncalibratedScores(t *testing.T) {
	pol := NarrowingPolicy()
	low := scoredDecision(0.3, false, map[string]float64{"calendar/show": 0.99})
	if _, ok, tr := chainOf(&pol, constProvider("llm", low)).Decide(context.Background(), req()); ok || tr.Attempts[0].Outcome != AttemptUncertain {
		t.Fatalf("ok=%v tr=%+v", ok, tr)
	}
	hi := scoredDecision(0.8, false, map[string]float64{"calendar/show": 0.1})
	if d, ok, _ := chainOf(&pol, constProvider("llm", hi)).Decide(context.Background(), req()); !ok || d.Outcome != OutcomeAccepted {
		t.Fatalf("ok=%v d=%+v", ok, d)
	}
}

// A provider that carries its own uncertain or none verdict (an engine built
// with a policy) is honoured even by a chain without a policy.
func TestChain_NoPolicyHonoursAProvidersExplicitNonActionableVerdict(t *testing.T) {
	for _, o := range []Outcome{OutcomeUncertain, OutcomeNone, OutcomeUnscored} {
		d := decided("calendar", "show", 0.95).stamped(o)
		if got, ok, tr := chainOf(nil, constProvider("e", d)).Decide(context.Background(), req()); ok || tr.Attempts[0].Outcome != AttemptUncertain || tr.Attempts[0].Detail != string(o) {
			t.Fatalf("%s: ok=%v got=%+v tr=%+v", o, ok, got, tr)
		}
		got, ok, tr := Chain{Providers: []Provider{constProvider("e", d)}, KeepNonSelected: true}.Decide(context.Background(), req())
		if !ok || got.Outcome != o || got.Actionable() || tr.Outcome != o {
			t.Fatalf("%s keep: ok=%v got=%+v tr=%+v", o, ok, got, tr)
		}
	}
	// A policy-less chain always stamps its own floor verdict over a provider's positive one.
	d := decided("calendar", "show", 0.95).stamped(OutcomeSelected)
	if got, ok, _ := chainOf(nil, constProvider("e", d)).Decide(context.Background(), req()); !ok || got.Outcome != OutcomeFloor {
		t.Fatalf("ok=%v got=%+v", ok, got)
	}
}

func TestChain_PolicyInvalidStillFallsThrough(t *testing.T) {
	pol := NarrowingPolicy()
	bad := scoredDecision(0.9, true, map[string]float64{"x": 1})
	bad.Module.Value = "nope"
	good := scoredDecision(0.9, true, map[string]float64{"calendar/show": 0.9, "y": 0.1})
	d, ok, tr := chainOf(&pol, constProvider("bad", bad), constProvider("good", good)).Decide(context.Background(), req())
	if !ok || tr.DecidedBy != "good" || tr.Attempts[0].Outcome != AttemptInvalid || d.Outcome != OutcomeSelected {
		t.Fatalf("ok=%v tr=%+v", ok, tr)
	}
}

func TestChain_UnavailableIsRecordedDistinctly(t *testing.T) {
	down := providerFunc{name: "jev", fn: func(context.Context, Request) (Decision, bool, error) {
		return Decision{}, false, fmt.Errorf("breaker open: %w", ErrUnavailable)
	}}
	_, ok, tr := chainOf(nil, down).Decide(context.Background(), req())
	if ok || tr.Attempts[0].Outcome != AttemptUnavailable {
		t.Fatalf("ok=%v tr=%+v", ok, tr)
	}
}

// tracedStub is a TracedProvider returning a canned decision and report.
type tracedStub struct {
	name string
	d    Decision
	ok   bool
	rep  Report
	err  error
}

func (s tracedStub) Name() string { return s.name }
func (s tracedStub) Decide(ctx context.Context, r Request) (Decision, bool, error) {
	d, ok, _, err := s.DecideTraced(ctx, r)
	return d, ok, err
}
func (s tracedStub) DecideTraced(context.Context, Request) (Decision, bool, Report, error) {
	return s.d, s.ok, s.rep, s.err
}

func TestChain_TracedProviderListsInnerEnginesAndFlags(t *testing.T) {
	stub := tracedStub{
		name: "hedged(jev,llm)", ok: true, d: decided("calendar", "show", 0.95),
		rep: Report{Strategy: "hedged", Engine: "llm", HedgeFired: true, FallbackFired: true, Attempts: []Attempt{
			{Provider: "jev", Outcome: AttemptCancelled, Role: "primary"},
			{Provider: "llm", Outcome: AttemptDecided, Role: "backup"},
		}},
	}
	d, ok, tr := Chain{Providers: []Provider{stub}}.Decide(context.Background(), req())
	if !ok || d.Module.Value != "calendar" {
		t.Fatalf("ok=%v d=%+v", ok, d)
	}
	if tr.DecidedBy != "hedged(jev,llm)" || tr.Engine != "llm" || tr.Strategy != "hedged" || !tr.HedgeFired || !tr.FallbackFired {
		t.Fatalf("trace = %+v", tr)
	}
	if len(tr.Attempts) != 2 || tr.Attempts[0].Provider != "jev" || tr.Attempts[1].Outcome != AttemptDecided {
		t.Fatalf("attempts = %+v", tr.Attempts)
	}
}

func TestChain_TracedProviderAnswerRejectedByChainIsRelabelled(t *testing.T) {
	stub := tracedStub{
		name: "fallback", ok: true, d: decided("calendar", "show", 0.2), // below the 0.7 floor
		rep: Report{Strategy: "fallback", Engine: "jev", Attempts: []Attempt{{Provider: "jev", Outcome: AttemptDecided}}},
	}
	_, ok, tr := Chain{Providers: []Provider{stub}}.Decide(context.Background(), req())
	if ok || tr.DecidedBy != "" || tr.Attempts[0].Outcome != AttemptLowConfidence {
		t.Fatalf("ok=%v tr=%+v", ok, tr)
	}
	if tr.Strategy != "" {
		t.Fatalf("strategy leaked into an undecided trace: %+v", tr)
	}
}

func TestChain_TracedProviderFailureKeepsReportedAttempts(t *testing.T) {
	stub := tracedStub{
		name: "fallback", err: errors.New("both failed"),
		rep: Report{Strategy: "fallback", Attempts: []Attempt{
			{Provider: "jev", Outcome: AttemptError}, {Provider: "llm", Outcome: AttemptError},
		}},
	}
	_, ok, tr := Chain{Providers: []Provider{stub}}.Decide(context.Background(), req())
	if ok || len(tr.Attempts) != 2 {
		t.Fatalf("ok=%v tr=%+v", ok, tr)
	}
}

func TestChain_TracedProviderWithoutAttemptsFallsBackToChainAttempt(t *testing.T) {
	stub := tracedStub{name: "odd", ok: false}
	_, ok, tr := Chain{Providers: []Provider{stub}}.Decide(context.Background(), req())
	if ok || len(tr.Attempts) != 1 || tr.Attempts[0].Provider != "odd" || tr.Attempts[0].Outcome != AttemptAbstained {
		t.Fatalf("ok=%v tr=%+v", ok, tr)
	}
}
