package decision

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
)

// countingProvider counts calls to a constant answer.
type countingProvider struct {
	name string
	d    Decision
	ok   bool
	err  error
	n    atomic.Int32
}

func (p *countingProvider) Name() string { return p.name }
func (p *countingProvider) Decide(context.Context, Request) (Decision, bool, error) {
	p.n.Add(1)
	return p.d, p.ok, p.err
}

// rule is what rules.Provider returns for a matched rule.
func rule(module, intent string) Decision {
	return Deterministic(Decision{
		Module: Scored{Value: module, Confidence: 1}, Intent: Scored{Value: intent, Confidence: 1},
		Interaction: InteractionCommand,
	})
}

func llm(conf float64) *countingProvider {
	return &countingProvider{name: "llm", d: decided("calendar", "show", conf), ok: true}
}

func TestProvenance_ClassesAndDeterministicConstructor(t *testing.T) {
	if got := (Decision{}).Provenance(); got != ProvenanceSelfReported {
		t.Fatalf("the zero decision is the weakest class, got %q", got)
	}
	if got := (Decision{Calibrated: true}).Provenance(); got != ProvenanceCalibrated {
		t.Fatalf("calibrated: %q", got)
	}
	d := Deterministic(Decision{Calibrated: true, Scores: map[string]float64{"a": 1}, Model: "jev-1", Interaction: InteractionChat})
	if d.Provenance() != ProvenanceDeterministic || d.Outcome != OutcomeDeterministic || d.Calibrated || d.Scores != nil || d.Model != "" || !d.Actionable() {
		t.Fatalf("Deterministic must clear every model claim: %+v", d)
	}
	if d.Interaction != InteractionChat {
		t.Fatalf("Deterministic must keep the answer: %+v", d)
	}
}

// The class lives in an unexported field: no JSON, whether from a cloud server, a
// stored decision or an LLM's structured output, can set it.
func TestProvenance_DeterministicCannotBeSetFromJSON(t *testing.T) {
	var d Decision
	body := `{"module":{"value":"calendar","confidence":1},"intent":{"value":"show","confidence":1},"interaction":"command",
	  "provenance":"deterministic","deterministic":true,"outcome":"deterministic"}`
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if d.Provenance() != ProvenanceSelfReported {
		t.Fatalf("JSON made a decision %q", d.Provenance())
	}
	// A deterministic decision serialises without the claim, and what comes back is
	// self-reported (its Outcome survives for the record only).
	b, err := json.Marshal(rule("calendar", "show"))
	if err != nil || strings.Contains(string(b), "provenance") || strings.Contains(string(b), "eterministic\":true") {
		t.Fatalf("%s %v", b, err)
	}
}

// M1: rules ahead of an engine under a durable policy.
func TestChain_DeterministicRuleIsActionableUnderDurablePolicyWithoutTheBackup(t *testing.T) {
	for name, pol := range map[string]SelectionPolicy{
		"durable": DurablePolicy(), "narrowing": NarrowingPolicy(),
		"durable accepting 1.0": func() SelectionPolicy { p := DurablePolicy(); p.AcceptUncalibratedAt = 1; return p }(),
	} {
		t.Run(name, func(t *testing.T) {
			rules := &countingProvider{name: "rules", d: rule("calendar", "show"), ok: true}
			backup := llm(1)
			d, ok, tr := Chain{Providers: []Provider{rules, backup}, Policy: &pol}.Decide(context.Background(), req())
			if !ok || !d.Actionable() || d.Outcome != OutcomeDeterministic || backup.n.Load() != 0 {
				t.Fatalf("ok=%v d=%+v backupCalls=%d tr=%+v", ok, d, backup.n.Load(), tr)
			}
			if tr.DecidedBy != "rules" || tr.Provenance != ProvenanceDeterministic || tr.Outcome != OutcomeDeterministic || tr.Calibrated {
				t.Fatalf("tr=%+v", tr)
			}
			if got := tr.Attempts[0]; got.Outcome != AttemptDecided || got.Detail != "deterministic: deterministic_rule" {
				t.Fatalf("attempt=%+v", got)
			}
		})
	}
}

// A module-less "yes" from a rule is deterministic too: no module confidence, no
// interaction confidence, still actionable (the side-effect gate is for models).
func TestChain_DeterministicSideEffectfulRuleNeedsNoConfidence(t *testing.T) {
	pol := DurablePolicy()
	yes := &countingProvider{name: "rules", d: Deterministic(Decision{Interaction: InteractionConfirmation}), ok: true}
	d, ok, _ := Chain{Providers: []Provider{yes}, Policy: &pol}.Decide(context.Background(), req())
	if !ok || d.Outcome != OutcomeDeterministic {
		t.Fatalf("ok=%v d=%+v", ok, d)
	}
}

func TestEvaluateDecision_DeterministicNeedsAValidPolicyAndIsNotGovernedByTheBar(t *testing.T) {
	if sel := (SelectionPolicy{}).EvaluateDecision(rule("calendar", "show")); sel.Actionable() || sel.Reason != ReasonInvalidPolicy {
		t.Fatalf("an invalid policy selects nothing: %+v", sel)
	}
	hi := DurablePolicy()
	hi.AcceptUncalibratedAt = 1
	low := rule("calendar", "show")
	low.Module.Confidence, low.Intent.Confidence = 0.1, 0.1
	if sel := hi.EvaluateDecision(low); sel.Outcome != OutcomeDeterministic || len(sel.Picks) != 1 || sel.Picks[0] != "calendar/show" {
		t.Fatalf("%+v", sel)
	}
}

// Nothing that merely produces data can claim to be deterministic.
func TestChain_NothingElseCanClaimDeterministic(t *testing.T) {
	dur := DurablePolicy()
	dur1 := DurablePolicy()
	dur1.AcceptUncalibratedAt = 1
	claims := map[string]Decision{
		"outcome only": func() Decision { d := decided("calendar", "show", 1); d.Outcome = OutcomeDeterministic; return d }(),
		"calibrated with a one-option Scores": func() Decision {
			d := decided("calendar", "show", 1)
			d.Calibrated, d.Scores = true, map[string]float64{"anything": 1}
			return d
		}(),
	}
	for name, d := range claims {
		t.Run(name, func(t *testing.T) {
			got, ok, tr := Chain{Providers: []Provider{&countingProvider{name: "x", d: d, ok: true}}, Policy: &dur}.Decide(context.Background(), req())
			if ok || got.Actionable() {
				t.Fatalf("durable accepted a claim: ok=%v d=%+v tr=%+v", ok, got, tr)
			}
		})
	}
	// Policy-less: the claimed outcome is dropped and the chain's own floor verdict
	// stamped; the provenance stays self-reported.
	d := decided("calendar", "show", 1)
	d.Outcome = OutcomeDeterministic
	got, ok, tr := Chain{Providers: []Provider{&countingProvider{name: "x", d: d, ok: true}}}.Decide(context.Background(), req())
	if !ok || got.Outcome != OutcomeFloor || tr.Provenance != ProvenanceSelfReported {
		t.Fatalf("ok=%v d=%+v tr=%+v", ok, got, tr)
	}
	// An LLM self-reporting 1.0 is only ever `accepted` at an explicit bar, never deterministic.
	got, ok, tr = Chain{Providers: []Provider{llm(1)}, Policy: &dur1}.Decide(context.Background(), req())
	if !ok || got.Outcome != OutcomeAccepted || tr.Provenance != ProvenanceSelfReported {
		t.Fatalf("ok=%v d=%+v tr=%+v", ok, got, tr)
	}
}

func TestSelectionPolicy_AtLeastCombinesTheSideEffectOptIn(t *testing.T) {
	a, b := NarrowingPolicy(), NarrowingPolicy()
	a.AcceptUncalibratedSideEffects, b.AcceptUncalibratedSideEffects = true, true
	if !a.AtLeast(b).AcceptUncalibratedSideEffects {
		t.Fatal("both opt in: opted in")
	}
	b.AcceptUncalibratedSideEffects = false
	if a.AtLeast(b).AcceptUncalibratedSideEffects || b.AtLeast(a).AcceptUncalibratedSideEffects {
		t.Fatal("either declines: declined")
	}
}

// ---- M2: no actionability by omission ----

func TestChain_WithoutAPolicyStampsAnExplicitOutcome(t *testing.T) {
	d, ok, tr := Chain{Providers: []Provider{llm(0.9)}}.Decide(context.Background(), req())
	if !ok || d.Outcome != OutcomeFloor || !d.Actionable() || tr.Outcome != OutcomeFloor {
		t.Fatalf("ok=%v d=%+v tr=%+v", ok, d, tr)
	}
	// Below the floor: not decided at all.
	if _, ok, _ := (Chain{Providers: []Provider{llm(0.05)}}).Decide(context.Background(), req()); ok {
		t.Fatal("below the floor")
	}
	// An engine's own actionable verdict (an engine built with a policy) is kept.
	e := decided("calendar", "show", 0.9)
	e.Outcome = OutcomeSelected
	d, ok, _ = Chain{Providers: []Provider{&countingProvider{name: "jev", d: e, ok: true}}}.Decide(context.Background(), req())
	if !ok || d.Outcome != OutcomeSelected {
		t.Fatalf("ok=%v d=%+v", ok, d)
	}
	// A deterministic answer says so.
	d, ok, tr = Chain{Providers: []Provider{&countingProvider{name: "rules", d: rule("calendar", "show"), ok: true}}}.Decide(context.Background(), req())
	if !ok || d.Outcome != OutcomeDeterministic || tr.Provenance != ProvenanceDeterministic {
		t.Fatalf("ok=%v d=%+v tr=%+v", ok, d, tr)
	}
}

func TestProvider_UsedDirectlyIsNotActionableUntilJudged(t *testing.T) {
	// A decision straight from a provider carries no verdict, whatever its confidence.
	d, ok, err := llm(0.05).Decide(context.Background(), req())
	if err != nil || !ok || d.Actionable() {
		t.Fatalf("d=%+v ok=%v err=%v", d, ok, err)
	}
}

// ---- M3: side-effectful interactions ----

func TestSideEffectful(t *testing.T) {
	for in, want := range map[Interaction]bool{
		InteractionConfirmation: true, InteractionRejection: true, InteractionCorrection: true,
		InteractionCancellation: true, InteractionUndo: true,
		InteractionCommand: false, InteractionQuestion: false, InteractionContinuation: false, InteractionChat: false,
	} {
		if SideEffectful(in) != want {
			t.Errorf("%s: %v", in, !want)
		}
	}
}

func uncalibrated(in Interaction, module string, conf, interaction float64) Decision {
	d := Decision{Interaction: in, InteractionConfidence: interaction}
	if module != "" {
		d.Module = Scored{Value: module, Confidence: conf}
		d.Intent = Scored{Value: "show", Confidence: conf}
	}
	return d
}

func TestPolicy_UncalibratedSideEffectsNeedTheirOwnOptIn(t *testing.T) {
	nar := NarrowingPolicy()
	optIn := NarrowingPolicy()
	optIn.AcceptUncalibratedSideEffects = true
	for _, in := range []Interaction{InteractionConfirmation, InteractionRejection, InteractionCorrection, InteractionCancellation, InteractionUndo} {
		t.Run(string(in), func(t *testing.T) {
			// Not even a module-less one at confidence 0: the finding.
			for _, d := range []Decision{uncalibrated(in, "", 0, 0), uncalibrated(in, "calendar", 0.99, 0.99), uncalibrated(in, "", 0, 1)} {
				if sel := nar.EvaluateDecision(d); sel.Actionable() || sel.Reason != ReasonSideEffectUncalibrated {
					t.Fatalf("narrowing accepted %+v: %+v", d, sel)
				}
			}
			// Opted in: still never at confidence 0 or below the durable bar...
			module, conf := "", 0.0
			if !moduleOptionalInteractions[in] { // a correction names what it corrects
				module, conf = "calendar", 0.9
			}
			for _, ic := range []float64{0, 0.5, 0.89} {
				if sel := optIn.EvaluateDecision(uncalibrated(in, module, conf, ic)); sel.Actionable() || sel.Reason != ReasonInteractionLowConfidence {
					t.Fatalf("interaction %v: %+v", ic, sel)
				}
			}
			// ...but at a confident interaction confidence it is accepted, as accepted.
			if sel := optIn.EvaluateDecision(uncalibrated(in, module, conf, 0.95)); sel.Outcome != OutcomeAccepted {
				t.Fatalf("%+v", sel)
			}
			// The module/intent bar still applies on top.
			if sel := optIn.EvaluateDecision(uncalibrated(in, "calendar", 0.2, 0.95)); sel.Actionable() || sel.Reason != ReasonLowConfidence {
				t.Fatalf("%+v", sel)
			}
		})
	}
	// An opt-in with a bar above the durable one raises the interaction bar too.
	strict := optIn
	strict.AcceptUncalibratedAt = 0.97
	if sel := strict.EvaluateDecision(uncalibrated(InteractionUndo, "", 0, 0.95)); sel.Actionable() {
		t.Fatalf("%+v", sel)
	}
	// Other interactions are unaffected, and may be module-less at confidence 0.
	for _, in := range []Interaction{InteractionChat} {
		if sel := nar.EvaluateDecision(uncalibrated(in, "", 0, 0)); sel.Outcome != OutcomeAccepted {
			t.Fatalf("%s: %+v", in, sel)
		}
	}
	// A durable policy never accepts an uncalibrated decision at all.
	dur := DurablePolicy()
	dur.AcceptUncalibratedSideEffects = true
	if sel := dur.EvaluateDecision(uncalibrated(InteractionConfirmation, "", 0, 1)); sel.Actionable() {
		t.Fatalf("%+v", sel)
	}
	// A calibrated decision without Scores is judged like an uncalibrated one.
	c := uncalibrated(InteractionConfirmation, "", 0, 0)
	c.Calibrated = true
	if sel := nar.EvaluateDecision(c); sel.Actionable() || sel.Reason != ReasonSideEffectUncalibrated {
		t.Fatalf("%+v", sel)
	}
}

// The interaction the calibrated engine abstained on must not reach a weaker gate:
// [calibrated engine that abstains, LLM that says "yes" at confidence 0].
func TestChain_CalibratedAbstentionDoesNotFallToAWeakerGate(t *testing.T) {
	jev := &countingProvider{name: "jev", ok: false}
	yes := &countingProvider{name: "llm", d: Decision{Interaction: InteractionConfirmation}, ok: true}
	nar := NarrowingPolicy()
	if d, ok, tr := (Chain{Providers: []Provider{jev, yes}, Policy: &nar}).Decide(context.Background(), req()); ok {
		t.Fatalf("narrowing accepted an uncalibrated module-less confirmation at confidence 0: %+v %+v", d, tr)
	}
	// With KeepNonSelected the verdict is visible, and not actionable.
	d, ok, tr := Chain{Providers: []Provider{jev, yes}, Policy: &nar, KeepNonSelected: true}.Decide(context.Background(), req())
	if !ok || d.Actionable() || tr.Attempts[1].Detail != "unscored: side_effect_uncalibrated" {
		t.Fatalf("ok=%v d=%+v tr=%+v", ok, d, tr)
	}
	// The same without a policy: the module-optional exemption no longer lets a
	// side-effectful uncalibrated decision through at confidence 0.
	if d, ok, tr := (Chain{Providers: []Provider{jev, yes}}).Decide(context.Background(), req()); ok {
		t.Fatalf("policy-less chain accepted it: %+v %+v", d, tr)
	}
	for name, c := range map[string]Chain{"floor": {}, "accept any": {MinConfidence: -1}} {
		c.Providers = []Provider{yes}
		if _, ok, tr := c.Decide(context.Background(), req()); ok || tr.Attempts[0].Outcome != AttemptLowConfidence || tr.Attempts[0].Detail != "interaction=0.00" {
			t.Fatalf("%s: ok=%v tr=%+v", name, ok, tr)
		}
	}
	// With an interaction confidence at the floor it is accepted, below it not.
	for ic, want := range map[float64]bool{0.8: true, 0.5: false} {
		d := Decision{Interaction: InteractionConfirmation, InteractionConfidence: ic}
		got, ok, _ := Chain{Providers: []Provider{&countingProvider{name: "llm", d: d, ok: true}}}.Decide(context.Background(), req())
		if ok != want || (ok && got.Outcome != OutcomeFloor) {
			t.Fatalf("interaction %v: ok=%v %+v", ic, ok, got)
		}
	}
	// Accept-any with a stated interaction confidence passes; calibrated and
	// module-ful decisions are not subject to this gate.
	d = Decision{Interaction: InteractionConfirmation, InteractionConfidence: 0.1}
	if _, ok, _ := (Chain{Providers: []Provider{&countingProvider{name: "x", d: d, ok: true}}, MinConfidence: -1}).Decide(context.Background(), req()); !ok {
		t.Fatal("accept-any with an interaction confidence")
	}
	d = Decision{Interaction: InteractionConfirmation, Calibrated: true}
	if _, ok, _ := (Chain{Providers: []Provider{&countingProvider{name: "x", d: d, ok: true}}}).Decide(context.Background(), req()); !ok {
		t.Fatal("a calibrated decision is gated by its own engine")
	}
	d = uncalibrated(InteractionCancellation, "calendar", 0.9, 0)
	if _, ok, _ := (Chain{Providers: []Provider{&countingProvider{name: "x", d: d, ok: true}}}).Decide(context.Background(), req()); !ok {
		t.Fatal("a module-ful decision keeps its module/intent bar")
	}
}

func TestValidate_InteractionConfidenceRange(t *testing.T) {
	for _, c := range []float64{-0.1, 1.1, math.NaN()} {
		d := decided("calendar", "show", 0.9)
		d.InteractionConfidence = c
		if err := Validate(d, taxonomy()); err == nil {
			t.Errorf("%v accepted", c)
		}
	}
	d := decided("calendar", "show", math.NaN())
	if err := Validate(d, taxonomy()); err == nil {
		t.Error("NaN module confidence accepted")
	}
}

// ---- m7: judge the decision's own option ----

func TestEvaluateDecision_JudgesTheDecisionsOwnOption(t *testing.T) {
	dur := DurablePolicy()
	mk := func(scores map[string]float64) Decision {
		d := decided("calendar", "show", 0.95)
		d.Calibrated, d.Scores = true, scores
		return d
	}
	// The reviewer's case: intent=show, but the Scores favour another intent.
	sel := dur.EvaluateDecision(mk(map[string]float64{"calendar/create": 0.9, "calendar/show": 0.1}))
	if sel.Outcome != OutcomeInvalid || sel.Reason != ReasonDecisionNotTop || sel.Actionable() {
		t.Fatalf("%+v", sel)
	}
	for name, tc := range map[string]struct {
		scores map[string]float64
		reason string
	}{
		"own option missing": {map[string]float64{"calendar/create": 0.97, "x": 0.03}, ReasonDecisionNotScored},
		"NaN":                {map[string]float64{"calendar/show": math.NaN()}, ReasonBadScores},
		"out of range":       {map[string]float64{"calendar/show": 1.5}, ReasonBadScores},
	} {
		if sel := dur.EvaluateDecision(mk(tc.scores)); sel.Outcome != OutcomeInvalid || sel.Reason != tc.reason {
			t.Errorf("%s: %+v", name, sel)
		}
	}
	// Consistent decisions are judged as before.
	if sel := dur.EvaluateDecision(mk(map[string]float64{"calendar/show": 0.97, "calendar/create": 0.03})); sel.Outcome != OutcomeSelected || sel.Picks[0] != "calendar/show" {
		t.Fatalf("%+v", sel)
	}
	// A tie reads as a zero gap: uncertain, whichever id sorts first.
	for _, own := range []string{"calendar/a", "calendar/z"} {
		d := decided("calendar", own[len("calendar/"):], 0.95)
		d.Calibrated, d.Scores = true, map[string]float64{"calendar/a": 0.5, "calendar/z": 0.5}
		if sel := dur.EvaluateDecision(d); sel.Outcome != OutcomeUncertain || sel.Reason != ReasonNarrowGap {
			t.Errorf("tie %s: %+v", own, sel)
		}
	}
	// Option ids of a choose:<role> request are the bare intent; a module with no
	// intents is the bare module.
	d := decided("choose:table", "orders", 0.95)
	d.Calibrated, d.Scores = true, map[string]float64{"orders": 0.97, "items": 0.03}
	if sel := dur.EvaluateDecision(d); sel.Outcome != OutcomeSelected {
		t.Fatalf("bare intent: %+v", sel)
	}
	d = decided("calendar", "", 0.95)
	d.Calibrated, d.Scores = true, map[string]float64{"calendar": 0.97, "other": 0.03}
	if sel := dur.EvaluateDecision(d); sel.Outcome != OutcomeSelected {
		t.Fatalf("bare module: %+v", sel)
	}
	// A decision with neither module nor intent has no key at all.
	e := Decision{Interaction: InteractionChat, Calibrated: true, Scores: map[string]float64{"x": 1}}
	if sel := dur.EvaluateDecision(e); sel.Outcome != OutcomeInvalid || sel.Reason != ReasonDecisionNotScored {
		t.Fatalf("%+v", sel)
	}
}

func TestChain_ContradictoryScoresAreInvalidAndRecordedEvenWithKeepNonSelected(t *testing.T) {
	dur := DurablePolicy()
	d := decided("calendar", "show", 0.95)
	d.Calibrated, d.Scores = true, map[string]float64{"calendar/create": 0.9, "calendar/show": 0.1}
	for _, keep := range []bool{false, true} {
		got, ok, tr := Chain{Providers: []Provider{&countingProvider{name: "jev", d: d, ok: true}}, Policy: &dur, KeepNonSelected: keep}.Decide(context.Background(), req())
		if ok || got.Actionable() || tr.Attempts[0].Outcome != AttemptInvalid || tr.Attempts[0].Detail != "invalid: decision_not_top" {
			t.Fatalf("keep=%v ok=%v d=%+v tr=%+v", keep, ok, got, tr)
		}
	}
}

// ---- m5: a chain stops at quota and misconfiguration ----

func TestChain_StopsAtQuotaAndMisconfigurationByDefault(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		outcome  string
		sentinel error
	}{
		{"quota", errorsJoin(ErrQuota), AttemptQuota, ErrQuota},
		{"misconfigured", errorsJoin(ErrMisconfigured), AttemptMisconfigured, ErrMisconfigured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, c := range map[string]Chain{"default": {}, "explicit on": {StopOnQuota: StopOn, StopOnMisconfigured: StopOn}} {
				paid := llm(0.9)
				c.Providers = []Provider{&countingProvider{name: "jev", err: tc.err}, paid}
				d, ok, tr := c.Decide(context.Background(), req())
				if ok || d.Actionable() || paid.n.Load() != 0 {
					t.Fatalf("%s: the paid backup was called: ok=%v calls=%d tr=%+v", name, ok, paid.n.Load(), tr)
				}
				err := tr.Err()
				if tr.StoppedBy != tc.outcome || !errors.Is(err, tc.sentinel) || !strings.Contains(err.Error(), "jev") || len(tr.Attempts) != 1 {
					t.Fatalf("%s: tr=%+v err=%v", name, tr, err)
				}
			}
		})
	}
}

func errorsJoin(sentinel error) error { return errors.Join(errors.New("jev said so"), sentinel) }

func TestChain_StopOffLetsTheNextProviderAnswerAndEachSwitchIsIndependent(t *testing.T) {
	quota, mis := errorsJoin(ErrQuota), errorsJoin(ErrMisconfigured)
	cases := []struct {
		c       Chain
		err     error
		wantRun bool
	}{
		{Chain{StopOnQuota: StopOff}, quota, true},
		{Chain{StopOnQuota: StopOff}, mis, false},
		{Chain{StopOnMisconfigured: StopOff}, mis, true},
		{Chain{StopOnMisconfigured: StopOff}, quota, false},
	}
	for i, tc := range cases {
		paid := llm(0.9)
		tc.c.Providers = []Provider{&countingProvider{name: "jev", err: tc.err}, paid}
		d, ok, tr := tc.c.Decide(context.Background(), req())
		if tc.wantRun {
			if !ok || !d.Actionable() || tr.StoppedBy != "" || tr.Err() != nil || tr.DecidedBy != "llm" {
				t.Fatalf("case %d: ok=%v tr=%+v", i, ok, tr)
			}
		} else if ok || tr.StoppedBy == "" {
			t.Fatalf("case %d: ok=%v tr=%+v", i, ok, tr)
		}
	}
}

func TestTrace_Err(t *testing.T) {
	if (Trace{}).Err() != nil || (Trace{StoppedBy: AttemptTimeout}).Err() != nil {
		t.Fatal("only quota and misconfigured stop a chain")
	}
	if err := (Trace{StoppedBy: AttemptQuota}).Err(); !errors.Is(err, ErrQuota) {
		t.Fatalf("no attempt to name: %v", err)
	}
	tr := Trace{StoppedBy: AttemptMisconfigured, Attempts: []Attempt{{Provider: "a", Outcome: AttemptError}, {Provider: "b", Outcome: AttemptMisconfigured, Detail: "wrong base URL"}}}
	if err := tr.Err(); !errors.Is(err, ErrMisconfigured) || !strings.Contains(err.Error(), "b") || !strings.Contains(err.Error(), "wrong base URL") {
		t.Fatalf("%v", err)
	}
	var s Stop
	if !s.on() || !StopOn.on() || StopOff.on() {
		t.Fatal("the zero Stop is ON")
	}
}
