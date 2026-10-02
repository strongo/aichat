// Package compose holds engine combinators over decision.Provider (and
// decision.ScoredProvider): Single, Fallback, Hedged and Race, plus Breaker, a
// circuit breaker that stops calling an engine that is down, and Budget, a cap on
// how often an engine (typically a paid backup) is called.
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
//     cause ErrSuperseded, which a Breaker tallies as SLOW, apart from failures
//     (BreakerStats). Slowness opens a breaker only with WithBreakerSlowThreshold
//     (default: never), and a primary that outruns its OWN timeout is a plain
//     timeout failure. Set the hedge delay above the primary's p99, or a healthy
//     primary is paid for twice on every slow call. A Race loser is never counted.
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
//   - An exhausted allowance (decision.ErrQuota, outcome "quota") is neither a
//     health failure nor transient, and a backup is typically a paid engine: it
//     does NOT start the backup unless WithFallbackOn(OnQuota) says so, and the
//     error is returned to the caller. A misconfigured endpoint
//     (decision.ErrMisconfigured, outcome "misconfigured") never starts a backup
//     and never opens a breaker: it is loud on purpose. The same holds for the
//     strategies that run engines at once: a Hedged primary's refusal, even after
//     the hedge fired and the backup is already running, and any Race engine's
//     refusal, ends the call with that error, cancels the others and returns no
//     answer (WithFallbackOn(OnQuota) opts out of the quota case only). A Hedged
//     backup's own refusal is just a failed backup.
//   - A spent budget (decision.ErrBudget, outcome "budget"; see Budget) is a
//     quota-class refusal: it is not the engine's fault (a Breaker ignores it), and
//     Fallback, Hedged and Race treat it exactly like an exhausted allowance (no
//     backup unless WithFallbackOn(OnQuota); it ends a concurrent call). An
//     abstention elsewhere in a call never swallows such a refusal.
//   - With WithPolicy, every decision an engine returns carries the policy's
//     stamped verdict (decision.Decision.Outcome, and the judged state Actionable
//     reads), and only an actionable one counts as decided: an answer the policy
//     judged uncertain, none or unscored is recorded "uncertain" (and returned, as
//     an answer, when nothing better exists) and Decision.Actionable is false for
//     it. A deterministic decision (decision.Deterministic, what
//     ai/decision/rules returns) is accepted under any policy as
//     OutcomeDeterministic, and a calibrated decision that contradicts its own
//     Scores is recorded "invalid". Without WithPolicy an engine has no bar to
//     judge by and returns the decision with whatever verdict an engine behind it
//     stamped, none for a plain provider (Decision.Actionable is false, whatever
//     Outcome the provider wrote): wrap it in a decision.Chain, which owns the bar
//     and stamps the verdict.
//   - A Breaker is transparent for tracing: it passes on the Report of an engine
//     that reports one (a combinator, or an engine that explains its abstentions,
//     as ai/decision/typesafe does), so a breaker never hides attempts from a
//     decision.Chain's trace.
//   - Each answered scored attempt carries its own decision.Usage, so a hedged or
//     fallen-back call can be metered per engine.
//   - A Breaker honours a failure's retry delay (a Retry-After, see
//     decision.RetryDelay) as a minimum open time, ignores the result of a call
//     that started before it last opened, returns when its context is done even
//     if the engine ignores cancellation, and bounds its half-open probe
//     (WithBreakerProbeTimeout) so a hung engine cannot hold it half-open.
//   - Scored questions are answered by every engine that is a
//     decision.ScoredProvider: a Fallback from a decision model to an LLM decider
//     (ai/decision/llmdecider) still answers them, with uncalibrated scores that
//     a decision.SelectionPolicy turns into a proposal, never a selection.
//
// # Cost control: the hole and the guard
//
// A provider's own errors cannot bound the bill of a paid backup. TypeSafe
// documents one 429 error, a rate limit, and no way to tell it from a spent
// account, so ai/decision/typesafe keeps every 429 transient; with
// Fallback(Breaker(typesafe), Breaker(llm)) an exhausted Jev account therefore
// sends EVERY call to the paid LLM (a probe saw 5 Jev calls and 50 LLM calls in
// 50 turns), and nothing bounds it. Budget is the guard: wrap the backup,
// INSIDE its Breaker so blocked calls are not counted,
//
//	compose.Fallback(
//		compose.NewBreaker(typesafe),
//		compose.NewBreaker(compose.NewBudget(llm, compose.BudgetOptions{MaxCalls: 200, Per: time.Hour})),
//	)
//
// and the call after the cap fails with decision.ErrBudget, which a decision.Chain
// stops at by default (Trace.StoppedBy "budget"). For anonymous or public traffic
// run the calibrated engine alone, with no paid backup, or with a budgeted one; and
// a product that sees a stopped chain must not escalate to its paid main LLM as if
// nobody had decided (check decision.Trace.StoppedBy).
package compose
