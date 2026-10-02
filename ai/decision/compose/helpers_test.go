package compose

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

// ---- fake clock ----

type fakeTimer struct {
	c       *fakeClock
	at      time.Time
	f       func()
	stopped bool
	fired   bool
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	cond   *sync.Cond
}

func newFakeClock() *fakeClock {
	c := &fakeClock{now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	c.cond.Broadcast()
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped && !t.fired
	t.stopped = true
	t.c.cond.Broadcast()
	return was
}

// Advance moves time forward and runs every due timer.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	for _, t := range c.timers {
		if !t.stopped && !t.fired && !t.at.After(c.now) {
			t.fired = true
			due = append(due, t)
		}
	}
	c.mu.Unlock()
	for _, t := range due {
		t.f()
	}
}

// WaitTimers blocks until n timers are pending (neither fired nor stopped).
func (c *fakeClock) WaitTimers(t *testing.T, n int) {
	t.Helper()
	deadline := time.AfterFunc(5*time.Second, func() { c.cond.Broadcast() })
	defer deadline.Stop()
	start := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.pending() != n {
		if time.Since(start) > 4*time.Second {
			t.Fatalf("timed out waiting for %d pending timers, have %d", n, c.pending())
		}
		c.cond.Wait()
	}
}

func (c *fakeClock) pending() int {
	n := 0
	for _, t := range c.timers {
		if !t.stopped && !t.fired {
			n++
		}
	}
	return n
}

// ---- fake providers ----

// fake is a decision.Provider and decision.ScoredProvider with scripted
// behaviour. It counts calls and goroutines that have not yet returned.
type fake struct {
	name    string
	timeout time.Duration
	decide  func(ctx context.Context, req decision.Request) (decision.Decision, bool, error)
	score   func(ctx context.Context, req decision.ScoreRequest) (decision.ScoreResult, error)

	calls   atomic.Int32
	running atomic.Int32
}

func (f *fake) Name() string { return f.name }
func (f *fake) DecisionTimeout() time.Duration {
	return f.timeout
}

func (f *fake) Decide(ctx context.Context, req decision.Request) (decision.Decision, bool, error) {
	f.calls.Add(1)
	f.running.Add(1)
	defer f.running.Add(-1)
	return f.decide(ctx, req)
}

func (f *fake) Score(ctx context.Context, req decision.ScoreRequest) (decision.ScoreResult, error) {
	f.calls.Add(1)
	f.running.Add(1)
	defer f.running.Add(-1)
	return f.score(ctx, req)
}

// noTimeout is a Provider with no DecisionTimeout and no Score method.
type noTimeout struct{ f *fake }

func (n noTimeout) Name() string { return n.f.name }
func (n noTimeout) Decide(ctx context.Context, r decision.Request) (decision.Decision, bool, error) {
	return n.f.Decide(ctx, r)
}

func taxonomy() decision.Taxonomy {
	return decision.Taxonomy{Modules: []decision.ModuleSpec{{Name: "m", Intents: []string{"i", "j"}}}}
}

func request() decision.Request {
	return decision.Request{Product: "p", Text: "hello", Taxonomy: taxonomy()}
}

func answer(intent string, conf float64) decision.Decision {
	return decision.Decision{
		Module: decision.Scored{Value: "m", Confidence: conf}, Intent: decision.Scored{Value: intent, Confidence: conf},
		Interaction: decision.InteractionCommand,
	}
}

// instant answers immediately.
func instant(name, intent string) *fake {
	return &fake{name: name, decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return answer(intent, 0.9), true, nil
	}}
}

// failing returns err immediately.
func failing(name string, err error) *fake {
	return &fake{name: name, decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return decision.Decision{}, false, err
	}}
}

// abstaining abstains immediately.
func abstaining(name string) *fake {
	return &fake{name: name, decide: func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return decision.Decision{}, false, nil
	}}
}

// cooperative blocks until its context is done and returns ctx.Err().
func cooperative(name string) *fake {
	return &fake{name: name, decide: func(ctx context.Context, _ decision.Request) (decision.Decision, bool, error) {
		<-ctx.Done()
		return decision.Decision{}, false, ctx.Err()
	}}
}

// gated blocks until released, or its context is done, then answers.
type gated struct {
	*fake
	release chan struct{}
}

func newGated(name, intent string) *gated {
	g := &gated{release: make(chan struct{})}
	g.fake = &fake{name: name, decide: func(ctx context.Context, _ decision.Request) (decision.Decision, bool, error) {
		select {
		case <-g.release:
			return answer(intent, 0.9), true, nil
		case <-ctx.Done():
			return decision.Decision{}, false, ctx.Err()
		}
	}}
	return g
}

// waitCalls blocks until the fake has been called n times.
func waitCalls(t *testing.T, f *fake, n int32) {
	t.Helper()
	waitFor(t, func() bool { return f.calls.Load() >= n }, "calls")
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// noLeak fails the test if goroutines started during it are still alive shortly
// after it ends. Tests using it must not be parallel.
func noLeak(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for runtime.NumGoroutine() > before {
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<16)
				n := runtime.Stack(buf, true)
				t.Fatalf("goroutine leak: %d before, %d after\n%s", before, runtime.NumGoroutine(), buf[:n])
			}
			time.Sleep(time.Millisecond)
		}
	})
}

func attemptOutcomes(rep decision.Report) []string {
	var out []string
	for _, a := range rep.Attempts {
		out = append(out, a.Provider+":"+a.Outcome)
	}
	return out
}
