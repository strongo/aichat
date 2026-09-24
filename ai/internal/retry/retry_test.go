package retry

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

type retryableErr struct{ retryable bool }

func (e retryableErr) Error() string     { return "boom" }
func (e retryableErr) IsRetryable() bool { return e.retryable }

func TestDo_SucceedsFirstTry(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Config{}, func(ctx context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestDo_RetriesRetryableThenSucceeds(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Config{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}, func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return retryableErr{retryable: true}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestDo_StopsOnNonRetryable(t *testing.T) {
	calls := 0
	wantErr := retryableErr{retryable: false}
	err := Do(context.Background(), Config{MaxAttempts: 5, BaseDelay: time.Millisecond}, func(ctx context.Context) error {
		calls++
		return wantErr
	})
	if !errors.Is(err, error(wantErr)) && err != wantErr {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (must not retry a non-retryable error)", calls)
	}
}

func TestDo_PlainErrorNotRetried(t *testing.T) {
	calls := 0
	plain := errors.New("plain")
	err := Do(context.Background(), Config{MaxAttempts: 5, BaseDelay: time.Millisecond}, func(ctx context.Context) error {
		calls++
		return plain
	})
	if !errors.Is(err, plain) {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (a plain error doesn't implement Retryable)", calls)
	}
}

func TestDo_ExhaustsAttempts(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Config{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}, func(ctx context.Context) error {
		calls++
		return retryableErr{retryable: true}
	})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestDo_ContextCancelledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	err := Do(ctx, Config{MaxAttempts: 10, BaseDelay: 50 * time.Millisecond, MaxDelay: 50 * time.Millisecond}, func(ctx context.Context) error {
		calls++
		return retryableErr{retryable: true}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls > 2 {
		t.Fatalf("calls = %d, want cancellation to cut the loop short", calls)
	}
}

func TestConfig_Defaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.MaxAttempts != 3 || c.BaseDelay != 250*time.Millisecond || c.MaxDelay != 4*time.Second || c.Jitter != 0.2 {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestJittered_BoundedAndNonNegative(t *testing.T) {
	for i := 0; i < 100; i++ {
		d := jittered(100*time.Millisecond, 0.5)
		if d < 0 {
			t.Fatalf("jittered returned negative: %v", d)
		}
		if d > 200*time.Millisecond {
			t.Fatalf("jittered too large: %v", d)
		}
	}
	if jittered(100*time.Millisecond, 0) != 100*time.Millisecond {
		t.Fatal("zero jitter must return d unchanged")
	}
}

func TestJittered_NegativeOffsetClampedToZero(t *testing.T) {
	// With a base d tiny relative to the jitter fraction, roughly half of
	// draws produce a negative raw offset; jittered must clamp those to 0
	// rather than returning a negative duration. Loop until we observe the
	// clamp (P(missing it 50 times running) is astronomically small) rather
	// than relying on a single random draw.
	sawZero := false
	for i := 0; i < 200; i++ {
		if jittered(time.Millisecond, 1000) == 0 {
			sawZero = true
			break
		}
	}
	if !sawZero {
		t.Fatal("expected at least one clamped-to-zero result across 200 draws")
	}
}

func TestWaitOnRetryAfter_EmptyHeaderNoop(t *testing.T) {
	WaitOnRetryAfter(context.Background(), "") // must return immediately, no panic
}

func TestWaitOnRetryAfter_UnparseableHeaderNoop(t *testing.T) {
	WaitOnRetryAfter(context.Background(), "not-a-duration-or-date")
}

func TestWaitOnRetryAfter_NonPositiveSecondsNoop(t *testing.T) {
	WaitOnRetryAfter(context.Background(), "0")
	WaitOnRetryAfter(context.Background(), "-5")
}

func TestWaitOnRetryAfter_PastHTTPDateNoop(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).UTC().Format(http.TimeFormat)
	WaitOnRetryAfter(context.Background(), past)
}

func TestWaitOnRetryAfter_CtxAlreadyDoneReturnsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	WaitOnRetryAfter(ctx, "30") // would sleep 30s if ctx.Done() weren't honoured
	if time.Since(start) > time.Second {
		t.Fatalf("took %v, want near-instant return via ctx.Done()", time.Since(start))
	}
}

func TestWaitOnRetryAfter_SecondsHeaderSleepsViaSeam(t *testing.T) {
	orig := waitOnRetryAfterSleep
	defer func() { waitOnRetryAfterSleep = orig }()
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	var gotDelay time.Duration
	waitOnRetryAfterSleep = func(d time.Duration) <-chan time.Time {
		gotDelay = d
		return ch
	}
	WaitOnRetryAfter(context.Background(), "5")
	if gotDelay != 5*time.Second {
		t.Errorf("delay = %v, want 5s", gotDelay)
	}
}

func TestWaitOnRetryAfter_HTTPDateHeaderSleepsViaSeam(t *testing.T) {
	orig := waitOnRetryAfterSleep
	defer func() { waitOnRetryAfterSleep = orig }()
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	var gotDelay time.Duration
	waitOnRetryAfterSleep = func(d time.Duration) <-chan time.Time {
		gotDelay = d
		return ch
	}
	future := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	WaitOnRetryAfter(context.Background(), future)
	if gotDelay <= 0 || gotDelay > 4*time.Second {
		t.Errorf("delay = %v, want roughly 3s", gotDelay)
	}
}

func TestWaitOnRetryAfter_CappedAtMaxWait(t *testing.T) {
	orig := waitOnRetryAfterSleep
	defer func() { waitOnRetryAfterSleep = orig }()
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	var gotDelay time.Duration
	waitOnRetryAfterSleep = func(d time.Duration) <-chan time.Time {
		gotDelay = d
		return ch
	}
	WaitOnRetryAfter(context.Background(), "3600") // 1 hour, way over the 30s cap
	if gotDelay != 30*time.Second {
		t.Errorf("delay = %v, want capped at 30s", gotDelay)
	}
}
