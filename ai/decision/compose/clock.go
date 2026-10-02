package compose

import "time"

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
