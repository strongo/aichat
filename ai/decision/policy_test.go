package decision

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func nan() float64 { return math.NaN() }

func relevance(scores ...Score) Answer {
	a := NewAnswer("q", KindRelevance, scores)
	a.Calibrated = true
	return a
}

func choice(conf float64, noneID string, scores ...Score) Answer {
	a := NewAnswer("q", KindChoice, scores)
	a.Calibrated, a.HasConfidence, a.Confidence, a.NoneID = true, true, conf, noneID
	return a
}

func TestPolicy_PresetsAreValid(t *testing.T) {
	for _, p := range []SelectionPolicy{NarrowingPolicy(), DurablePolicy()} {
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", p.Name, err)
		}
	}
	if NarrowingPolicy().MinProbability != 0.60 || DurablePolicy().MinProbability != 0.90 {
		t.Fatal("documented defaults changed")
	}
}

func TestPolicy_Validate(t *testing.T) {
	bad := []SelectionPolicy{
		{},                   // the zero value would select everything
		{MinConfidence: 1.2}, // out of range
		{MinConfidence: 0.5, MinProbability: 0.5, StrongProbability: 0.9, MinGap: -0.1},
		{MinConfidence: 0.5, MinProbability: 0.5, StrongProbability: 0.4},
		{MinConfidence: 0.5, PotentialProbability: 0.5, MinProbability: 0.4, StrongProbability: 0.9},
		{MinConfidence: 0.5, MinProbability: 0.5, StrongProbability: 0.9, MaxPicks: -1},
		{MinConfidence: nan(), MinProbability: 0.5, StrongProbability: 0.9},
		{MinConfidence: 0, MinProbability: 0.5, StrongProbability: 0.9},
		{MinConfidence: 0.5, MinProbability: 0, StrongProbability: 0.9},
	}
	for i, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("case %d: expected an error", i)
		}
	}
}

func TestPolicy_WorkedExamples(t *testing.T) {
	p := NarrowingPolicy()

	// 0.96 / 0.91 / 0.72: independent relevance, selects several.
	sel := p.Evaluate(relevance(Score{"loans", 0.96}, Score{"loan_items", 0.91}, Score{"members", 0.72}))
	if sel.Outcome != OutcomeSeveral || !reflect.DeepEqual(sel.Picks, []string{"loans", "loan_items", "members"}) {
		t.Fatalf("several: %+v", sel)
	}
	if !reflect.DeepEqual(sel.Strong, []string{"loans", "loan_items"}) {
		t.Fatalf("strong = %v", sel.Strong)
	}

	// 0.38 / 0.35 / 0.33 is uncertain, never "A wins", as a choice...
	sel = p.Evaluate(choice(0.07, "", Score{"A", 0.38}, Score{"B", 0.35}, Score{"C", 0.33}))
	if sel.Outcome != OutcomeUncertain || sel.Reason != ReasonLowConfidence {
		t.Fatalf("choice: %+v", sel)
	}
	// ...with no engine confidence the top probability stands in for it...
	c := choice(0, "", Score{"A", 0.38}, Score{"B", 0.35}, Score{"C", 0.33})
	c.HasConfidence = false
	sel = p.Evaluate(c)
	if sel.Outcome != OutcomeUncertain || sel.Reason != ReasonLowConfidence {
		t.Fatalf("no confidence: %+v", sel)
	}
	// ...and the gap rule still catches a close pair above the floor.
	c = choice(0, "", Score{"A", 0.55}, Score{"B", 0.45})
	c.HasConfidence = false
	sel = p.Evaluate(c)
	if sel.Outcome != OutcomeUncertain || sel.Reason != ReasonNarrowGap {
		t.Fatalf("gap: %+v", sel)
	}
	// ...and as an independent relevance list (nothing reaches the floor, but
	// all are potentially relevant).
	sel = p.Evaluate(relevance(Score{"A", 0.38}, Score{"B", 0.35}, Score{"C", 0.33}))
	if sel.Outcome != OutcomeUncertain || sel.Reason != ReasonOnlyPotential || len(sel.Potential) != 3 {
		t.Fatalf("relevance: %+v", sel)
	}
}

func TestPolicy_Choice(t *testing.T) {
	p := NarrowingPolicy()
	sel := p.Evaluate(choice(0.81, "none", Score{"billing", 0.88}, Score{"technical", 0.12}, Score{"none", 0}))
	if sel.Outcome != OutcomeSelected || !reflect.DeepEqual(sel.Picks, []string{"billing"}) {
		t.Fatalf("selected: %+v", sel)
	}
	sel = p.Evaluate(choice(0.68, "none", Score{"none", 0.71}, Score{"members", 0.13}))
	if sel.Outcome != OutcomeNone || sel.Reason != ReasonNoneOfThese || len(sel.Picks) != 0 {
		t.Fatalf("none: %+v", sel)
	}
	// A single-candidate choice has no runner-up to gap against.
	sel = p.Evaluate(choice(0.9, "", Score{"only", 1}))
	if sel.Outcome != OutcomeSelected {
		t.Fatalf("single: %+v", sel)
	}
	// Confident but the runner-up is too close.
	sel = p.Evaluate(choice(0.9, "", Score{"a", 0.5}, Score{"b", 0.4}))
	if sel.Outcome != OutcomeUncertain || sel.Reason != ReasonNarrowGap {
		t.Fatalf("gap: %+v", sel)
	}
	// The durable bar is higher: 0.7 confidence passes narrowing, not durable.
	good := choice(0.7, "", Score{"a", 0.8}, Score{"b", 0.2})
	if got := NarrowingPolicy().Evaluate(good).Outcome; got != OutcomeSelected {
		t.Fatalf("narrowing = %v", got)
	}
	if got := DurablePolicy().Evaluate(good).Outcome; got != OutcomeUncertain {
		t.Fatalf("durable = %v", got)
	}
}

// An invalid policy (the zero value, which would select everything) selects
// nothing, whatever the answer.
func TestPolicy_InvalidPolicySelectsNothing(t *testing.T) {
	for _, a := range []Answer{
		relevance(Score{"a", 0.01}),
		choice(0.9, "", Score{"a", 1}),
	} {
		sel := SelectionPolicy{}.Evaluate(a)
		if sel.Outcome != OutcomeUncertain || sel.Reason != ReasonInvalidPolicy || len(sel.Picks) != 0 || sel.Actionable() {
			t.Fatalf("%+v", sel)
		}
	}
}

// A calibrated choice with no confidence of its own leans on the top
// probability, so a lone candidate at 0.1 is not "selected".
func TestPolicy_ChoiceWithoutConfidenceNeedsAConfidentTopProbability(t *testing.T) {
	c := choice(0, "", Score{"a", 0.1})
	c.HasConfidence = false
	if sel := NarrowingPolicy().Evaluate(c); sel.Outcome != OutcomeUncertain || sel.Reason != ReasonLowConfidence {
		t.Fatalf("%+v", sel)
	}
	c = choice(0, "", Score{"a", 1})
	c.HasConfidence = false
	if sel := NarrowingPolicy().Evaluate(c); sel.Outcome != OutcomeSelected {
		t.Fatalf("%+v", sel)
	}
}

// An uncalibrated answer is never a clear winner: the outcome is "unscored" and
// the picks are a proposal, never strong and never actionable.
func TestPolicy_UncalibratedIsAProposalNeverASelection(t *testing.T) {
	a := choice(0.99, "", Score{"a", 0.99}, Score{"b", 0.01})
	a.Calibrated = false
	sel := NarrowingPolicy().Evaluate(a)
	if sel.Outcome != OutcomeUnscored || sel.Reason != ReasonNotCalibrated || !sel.Proposal ||
		!reflect.DeepEqual(sel.Picks, []string{"a"}) || len(sel.Strong) != 0 || sel.Actionable() {
		t.Fatalf("choice: %+v", sel)
	}
	// A choice whose top candidate is "none of these" proposes nothing.
	n := choice(0.9, "none", Score{"none", 0.8}, Score{"a", 0.2})
	n.Calibrated = false
	if sel := NarrowingPolicy().Evaluate(n); !sel.Proposal || len(sel.Picks) != 0 {
		t.Fatalf("none: %+v", sel)
	}
	// Relevance proposes every candidate at or above MinProbability, best first,
	// capped by MaxPicks; a calibrated list with the same numbers is a selection.
	r := relevance(Score{"x", 0.95}, Score{"y", 0.7}, Score{"z", 0.2}, Score{"w", 0.65})
	r.Calibrated = false
	p := NarrowingPolicy()
	if sel := p.Evaluate(r); sel.Outcome != OutcomeUnscored || !reflect.DeepEqual(sel.Picks, []string{"x", "y", "w"}) || len(sel.Strong) != 0 || len(sel.Potential) != 0 {
		t.Fatalf("relevance: %+v", sel)
	}
	p.MaxPicks = 2
	if sel := p.Evaluate(r); !reflect.DeepEqual(sel.Picks, []string{"x", "y"}) {
		t.Fatalf("capped: %+v", sel)
	}
	r.Calibrated = true
	if sel := p.Evaluate(r); !sel.Actionable() || sel.Proposal {
		t.Fatalf("calibrated: %+v", sel)
	}
	// No scores at all: nothing to propose.
	if sel := p.Evaluate(Answer{Kind: KindChoice}); sel.Outcome != OutcomeUnscored || sel.Proposal || len(sel.Picks) != 0 {
		t.Fatalf("empty: %+v", sel)
	}
}

func TestPolicy_NoScoresIsUncertain(t *testing.T) {
	a := Answer{Kind: KindChoice, Calibrated: true}
	sel := NarrowingPolicy().Evaluate(a)
	if sel.Outcome != OutcomeUncertain || sel.Reason != ReasonNoScores {
		t.Fatalf("%+v", sel)
	}
}

func TestPolicy_RankIsDefensive(t *testing.T) {
	// Scores deliberately not sorted, as a caller that skipped NewAnswer might pass.
	a := Answer{Kind: KindChoice, Calibrated: true, HasConfidence: true, Confidence: 0.9,
		Scores: []Score{{"low", 0.1}, {"high", 0.9}}}
	sel := NarrowingPolicy().Evaluate(a)
	if sel.Outcome != OutcomeSelected || sel.Picks[0] != "high" {
		t.Fatalf("%+v", sel)
	}
}

func TestPolicy_RelevanceSelectedNoneAndCap(t *testing.T) {
	p := NarrowingPolicy()
	sel := p.Evaluate(relevance(Score{"a", 0.7}, Score{"b", 0.2}))
	if sel.Outcome != OutcomeSelected || len(sel.Strong) != 0 {
		t.Fatalf("selected: %+v", sel)
	}
	sel = p.Evaluate(relevance(Score{"a", 0.1}, Score{"b", 0.2}))
	if sel.Outcome != OutcomeNone || sel.Reason != ReasonNothingAbove {
		t.Fatalf("none: %+v", sel)
	}
	p.MaxPicks = 2
	sel = p.Evaluate(relevance(Score{"a", 0.99}, Score{"b", 0.9}, Score{"c", 0.8}, Score{"d", 0.7}))
	if sel.Outcome != OutcomeSeveral || len(sel.Picks) != 2 || sel.Reason != ReasonTruncatedToMax || len(sel.Strong) != 2 {
		t.Fatalf("cap: %+v", sel)
	}
	p.MaxPicks = 1
	sel = p.Evaluate(relevance(Score{"a", 0.99}, Score{"b", 0.9}))
	if sel.Outcome != OutcomeSelected || len(sel.Strong) != 1 {
		t.Fatalf("cap to one: %+v", sel)
	}
}

func TestPolicy_EvaluateDecision(t *testing.T) {
	p := NarrowingPolicy()
	d := Decision{
		Module: Scored{Value: "m", Confidence: 0.9}, Intent: Scored{Value: "i", Confidence: 0.8},
		Scores: map[string]float64{"m/i": 0.9, "m/j": 0.1}, Calibrated: true,
	}
	if sel := p.EvaluateDecision(d); sel.Outcome != OutcomeSelected {
		t.Fatalf("%+v", sel)
	}
	// No Intent: the module's confidence is the one judged.
	d.Intent = Scored{}
	d.Module.Confidence = 0.1
	if sel := p.EvaluateDecision(d); sel.Outcome != OutcomeUncertain || !strings.Contains(sel.Reason, "confidence") {
		t.Fatalf("%+v", sel)
	}
}
