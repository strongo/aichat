package decision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPolicy_AcceptUncalibratedAtDefaultsAndValidation(t *testing.T) {
	if p := NarrowingPolicy(); p.AcceptUncalibratedAt != NarrowingAcceptUncalibratedAt || NarrowingAcceptUncalibratedAt != 0.70 {
		t.Fatalf("narrowing = %v", p.AcceptUncalibratedAt)
	}
	if p := DurablePolicy(); p.AcceptUncalibratedAt != 0 || DurableAcceptUncalibratedAt != 0 {
		t.Fatalf("durable must never act on an uncalibrated decision: %v", p.AcceptUncalibratedAt)
	}
	for _, v := range []float64{-0.1, 1.1} {
		p := DurablePolicy()
		p.AcceptUncalibratedAt = v
		if p.Validate() == nil {
			t.Errorf("%v accepted", v)
		}
	}
}

func TestPolicy_EvaluateDecisionUncalibrated(t *testing.T) {
	nar, dur := NarrowingPolicy(), DurablePolicy()
	d := decided("calendar", "show", 0.9)
	sel := nar.EvaluateDecision(d)
	if sel.Outcome != OutcomeAccepted || sel.Reason != ReasonAcceptedUncalibrated || !sel.Actionable() || len(sel.Picks) != 1 || sel.Picks[0] != "calendar/show" || len(sel.Proposals) != 0 {
		t.Fatalf("accepted: %+v", sel)
	}
	// Module-only decision names the module; a module-optional interaction is
	// exempt from the module confidence.
	m := decided("calendar", "", 0.9)
	if sel := nar.EvaluateDecision(m); sel.Picks[0] != "calendar" {
		t.Fatalf("module only: %+v", sel)
	}
	// A module-optional, non-side-effectful interaction (chat) is exempt from the
	// module confidence; a side-effectful one is not accepted at all (see
	// TestPolicy_UncalibratedSideEffectsNeedTheirOwnOptIn).
	chat := Decision{Interaction: InteractionChat, Module: Scored{}, Intent: Scored{}}
	if sel := nar.EvaluateDecision(chat); sel.Outcome != OutcomeAccepted {
		t.Fatalf("module-optional: %+v", sel)
	}
	// Durable: unscored, not actionable, whatever the confidence.
	if sel := dur.EvaluateDecision(decided("calendar", "show", 1)); sel.Outcome != OutcomeUnscored || sel.Reason != ReasonNotCalibrated || sel.Actionable() || len(sel.Picks) != 0 {
		t.Fatalf("durable: %+v", sel)
	}
	// Below the bar.
	if sel := nar.EvaluateDecision(decided("calendar", "show", 0.5)); sel.Outcome != OutcomeUnscored || sel.Reason != ReasonLowConfidence {
		t.Fatalf("low: %+v", sel)
	}
	// An invalid policy selects nothing, even for an uncalibrated decision.
	if sel := (SelectionPolicy{}).EvaluateDecision(d); sel.Outcome != OutcomeUncertain || sel.Reason != ReasonInvalidPolicy || sel.Actionable() {
		t.Fatalf("invalid: %+v", sel)
	}
}

// Decision.Actionable and Selection.Actionable are one rule: for every decision
// a policy judges, the Chain's decision and the policy's selection agree.
func TestDecisionAndSelectionAgreeOnActionable(t *testing.T) {
	nar, dur := NarrowingPolicy(), DurablePolicy()
	cases := []Decision{
		scoredDecision(0.9, true, map[string]float64{"calendar/show": 0.9, "calendar/create": 0.1}),
		scoredDecision(0.07, true, map[string]float64{"calendar/show": 0.38, "calendar/create": 0.35, "x": 0.27}),
		scoredDecision(0.9, false, map[string]float64{"calendar/show": 0.9}),
		decided("calendar", "show", 0.72),
		decided("calendar", "show", 0.4),
	}
	for i, d := range cases {
		for _, pol := range []SelectionPolicy{nar, dur} {
			got, ok, _ := Chain{Providers: []Provider{constProvider("p", d)}, Policy: &pol, KeepNonSelected: true}.Decide(t.Context(), req())
			if !ok {
				t.Fatalf("case %d: kept answer missing", i)
			}
			if sel := pol.EvaluateDecision(d); got.Actionable() != sel.Actionable() || got.Outcome != sel.Outcome {
				t.Errorf("case %d under %s: decision %q/%v, selection %q/%v", i, pol.Name, got.Outcome, got.Actionable(), sel.Outcome, sel.Actionable())
			}
		}
	}
}

func TestSelectionPolicy_AtLeast(t *testing.T) {
	nar, dur := NarrowingPolicy(), DurablePolicy()
	s := nar.AtLeast(dur)
	if s.MinConfidence != dur.MinConfidence || s.MinGap != dur.MinGap || s.MinProbability != dur.MinProbability ||
		s.StrongProbability != dur.StrongProbability || s.PotentialProbability != dur.PotentialProbability ||
		s.AcceptUncalibratedAt != 0 || s.Name != "narrowing+strict" || s.Validate() != nil {
		t.Fatalf("%+v", s)
	}
	// Symmetric, and a custom loose policy cannot loosen the other.
	if r := dur.AtLeast(nar); r.MinConfidence != dur.MinConfidence || r.AcceptUncalibratedAt != 0 {
		t.Fatalf("%+v", r)
	}
	a, b := nar, nar
	a.AcceptUncalibratedAt, b.AcceptUncalibratedAt = 0.8, 0.9
	if r := a.AtLeast(b); r.AcceptUncalibratedAt != 0.9 {
		t.Fatalf("both opt in: %v", r.AcceptUncalibratedAt)
	}
	// MaxPicks: the smaller non-zero cap.
	a.MaxPicks, b.MaxPicks = 0, 3
	if r := a.AtLeast(b); r.MaxPicks != 3 {
		t.Fatalf("%d", r.MaxPicks)
	}
	a.MaxPicks, b.MaxPicks = 5, 0
	if r := a.AtLeast(b); r.MaxPicks != 5 {
		t.Fatalf("%d", r.MaxPicks)
	}
	a.MaxPicks, b.MaxPicks = 5, 3
	if r := a.AtLeast(b); r.MaxPicks != 3 {
		t.Fatalf("%d", r.MaxPicks)
	}
}

func TestOutcome_Actionable(t *testing.T) {
	for o, want := range map[Outcome]bool{OutcomeSelected: true, OutcomeSeveral: true, OutcomeAccepted: true,
		OutcomeUnscored: false, OutcomeUncertain: false, OutcomeNone: false, "": false} {
		if o.Actionable() != want {
			t.Errorf("%q", o)
		}
	}
}

// ---- ValidateScoreRequest: positions only, matches ErrInvalidRequest ----

func TestValidateScoreRequest_MatchesTheSentinelAndQuotesNoIds(t *testing.T) {
	r := ScoreRequest{Text: "t", Questions: []Question{
		{ID: "q_private", Kind: KindChoice, NoneID: "ghost_none", Candidates: []Candidate{{ID: "secret_table"}, {ID: "secret_table"}}},
		{ID: "q_private", Kind: "weird", Candidates: []Candidate{{ID: ""}}},
		{ID: "", Kind: KindRelevance},
	}}
	err := ValidateScoreRequest(r)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v", err)
	}
	for _, leak := range []string{"q_private", "secret_table", "ghost_none"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("the error quotes %q: %v", leak, err)
		}
	}
	for _, want := range []string{"question 1: candidate 2: duplicate candidate id", "question 1: noneId", "question 2: duplicate question id", "question 2: unknown kind", "question 2: candidate 1: id is required", "question 3: id is required", "question 3: no candidates"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	if err := ValidateScoreRequest(ScoreRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("no questions: %v", err)
	}
}

// ---- Attempt wire form ----

func TestAttempt_JSONCarriesLatencyMsAndStaysBackwardCompatible(t *testing.T) {
	a := Attempt{Provider: "jev", Outcome: AttemptDecided, Latency: 1234 * time.Millisecond, Role: "primary", Usage: &Usage{InputTokens: 7, OutputTokens: 2}}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["latencyMs"] != float64(1234) || m["latency"] != float64(1234*time.Millisecond) || m["usage"] == nil {
		t.Fatalf("wire = %s", b)
	}
	var back Attempt
	if err := json.Unmarshal(b, &back); err != nil || back.Latency != 1234*time.Millisecond || back.Usage == nil || back.Usage.InputTokens != 7 || back.Role != "primary" {
		t.Fatalf("back = %+v err=%v", back, err)
	}
	// latencyMs wins over the legacy field; the legacy field alone still reads.
	if err := json.Unmarshal([]byte(`{"provider":"p","outcome":"decided","latency":9000000000,"latencyMs":42}`), &back); err != nil || back.Latency != 42*time.Millisecond || back.Usage != nil {
		t.Fatalf("back = %+v err=%v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"provider":"p","outcome":"decided","latency":5000000}`), &back); err != nil || back.Latency != 5*time.Millisecond {
		t.Fatalf("legacy = %+v err=%v", back, err)
	}
	// Milliseconds only (a server that never sent nanoseconds).
	if err := json.Unmarshal([]byte(`{"provider":"p","outcome":"decided","latencyMs":3}`), &back); err != nil || back.Latency != 3*time.Millisecond {
		t.Fatalf("ms = %+v err=%v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"provider":5}`), &back); err == nil {
		t.Fatal("a wrongly typed field accepted")
	}
	// Report and Trace carry the same form.
	rb, _ := json.Marshal(Report{Attempts: []Attempt{a}})
	if !strings.Contains(string(rb), `"latencyMs":1234`) {
		t.Fatalf("report = %s", rb)
	}
}

func TestMergeReport_CarriesTheJudgedAttemptsUsage(t *testing.T) {
	rep := Report{Engine: "jev", Attempts: []Attempt{{Provider: "jev", Outcome: AttemptDecided}}}
	u := &Usage{InputTokens: 3}
	got := MergeReport(rep, Attempt{Provider: "jev", Outcome: AttemptUncertain, Usage: u}, true)
	if got[0].Outcome != AttemptUncertain || got[0].Usage != u {
		t.Fatalf("%+v", got)
	}
	// An engine-reported usage is not overwritten.
	own := &Usage{InputTokens: 9}
	rep.Attempts[0].Usage = own
	if got := MergeReport(rep, Attempt{Provider: "jev", Outcome: AttemptDecided, Usage: u}, true); got[0].Usage != own {
		t.Fatalf("%+v", got)
	}
}

// ---- Retry-After ----

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	date := func(d time.Duration) string { return now.Add(d).UTC().Format(http.TimeFormat) }
	cases := map[string]time.Duration{
		"":                          0,
		"   ":                       0,
		"90":                        90 * time.Second,
		" 7 ":                       7 * time.Second,
		"0":                         0,
		"-5":                        0,
		"soon":                      0,
		"1.5":                       0,
		"99999999999999999999":      0, // overflows Atoi: malformed
		"9999999999":                MaxRetryDelay,
		"86400":                     MaxRetryDelay,
		date(2 * time.Minute):       2 * time.Minute,
		date(-time.Hour):            0,
		date(48 * time.Hour):        MaxRetryDelay,
		"Mon, 99 Foo 2026 25:00:00": 0,
	}
	for in, want := range cases {
		if got := ParseRetryAfter(in, now); got != want {
			t.Errorf("%q: %v, want %v", in, got, want)
		}
	}
}

type delayErr struct{ d time.Duration }

func (e delayErr) Error() string             { return "slow down" }
func (e delayErr) RetryDelay() time.Duration { return e.d }

func TestRetryDelay_CapsAndIgnoresNegative(t *testing.T) {
	if RetryDelay(errors.New("x")) != 0 || RetryDelay(nil) != 0 {
		t.Fatal("no delay expected")
	}
	if got := RetryDelay(delayErr{90 * time.Second}); got != 90*time.Second {
		t.Fatal(got)
	}
	if got := RetryDelay(delayErr{100 * time.Hour}); got != MaxRetryDelay {
		t.Fatal(got)
	}
	if got := RetryDelay(delayErr{-time.Second}); got != 0 {
		t.Fatal(got)
	}
}

func TestChain_RecordsQuotaAndMisconfiguredDistinctly(t *testing.T) {
	for sentinel, want := range map[error]string{ErrQuota: AttemptQuota, ErrMisconfigured: AttemptMisconfigured} {
		p := providerFunc{name: "cloud", fn: func(context.Context, Request) (Decision, bool, error) {
			return Decision{}, false, fmt.Errorf("cloud: %w", sentinel)
		}}
		_, ok, tr := chainOf(nil, p).Decide(t.Context(), req())
		if ok || tr.Attempts[0].Outcome != want {
			t.Fatalf("%v: ok=%v tr=%+v", sentinel, ok, tr)
		}
	}
}
