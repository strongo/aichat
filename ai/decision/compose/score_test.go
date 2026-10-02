package compose

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/strongo/aichat/ai/decision"
)

func scoreRequest() decision.ScoreRequest {
	return decision.ScoreRequest{
		Text: "which tables matter?",
		Questions: []decision.Question{
			{ID: "pick", Kind: decision.KindChoice, NoneID: "none", Candidates: []decision.Candidate{{ID: "a"}, {ID: "b"}, {ID: "none"}}},
			{ID: "rel", Kind: decision.KindRelevance, Candidates: []decision.Candidate{{ID: "a"}, {ID: "b"}}},
		},
	}
}

func goodScore(engine string) decision.ScoreResult {
	pick := decision.NewAnswer("pick", decision.KindChoice, []decision.Score{{ID: "a", Probability: 0.9}, {ID: "b", Probability: 0.05}, {ID: "none", Probability: 0.05}})
	pick.Calibrated, pick.HasConfidence, pick.Confidence, pick.NoneID = true, true, 0.8, "none"
	rel := decision.NewAnswer("rel", decision.KindRelevance, []decision.Score{{ID: "a", Probability: 0.9}, {ID: "b", Probability: 0.7}})
	rel.Calibrated = true
	return decision.ScoreResult{Engine: engine, Model: "m-1", Answers: map[string]decision.Answer{"pick": pick, "rel": rel}}
}

func scorer(name string, res decision.ScoreResult, err error) *fake {
	return &fake{name: name, score: func(context.Context, decision.ScoreRequest) (decision.ScoreResult, error) {
		return res, err
	}}
}

func uncertainScore(engine string) decision.ScoreResult {
	res := goodScore(engine)
	a := decision.NewAnswer("pick", decision.KindChoice, []decision.Score{{ID: "a", Probability: 0.38}, {ID: "b", Probability: 0.35}, {ID: "none", Probability: 0.27}})
	a.Calibrated, a.NoneID = true, "none"
	res.Answers["pick"] = a
	return res
}

func TestScore_SingleAndReport(t *testing.T) {
	noLeak(t)
	e := Single(scorer("jev", goodScore("jev"), nil), WithClock(newFakeClock()))
	res, err := e.Score(context.Background(), scoreRequest())
	if err != nil || res.Engine != "jev" || res.Model != "m-1" || res.Report == nil {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if res.Report.Strategy != "single" || res.Report.Engine != "jev" || len(res.Report.Attempts) != 1 {
		t.Fatalf("report = %+v", res.Report)
	}
}

func TestScore_InvalidRequestIsRejectedBeforeAnyCall(t *testing.T) {
	noLeak(t)
	p := scorer("jev", goodScore("jev"), nil)
	_, rep, err := Single(p, WithClock(newFakeClock())).ScoreTraced(context.Background(), decision.ScoreRequest{})
	if err == nil || p.calls.Load() != 0 || rep.Strategy != "single" {
		t.Fatalf("err=%v calls=%d rep=%+v", err, p.calls.Load(), rep)
	}
}

func TestScore_FallbackOnFailureUnsupportedAndInvalid(t *testing.T) {
	noLeak(t)
	invalid := goodScore("jev")
	delete(invalid.Answers, "rel")
	cases := map[string]decision.Provider{
		"error":       scorer("jev", decision.ScoreResult{}, errBoom),
		"unavailable": scorer("jev", decision.ScoreResult{}, decision.ErrUnavailable),
		"invalid":     scorer("jev", invalid, nil),
		"unsupported": noTimeout{instant("jev", "i")},
	}
	for name, primary := range cases {
		t.Run(name, func(t *testing.T) {
			e := Fallback(primary, scorer("llm", goodScore("llm"), nil), WithClock(newFakeClock()))
			res, rep, err := e.ScoreTraced(context.Background(), scoreRequest())
			if err != nil || res.Engine != "llm" || !rep.FallbackFired || rep.Engine != "llm" {
				t.Fatalf("res=%+v rep=%+v err=%v", res, rep, err)
			}
		})
	}
}

func TestScore_NonScoredProviderIsUnsupported(t *testing.T) {
	noLeak(t)
	e := Single(noTimeout{instant("jev", "i")}, WithClock(newFakeClock()))
	_, rep, err := e.ScoreTraced(context.Background(), scoreRequest())
	if !errors.Is(err, decision.ErrUnsupported) || rep.Attempts[0].Outcome != decision.AttemptUnsupported {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
}

func TestScore_UncertainStaysWithPrimaryByDefaultAndFallsBackOnOptIn(t *testing.T) {
	noLeak(t)
	backup := scorer("llm", goodScore("llm"), nil)
	opts := []Option{WithClock(newFakeClock()), WithPolicy(decision.NarrowingPolicy())}

	res, rep, err := Fallback(scorer("jev", uncertainScore("jev"), nil), backup, opts...).ScoreTraced(context.Background(), scoreRequest())
	if err != nil || res.Engine != "jev" || backup.calls.Load() != 0 || rep.Attempts[0].Outcome != decision.AttemptUncertain {
		t.Fatalf("default: res=%+v rep=%+v err=%v", res.Engine, rep, err)
	}

	res, rep, err = Fallback(scorer("jev", uncertainScore("jev"), nil), backup, append(opts, WithFallbackOn(OnUncertain))...).
		ScoreTraced(context.Background(), scoreRequest())
	if err != nil || res.Engine != "llm" || !rep.FallbackFired {
		t.Fatalf("opt-in: res=%+v rep=%+v err=%v", res.Engine, rep, err)
	}

	// An uncalibrated answer is never "uncertain": it has no calibrated numbers.
	unc := uncertainScore("llm")
	a := unc.Answers["pick"]
	a.Calibrated = false
	unc.Answers["pick"] = a
	rel := unc.Answers["rel"]
	rel.Calibrated = false
	unc.Answers["rel"] = rel
	res, _, err = Fallback(scorer("llm", unc, nil), backup, append(opts, WithFallbackOn(OnUncertain))...).ScoreTraced(context.Background(), scoreRequest())
	if err != nil || res.Engine != "llm" {
		t.Fatalf("uncalibrated: %+v %v", res.Engine, err)
	}
}

func TestScore_AllFail(t *testing.T) {
	noLeak(t)
	e := Fallback(scorer("jev", decision.ScoreResult{}, errBoom), scorer("llm", decision.ScoreResult{}, errBoom), WithClock(newFakeClock()))
	_, rep, err := e.ScoreTraced(context.Background(), scoreRequest())
	if !errors.Is(err, errBoom) || rep.Engine != "" || len(rep.Attempts) != 2 {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
}

func TestScore_RaceAndHedgedWork(t *testing.T) {
	noLeak(t)
	slow := &fake{name: "slow", score: func(ctx context.Context, _ decision.ScoreRequest) (decision.ScoreResult, error) {
		<-ctx.Done()
		return decision.ScoreResult{}, ctx.Err()
	}}
	fast := scorer("fast", goodScore("fast"), nil)
	res, rep, err := Race([]decision.Provider{slow, fast}, WithClock(newFakeClock())).ScoreTraced(context.Background(), scoreRequest())
	if err != nil || res.Engine != "fast" || !reflect.DeepEqual(attemptOutcomes(rep), []string{"slow:cancelled", "fast:decided"}) {
		t.Fatalf("race: %+v %v %v", res.Engine, attemptOutcomes(rep), err)
	}
	waitFor(t, func() bool { return slow.running.Load() == 0 }, "slow to exit")

	res, rep, err = Hedged(scorer("jev", decision.ScoreResult{}, errBoom), fast, 0, WithClock(newFakeClock())).ScoreTraced(context.Background(), scoreRequest())
	if err != nil || res.Engine != "fast" || !rep.FallbackFired {
		t.Fatalf("hedged: %+v %+v %v", res.Engine, rep, err)
	}
}

func TestScore_NestedEngineFlattensAttempts(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	inner := Fallback(scorer("jev", decision.ScoreResult{}, errBoom), scorer("llm", goodScore("llm"), nil), WithClock(clk), WithName("inner"))
	outer := Single(inner, WithClock(clk), WithName("outer"))
	res, rep, err := outer.ScoreTraced(context.Background(), scoreRequest())
	if err != nil || res.Engine != "llm" || rep.Engine != "llm" {
		t.Fatalf("res=%+v rep=%+v err=%v", res.Engine, rep, err)
	}
	if !reflect.DeepEqual(attemptOutcomes(rep), []string{"jev:error", "llm:decided"}) {
		t.Fatalf("attempts = %v", attemptOutcomes(rep))
	}
}

func TestScore_InvalidResultFromNestedWinnerIsRelabelled(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	bad := goodScore("llm")
	delete(bad.Answers, "rel")
	// The inner engine cannot know the result is invalid for the outer request
	// shape only when it is: here the inner passes it (same request), so use a
	// stub traced scorer whose report claims "decided" for an invalid result.
	stub := stubTracedScorer{res: bad, rep: decision.Report{Strategy: "x", Engine: "llm", Attempts: []decision.Attempt{{Provider: "llm", Outcome: decision.AttemptDecided}}}}
	_, rep, err := Single(stub, WithClock(clk)).ScoreTraced(context.Background(), scoreRequest())
	if err == nil || rep.Attempts[0].Outcome != decision.AttemptInvalid || !strings.Contains(rep.Attempts[0].Detail, "validation") {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
}

type stubTracedScorer struct {
	res decision.ScoreResult
	rep decision.Report
}

func (stubTracedScorer) Name() string { return "stub" }
func (s stubTracedScorer) Decide(context.Context, decision.Request) (decision.Decision, bool, error) {
	return decision.Decision{}, false, nil
}
func (s stubTracedScorer) Score(context.Context, decision.ScoreRequest) (decision.ScoreResult, error) {
	return s.res, nil
}
func (s stubTracedScorer) ScoreTraced(context.Context, decision.ScoreRequest) (decision.ScoreResult, decision.Report, error) {
	return s.res, s.rep, nil
}
