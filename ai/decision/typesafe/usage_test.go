package typesafe

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

// oneAttempt extracts the one attempt a traced call must report.
func oneAttempt(t *testing.T, rep decision.Report) decision.Attempt {
	t.Helper()
	if len(rep.Attempts) != 1 {
		t.Fatalf("a call reports exactly one attempt, got %+v", rep.Attempts)
	}
	if rep.Engine != DefaultName {
		t.Fatalf("engine = %q", rep.Engine)
	}
	return rep.Attempts[0]
}

func wantUsage(t *testing.T, got *decision.Usage, in, out int) {
	t.Helper()
	if got == nil || got.InputTokens != in || got.OutputTokens != out {
		t.Fatalf("usage = %+v, want %d in / %d out", got, in, out)
	}
}

// Every upstream call reports one attempt with the usage it billed: a decided
// answer included, which used to leave the report empty.
func TestDecideTraced_ReportsUsageForEveryOutcome(t *testing.T) {
	const apiErr = `{"detail":{"error_type":"overloaded"},"usage":{"input_tokens":31,"output_tokens":4}}`
	cases := []struct {
		name    string
		doer    *fakeDoer
		outcome string
		detail  string
		wantErr error
		usage   *[2]int // nil: the call reported none
		model   string
	}{
		{name: "decided", doer: &fakeDoer{body: response(fullAnswers()...)}, outcome: decision.AttemptDecided, usage: &[2]int{100, 20}, model: testModel},
		{name: "abstained", doer: &fakeDoer{body: response(choiceJSON("intent", "other", 0.9, `"other":0.9,"calendar/show":0.1`), interactionAnswer("question", 0.9))},
			outcome: decision.AttemptAbstained, detail: AbstainIntentOther, usage: &[2]int{100, 20}, model: testModel},
		{name: "a 2xx that fails validation carries its usage", doer: &fakeDoer{body: response(interactionAnswer("question", 0.9))},
			outcome: decision.AttemptError, wantErr: ErrBadResponse, usage: &[2]int{100, 20}, model: testModel},
		{name: "a non-2xx whose body has usage", doer: &fakeDoer{status: 529, body: apiErr}, outcome: decision.AttemptError, wantErr: ErrOverloaded, usage: &[2]int{31, 4}},
		{name: "a non-2xx with no usage", doer: &fakeDoer{status: 500, body: `{"detail":"boom"}`}, outcome: decision.AttemptError, wantErr: ErrServer},
		{name: "a network error", doer: &fakeDoer{err: errNetwork}, outcome: decision.AttemptError, wantErr: errNetwork},
		{name: "a body that cannot be read", doer: &fakeDoer{readErr: errNetwork, body: "x"}, outcome: decision.AttemptError, wantErr: errNetwork},
		{name: "a 2xx with no answers", doer: &fakeDoer{body: `{"model":"m","usage":{"input_tokens":9,"output_tokens":1}}`}, outcome: decision.AttemptError, wantErr: ErrBadResponse, usage: &[2]int{9, 1}},
		{name: "a 2xx that is not the documented shape", doer: &fakeDoer{body: `{"answers":"x","usage":{"input_tokens":8,"output_tokens":2}}`}, outcome: decision.AttemptError, wantErr: ErrBadResponse, usage: &[2]int{8, 2}},
		{name: "a 2xx that is not JSON", doer: &fakeDoer{body: `not json`}, outcome: decision.AttemptError, wantErr: ErrBadResponse},
		{name: "a 2xx too large to read", doer: &fakeDoer{body: response(fullAnswers()...)}, outcome: decision.AttemptError, wantErr: ErrBadResponse},
		{name: "a response that reports zero usage reports none", doer: &fakeDoer{body: strings.Replace(response(fullAnswers()...), `"usage":{"input_tokens":100,"output_tokens":20}`, `"usage":{}`, 1)},
			outcome: decision.AttemptDecided, model: testModel},
		{name: "a response that has no usage at all", doer: &fakeDoer{body: strings.Replace(response(fullAnswers()...), `,"usage":{"input_tokens":100,"output_tokens":20}`, ``, 1)},
			outcome: decision.AttemptDecided, model: testModel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, tc.doer, func(c *Config) {
				if strings.Contains(tc.name, "too large") {
					c.MaxResponseBytes = 10
				}
			})
			_, ok, rep, err := c.DecideTraced(context.Background(), decideRequest())
			a := oneAttempt(t, rep)
			if a.Provider != DefaultName || a.Outcome != tc.outcome || a.Detail != tc.detail && tc.wantErr == nil || a.Latency < 0 || a.Role != "" {
				t.Fatalf("attempt = %+v", a)
			}
			if rep.Model != tc.model {
				t.Fatalf("model = %q", rep.Model)
			}
			if tc.wantErr != nil {
				if ok || !errors.Is(err, tc.wantErr) || a.Detail != err.Error() {
					t.Fatalf("ok=%v err=%v detail=%q", ok, err, a.Detail)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if tc.usage == nil {
				if a.Usage != nil {
					t.Fatalf("a call that reported no usage must carry nil, not %+v", a.Usage)
				}
				return
			}
			wantUsage(t, a.Usage, tc.usage[0], tc.usage[1])
		})
	}
}

func TestDecideTraced_ALocallyRefusedRequestReportsAnErrorAttemptWithoutUsage(t *testing.T) {
	d := &fakeDoer{body: response(fullAnswers()...)}
	c := newClient(t, d, func(c *Config) { c.MaxStateTokens = 1 })
	_, ok, rep, err := c.DecideTraced(context.Background(), decideRequest())
	a := oneAttempt(t, rep)
	if ok || !errors.Is(err, ErrStateTooLarge) || a.Outcome != decision.AttemptError || a.Usage != nil || d.calls != 0 {
		t.Fatalf("ok=%v err=%v attempt=%+v calls=%d", ok, err, a, d.calls)
	}
}

func TestOnCall_ReportsUsageOfAFailedCall(t *testing.T) {
	var ev CallEvent
	d := &fakeDoer{status: 500, body: `{"usage":{"input_tokens":5,"output_tokens":6}}`}
	c := newClient(t, d, func(c *Config) { c.OnCall = func(e CallEvent) { ev = e } })
	if _, err := c.Ask(context.Background(), AskRequest{State: "s", Questions: map[string]Question{"q": Noul("x")}}); !errors.Is(err, ErrServer) {
		t.Fatal(err)
	}
	if ev.Usage != (Usage{InputTokens: 5, OutputTokens: 6}) || ev.Err == nil || ev.Model != "" || ev.Status != 500 {
		t.Fatalf("event = %+v", ev)
	}
}

func TestScoreTraced_ReportsUsageForEveryOutcome(t *testing.T) {
	cases := []struct {
		name    string
		doer    *fakeDoer
		req     decision.ScoreRequest
		outcome string
		wantErr error
		usage   *[2]int
	}{
		{name: "decided", doer: &fakeDoer{body: libraryResponse}, req: libraryRequest(), outcome: decision.AttemptDecided, usage: &[2]int{512, 127}},
		{name: "decided without usage", doer: &fakeDoer{body: strings.Replace(libraryResponse, `,"usage":{"input_tokens":512,"output_tokens":127}`, ``, 1)}, req: libraryRequest(), outcome: decision.AttemptDecided},
		{name: "a non-2xx whose body has usage", doer: &fakeDoer{status: 503, body: `{"usage":{"input_tokens":40,"output_tokens":0}}`}, req: libraryRequest(), outcome: decision.AttemptError, wantErr: ErrServer, usage: &[2]int{40, 0}},
		{name: "a network error", doer: &fakeDoer{err: errNetwork}, req: libraryRequest(), outcome: decision.AttemptError, wantErr: errNetwork},
		{name: "a missing answer after a billed call", doer: &fakeDoer{body: `{"model":"jev-1.13.0","answers":{"q1":{"type":"choice","choice":"relate","confidence":0.5,"probabilities":{"relate":1}}},"usage":{"input_tokens":512,"output_tokens":127}}`},
			req: libraryRequest(), outcome: decision.AttemptError, wantErr: ErrBadResponse, usage: &[2]int{512, 127}},
		{name: "an answer that fails validation after a billed call", doer: &fakeDoer{body: strings.Replace(libraryResponse, `"aggregate":0.8,"relate":0.15,"other":0.05`, `"aggregate":0.8,"relate":0.15,"other":0.5`, 1)},
			req: libraryRequest(), outcome: decision.AttemptError, wantErr: ErrBadResponse, usage: &[2]int{512, 127}},
		{name: "a request refused before any call", doer: &fakeDoer{body: libraryResponse}, req: decision.ScoreRequest{}, outcome: decision.AttemptError, wantErr: ErrInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, tc.doer)
			res, rep, err := c.ScoreTraced(context.Background(), tc.req)
			a := oneAttempt(t, rep)
			if a.Provider != DefaultName || a.Outcome != tc.outcome || a.Latency < 0 {
				t.Fatalf("attempt = %+v", a)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || a.Detail != err.Error() || res.Answers != nil {
					t.Fatalf("err=%v detail=%q res=%+v", err, a.Detail, res)
				}
			} else if err != nil || a.Detail != "" || rep.Model != testModel || res.Engine != DefaultName {
				t.Fatalf("err=%v attempt=%+v rep=%+v", err, a, rep)
			}
			if tc.usage == nil {
				if a.Usage != nil {
					t.Fatalf("a call that reported no usage must carry nil, not %+v", a.Usage)
				}
				return
			}
			wantUsage(t, a.Usage, tc.usage[0], tc.usage[1])
		})
	}
}

// The Chain keeps the engine's usage when it judges the answer, and classifies a
// failure itself: the attempt's usage survives either way.
func TestChain_KeepsTheUsageOfEveryCall(t *testing.T) {
	judged := func(body string, status int) (decision.Trace, bool) {
		pol := decision.NarrowingPolicy()
		c := newClient(t, &fakeDoer{status: status, body: body})
		_, ok, tr := decision.Chain{Providers: []decision.Provider{c}, Policy: &pol}.Decide(context.Background(), decideRequest())
		return tr, ok
	}
	uncertain := []string{
		choiceJSON("intent", "calendar/show", 0.3, `"calendar/show":0.4,"calendar/create":0.35,"contacts/find":0.15,"other":0.1`),
		noulJSON("scope0", 0.95), noulJSON("scope1", 0.2), noulJSON("scope2", 0.9), noulJSON("data0", 0.7),
		choiceJSON("entity", "happening", 0.8, `"happening":0.9,"contact":0.05,"none":0.05`),
		choiceJSON("presentation", "day_calendar", 0.9, `"day_calendar":0.95,"list":0.03,"none":0.02`),
		interactionAnswer("question", 0.9),
	}
	cases := []struct {
		name    string
		body    string
		status  int
		ok      bool
		outcome string
		usage   *[2]int
	}{
		{name: "decided", body: response(fullAnswers()...), ok: true, outcome: decision.AttemptDecided, usage: &[2]int{100, 20}},
		{name: "uncertain", body: response(uncertain...), outcome: decision.AttemptUncertain, usage: &[2]int{100, 20}},
		{name: "abstained", body: response(choiceJSON("intent", "other", 0.9, `"other":0.9,"calendar/show":0.1`), interactionAnswer("question", 0.9)), outcome: decision.AttemptAbstained, usage: &[2]int{100, 20}},
		{name: "failed with usage", status: 500, body: `{"usage":{"input_tokens":3,"output_tokens":1}}`, outcome: decision.AttemptError, usage: &[2]int{3, 1}},
		{name: "refused credentials with usage", status: 401, body: `{"usage":{"input_tokens":2,"output_tokens":0}}`, outcome: decision.AttemptAuth, usage: &[2]int{2, 0}},
		{name: "rejected request with usage", status: 400, body: `{"usage":{"input_tokens":2,"output_tokens":0}}`, outcome: decision.AttemptRejected, usage: &[2]int{2, 0}},
		{name: "failed without usage", status: 500, body: `{}`, outcome: decision.AttemptError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, ok := judged(tc.body, tc.status)
			if ok != tc.ok || len(tr.Attempts) != 1 {
				t.Fatalf("ok=%v trace=%+v", ok, tr)
			}
			a := tr.Attempts[0]
			if a.Provider != DefaultName || a.Outcome != tc.outcome {
				t.Fatalf("attempt = %+v", a)
			}
			if tc.usage == nil {
				if a.Usage != nil {
					t.Fatalf("usage = %+v, want nil", a.Usage)
				}
				return
			}
			wantUsage(t, a.Usage, tc.usage[0], tc.usage[1])
		})
	}
}

// A timeout is classified by the Chain; the call never returned, so its usage is
// unknown (nil), not zero.
func TestChain_ATimedOutCallHasNoUsage(t *testing.T) {
	pol := decision.NarrowingPolicy()
	c := newClient(t, &fakeDoer{err: context.DeadlineExceeded}, func(c *Config) { c.Timeout = time.Second })
	_, ok, tr := decision.Chain{Providers: []decision.Provider{c}, Policy: &pol}.Decide(context.Background(), decideRequest())
	if ok || len(tr.Attempts) != 1 || tr.Attempts[0].Outcome != decision.AttemptTimeout || tr.Attempts[0].Usage != nil {
		t.Fatalf("ok=%v trace=%+v", ok, tr)
	}
}
