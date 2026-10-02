package decision_test

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/decision/compose"
)

// m-a: a verdict is bound to the content it was stamped on.
func TestJudgedDecisionEditedAfterwardsIsNoLongerActionable(t *testing.T) {
	judged, ok, _ := decision.Chain{Providers: []decision.Provider{stub{name: "p", d: answer(0.9), ok: true}}}.Decide(context.Background(), request())
	if !ok || !judged.Actionable() {
		t.Fatalf("%+v", judged)
	}
	edits := map[string]func(*decision.Decision){
		"interaction":            func(d *decision.Decision) { d.Interaction = decision.InteractionUndo },
		"interaction confidence": func(d *decision.Decision) { d.InteractionConfidence = 0.2 },
		"module":                 func(d *decision.Decision) { d.Module.Value = "" },
		"intent":                 func(d *decision.Decision) { d.Intent.Value = "j" },
		"module confidence":      func(d *decision.Decision) { d.Module.Confidence = 0.1 },
		"intent confidence":      func(d *decision.Decision) { d.Intent.Confidence = 0.1 },
		"calibrated":             func(d *decision.Decision) { d.Calibrated = true },
	}
	for name, edit := range edits {
		d := judged // a copy
		edit(&d)
		if d.Actionable() {
			t.Errorf("a judged decision with an edited %s is still actionable", name)
		}
	}
	if !judged.Actionable() {
		t.Fatal("editing a copy must not touch the original")
	}
	// The probe: judged low-risk, then turned into a module-less undo at confidence 0.
	m := judged
	m.Interaction, m.InteractionConfidence, m.Module.Value, m.Intent.Value = decision.InteractionUndo, 0, "", ""
	if m.Actionable() {
		t.Fatal("a mutated command became an actionable undo")
	}
	// A rule decision edited after Deterministic loses it too, and so does an edit of the
	// pieces that are not bound only by a fresh judgement (documented).
	rule := decision.Deterministic(answer(1))
	rule.Interaction = decision.InteractionUndo
	if rule.Actionable() {
		t.Fatal("an edited deterministic decision stays actionable")
	}
	if re, ok := (decision.Chain{}).Rejudge(m, tax); ok || re.Actionable() {
		t.Fatalf("an edited decision must be judged again, and this one fails: %+v", re)
	}
}

func TestUnmarshalIntoAJudgedDecisionClearsTheVerdictAndTheClass(t *testing.T) {
	rule := decision.Deterministic(answer(1))
	if err := json.Unmarshal([]byte(`{"interaction":"undo","module":{"value":""},"intent":{"value":""},"outcome":"uncertain"}`), &rule); err != nil {
		t.Fatal(err)
	}
	if rule.Actionable() || rule.Provenance() != decision.ProvenanceSelfReported || rule.Interaction != decision.InteractionUndo {
		t.Fatalf("%+v", rule)
	}
	judged, _, _ := decision.Chain{Providers: []decision.Provider{stub{name: "p", d: answer(0.9), ok: true}}}.Decide(context.Background(), request())
	if err := json.Unmarshal([]byte(`{}`), &judged); err != nil || judged.Actionable() {
		t.Fatalf("even an empty body discards the old verdict: %v %+v", err, judged)
	}
	if err := json.Unmarshal([]byte(`{"module":"not an object"}`), &judged); err == nil {
		t.Fatal("a body of the wrong shape must be an error")
	}
	// The wire form is unchanged.
	var back decision.Decision
	b, _ := json.Marshal(answer(0.9))
	if err := json.Unmarshal(b, &back); err != nil || back.Module.Value != "m" || back.Actionable() {
		t.Fatalf("%+v %v", back, err)
	}
}

// m-b: Rejudge never upgrades a stored refusal.
func TestRejudgeKeepsAStoredRefusal(t *testing.T) {
	d := decision.Decision{
		Module: decision.Scored{Value: "m", Confidence: 0.75}, Intent: decision.Scored{Value: "i", Confidence: 0.75},
		Interaction: decision.InteractionCommand, Calibrated: true, Scores: map[string]float64{"m/i": 0.55, "m/j": 0.45},
	}
	inner := compose.Single(stub{name: "jev", d: d, ok: true}, compose.WithPolicy(decision.DurablePolicy()))
	live := decision.Chain{Providers: []decision.Provider{inner}, KeepNonSelected: true}
	out, ok, _ := live.Decide(context.Background(), request())
	if !ok || out.Actionable() || out.Outcome != decision.OutcomeUncertain {
		t.Fatalf("the live chain refused it: ok=%v %+v", ok, out)
	}
	b, _ := json.Marshal(out)
	var stored decision.Decision
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]decision.Decision{"stored": stored, "in-process": out} {
		re, rok := live.Rejudge(got, tax)
		if rok || re.Actionable() || re.Outcome != decision.OutcomeUncertain {
			t.Fatalf("%s: a refusal was upgraded by the policy-less floor: ok=%v %+v", name, rok, re)
		}
	}
	// To judge it afresh under another bar the caller clears the recorded refusal.
	stored.Outcome = ""
	if re, rok := (decision.Chain{}).Rejudge(stored, tax); !rok || re.Outcome != decision.OutcomeFloor {
		t.Fatalf("%+v", re)
	}
	// The same decision judged by a chain that carries the policy is refused by it.
	dur := decision.DurablePolicy()
	if re, rok := (decision.Chain{Policy: &dur}).Rejudge(stored, tax); rok || re.Outcome != decision.OutcomeUncertain {
		t.Fatalf("%+v", re)
	}
}

// m-c: what a non-actionable Rejudge result carries.
func TestRejudgeOutcomesOfRefusals(t *testing.T) {
	low, _ := (decision.Chain{}).Rejudge(answer(0.1), tax)
	if low.Actionable() || low.Outcome != "" {
		t.Fatalf("the floor names no policy outcome: %+v", low)
	}
	se := decision.Decision{Interaction: decision.InteractionUndo, InteractionConfidence: 0.2}
	if got, ok := (decision.Chain{}).Rejudge(se, tax); ok || got.Outcome != "" {
		t.Fatalf("%+v", got)
	}
	bad := answer(0.9)
	bad.Module.Value = "zzz"
	if got, ok := (decision.Chain{}).Rejudge(bad, tax); ok || got.Outcome != decision.OutcomeInvalid {
		t.Fatalf("%+v", got)
	}
	nar := decision.NarrowingPolicy()
	if got, ok := (decision.Chain{Policy: &nar}).Rejudge(se, tax); ok || got.Outcome != decision.OutcomeUnscored {
		t.Fatalf("a policy's own refusal is kept: %+v", got)
	}
}

// m-d: JudgeDecision cannot take a taxonomy, but it never trusts a number Validate refuses.
func TestJudgeDecisionRefusesNonsenseNumbers(t *testing.T) {
	pols := map[string]decision.SelectionPolicy{"durable": decision.DurablePolicy(), "narrowing": decision.NarrowingPolicy()}
	cases := map[string]func(*decision.Decision){
		"NaN interaction confidence": func(d *decision.Decision) { d.InteractionConfidence = math.NaN() },
		"interaction confidence > 1": func(d *decision.Decision) { d.InteractionConfidence = 1.5 },
		"negative interaction conf":  func(d *decision.Decision) { d.InteractionConfidence = -0.1 },
		"NaN interaction score":      func(d *decision.Decision) { d.InteractionScores = map[string]float64{"confirmation": math.NaN()} },
		"interaction score > 1": func(d *decision.Decision) {
			d.InteractionScores = map[string]float64{"confirmation": 1.5, "command": 0}
		},
		"NaN module confidence":        func(d *decision.Decision) { d.Module.Confidence = math.NaN() },
		"intent confidence > 1":        func(d *decision.Decision) { d.Intent.Confidence = 2 },
		"deterministic with a NaN too": func(d *decision.Decision) { *d = decision.Deterministic(*d); d.InteractionConfidence = math.NaN() },
	}
	for pn, pol := range pols {
		for cn, mutate := range cases {
			d := moduleDecision(decision.InteractionConfirmation, 0.95, map[string]float64{"confirmation": 0.97, "command": 0.03})
			mutate(&d)
			got, sel := pol.JudgeDecision(d)
			if got.Actionable() || sel.Outcome != decision.OutcomeInvalid || sel.Reason != decision.ReasonBadConfidence {
				t.Errorf("%s / %s: %+v", pn, cn, sel)
			}
		}
	}
}

// m-e: the interaction's own probability has to carry the bar.
func TestBackingScoresNeedTheInteractionsOwnProbabilityAtTheBar(t *testing.T) {
	dur, nar := decision.DurablePolicy(), decision.NarrowingPolicy()
	for name, scores := range map[string]map[string]float64{
		"a lone 0.01":               {"undo": 0.01},
		"top but 0.5 against 0.1":   {"undo": 0.5, "chat": 0.1},
		"0.85, runner-up 0.05":      {"undo": 0.85, "chat": 0.05},
		"top but below the bar 0.3": {"undo": 0.3, "chat": 0.05, "command": 0.05},
	} {
		d := moduleDecision(decision.InteractionUndo, 0.90, scores)
		for pn, pol := range map[string]decision.SelectionPolicy{"durable": dur, "narrowing": nar} {
			if got, sel := pol.JudgeDecision(d); got.Actionable() || sel.Reason != decision.ReasonInteractionLowConfidence {
				t.Errorf("%s / %s: %+v", name, pn, sel)
			}
		}
		if _, ok, _ := (decision.Chain{Providers: []decision.Provider{stub{name: "c", d: d, ok: true}}}).Decide(context.Background(), request()); ok {
			t.Errorf("%s: a policy-less chain accepted it", name)
		}
	}
	good := moduleDecision(decision.InteractionUndo, 0.90, map[string]float64{"undo": 0.9, "chat": 0.05})
	if got, sel := nar.JudgeDecision(good); !got.Actionable() || sel.Outcome != decision.OutcomeSelected {
		t.Fatalf("%+v", sel)
	}
}
