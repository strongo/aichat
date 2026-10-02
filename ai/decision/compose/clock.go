package compose

import (
	"context"
	"time"
)

// Clock is the source of time the combinators and the breaker use, so tests can
// drive timeouts, hedge delays and cooldowns deterministically.
type Clock interface {
	Now() time.Time
	// AfterFunc calls f in its own goroutine after d, unless stopped first.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a pending AfterFunc.
type Timer interface {
	// Stop prevents the call if it has not happened yet.
	Stop() bool
}

type systemClock struct{}

// SystemClock is the real clock.
func SystemClock() Clock { return systemClock{} }

func (systemClock) Now() time.Time { return time.Now() }
func (systemClock) AfterFunc(d time.Duration, f func()) Timer {
	return time.AfterFunc(d, f)
}

// withTimeout derives a context that is cancelled with the cause
// context.DeadlineExceeded after d on clk (so deadlines follow a fake clock in
// tests, and callers can tell a timeout from a cancellation). The returned
// function releases the timer and the context.
func withTimeout(ctx context.Context, clk Clock, d time.Duration) (context.Context, func()) {
	c, cancel := context.WithCancelCause(ctx)
	t := clk.AfterFunc(d, func() { cancel(context.DeadlineExceeded) })
	return c, func() { t.Stop(); cancel(nil) }
}
