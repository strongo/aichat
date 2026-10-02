package decision_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/strongo/aichat/ai/decision"
)

// S3: a decision read back from storage or the wire is not actionable and has
// lost its class; Chain.Rejudge is the explicit way back.
func TestRoundTripLosesVerdictAndClassAndRejudgeIsTheWayBack(t *testing.T) {
	rule := decision.Deterministic(answer(1))
	if !rule.Actionable() || rule.Provenance() != decision.ProvenanceDeterministic {
		t.Fatalf("%+v", rule)
	}
	b, err := json.Marshal(rule)
	if err != nil || !strings.Contains(string(b), `"outcome":"deterministic"`) {
		t.Fatalf("the trace-visible outcome stays on the wire: %s %v", b, err)
	}
	var back decision.Decision
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Actionable() || back.Provenance() != decision.ProvenanceSelfReported {
		t.Fatalf("a replayed rule decision must not be actionable or deterministic: %+v", back)
	}
	// Rejudge: through a chain's bar, as an ordinary (self-reported) decision.
	chain := decision.Chain{}
	got, ok := chain.Rejudge(back, tax)
	if !ok || !got.Actionable() || got.Outcome != decision.OutcomeFloor || got.Provenance() != decision.ProvenanceSelfReported {
		t.Fatalf("policy-less rejudge: ok=%v %+v", ok, got)
	}
	// It cannot be made deterministic again, not even from the in-process value.
	got, ok = chain.Rejudge(rule, tax)
	if !ok || got.Provenance() != decision.ProvenanceSelfReported || got.Outcome != decision.OutcomeFloor {
		t.Fatalf("rejudge of a rule decision: ok=%v %+v", ok, got)
	}
	// Under a durable policy a stored self-reported decision is refused.
	dur := decision.DurablePolicy()
	got, ok = decision.Chain{Policy: &dur}.Rejudge(back, tax)
	if ok || got.Actionable() || got.Outcome != decision.OutcomeUnscored {
		t.Fatalf("durable rejudge: ok=%v %+v", ok, got)
	}
	// A stored low-confidence decision whose Outcome claims "selected" is not revived.
	low := answer(0.1)
	low.Outcome = decision.OutcomeSelected
	if got, ok := chain.Rejudge(low, tax); ok || got.Actionable() {
		t.Fatalf("%+v", got)
	}
	// An invalid decision comes back stamped invalid.
	bad := answer(0.9)
	bad.Module.Value = "zzz"
	if got, ok := chain.Rejudge(bad, tax); ok || got.Outcome != decision.OutcomeInvalid {
		t.Fatalf("%+v", got)
	}
	// KeepNonSelected does not make a refusal actionable.
	nar := decision.NarrowingPolicy()
	scored := moduleDecision(decision.InteractionCommand, 0, nil)
	scored.Scores = map[string]float64{"m/i": 0.4, "m/j": 0.35}
	if got, ok := (decision.Chain{Policy: &nar, KeepNonSelected: true}).Rejudge(scored, tax); ok || got.Outcome != decision.OutcomeUncertain {
		t.Fatalf("%+v", got)
	}
}

// Trace.Err states the condition once and matches its sentinel.
func TestTraceErrMessageIsNotRepeated(t *testing.T) {
	quota := fmt.Errorf("jev: %w", decision.ErrQuota)
	_, ok, tr := decision.Chain{Providers: []decision.Provider{stub{name: "jev", err: quota}}}.Decide(context.Background(), request())
	if ok {
		t.Fatal("decided")
	}
	err := tr.Err()
	if !errors.Is(err, decision.ErrQuota) {
		t.Fatalf("%v", err)
	}
	if n := strings.Count(err.Error(), decision.ErrQuota.Error()); n != 1 {
		t.Fatalf("the sentinel text appears %d times: %q", n, err)
	}
	if !strings.Contains(err.Error(), "jev") {
		t.Fatalf("%q", err)
	}
	// With no attempt to name, the sentinel's own text stands in.
	err = decision.Trace{StoppedBy: decision.AttemptBudget}.Err()
	if !errors.Is(err, decision.ErrBudget) || !strings.Contains(err.Error(), decision.ErrBudget.Error()) {
		t.Fatalf("%v", err)
	}
	if decision.ErrBudget == decision.ErrQuota || errors.Is(decision.ErrBudget, decision.ErrQuota) {
		t.Fatal("ErrBudget is distinct from ErrQuota")
	}
}

// A budget is a quota-class stop, and it is classified ahead of the quiet failures
// an engine may join it with.
func TestChainStopsAtBudgetAndTheLoudConditionWinsOverTheQuietOne(t *testing.T) {
	paid := &stubCount{stub: stub{name: "paid", d: answer(0.9), ok: true}}
	for name, err := range map[string]error{
		"budget":                   fmt.Errorf("llm: %w", decision.ErrBudget),
		"unavailable then budget":  errors.Join(fmt.Errorf("jev: %w", decision.ErrUnavailable), fmt.Errorf("llm: %w", decision.ErrBudget)),
		"auth then quota":          errors.Join(fmt.Errorf("jev: %w", decision.ErrAuth), fmt.Errorf("llm: %w", decision.ErrQuota)),
		"rejected, misconfigured":  errors.Join(decision.ErrInvalidRequest, decision.ErrMisconfigured),
		"unavailable, misconfig.":  errors.Join(decision.ErrUnavailable, decision.ErrMisconfigured),
		"timeout-ish then quota":   errors.Join(context.DeadlineExceeded, decision.ErrQuota),
		"unavailable then quota":   errors.Join(decision.ErrUnavailable, decision.ErrQuota),
		"auth then budget":         errors.Join(decision.ErrAuth, decision.ErrBudget),
		"misconfigured then quota": errors.Join(decision.ErrMisconfigured, decision.ErrQuota),
	} {
		paid.n = 0
		d, ok, tr := decision.Chain{Providers: []decision.Provider{stub{name: "engine", err: err}, paid}}.Decide(context.Background(), request())
		if ok || d.Actionable() || paid.n != 0 || tr.StoppedBy == "" || tr.Err() == nil {
			t.Errorf("%s: ok=%v paidCalls=%d tr=%+v", name, ok, paid.n, tr)
		}
	}
	// FallThrough lets the paid provider answer after a budget stop.
	paid.n = 0
	_, ok, tr := decision.Chain{
		Providers:   []decision.Provider{stub{name: "engine", err: decision.ErrBudget}, paid},
		StopOnQuota: decision.FallThrough,
	}.Decide(context.Background(), request())
	if !ok || paid.n != 1 || tr.StoppedBy != "" || tr.Attempts[0].Outcome != decision.AttemptBudget {
		t.Fatalf("ok=%v calls=%d tr=%+v", ok, paid.n, tr)
	}
	// A misconfigured stop is its own switch: FallThrough on the quota switch leaves it.
	_, ok, tr = decision.Chain{
		Providers:   []decision.Provider{stub{name: "engine", err: decision.ErrMisconfigured}, paid},
		StopOnQuota: decision.FallThrough,
	}.Decide(context.Background(), request())
	if ok || tr.StoppedBy != decision.AttemptMisconfigured {
		t.Fatalf("ok=%v tr=%+v", ok, tr)
	}
}

type stubCount struct {
	stub
	n int
}

func (s *stubCount) Decide(ctx context.Context, r decision.Request) (decision.Decision, bool, error) {
	s.n++
	return s.stub.Decide(ctx, r)
}

func TestStopPolicyText(t *testing.T) {
	var z decision.StopPolicy
	if z != decision.StopChain {
		t.Fatal("the zero value must stop")
	}
	for p, want := range map[decision.StopPolicy]string{decision.StopChain: "stop", decision.FallThrough: "fall_through", decision.StopPolicy(9): "StopPolicy(9)"} {
		if p.String() != want {
			t.Errorf("%d: %q", p, p.String())
		}
	}
	b, err := json.Marshal(struct{ P decision.StopPolicy }{decision.FallThrough})
	if err != nil || string(b) != `{"P":"fall_through"}` {
		t.Fatalf("%s %v", b, err)
	}
	var out struct{ P decision.StopPolicy }
	if err := json.Unmarshal([]byte(`{"P":"stop"}`), &out); err != nil || out.P != decision.StopChain {
		t.Fatalf("%+v %v", out, err)
	}
	if err := json.Unmarshal([]byte(`{"P":"fall_through"}`), &out); err != nil || out.P != decision.FallThrough {
		t.Fatalf("%+v %v", out, err)
	}
	if err := json.Unmarshal([]byte(`{"P":"maybe"}`), &out); err == nil || !strings.Contains(err.Error(), "maybe") {
		t.Fatalf("an unknown value must be an error, not a silent stop: %v", err)
	}
}

// The wire form of a judged decision is unchanged: Outcome stays a plain string.
func TestJudgedDecisionJSONKeepsTheOutcome(t *testing.T) {
	d, ok, _ := decision.Chain{Providers: []decision.Provider{stub{name: "x", d: answer(0.9), ok: true}}}.Decide(context.Background(), request())
	if !ok {
		t.Fatal("not decided")
	}
	b, _ := json.Marshal(d)
	if !strings.Contains(string(b), `"outcome":"floor"`) || strings.Contains(string(b), "verdict") || strings.Contains(string(b), "deterministic\":") {
		t.Fatalf("%s", b)
	}
}
