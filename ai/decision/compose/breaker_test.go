package compose

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

type transition struct {
	engine   string
	from, to BreakerState
}

// breakerRig is a Breaker over a scripted provider, with a fake clock and a
// record of state transitions.
type breakerRig struct {
	b     *Breaker
	clk   *fakeClock
	p     *fake
	mu    sync.Mutex
	moves []transition
}

func newRig(t *testing.T, opts ...BreakerOption) *breakerRig {
	t.Helper()
	r := &breakerRig{clk: newFakeClock(), p: &fake{name: "jev"}}
	r.p.decide = func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return answer("i", 0.9), true, nil
	}
	all := append([]BreakerOption{
		WithBreakerClock(r.clk),
		WithBreakerOnChange(func(e string, from, to BreakerState) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.moves = append(r.moves, transition{e, from, to})
		}),
	}, opts...)
	r.b = NewBreaker(r.p, all...)
	return r
}

func (r *breakerRig) fail(err error) {
	r.p.decide = func(context.Context, decision.Request) (decision.Decision, bool, error) {
		return decision.Decision{}, false, err
	}
}

func (r *breakerRig) call(t *testing.T) error {
	t.Helper()
	_, _, err := r.b.Decide(context.Background(), request())
	return err
}

func (r *breakerRig) moveList() []transition {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]transition(nil), r.moves...)
}

func TestBreaker_TransparentWhenHealthy(t *testing.T) {
	r := newRig(t)
	r.p.timeout = 2 * time.Second
	d, ok, err := r.b.Decide(context.Background(), request())
	if err != nil || !ok || d.Intent.Value != "i" {
		t.Fatalf("d=%+v ok=%v err=%v", d, ok, err)
	}
	if r.b.Name() != "jev" || r.b.DecisionTimeout() != 2*time.Second || r.b.State() != BreakerClosed {
		t.Fatalf("name=%q timeout=%v state=%v", r.b.Name(), r.b.DecisionTimeout(), r.b.State())
	}
	// A wrapped provider with no DecisionTimeout reports none.
	if got := NewBreaker(noTimeout{r.p}).DecisionTimeout(); got != 0 {
		t.Fatalf("timeout = %v", got)
	}
}

func TestBreaker_OpensAfterFiveConsecutiveFailuresThenFailsFast(t *testing.T) {
	r := newRig(t)
	r.fail(errBoom)
	for i := 0; i < DefaultBreakerThreshold-1; i++ {
		if err := r.call(t); !errors.Is(err, errBoom) {
			t.Fatalf("call %d: %v", i, err)
		}
		if r.b.State() != BreakerClosed {
			t.Fatalf("opened early, after %d failures", i+1)
		}
	}
	if err := r.call(t); !errors.Is(err, errBoom) || r.b.State() != BreakerOpen {
		t.Fatalf("fifth failure: err=%v state=%v", err, r.b.State())
	}
	calls := r.p.calls.Load()
	err := r.call(t)
	if !errors.Is(err, decision.ErrUnavailable) || r.p.calls.Load() != calls {
		t.Fatalf("open breaker called the engine: err=%v calls %d -> %d", err, calls, r.p.calls.Load())
	}
	if want := []transition{{"jev", BreakerClosed, BreakerOpen}}; !reflect.DeepEqual(r.moveList(), want) {
		t.Fatalf("moves = %v", r.moveList())
	}
}

func TestBreaker_SuccessResetsTheRun(t *testing.T) {
	r := newRig(t, WithBreakerThreshold(3))
	r.fail(errBoom)
	_ = r.call(t)
	_ = r.call(t)
	r.p.decide = instant("jev", "i").decide
	if err := r.call(t); err != nil {
		t.Fatal(err)
	}
	r.fail(errBoom)
	_ = r.call(t)
	_ = r.call(t)
	if r.b.State() != BreakerClosed {
		t.Fatal("two failures after a success must not open a threshold-3 breaker")
	}
}

func TestBreaker_FailuresOutsideTheWindowDoNotAccumulate(t *testing.T) {
	r := newRig(t, WithBreakerThreshold(2), WithBreakerWindow(10*time.Second))
	r.fail(errBoom)
	_ = r.call(t)
	r.clk.Advance(11 * time.Second)
	_ = r.call(t) // starts a new run: one failure
	if r.b.State() != BreakerClosed {
		t.Fatal("failures 11s apart opened a 10s-window breaker")
	}
	r.clk.Advance(5 * time.Second)
	_ = r.call(t) // second failure inside the window of the new run
	if r.b.State() != BreakerOpen {
		t.Fatalf("state = %v", r.b.State())
	}
}

func TestBreaker_HalfOpenProbeRecovers(t *testing.T) {
	r := newRig(t, WithBreakerThreshold(1), WithBreakerCooldown(30*time.Second))
	r.fail(errBoom)
	_ = r.call(t)
	r.clk.Advance(29 * time.Second)
	if err := r.call(t); !errors.Is(err, decision.ErrUnavailable) {
		t.Fatalf("still cooling down: %v", err)
	}
	r.clk.Advance(time.Second)
	r.p.decide = instant("jev", "i").decide
	if err := r.call(t); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if r.b.State() != BreakerClosed {
		t.Fatalf("state = %v", r.b.State())
	}
	want := []transition{
		{"jev", BreakerClosed, BreakerOpen}, {"jev", BreakerOpen, BreakerHalfOpen}, {"jev", BreakerHalfOpen, BreakerClosed},
	}
	if !reflect.DeepEqual(r.moveList(), want) {
		t.Fatalf("moves = %v", r.moveList())
	}
}

func TestBreaker_FailedProbeReopens(t *testing.T) {
	r := newRig(t, WithBreakerThreshold(1), WithBreakerCooldown(time.Minute))
	r.fail(errBoom)
	_ = r.call(t)
	r.clk.Advance(time.Minute)
	if err := r.call(t); !errors.Is(err, errBoom) || r.b.State() != BreakerOpen {
		t.Fatalf("probe: err=%v state=%v", err, r.b.State())
	}
	// The cooldown restarts from the failed probe.
	r.clk.Advance(59 * time.Second)
	if err := r.call(t); !errors.Is(err, decision.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestBreaker_OnlyOneProbeAtATime(t *testing.T) {
	r := newRig(t, WithBreakerThreshold(1), WithBreakerCooldown(time.Second))
	r.fail(errBoom)
	_ = r.call(t)
	r.clk.Advance(time.Second)

	release := make(chan struct{})
	r.p.decide = func(ctx context.Context, _ decision.Request) (decision.Decision, bool, error) {
		<-release
		return answer("i", 0.9), true, nil
	}
	probeDone := make(chan error, 1)
	go func() { probeDone <- r.call(t) }()
	waitFor(t, func() bool { return r.b.State() == BreakerHalfOpen && r.p.running.Load() == 1 }, "probe to start")
	if err := r.call(t); !errors.Is(err, decision.ErrUnavailable) {
		t.Fatalf("second caller during the probe: %v", err)
	}
	close(release)
	if err := <-probeDone; err != nil {
		t.Fatal(err)
	}
	if r.b.State() != BreakerClosed {
		t.Fatalf("state = %v", r.b.State())
	}
}

func TestBreaker_CancellationIsNotAFailure(t *testing.T) {
	r := newRig(t, WithBreakerThreshold(1))
	r.p.decide = func(ctx context.Context, _ decision.Request) (decision.Decision, bool, error) {
		<-ctx.Done()
		return decision.Decision{}, false, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 3; i++ {
		if _, _, err := r.b.Decide(ctx, request()); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if r.b.State() != BreakerClosed {
		t.Fatalf("a hedge/race loser's cancellation opened the breaker: %v", r.b.State())
	}
}

func TestBreaker_CancelledProbeReleasesTheProbeSlot(t *testing.T) {
	r := newRig(t, WithBreakerThreshold(1), WithBreakerCooldown(time.Second))
	r.fail(errBoom)
	_ = r.call(t)
	r.clk.Advance(time.Second)
	r.p.decide = func(ctx context.Context, _ decision.Request) (decision.Decision, bool, error) {
		return decision.Decision{}, false, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _ = r.b.Decide(ctx, request()) // the probe, cancelled: no verdict
	if r.b.State() != BreakerHalfOpen {
		t.Fatalf("state = %v", r.b.State())
	}
	// The breaker returns without waiting for the engine's goroutine, so wait
	// for it before the test swaps the engine's behaviour.
	waitFor(t, func() bool { return r.p.calls.Load() == 2 && r.p.running.Load() == 0 }, "the probe call to end")
	r.p.decide = instant("jev", "i").decide
	if err := r.call(t); err != nil || r.b.State() != BreakerClosed {
		t.Fatalf("next call should take the probe: err=%v state=%v", err, r.b.State())
	}
}

func TestBreaker_TimeoutsCountAsFailures(t *testing.T) {
	r := newRig(t, WithBreakerThreshold(1))
	r.fail(context.DeadlineExceeded)
	_ = r.call(t)
	if r.b.State() != BreakerOpen {
		t.Fatalf("a wrapped-deadline error must count: %v", r.b.State())
	}

	// A provider that returns the plain cancellation of a context whose CAUSE is
	// a deadline (how Engine enforces per-engine timeouts) is also a timeout.
	r2 := newRig(t, WithBreakerThreshold(1))
	r2.p.decide = func(ctx context.Context, _ decision.Request) (decision.Decision, bool, error) {
		<-ctx.Done()
		return decision.Decision{}, false, ctx.Err()
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(context.DeadlineExceeded)
	_, _, _ = r2.b.Decide(ctx, request())
	if r2.b.State() != BreakerOpen {
		t.Fatalf("deadline-caused cancellation must count: %v", r2.b.State())
	}
}

func TestBreaker_AbstentionAndInvalidAnswersAreHealthy(t *testing.T) {
	r := newRig(t, WithBreakerThreshold(1))
	r.p.decide = abstaining("jev").decide
	for i := 0; i < 3; i++ {
		_, _, _ = r.b.Decide(context.Background(), request())
	}
	if r.b.State() != BreakerClosed {
		t.Fatalf("state = %v", r.b.State())
	}
}

func TestBreaker_ErrorWhileContextDoneButNotCancelledErrorIsNeutral(t *testing.T) {
	// The engine returned some other error after the caller's context ended:
	// whatever went wrong is the caller's doing, not evidence the engine is down.
	r := newRig(t, WithBreakerThreshold(1))
	ctx, cancel := context.WithCancel(context.Background())
	r.p.decide = func(context.Context, decision.Request) (decision.Decision, bool, error) {
		cancel()
		return decision.Decision{}, false, errBoom
	}
	_, _, _ = r.b.Decide(ctx, request())
	if r.b.State() != BreakerClosed {
		t.Fatalf("state = %v", r.b.State())
	}
}

func TestBreaker_ScoredPath(t *testing.T) {
	r := newRig(t, WithBreakerThreshold(1))
	// Not a scored provider: unsupported, and the breaker is untouched.
	if _, err := NewBreaker(noTimeout{r.p}).Score(context.Background(), scoreRequest()); !errors.Is(err, decision.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	r.p.score = func(context.Context, decision.ScoreRequest) (decision.ScoreResult, error) {
		return goodScore("jev"), nil
	}
	if res, err := r.b.Score(context.Background(), scoreRequest()); err != nil || res.Engine != "jev" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	r.p.score = func(context.Context, decision.ScoreRequest) (decision.ScoreResult, error) {
		return decision.ScoreResult{}, errBoom
	}
	_, _ = r.b.Score(context.Background(), scoreRequest())
	if r.b.State() != BreakerOpen {
		t.Fatalf("state = %v", r.b.State())
	}
	if _, err := r.b.Score(context.Background(), scoreRequest()); !errors.Is(err, decision.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	// An engine that reports ErrUnsupported says nothing about its health.
	r2 := newRig(t, WithBreakerThreshold(1))
	r2.p.score = func(context.Context, decision.ScoreRequest) (decision.ScoreResult, error) {
		return decision.ScoreResult{}, decision.ErrUnsupported
	}
	_, _ = r2.b.Score(context.Background(), scoreRequest())
	if r2.b.State() != BreakerClosed {
		t.Fatalf("state = %v", r2.b.State())
	}
}

func TestBreakerState_String(t *testing.T) {
	for s, want := range map[BreakerState]string{BreakerClosed: "closed", BreakerOpen: "open", BreakerHalfOpen: "half-open"} {
		if s.String() != want {
			t.Errorf("%d = %q", s, s.String())
		}
	}
}
