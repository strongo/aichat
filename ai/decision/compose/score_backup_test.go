package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"iter"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/decision/llmdecider"
	"github.com/strongo/aichat/ai/decision/typesafe"
)

// These tests wire the real engines together: the TypeSafe client (over a fake
// HTTP transport) as the primary and the LLM decider (over a fake LLM) as the
// backup, each behind a circuit breaker, and ask a SCORED question (table
// narrowing). With Jev down, the answer must come from the LLM, and the policy
// must still narrow, as a proposal.

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// jevDown answers every call with HTTP 500.
func jevDown(calls *atomic.Int32) doerFunc {
	return func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}
}

// jevHangs blocks until the request's context is done.
func jevHangs(calls *atomic.Int32) doerFunc {
	return func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
}

const llmScores = `{"answers":[
 {"questionId":"pick","scores":[{"id":"a","probability":0.8},{"id":"b","probability":0.15},{"id":"none","probability":0.05}]},
 {"questionId":"rel","scores":[{"id":"a","probability":0.9},{"id":"b","probability":0.2}]}]}`

// scriptedLLM answers every call with the canned scores.
type scriptedLLM struct{ calls atomic.Int32 }

func (*scriptedLLM) Name() string { return "haiku" }
func (l *scriptedLLM) Stream(context.Context, ai.ChatRequest) iter.Seq2[ai.Event, error] {
	l.calls.Add(1)
	return func(yield func(ai.Event, error) bool) {
		if yield(ai.Event{Type: ai.EventStructured, Structured: json.RawMessage(llmScores)}, nil) {
			yield(ai.Event{Type: ai.EventCompleted}, nil)
		}
	}
}

type backupRig struct {
	clk    *fakeClock
	jev    *typesafe.Client
	jevB   *Breaker
	llm    *llmdecider.Decider
	llmB   *Breaker
	jevHit atomic.Int32
	llmHit *scriptedLLM
}

func newBackupRig(t *testing.T, jev func(*atomic.Int32) doerFunc, breakerOpts ...BreakerOption) *backupRig {
	t.Helper()
	r := &backupRig{clk: newFakeClock(), llmHit: &scriptedLLM{}}
	var err error
	r.jev, err = typesafe.New(typesafe.Config{APIKey: "test-key-not-a-secret", Model: "jev-1.13.0", Name: "jev", HTTPClient: jev(&r.jevHit), Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r.llm = llmdecider.New(r.llmHit, llmdecider.Options{Name: "haiku"})
	opts := append([]BreakerOption{WithBreakerClock(r.clk)}, breakerOpts...)
	r.jevB, r.llmB = NewBreaker(r.jev, opts...), NewBreaker(r.llm, opts...)
	return r
}

type scored struct {
	res decision.ScoreResult
	rep decision.Report
	err error
}

func (r *backupRig) start(e *Engine) <-chan scored {
	ch := make(chan scored, 1)
	go func() {
		res, rep, err := e.ScoreTraced(context.Background(), scoreRequest())
		ch <- scored{res, rep, err}
	}()
	return ch
}

// expectLLMAnswered checks the answer came from the LLM and that the narrowing
// policy still narrows with it, as a proposal and never a selection.
func expectLLMAnswered(t *testing.T, s scored, wantAttempts ...string) {
	t.Helper()
	if s.err != nil || s.res.Engine != "haiku" || s.rep.Engine != "haiku" {
		t.Fatalf("err=%v engine=%q report=%+v", s.err, s.res.Engine, s.rep)
	}
	if got := attemptOutcomes(s.rep); !reflect.DeepEqual(got, wantAttempts) {
		t.Fatalf("attempts = %v, want %v", got, wantAttempts)
	}
	rel := s.res.Answers["rel"]
	if rel.Calibrated || rel.HasConfidence {
		t.Fatalf("an LLM answer must not look calibrated: %+v", rel)
	}
	sel := decision.NarrowingPolicy().Evaluate(rel)
	if sel.Outcome != decision.OutcomeUnscored || !sel.Proposal || sel.Actionable() || !reflect.DeepEqual(sel.Picks, []string{"a"}) {
		t.Fatalf("the policy must narrow with a proposal: %+v", sel)
	}
	pick := decision.NarrowingPolicy().Evaluate(s.res.Answers["pick"])
	if !pick.Proposal || !reflect.DeepEqual(pick.Picks, []string{"a"}) {
		t.Fatalf("choice proposal: %+v", pick)
	}
}

func TestScoreFallback_JevErrors(t *testing.T) {
	noLeak(t)
	r := newBackupRig(t, jevDown)
	e := Fallback(r.jevB, r.llmB, WithClock(r.clk))
	s := <-r.start(e)
	expectLLMAnswered(t, s, "jev:error", "haiku:decided")
	if !s.rep.FallbackFired || r.jevHit.Load() != 1 || r.llmHit.calls.Load() != 1 {
		t.Fatalf("rep=%+v jev=%d llm=%d", s.rep, r.jevHit.Load(), r.llmHit.calls.Load())
	}
}

func TestScoreFallback_JevTimesOut(t *testing.T) {
	noLeak(t)
	r := newBackupRig(t, jevHangs)
	e := Fallback(r.jevB, r.llmB, WithClock(r.clk))
	ch := r.start(e)
	r.clk.WaitTimers(t, 1) // jev's 2s leg timeout
	r.clk.Advance(2 * time.Second)
	s := <-ch
	expectLLMAnswered(t, s, "jev:timeout", "haiku:decided")
}

func TestScoreFallback_JevBreakerOpen(t *testing.T) {
	noLeak(t)
	r := newBackupRig(t, jevDown, WithBreakerThreshold(1))
	e := Fallback(r.jevB, r.llmB, WithClock(r.clk))
	expectLLMAnswered(t, <-r.start(e), "jev:error", "haiku:decided") // opens the breaker
	if r.jevB.State() != BreakerOpen {
		t.Fatalf("state = %v", r.jevB.State())
	}
	s := <-r.start(e)
	expectLLMAnswered(t, s, "jev:unavailable", "haiku:decided")
	if r.jevHit.Load() != 1 {
		t.Fatalf("an open breaker still called Jev: %d calls", r.jevHit.Load())
	}
}

func TestScoreHedged_JevHangsOrFails(t *testing.T) {
	noLeak(t)
	// Slow: the hedge fires, the LLM answers, the hung Jev is cancelled.
	r := newBackupRig(t, jevHangs)
	e := Hedged(r.jevB, r.llmB, 100*time.Millisecond, WithClock(r.clk))
	ch := r.start(e)
	r.clk.WaitTimers(t, 2)
	r.clk.Advance(100 * time.Millisecond)
	s := <-ch
	expectLLMAnswered(t, s, "jev:cancelled", "haiku:decided")
	if !s.rep.HedgeFired {
		t.Fatalf("rep = %+v", s.rep)
	}

	// Down: the backup starts at once, no hedge delay.
	r = newBackupRig(t, jevDown)
	s = <-r.start(Hedged(r.jevB, r.llmB, time.Hour, WithClock(r.clk)))
	expectLLMAnswered(t, s, "jev:error", "haiku:decided")
	if s.rep.HedgeFired || !s.rep.FallbackFired {
		t.Fatalf("rep = %+v", s.rep)
	}

	// Breaker open: same, without calling Jev.
	r = newBackupRig(t, jevDown, WithBreakerThreshold(1))
	e = Hedged(r.jevB, r.llmB, time.Hour, WithClock(r.clk))
	<-r.start(e)
	s = <-r.start(e)
	expectLLMAnswered(t, s, "jev:unavailable", "haiku:decided")
	if r.jevHit.Load() != 1 {
		t.Fatalf("jev calls = %d", r.jevHit.Load())
	}
}

func TestScoreRace_JevHangsOrFails(t *testing.T) {
	noLeak(t)
	r := newBackupRig(t, jevHangs)
	s := <-r.start(Race([]decision.Provider{r.jevB, r.llmB}, WithClock(r.clk)))
	expectLLMAnswered(t, s, "jev:cancelled", "haiku:decided")

	r = newBackupRig(t, jevDown)
	s = <-r.start(Race([]decision.Provider{r.jevB, r.llmB}, WithClock(r.clk)))
	if s.err != nil || s.res.Engine != "haiku" {
		t.Fatalf("err=%v engine=%q", s.err, s.res.Engine)
	}

	r = newBackupRig(t, jevDown, WithBreakerThreshold(1))
	e := Race([]decision.Provider{r.jevB, r.llmB}, WithClock(r.clk))
	// Open the breaker directly: in a race the LLM may win before Jev's failure
	// is seen, and a cancelled loser is not a failure.
	if _, err := r.jevB.Score(context.Background(), scoreRequest()); err == nil || r.jevB.State() != BreakerOpen {
		t.Fatalf("err=%v state=%v", err, r.jevB.State())
	}
	hitsBefore := r.jevHit.Load()
	s = <-r.start(e)
	// In a race the LLM may answer before the open breaker has even refused Jev,
	// in which case Jev is recorded cancelled; either way Jev was never called.
	if got := attemptOutcomes(s.rep); len(got) != 2 || (got[0] != "jev:unavailable" && got[0] != "jev:cancelled") {
		t.Fatalf("attempts = %v", got)
	}
	if s.err != nil || s.res.Engine != "haiku" || r.jevHit.Load() != hitsBefore {
		t.Fatalf("err=%v engine=%q jev calls %d -> %d", s.err, s.res.Engine, hitsBefore, r.jevHit.Load())
	}
}
