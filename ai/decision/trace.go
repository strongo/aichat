package decision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Attempt outcomes recorded in Attempt.Outcome.
const (
	AttemptDecided       = "decided"
	AttemptAbstained     = "abstained"
	AttemptLowConfidence = "low_confidence"
	AttemptInvalid       = "invalid"
	AttemptError         = "error"
	AttemptTimeout       = "timeout"
	// AttemptUnavailable: a circuit breaker was open, so the engine was not
	// called at all.
	AttemptUnavailable = "unavailable"
	// AttemptCancelled: the engine was started and then cancelled because
	// another engine answered first (a hedge or race loser). It is not a failure
	// of the engine and does not count against a breaker.
	AttemptCancelled = "cancelled"
	// AttemptUncertain: the engine answered, but its calibrated answer was
	// judged uncertain by the combinator's policy.
	AttemptUncertain = "uncertain"
	// AttemptUnsupported: the engine does not implement the asked operation
	// (for example it is not a ScoredProvider).
	AttemptUnsupported = "unsupported"
	// AttemptRejected: the engine refused the request itself as invalid (a
	// caller fault: an oversized state, too many options, a malformed
	// question). It says nothing about the engine's health.
	AttemptRejected = "rejected"
	// AttemptAuth: the engine refused the caller's credentials. Distinct and
	// loud on purpose: retrying or failing over hides a misconfiguration that a
	// person has to fix. It says nothing about the engine's health.
	AttemptAuth = "auth"
	// AttemptQuota: the engine refused because the caller's allowance is
	// exhausted (ErrQuota). It is not a fault of the engine (a circuit breaker
	// ignores it) and not transient (retrying cannot help), so a combinator does
	// not hand the call to a backup unless configured to (compose.OnQuota): a
	// paid backup would silently take over a metered caller's traffic.
	AttemptQuota = "quota"
	// AttemptBudget: a spending cap the caller set on an engine (compose.NewBudget)
	// is used up, so the engine was not called (ErrBudget). Like AttemptQuota it
	// is neither a fault of the engine nor transient, and a chain stops at it by
	// default instead of handing the call to the next, possibly paid, provider.
	AttemptBudget = "budget"
	// AttemptMisconfigured: the engine's endpoint answered in a way that means
	// the CONFIGURATION is wrong (an unknown product, a base URL that does not
	// speak the protocol; ErrMisconfigured). A person has to fix it, so it is
	// loud: a breaker ignores it and a combinator does not hide it behind a
	// backup.
	AttemptMisconfigured = "misconfigured"
)

var (
	// ErrUnavailable is returned (wrapped) by a provider that refuses to call
	// its engine because the engine is known to be down (a circuit breaker is
	// open). Chain records it as AttemptUnavailable.
	ErrUnavailable = errors.New("decision: engine unavailable")
	// ErrUnsupported is returned (wrapped) when an engine does not implement
	// the asked operation.
	ErrUnsupported = errors.New("decision: operation not supported by engine")
	// ErrInvalidRequest is matched (errors.Is) by an engine error that says the
	// REQUEST was refused as invalid (HTTP 400/422, an oversized state, too many
	// options). It is the caller's fault, not the engine's: a circuit breaker
	// does not count it, because tripping everyone's breaker over one caller's
	// bad request would take a healthy engine out of service.
	ErrInvalidRequest = errors.New("decision: request rejected as invalid")
	// ErrAuth is matched by an engine error that says the credentials were
	// refused (HTTP 401/403). It does not count against a circuit breaker
	// either, and is recorded as AttemptAuth so it stays visible.
	ErrAuth = errors.New("decision: authentication failed")
	// ErrQuota is matched by an engine error that says the caller's allowance is
	// exhausted (an HTTP 429 with the error code "quota"). Neither the engine's
	// fault (a breaker ignores it) nor transient: retrying cannot help, and
	// handing the call to a paid backup is an explicit choice (compose.OnQuota).
	// It is recorded as AttemptQuota and surfaced to the caller. A plain
	// rate limit (429 "rate_limited") is NOT a quota: it is transient and counts
	// as an engine-health failure.
	ErrQuota = errors.New("decision: allowance exhausted")
	// ErrBudget is matched by an error that says a spending cap the CALLER set on
	// an engine is used up (compose.NewBudget): the engine was not called. It is the
	// guard for the one case a provider's own errors cannot cover: an engine that
	// answers a transient error (a rate limit it does not tell from a spent
	// account) while a paid backup behind it takes every call. Like ErrQuota it is
	// not the engine's fault (a breaker ignores it), not transient, and not handed
	// to a backup unless compose.OnQuota says so; a Chain stops at it by default
	// (Chain.StopOnQuota). It is recorded as AttemptBudget.
	ErrBudget = errors.New("decision: budget exhausted")
	// ErrMisconfigured is matched by an engine error that says the endpoint is
	// configured wrongly (for example a base URL that does not speak the
	// protocol, or an unknown product). A breaker ignores it and Fallback does
	// not start its backup for it: it must be seen and fixed, not absorbed by an
	// uncalibrated backup forever.
	ErrMisconfigured = errors.New("decision: engine misconfigured")
)

// MaxRetryDelay caps any retry delay an engine asks for, so a bad header cannot
// take an engine out of service for longer.
const MaxRetryDelay = 10 * time.Minute

// ParseRetryAfter reads a Retry-After header value: a number of seconds or an
// HTTP date (relative to now). It returns 0 for a missing, malformed, negative or
// past value, and never more than MaxRetryDelay (a huge number of seconds cannot
// overflow).
func ParseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(min(secs, int(MaxRetryDelay/time.Second)+1)) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	return min(max(d, 0), MaxRetryDelay)
}

// RetryDelayer is implemented by an engine error that carries the delay the
// engine asked callers to wait before trying again (a Retry-After header).
type RetryDelayer interface {
	error
	RetryDelay() time.Duration
}

// RetryDelay returns the delay err asks for (0 when it carries none), capped at
// MaxRetryDelay. A circuit breaker uses it as a minimum open time.
func RetryDelay(err error) time.Duration {
	var r RetryDelayer
	if !errors.As(err, &r) {
		return 0
	}
	return min(max(r.RetryDelay(), 0), MaxRetryDelay)
}

// InvalidDetail is the text recorded in Attempt.Detail when an answer fails
// validation (Validate, ValidateScoreResult). It states the number of problems
// and nothing else: the messages of those errors quote candidate and question
// ids, and a trace must never carry caller-supplied content.
func InvalidDetail(err error) string {
	n := 1
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		n = len(j.Unwrap())
	}
	return fmt.Sprintf("answer failed validation (%d problem(s))", n)
}

// Report is how a (possibly composite) provider answered one call: which
// engine produced the answer, by what strategy, and every engine that ran.
type Report struct {
	// Strategy is "single", "fallback", "hedged" or "race" for a combinator,
	// and "" for a plain provider.
	Strategy string `json:"strategy,omitempty"`
	// Engine is the leaf engine that answered ("" when none did).
	Engine string `json:"engine,omitempty"`
	// Attempts lists each leaf engine tried, in start order.
	Attempts []Attempt `json:"attempts"`
	// FallbackFired is true when a backup engine ran because the primary
	// failed or was unavailable.
	FallbackFired bool `json:"fallbackFired,omitempty"`
	// HedgeFired is true when a backup engine was started only because the
	// primary was slow.
	HedgeFired bool `json:"hedgeFired,omitempty"`
	// Model is the model id the answering engine reported ("" when it reports
	// none). Thresholds are only meaningful for the model they were measured
	// on, so every answer says which model produced it.
	Model string `json:"model,omitempty"`
}

// TracedProvider is a Provider that also reports how it decided. Chain uses
// DecideTraced when available so Trace lists every engine tried, not only the
// outermost wrapper.
type TracedProvider interface {
	Provider
	DecideTraced(ctx context.Context, req Request) (Decision, bool, Report, error)
}
