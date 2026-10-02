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

func TestChain_PolicyReturnsNonClearAnswersWithOutcome(t *testing.T) {
	pol := NarrowingPolicy()
	cases := []struct {
		name    string
		d       Decision
		outcome Outcome
		detail  string
	}{
		{"selected", scoredDecision(0.9, true, map[string]float64{"calendar/show": 0.9, "calendar/create": 0.1}), OutcomeSelected, "selected"},
		// 0.38/0.35/0.33: with the legacy 0.7 floor this vanished into an empty
		// decision; under a policy it reaches the caller as uncertain.
		{"uncertain", scoredDecision(0.07, true, map[string]float64{"calendar/show": 0.38, "calendar/create": 0.35, "x": 0.33}), OutcomeUncertain, "uncertain: low_confidence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ok, tr := chainOf(&pol, constProvider("jev", tc.d)).Decide(context.Background(), req())
			if !ok || d.Outcome != tc.outcome {
				t.Fatalf("ok=%v outcome=%q", ok, d.Outcome)
			}
			if tr.Outcome != tc.outcome || !tr.Calibrated || tr.DecidedBy != "jev" || tr.Engine != "jev" {
				t.Fatalf("trace = %+v", tr)
			}
			if got := tr.Attempts[0]; got.Outcome != AttemptDecided || got.Detail != tc.detail {
				t.Fatalf("attempt = %+v", got)
			}
		})
	}
}

func TestChain_NilPolicyKeepsLegacyBehaviour(t *testing.T) {
	d := scoredDecision(0.07, true, map[string]float64{"calendar/show": 0.38})
	got, ok, tr := chainOf(nil, constProvider("jev", d)).Decide(context.Background(), req())
	if ok || got.Outcome != "" || tr.Attempts[0].Outcome != AttemptLowConfidence {
		t.Fatalf("ok=%v d=%+v tr=%+v", ok, got, tr)
	}
}

func TestChain_PolicyUncalibratedIsUnscoredAndKeepsFloor(t *testing.T) {
	pol := NarrowingPolicy()
	// An emulator's confident, score-less answer is accepted but unscored.
	d, ok, tr := chainOf(&pol, constProvider("llm", decided("calendar", "show", 0.95))).Decide(context.Background(), req())
	if !ok || d.Outcome != OutcomeUnscored || tr.Outcome != OutcomeUnscored || tr.Calibrated {
		t.Fatalf("ok=%v d=%+v tr=%+v", ok, d, tr)
	}
	// Uncalibrated numbers are never thresholded by the policy; the legacy
	// floor still rejects a low self-reported confidence.
	low := scoredDecision(0.3, false, map[string]float64{"calendar/show": 0.99})
	if _, ok, tr := chainOf(&pol, constProvider("llm", low)).Decide(context.Background(), req()); ok || tr.Attempts[0].Outcome != AttemptLowConfidence {
		t.Fatalf("ok=%v tr=%+v", ok, tr)
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
