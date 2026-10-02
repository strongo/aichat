package decision

import (
	"context"
	"errors"
	"fmt"
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
)

// MaxRetryDelay caps any retry delay an engine asks for, so a bad header cannot
// take an engine out of service for longer.
const MaxRetryDelay = 10 * time.Minute

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
