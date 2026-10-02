// Package compose holds engine combinators over decision.Provider (and
// decision.ScoredProvider): Single, Fallback, Hedged and Race, plus Breaker, a
// circuit breaker that stops calling an engine that is down.
//
// Every combinator is itself a decision.Provider, so it nests inside a
// decision.Chain (and inside another combinator), and every one is a
// decision.TracedProvider: its Report says which engine answered, by what
// strategy, how long each engine took, and whether a fallback or hedge fired.
//
// Which engines run, and how they are combined, is configuration rather than
// code (see package aiconfig), so the engine behind a decision can be swapped
// without touching the callers.
//
// Semantics shared by all strategies:
//
//   - An engine's answer is accepted only when it is valid
//     (decision.Validate / decision.ValidateScoreResult).
//   - Each engine runs under its own timeout (its DecisionTimeout(), or
//     WithDefaultTimeout), enforced here even for an engine that ignores its
//     context. An engine that ignores cancellation can still leak its own
//     goroutine; the combinator itself never blocks past the timeout.
//   - An engine cancelled because another engine answered first records the
//     outcome "cancelled", which is not a failure and does not count against a
//     Breaker -- with one exception: a Hedged primary that was still running when
//     its hedge answered missed its latency budget, and is cancelled with the
//     cause ErrSuperseded, which a Breaker counts as a failure (otherwise a
//     primary that hangs would never open its breaker). A Race loser is never
//     counted.
//   - An abstention or an uncertain answer is an answer, not a failure: by
//     default it does NOT start the backup engine. Asking a second model the
//     same question until one sounds surer would make answers depend on timing.
//     WithFallbackOn opts in.
//   - Hedged has one rule for them, whatever the timing: when the primary
//     abstains or answers uncertain and WithFallbackOn names that outcome, the
//     backup is used (started at once if the hedge had not fired yet; waited for
//     if it was already running); otherwise the primary's abstention or uncertain
//     answer stands and a backup still running is cancelled. (A backup that
//     already returned a valid answer before the primary finished has simply won
//     the hedge race.)
//   - No retries happen here or in the providers: trying another engine is the
//     only retry.
//   - Constructors never panic: an engine built with no providers or a nil
//     provider returns an error wrapping ErrNoEngine on its first call.
//   - Failures are engine-health failures. A Breaker does not count a request
//     the engine rejected as invalid, or an authentication failure
//     (decision.ErrInvalidRequest, decision.ErrAuth, recorded as the outcomes
//     "rejected" and "auth"): those are the caller's fault, and counting them
//     would take a healthy engine out of service for everyone. They still make
//     Fallback and Hedged start the backup.
//   - A Breaker honours a failure's retry delay (a Retry-After, see
//     decision.RetryDelay) as a minimum open time, ignores the result of a call
//     that started before it last opened, returns when its context is done even
//     if the engine ignores cancellation, and bounds its half-open probe
//     (WithBreakerProbeTimeout) so a hung engine cannot hold it half-open.
//   - Scored questions are answered by every engine that is a
//     decision.ScoredProvider: a Fallback from a decision model to an LLM decider
//     (ai/decision/llmdecider) still answers them, with uncalibrated scores that
//     a decision.SelectionPolicy turns into a proposal, never a selection.
package compose
