package compose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/decision/typesafe"
)

// These tests drive the REAL typesafe client over a fake HTTP exchange through
// the combinators, because the property under test is end to end: the tokens a
// call billed reach decision.Trace.Attempts, whatever the answer was.

// jevDoer is a canned HTTP exchange. block, when set, holds the call until its
// context is done (a slow engine); after, when set, is called once the exchange
// has been answered.
type jevDoer struct {
	status int
	body   string
	err    error
	block  bool
	after  func()
	calls  atomic.Int32
}

func (d *jevDoer) Do(r *http.Request) (*http.Response, error) {
	d.calls.Add(1)
	if d.block {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	if d.after != nil {
		defer d.after()
	}
	if d.err != nil {
		return nil, d.err
	}
	status := d.status
	if status == 0 {
		status = 200
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader([]byte(d.body)))}, nil
}

func jev(t *testing.T, name string, d *jevDoer) *typesafe.Client {
	t.Helper()
	c, err := typesafe.New(typesafe.Config{APIKey: "test-key-not-a-secret", Model: "jev-1.13.0", Name: name, HTTPClient: d, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func jevBody(in, out int, answers ...string) string {
	return fmt.Sprintf(`{"model":"jev-1.13.0","answers":{%s},"usage":{"input_tokens":%d,"output_tokens":%d}}`, strings.Join(answers, ","), in, out)
}

func usageErrBody(in, out int) string {
	return fmt.Sprintf(`{"detail":{"error_type":"overloaded"},"usage":{"input_tokens":%d,"output_tokens":%d}}`, in, out)
}

func choice(key, top string, conf float64, probs string) string {
	return fmt.Sprintf(`"%s":{"type":"choice","choice":"%s","confidence":%v,"probabilities":{%s}}`, key, top, conf, probs)
}

func noul(key string, p float64) string { return fmt.Sprintf(`"%s":{"type":"noul","noul":%v}`, key, p) }

// the three kinds of answer to request() (module m, intents i and j).
func decidedBody(in, out int) string {
	return jevBody(in, out, choice("intent", "m/i", 0.9, `"m/i":0.9,"m/j":0.05,"other":0.05`), noul("scope0", 0.9), choice("interaction", "question", 0.9, `"question":0.9,"command":0.1`))
}

func uncertainBody(in, out int) string {
	return jevBody(in, out, choice("intent", "m/i", 0.3, `"m/i":0.4,"m/j":0.35,"other":0.25`), noul("scope0", 0.9), choice("interaction", "question", 0.9, `"question":0.9,"command":0.1`))
}

func abstainBody(in, out int) string {
	return jevBody(in, out, choice("intent", "other", 0.9, `"other":0.9,"m/i":0.1`), noul("scope0", 0.9), choice("interaction", "question", 0.9, `"question":0.9,"command":0.1`))
}

func wantUsage(t *testing.T, a decision.Attempt, in, out int) {
	t.Helper()
	if a.Usage == nil || a.Usage.InputTokens != in || a.Usage.OutputTokens != out {
		t.Fatalf("attempt %+v: usage = %+v, want %d in / %d out", a, a.Usage, in, out)
	}
}

func wantNoUsage(t *testing.T, a decision.Attempt) {
	t.Helper()
	if a.Usage != nil {
		t.Fatalf("attempt %+v: usage = %+v, want nil (unknown), not zero", a, a.Usage)
	}
}

func attemptsOf(t *testing.T, rep decision.Report, outcomes ...string) []decision.Attempt {
	t.Helper()
	if got := attemptOutcomes(rep); len(got) != len(outcomes) {
		t.Fatalf("attempts = %v, want %v", got, outcomes)
	}
	for i, a := range rep.Attempts {
		if a.Outcome != outcomes[i] {
			t.Fatalf("attempts = %v, want %v", attemptOutcomes(rep), outcomes)
		}
	}
	return rep.Attempts
}

func decide(e decision.TracedProvider) (decision.Decision, bool, decision.Report, error) {
	return e.DecideTraced(context.Background(), request())
}

// strategies builds every combinator with `primary` first and a `backup` that
// should never answer.
func strategies(primary, backup decision.Provider, opts ...Option) map[string]*Engine {
	opts = append([]Option{WithClock(newFakeClock())}, opts...)
	return map[string]*Engine{
		"single":   Single(primary, opts...),
		"fallback": Fallback(primary, backup, opts...),
		"hedged":   Hedged(primary, backup, time.Hour, opts...),
	}
}

func TestEngines_ReportUsageOfDecidedUncertainAndAbstainedAnswers(t *testing.T) {
	noLeak(t)
	cases := []struct {
		name    string
		body    string
		outcome string
		ok      bool
		in, out int
	}{
		{"decided", decidedBody(120, 17), decision.AttemptDecided, true, 120, 17},
		{"uncertain", uncertainBody(121, 18), decision.AttemptUncertain, true, 121, 18},
		{"abstained", abstainBody(122, 19), decision.AttemptAbstained, false, 122, 19},
	}
	for _, tc := range cases {
		for name, e := range strategies(jev(t, "jev", &jevDoer{body: tc.body}), failing("llm", errBoom), WithPolicy(decision.NarrowingPolicy())) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				_, ok, rep, err := decide(e)
				if err != nil || ok != tc.ok {
					t.Fatalf("ok=%v err=%v rep=%+v", ok, err, rep)
				}
				a := attemptsOf(t, rep, tc.outcome)[0]
				wantUsage(t, a, tc.in, tc.out)
				if a.Provider != "jev" || a.Role != "primary" || a.Latency < 0 {
					t.Fatalf("attempt = %+v", a)
				}
			})
		}
	}
}

func TestRace_ReportsTheWinnersUsageAndALoserWithNoneHasNil(t *testing.T) {
	noLeak(t)
	e := Race([]decision.Provider{jev(t, "jev", &jevDoer{body: decidedBody(120, 17)}), cooperative("slow")}, WithClock(newFakeClock()))
	_, ok, rep, err := decide(e)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v rep=%+v", ok, err, rep)
	}
	as := attemptsOf(t, rep, decision.AttemptDecided, decision.AttemptCancelled)
	wantUsage(t, as[0], 120, 17)
	if as[0].Role != "racer" || as[1].Role != "racer" {
		t.Fatalf("roles: %+v", as)
	}
	wantNoUsage(t, as[1]) // still running when cancelled: its cost is unknown
}

func TestRace_AFinishedLoserKeepsItsUsage(t *testing.T) {
	noLeak(t)
	// The first racer fails (billed) before the second, held back until then,
	// answers.
	failedBack := make(chan struct{})
	failed := jev(t, "jev", &jevDoer{status: 529, body: usageErrBody(31, 4), after: func() { close(failedBack) }})
	winner := tracedAfter{jev(t, "backup", &jevDoer{body: decidedBody(60, 9)}), failedBack}
	_, ok, rep, err := decide(Race([]decision.Provider{failed, winner}, WithClock(newFakeClock())))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v rep=%+v", ok, err, rep)
	}
	as := attemptsOf(t, rep, decision.AttemptError, decision.AttemptDecided)
	wantUsage(t, as[0], 31, 4)
	wantUsage(t, as[1], 60, 9)
}

// tracedAfter delays a traced provider until a channel is closed, and then a
// little, so another racer's result is recorded first.
type tracedAfter struct {
	*typesafe.Client
	wait chan struct{}
}

func (p tracedAfter) DecideTraced(ctx context.Context, req decision.Request) (decision.Decision, bool, decision.Report, error) {
	<-p.wait
	time.Sleep(20 * time.Millisecond)
	return p.Client.DecideTraced(ctx, req)
}

func TestFallback_AFailedPrimaryKeepsItsUsageAndClassification(t *testing.T) {
	noLeak(t)
	cases := []struct {
		name    string
		doer    *jevDoer
		outcome string
		usage   *[2]int
	}{
		{"overloaded with usage", &jevDoer{status: 529, body: usageErrBody(31, 4)}, decision.AttemptError, &[2]int{31, 4}},
		{"refused credentials with usage", &jevDoer{status: 401, body: `{"usage":{"input_tokens":2,"output_tokens":0}}`}, decision.AttemptAuth, &[2]int{2, 0}},
		{"rejected request with usage", &jevDoer{status: 400, body: `{"usage":{"input_tokens":3,"output_tokens":0}}`}, decision.AttemptRejected, &[2]int{3, 0}},
		{"a 2xx that fails validation, with usage", &jevDoer{body: jevBody(44, 5, noul("scope0", 0.9))}, decision.AttemptError, &[2]int{44, 5}},
		{"no usage in the body", &jevDoer{status: 500, body: `{}`}, decision.AttemptError, nil},
		{"a network error", &jevDoer{err: errBoom}, decision.AttemptError, nil},
		{"a timeout", &jevDoer{err: context.DeadlineExceeded}, decision.AttemptTimeout, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Fallback(jev(t, "jev", tc.doer), jev(t, "llm", &jevDoer{body: decidedBody(60, 9)}), WithClock(newFakeClock()))
			_, ok, rep, err := decide(e)
			if err != nil || !ok || !rep.FallbackFired || rep.Engine != "llm" {
				t.Fatalf("ok=%v err=%v rep=%+v", ok, err, rep)
			}
			as := attemptsOf(t, rep, tc.outcome, decision.AttemptDecided)
			if tc.usage == nil {
				wantNoUsage(t, as[0])
			} else {
				wantUsage(t, as[0], tc.usage[0], tc.usage[1])
			}
			wantUsage(t, as[1], 60, 9)
			if as[0].Role != "primary" || as[1].Role != "backup" {
				t.Fatalf("roles: %+v", as)
			}
		})
	}
}

func TestSingle_AFailedCallWithUsageReportsItAlongsideTheError(t *testing.T) {
	noLeak(t)
	_, ok, rep, err := decide(Single(jev(t, "jev", &jevDoer{status: 529, body: usageErrBody(31, 4)}), WithClock(newFakeClock())))
	if ok || !errors.Is(err, typesafe.ErrOverloaded) {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	wantUsage(t, attemptsOf(t, rep, decision.AttemptError)[0], 31, 4)
}

func TestHedged_AFastFailingPrimaryAndItsBackupBothReportUsage(t *testing.T) {
	noLeak(t)
	e := Hedged(jev(t, "jev", &jevDoer{status: 529, body: usageErrBody(31, 4)}), jev(t, "llm", &jevDoer{body: decidedBody(60, 9)}), time.Hour, WithClock(newFakeClock()))
	_, ok, rep, err := decide(e)
	if err != nil || !ok || !rep.FallbackFired {
		t.Fatalf("ok=%v err=%v rep=%+v", ok, err, rep)
	}
	as := attemptsOf(t, rep, decision.AttemptError, decision.AttemptDecided)
	wantUsage(t, as[0], 31, 4)
	wantUsage(t, as[1], 60, 9)
}

func TestHedged_ASlowPrimaryCancelledByTheHedgeHasUnknownUsageAndTheBackupItsOwn(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	primaryDoer := &jevDoer{block: true}
	e := Hedged(jev(t, "jev", primaryDoer), jev(t, "llm", &jevDoer{body: decidedBody(60, 9)}), 600*time.Millisecond, WithClock(clk))
	type res struct {
		rep decision.Report
		err error
	}
	done := make(chan res, 1)
	go func() {
		_, _, rep, err := decide(e)
		done <- res{rep, err}
	}()
	waitFor(t, func() bool { return primaryDoer.calls.Load() == 1 }, "the primary to start")
	clk.WaitTimers(t, 2)
	clk.Advance(600 * time.Millisecond)
	r := <-done
	if r.err != nil || !r.rep.HedgeFired {
		t.Fatalf("%+v", r)
	}
	as := attemptsOf(t, r.rep, decision.AttemptCancelled, decision.AttemptDecided)
	wantNoUsage(t, as[0])
	wantUsage(t, as[1], 60, 9)
}

func TestBreakerAndBudget_PassTheEnginesUsageOn(t *testing.T) {
	noLeak(t)
	wrap := map[string]func(decision.Provider) decision.Provider{
		"breaker": func(p decision.Provider) decision.Provider { return NewBreaker(p) },
		"budget": func(p decision.Provider) decision.Provider {
			return NewBudget(p, BudgetOptions{MaxCalls: 10, Clock: newFakeClock()})
		},
		"breaker(budget)": func(p decision.Provider) decision.Provider {
			return NewBreaker(NewBudget(p, BudgetOptions{MaxCalls: 10, Clock: newFakeClock()}))
		},
	}
	for name, w := range wrap {
		for _, tc := range []struct {
			name    string
			doer    *jevDoer
			outcome string
			usage   *[2]int
		}{
			{"decided", &jevDoer{body: decidedBody(120, 17)}, decision.AttemptDecided, &[2]int{120, 17}},
			{"failed with usage", &jevDoer{status: 529, body: usageErrBody(31, 4)}, decision.AttemptError, &[2]int{31, 4}},
			{"failed without usage", &jevDoer{err: errBoom}, decision.AttemptError, nil},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				p := w(jev(t, "jev", tc.doer))
				// Directly: the wrapper hands the engine's report on.
				_, _, rep, _ := p.(decision.TracedProvider).DecideTraced(context.Background(), request())
				// And inside an engine, where the leg's own record joins it.
				_, _, rep2, _ := decide(Single(p, WithClock(newFakeClock())))
				for _, r := range []decision.Report{rep, rep2} {
					a := attemptsOf(t, r, tc.outcome)[0]
					if tc.usage == nil {
						wantNoUsage(t, a)
					} else {
						wantUsage(t, a, tc.usage[0], tc.usage[1])
					}
				}
				if rep2.Attempts[0].Role != "primary" {
					t.Fatalf("role = %q", rep2.Attempts[0].Role)
				}
			})
		}
	}
}

func TestBudget_ARefusedCallReportsNoUsage(t *testing.T) {
	noLeak(t)
	doer := &jevDoer{body: decidedBody(120, 17)}
	b := NewBudget(jev(t, "jev", doer), BudgetOptions{MaxCalls: 1, Clock: newFakeClock()})
	e := Single(b, WithClock(newFakeClock()))
	if _, ok, _, err := decide(e); !ok || err != nil {
		t.Fatal(ok, err)
	}
	_, ok, rep, err := decide(e)
	if ok || !errors.Is(err, decision.ErrBudget) || doer.calls.Load() != 1 {
		t.Fatalf("ok=%v err=%v calls=%d", ok, err, doer.calls.Load())
	}
	wantNoUsage(t, attemptsOf(t, rep, decision.AttemptBudget)[0])
}

// The Chain over the whole stack: the usage of every call reaches
// decision.Trace.Attempts, with the Chain's own verdict on the answer.
func TestChain_OverTheStackKeepsEveryCallsUsage(t *testing.T) {
	noLeak(t)
	pol := decision.NarrowingPolicy()
	for _, tc := range []struct {
		name    string
		doer    *jevDoer
		ok      bool
		outcome string
		usage   *[2]int
	}{
		{"decided", &jevDoer{body: decidedBody(120, 17)}, true, decision.AttemptDecided, &[2]int{120, 17}},
		{"uncertain", &jevDoer{body: uncertainBody(121, 18)}, false, decision.AttemptUncertain, &[2]int{121, 18}},
		{"abstained", &jevDoer{body: abstainBody(122, 19)}, false, decision.AttemptAbstained, &[2]int{122, 19}},
		{"failed with usage", &jevDoer{status: 529, body: usageErrBody(31, 4)}, false, decision.AttemptError, &[2]int{31, 4}},
		{"failed without usage", &jevDoer{err: errBoom}, false, decision.AttemptError, nil},
	} {
		stacks := map[string]func(*typesafe.Client) decision.Provider{
			"bare":   func(c *typesafe.Client) decision.Provider { return c },
			"single": func(c *typesafe.Client) decision.Provider { return Single(c, WithClock(newFakeClock())) },
			"fallback(breaker(budget))": func(c *typesafe.Client) decision.Provider {
				return Fallback(NewBreaker(NewBudget(c, BudgetOptions{MaxCalls: 10, Clock: newFakeClock()})), failing("llm", errBoom), WithClock(newFakeClock()))
			},
			"race": func(c *typesafe.Client) decision.Provider {
				return Race([]decision.Provider{c, cooperative("slow")}, WithClock(newFakeClock()))
			},
		}
		for sname, mk := range stacks {
			t.Run(tc.name+"/"+sname, func(t *testing.T) {
				_, ok, tr := decision.Chain{Providers: []decision.Provider{mk(jev(t, "jev", tc.doer))}, Policy: &pol}.Decide(context.Background(), request())
				if ok != tc.ok || len(tr.Attempts) == 0 {
					t.Fatalf("ok=%v trace=%+v", ok, tr)
				}
				a := tr.Attempts[0]
				if a.Provider != "jev" || a.Outcome != tc.outcome {
					t.Fatalf("attempts = %+v", tr.Attempts)
				}
				if tc.usage == nil {
					wantNoUsage(t, a)
				} else {
					wantUsage(t, a, tc.usage[0], tc.usage[1])
				}
			})
		}
	}
}

// ---- scoring ----

func scoreBody(in, out int, pick string) string {
	return jevBody(in, out, choice("q0", "a", 0.8, pick), noul("q1.c0", 0.9), noul("q1.c1", 0.7))
}

func TestScore_EnginesReportUsageOfEveryCall(t *testing.T) {
	noLeak(t)
	pol := decision.NarrowingPolicy()
	cases := []struct {
		name    string
		doer    *jevDoer
		outcome string
		usage   *[2]int
	}{
		{"decided", &jevDoer{body: scoreBody(512, 127, `"a":0.9,"b":0.05,"none":0.05`)}, decision.AttemptDecided, &[2]int{512, 127}},
		{"decided without usage", &jevDoer{body: strings.Replace(scoreBody(0, 0, `"a":0.9,"b":0.05,"none":0.05`), `,"usage":{"input_tokens":0,"output_tokens":0}`, ``, 1)}, decision.AttemptDecided, nil},
		{"uncertain", &jevDoer{body: jevBody(513, 128, choice("q0", "a", 0.3, `"a":0.38,"b":0.35,"none":0.27`), noul("q1.c0", 0.9), noul("q1.c1", 0.7))}, decision.AttemptUncertain, &[2]int{513, 128}},
		{"failed with usage", &jevDoer{status: 529, body: usageErrBody(31, 4)}, decision.AttemptError, &[2]int{31, 4}},
		{"failed without usage", &jevDoer{err: errBoom}, decision.AttemptError, nil},
	}
	opts := []Option{WithPolicy(pol), WithClock(newFakeClock())}
	backup := func() decision.Provider { return scorer("llm", goodScore("llm"), nil) }
	stacks := map[string]struct {
		build    func(*typesafe.Client) *Engine
		fallback bool // a backup answers when the primary fails
	}{
		"single":  {func(c *typesafe.Client) *Engine { return Single(c, opts...) }, false},
		"breaker": {func(c *typesafe.Client) *Engine { return Single(NewBreaker(c), opts...) }, false},
		"budget": {func(c *typesafe.Client) *Engine {
			return Single(NewBudget(c, BudgetOptions{MaxCalls: 5, Clock: newFakeClock()}), opts...)
		}, false},
		"fallback": {func(c *typesafe.Client) *Engine { return Fallback(c, backup(), opts...) }, true},
		"hedged":   {func(c *typesafe.Client) *Engine { return Hedged(c, backup(), time.Hour, opts...) }, true},
	}
	for _, tc := range cases {
		for name, st := range stacks {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				_, rep, err := st.build(jev(t, "jev", tc.doer)).ScoreTraced(context.Background(), scoreRequest())
				failed := tc.outcome == decision.AttemptError
				if (err != nil) != (failed && !st.fallback) {
					t.Fatalf("err=%v rep=%+v", err, rep)
				}
				if len(rep.Attempts) == 0 {
					t.Fatalf("rep = %+v", rep)
				}
				a := rep.Attempts[0]
				if a.Provider != "jev" || a.Outcome != tc.outcome || a.Role != "primary" {
					t.Fatalf("attempts = %+v", rep.Attempts)
				}
				if tc.usage == nil {
					wantNoUsage(t, a)
				} else {
					wantUsage(t, a, tc.usage[0], tc.usage[1])
				}
			})
		}
	}
}

// blockingScorer is a scored provider that never answers until cancelled.
func blockingScorer(name string) *fake {
	return &fake{name: name, score: func(ctx context.Context, _ decision.ScoreRequest) (decision.ScoreResult, error) {
		<-ctx.Done()
		return decision.ScoreResult{}, ctx.Err()
	}}
}

func TestScore_RaceReportsTheWinnersUsage(t *testing.T) {
	noLeak(t)
	e := Race([]decision.Provider{jev(t, "jev", &jevDoer{body: scoreBody(512, 127, `"a":0.9,"b":0.05,"none":0.05`)}), blockingScorer("slow")}, WithClock(newFakeClock()))
	_, rep, err := e.ScoreTraced(context.Background(), scoreRequest())
	if err != nil {
		t.Fatal(err)
	}
	as := attemptsOf(t, rep, decision.AttemptDecided, decision.AttemptCancelled)
	wantUsage(t, as[0], 512, 127)
	wantNoUsage(t, as[1])
}
