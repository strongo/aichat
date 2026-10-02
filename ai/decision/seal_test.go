package decision_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/strongo/aichat/ai/decision"
)

// These are the review probes of PR 19 round 4, written against the exported API
// only: the verdict and the provenance class cannot be forged from outside the
// package, and a side-effectful interaction needs positive, backed confidence
// with or without a policy.

type stub struct {
	name string
	d    decision.Decision
	ok   bool
	err  error
}

func (s stub) Name() string { return s.name }
func (s stub) Decide(context.Context, decision.Request) (decision.Decision, bool, error) {
	return s.d, s.ok, s.err
}

var tax = decision.Taxonomy{Modules: []decision.ModuleSpec{{Name: "m", Intents: []string{"i", "j"}}}}

func request() decision.Request { return decision.Request{Text: "hello", Taxonomy: tax} }

func answer(conf float64) decision.Decision {
	return decision.Decision{
		Module: decision.Scored{Value: "m", Confidence: conf}, Intent: decision.Scored{Value: "i", Confidence: conf},
		Interaction: decision.InteractionCommand,
	}
}

func chains(t *testing.T, p decision.Provider) map[string]decision.Chain {
	t.Helper()
	dur, nar := decision.DurablePolicy(), decision.NarrowingPolicy()
	optIn := decision.NarrowingPolicy()
	optIn.AcceptUncalibratedSideEffects = true
	return map[string]decision.Chain{
		"no policy":         {Providers: []decision.Provider{p}},
		"accept any":        {Providers: []decision.Provider{p}, MinConfidence: -1},
		"durable":           {Providers: []decision.Provider{p}, Policy: &dur},
		"narrowing":         {Providers: []decision.Provider{p}, Policy: &nar},
		"narrowing, opt-in": {Providers: []decision.Provider{p}, Policy: &optIn},
	}
}

// S2: unmarshalling an outcome must not make a decision actionable.
func TestActionableIsNotReadFromTheWire(t *testing.T) {
	for _, o := range []string{"deterministic", "selected", "several", "accepted", "floor"} {
		var d decision.Decision
		body := `{"module":{"value":"m","confidence":1},"intent":{"value":"i","confidence":1},"interaction":"command","outcome":"` + o + `"}`
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			t.Fatal(err)
		}
		if d.Actionable() || d.Provenance() != decision.ProvenanceSelfReported {
			t.Fatalf("outcome %q read off the wire: actionable=%v provenance=%s", o, d.Actionable(), d.Provenance())
		}
	}
}

// S2: a provider that writes an Outcome into its answer is not believed: the
// chain judges, whatever the answer claims.
func TestChainIgnoresAProvidersClaimedOutcome(t *testing.T) {
	for _, o := range []decision.Outcome{decision.OutcomeDeterministic, decision.OutcomeSelected, decision.OutcomeAccepted, decision.OutcomeFloor, decision.OutcomeSeveral} {
		low := answer(0.2)
		low.Outcome = o
		for name, c := range chains(t, stub{name: "liar", d: low, ok: true}) {
			if name == "accept any" {
				continue
			}
			if d, ok, tr := c.Decide(context.Background(), request()); ok || d.Actionable() {
				t.Fatalf("%s: a provider's claimed %q at confidence 0.2 was believed: %+v %+v", name, o, d, tr)
			}
		}
		high := answer(0.9)
		high.Outcome = o
		d, ok, tr := decision.Chain{Providers: []decision.Provider{stub{name: "liar", d: high, ok: true}}}.Decide(context.Background(), request())
		if !ok || d.Outcome != decision.OutcomeFloor || tr.Outcome != decision.OutcomeFloor || d.Provenance() != decision.ProvenanceSelfReported {
			t.Fatalf("a policy-less chain stamps its own floor over %q: %+v %+v", o, d, tr)
		}
	}
}

// S1: the probe: a remote engine's calibrated flag alone, on a module-less undo
// at confidence 0, must never be acted on, with or without a policy.
func TestSideEffectfulCalibratedClaimWithoutEvidenceIsRefused(t *testing.T) {
	for _, in := range []decision.Interaction{decision.InteractionConfirmation, decision.InteractionRejection, decision.InteractionCorrection, decision.InteractionCancellation, decision.InteractionUndo} {
		claim := decision.Decision{Interaction: in, Calibrated: true}
		for name, c := range chains(t, stub{name: "cloud", d: claim, ok: true}) {
			if d, ok, tr := c.Decide(context.Background(), request()); ok || d.Actionable() {
				t.Fatalf("%s %s: calibrated claim at confidence 0 was accepted: %+v %+v", name, in, d, tr)
			}
			keep := c
			keep.KeepNonSelected = true
			if d, _, _ := keep.Decide(context.Background(), request()); d.Actionable() {
				t.Fatalf("%s %s keep: %+v", name, in, d)
			}
		}
	}
}

// S1: the same bar everywhere: a side-effectful interaction needs a positive
// interaction confidence at the durable bar, with or without a policy.
func TestSideEffectfulNeedsPositiveInteractionConfidenceAtTheDurableBar(t *testing.T) {
	optIn := decision.NarrowingPolicy()
	optIn.AcceptUncalibratedSideEffects = true
	for ic, want := range map[float64]bool{0: false, 0.5: false, 0.89: false, 0.9: true, 1: true} {
		d := decision.Decision{Interaction: decision.InteractionConfirmation, InteractionConfidence: ic}
		for _, c := range []struct {
			name  string
			chain decision.Chain
		}{
			{"no policy", decision.Chain{}},
			{"accept any", decision.Chain{MinConfidence: -1}},
			{"narrowing, opt-in", decision.Chain{Policy: &optIn}},
		} {
			c.chain.Providers = []decision.Provider{stub{name: "llm", d: d, ok: true}}
			if _, ok, _ := c.chain.Decide(context.Background(), request()); ok != want {
				t.Errorf("%s at interaction confidence %v: ok=%v want %v", c.name, ic, ok, want)
			}
		}
	}
}

// moduleDecision is what a calibrated engine returns for "yes": a module and an
// intent with calibrated scores, and an interaction with its own probabilities.
func moduleDecision(in decision.Interaction, ic float64, scores map[string]float64) decision.Decision {
	return decision.Decision{
		Module: decision.Scored{Value: "m", Confidence: 0.97}, Intent: decision.Scored{Value: "i", Confidence: 0.97},
		Interaction: in, InteractionConfidence: ic, InteractionScores: scores,
		Scores: map[string]float64{"m/i": 0.97, "m/j": 0.03}, Calibrated: true,
	}
}

func TestSideEffectfulCalibratedClaimNeedsItsInteractionScores(t *testing.T) {
	nar := decision.NarrowingPolicy()
	backed := map[string]float64{"confirmation": 0.97, "command": 0.03}
	for _, tc := range []struct {
		name    string
		d       decision.Decision
		outcome decision.Outcome
		reason  string
	}{
		{"backed", moduleDecision(decision.InteractionConfirmation, 0.95, backed), decision.OutcomeSelected, ""},
		{"unbacked: a self-report, and narrowing has no side-effect opt-in", moduleDecision(decision.InteractionConfirmation, 0.95, nil), decision.OutcomeUnscored, decision.ReasonSideEffectUncalibrated},
		{"contradicted", moduleDecision(decision.InteractionConfirmation, 0.95, map[string]float64{"confirmation": 0.2, "command": 0.8}), decision.OutcomeInvalid, decision.ReasonInteractionNotTop},
		{"missing from its scores", moduleDecision(decision.InteractionConfirmation, 0.95, map[string]float64{"command": 0.9}), decision.OutcomeInvalid, decision.ReasonInteractionNotTop},
		{"narrow gap", moduleDecision(decision.InteractionConfirmation, 0.95, map[string]float64{"confirmation": 0.95, "command": 0.9}), decision.OutcomeUncertain, decision.ReasonInteractionNarrowGap},
		{"low confidence", moduleDecision(decision.InteractionConfirmation, 0.6, backed), decision.OutcomeUncertain, decision.ReasonInteractionLowConfidence},
		{"zero confidence", moduleDecision(decision.InteractionConfirmation, 0, backed), decision.OutcomeUncertain, decision.ReasonInteractionLowConfidence},
		{"not side-effectful: no interaction evidence needed", moduleDecision(decision.InteractionCommand, 0, nil), decision.OutcomeSelected, ""},
	} {
		sel := nar.EvaluateDecision(tc.d)
		if sel.Outcome != tc.outcome || sel.Reason != tc.reason {
			t.Errorf("%s: %+v", tc.name, sel)
		}
		got, sel2 := nar.JudgeDecision(tc.d)
		if got.Outcome != tc.outcome || got.Actionable() != sel2.Actionable() {
			t.Errorf("%s: JudgeDecision %+v", tc.name, got)
		}
	}
	// The policy-less chain applies the same evidence rules.
	for name, tc := range map[string]struct {
		d  decision.Decision
		ok bool
	}{
		"backed":         {moduleDecision(decision.InteractionConfirmation, 0.95, backed), true},
		"unbacked":       {moduleDecision(decision.InteractionConfirmation, 0.95, nil), true}, // a self-report at the durable bar
		"contradicted":   {moduleDecision(decision.InteractionConfirmation, 0.95, map[string]float64{"confirmation": 0.2, "command": 0.8}), false},
		"narrow gap":     {moduleDecision(decision.InteractionConfirmation, 0.95, map[string]float64{"confirmation": 0.95, "command": 0.9}), false},
		"zero":           {moduleDecision(decision.InteractionConfirmation, 0, backed), false},
		"out of [0,1]":   {moduleDecision(decision.InteractionConfirmation, 0.95, map[string]float64{"confirmation": 1.5}), false},
		"unbacked low":   {moduleDecision(decision.InteractionConfirmation, 0.8, nil), false},
		"command at all": {moduleDecision(decision.InteractionCommand, 0, nil), true},
	} {
		d, ok, tr := decision.Chain{Providers: []decision.Provider{stub{name: "cloud", d: tc.d, ok: true}}}.Decide(context.Background(), request())
		if ok != tc.ok || (ok && !d.Actionable()) {
			t.Errorf("policy-less %s: ok=%v %+v", name, ok, tr)
		}
	}
	// An uncalibrated engine's interaction scores are proposals, never evidence.
	un := moduleDecision(decision.InteractionConfirmation, 0.95, backed)
	un.Calibrated, un.Scores = false, nil
	if sel := nar.EvaluateDecision(un); sel.Actionable() {
		t.Fatalf("%+v", sel)
	}
}
