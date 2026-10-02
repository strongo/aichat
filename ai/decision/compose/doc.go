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
//     outcome "cancelled", which is not a failure and never counts against a
//     Breaker.
//   - An abstention or an uncertain answer is an answer, not a failure: by
//     default it does NOT start the backup engine. Asking a second model the
//     same question until one sounds surer would make answers depend on timing.
//     WithFallbackOn opts in.
//   - No retries happen here or in the providers: trying another engine is the
//     only retry.
package compose
