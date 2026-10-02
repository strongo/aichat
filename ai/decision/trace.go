package decision

import (
	"context"
	"errors"
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
)

var (
	// ErrUnavailable is returned (wrapped) by a provider that refuses to call
	// its engine because the engine is known to be down (a circuit breaker is
	// open). Chain records it as AttemptUnavailable.
	ErrUnavailable = errors.New("decision: engine unavailable")
	// ErrUnsupported is returned (wrapped) when an engine does not implement
	// the asked operation.
	ErrUnsupported = errors.New("decision: operation not supported by engine")
)

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
}

// TracedProvider is a Provider that also reports how it decided. Chain uses
// DecideTraced when available so Trace lists every engine tried, not only the
// outermost wrapper.
type TracedProvider interface {
	Provider
	DecideTraced(ctx context.Context, req Request) (Decision, bool, Report, error)
}
