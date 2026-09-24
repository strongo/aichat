// Package retry is a tiny internal helper shared by the LLMProvider HTTP
// adapters (ai/openaicompat, ai/anthropic, ai/cloud). It bounds retries with
// backoff+jitter, and only before the first byte of a stream: once a caller
// has started reading from the response body, retrying would duplicate or
// corrupt output, so adapters must stop calling this helper at that point.
package retry

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Config bounds a retry loop.
type Config struct {
	// MaxAttempts is the total number of tries, including the first
	// (default 3).
	MaxAttempts int
	// BaseDelay is the delay before the first retry (default 250ms),
	// doubled on each subsequent attempt (capped by MaxDelay).
	BaseDelay time.Duration
	// MaxDelay caps the backoff delay (default 4s).
	MaxDelay time.Duration
	// Jitter is the fraction of the computed delay randomised, in [0,1]
	// (default 0.2: +/-20%).
	Jitter float64
}

func (c Config) withDefaults() Config {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.BaseDelay <= 0 {
		c.BaseDelay = 250 * time.Millisecond
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = 4 * time.Second
	}
	if c.Jitter <= 0 {
		c.Jitter = 0.2
	}
	return c
}

// Retryable is implemented by errors that can say whether they are worth
// retrying (e.g. *ai.Error).
type Retryable interface {
	error
	IsRetryable() bool
}

// Do calls fn until it succeeds, returns a non-retryable error, or attempts
// are exhausted. fn must not have produced any observable output (e.g. must
// not have streamed bytes to a consumer) by the time it returns an error --
// Do is only for "before the first byte" retries; once streaming begins the
// caller must handle errors itself and never call Do again for that stream.
func Do(ctx context.Context, cfg Config, fn func(ctx context.Context) error) error {
	cfg = cfg.withDefaults()
	var lastErr error
	delay := cfg.BaseDelay
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			d := jittered(delay, cfg.Jitter)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
			delay *= 2
			if delay > cfg.MaxDelay {
				delay = cfg.MaxDelay
			}
		}
		err := fn(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		var r Retryable
		if !errors.As(err, &r) || !r.IsRetryable() {
			return err
		}
	}
	return lastErr
}

// WaitOnRetryAfter blocks for the duration a 429 response's Retry-After
// header asks for (seconds, or an HTTP-date), up to a sane cap, before the
// caller's own retry-loop backoff runs. It never blocks past ctx
// cancellation and silently does nothing for an empty or unparseable
// header. Shared by every HTTP adapter (ai/openaicompat, ai/anthropic) that
// wants to honour a server's own requested wait ahead of Do's backoff+jitter.
func WaitOnRetryAfter(ctx context.Context, header string) {
	if header == "" {
		return
	}
	var d time.Duration
	if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(header); err == nil {
		d = time.Until(t)
	} else {
		return
	}
	if d <= 0 {
		return
	}
	const maxWait = 30 * time.Second
	if d > maxWait {
		d = maxWait
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func jittered(d time.Duration, frac float64) time.Duration {
	if frac <= 0 {
		return d
	}
	delta := float64(d) * frac
	// random in [-delta, +delta]
	offset := (rand.Float64()*2 - 1) * delta
	out := time.Duration(float64(d) + offset)
	if out < 0 {
		out = 0
	}
	return out
}
