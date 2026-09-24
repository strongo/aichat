package retry

import (
	"context"
	"errors"
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
