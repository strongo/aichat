---
format: https://specscore.md/feature-specification
status: Draft
---

# Feature: AI Layer

> [SpecScore.**Studio**](https://specscore.studio): | [Explore](https://specscore.studio/app/github.com/strongo/aichat/spec/features/ai-layer?op=explore) | [Edit](https://specscore.studio/app/github.com/strongo/aichat/spec/features/ai-layer?op=edit) | [Ask question](https://specscore.studio/app/github.com/strongo/aichat/spec/features/ai-layer?op=ask) | [Request change](https://specscore.studio/app/github.com/strongo/aichat/spec/features/ai-layer?op=request-change) |
**Status:** Draft
**Source Ideas:** —

## Summary

The shared, product-neutral AI provider layer under `github.com/strongo/aichat`: the streaming event model and `LLMProvider` contract (`ai`), a decision chain of pluggable `decision.Provider`s including a deterministic rule engine (`ai/decision/rules`) and a single-inference LLM decider (`ai/decision/llmdecider`), three concrete providers (`ai/openaicompat`, `ai/anthropic`, `ai/cloud`), a context manager that keeps a provider-cacheable prompt prefix stable (`ai/ctxmgr`), product-facing config that wires cloud vs. BYOK independently for chat and decision (`ai/aiconfig`), and diagnostics (`ai/diag`). It is consumed first by Sneat's chat MVP and by DataTug.

## Problem

Sneat and DataTug both need conversational AI: a chat surface backed by a streaming LLM, a way to route a user's turn deterministically when possible (cheaper, faster, more predictable than an LLM call) and fall back to an LLM otherwise, and a way to keep per-request prompt cost down as context grows. Building this per-product duplicates the event model, the streaming/parsing/error-mapping code for each provider's wire protocol, and the judgment calls around what to cache and what to always resend.

Putting it in `strongo/aichat` (Apache-2.0, dependency-free of any product) means:

- one streaming event model and one `LLMProvider` contract every adapter and every product's UI renders the same way;
- one place that gets OpenAI-compatible and Anthropic SSE parsing, HTTP error mapping, and retry-before-first-byte right, instead of N places;
- a decision chain that lets a product register its own fast, deterministic rules ahead of a shared LLM decider or a cloud-hosted one ("Jev"), with a single, uniform fallback: no decider (or none confident) means the product's main LLM path classifies and answers in one inference;
- a context manager whose caching policy (retain the sent static prefix, only compact under budget pressure) is written and tested once, not re-derived per product;
- BYOK (bring your own key) that always talks to the provider directly — the cloud is never a required relay — while a cloud LLM and a cloud-hosted decision service can still be used independently of each other.

## Behavior

### Package boundaries

#### REQ: package-boundaries

The module MUST be organised as: `ai` (event model, `LLMProvider`, `Collect`), `ai/session` (conversational working state), `ai/decision` (the `Provider` contract, `Chain`, `Validate`), `ai/cloudproto` (the wire protocol between clients and a cloud AI boundary), `ai/openaicompat`, `ai/anthropic`, `ai/cloud` (three `LLMProvider`/`decision.Provider` adapters), `ai/decision/rules` (deterministic table-driven provider), `ai/decision/llmdecider` (single-inference LLM provider), `ai/ctxmgr` (context selection), `ai/aiconfig` (config + wiring), `ai/diag` (turn diagnostics), and `ai/internal/retry` (unexported retry helper shared by the three HTTP adapters). No package outside `ai/internal/...` MAY depend on another product's code; `strongo/aichat` has no Sneat- or DataTug-specific types anywhere.

### Event model and streaming

#### REQ: llmprovider-contract

Every chat provider MUST implement `ai.LLMProvider`: `Name() string` and `Stream(ctx, ai.ChatRequest) iter.Seq2[ai.Event, error]`. A stream MUST yield `EventStarted` ONLY once the HTTP request has actually succeeded (a request that fails before any response is received produces no `EventStarted` at all), then any number of `EventTextDelta` / `EventStructured` / `EventUsage`, then exactly one of `EventCompleted` (success) or the FATAL pair (see REQ: fatal-error-contract). Implementations MUST NOT buffer the full response (except when accumulating text to parse a `ResponseSchema` result -- see REQ: structured-output) and MUST stop promptly when `ctx` is cancelled or the consumer stops iterating (Go 1.23+ range-over-func semantics: returning `false` from `yield` ends the stream).

#### REQ: fatal-error-contract

Every fatal condition (a failed request before streaming, a malformed chunk, an in-stream provider error, a truncated stream, a `finish_reason`/`stop_reason` indicating the model was cut off mid-structured-output) MUST be reported as EXACTLY ONE final yield of `(ai.Event{Type: ai.EventError, Error: e}, e)`, where `e` is a non-nil `*ai.Error`; nothing is yielded after it, and the implementation returns immediately afterward. An `EventError` yielded with a NIL Go error is explicitly NOT fatal (a stream continuing past a recoverable, informational error) and consumers MUST keep ranging past it. `ai.Collect` follows this: it terminates only on a non-nil Go error, and drains straight through a non-fatal `EventError`. A stream ended by context cancellation MUST yield the fatal pair with `Code: ai.ErrCodeCanceled` and `Retryable: false`, checked ahead of any other error classification. `cloudproto.ReadEvents` also conforms: an `EventError` frame on the wire is always fatal there and yields the fatal pair; an EOF before either `response.completed` or an error frame was seen is itself reported as a fatal truncation the same way.

#### REQ: context-rendering-order

Static context (`ai.ChatRequest.System` plus `ContextStatic` blocks) MUST be rendered as the leading, STABLE portion of the request (a system message for `ai/openaicompat`; `system` blocks for `ai/anthropic`) so a provider-side prefix cache can hit turn to turn. `ContextDynamic` blocks MUST NOT be rendered into that stable prefix at all -- per-turn data ahead of or inside the cached prefix would invalidate it on every turn. Instead, dynamic context is spliced as a clearly delimited (`[context]...[/context]`) PREFIX of the LAST message (the current user turn) in both HTTP adapters (see REQ: ctxmgr-stable-order for the companion rule on which scopes are selected).

#### REQ: anthropic-history-cache

`ai/anthropic` MUST additionally mark `cache_control: ephemeral` on the last message BEFORE the final turn (i.e. `Messages[len-2]` when there are at least two messages), not only on the system prompt, so the conversation history itself is cached across turns and not just the static instructions.

#### REQ: structured-output

When `ChatRequest.ResponseSchema` is set, `ai/openaicompat` MUST use native structured output (`response_format: json_schema`, `strict` controlled by `ChatRequest.StrictSchema` -- nil/true means strict, an explicit false pointer opts out) and `ai/anthropic` MUST instruct the model to reply with only matching JSON (appended to the system prompt); either way the final object arrives as a single `EventStructured` event, tolerating a ` ```json ` fence around the model's raw text. A schema an adapter submits in strict mode MUST itself be strict-valid (see REQ: llmdecider-strict-schema for how `ai/decision/llmdecider` satisfies this). A stream MUST NOT accumulate the full response text at all when `ResponseSchema` is unset -- only when it's set is there anything to parse from the accumulated text.

#### REQ: truncation-is-fatal

`ai/openaicompat` MUST treat an EOF that arrives without having seen the `[DONE]` marker as a fatal `ai.ErrCodeUpstream` truncation, and MUST parse in-stream `{"error": ...}` chunks as fatal too. `ai/anthropic` MUST treat an EOF without a `message_stop` event the same way. Both MUST treat a `finish_reason: "length"` / `stop_reason: "max_tokens"` as FATAL only when `ResponseSchema` was requested (an incomplete structured JSON object is useless) -- otherwise it is an ordinary, non-fatal completion.

#### REQ: retry-before-first-byte

Adapters (`ai/openaicompat`, `ai/anthropic`, `ai/cloud`) MUST bound retries with backoff and jitter (`ai/internal/retry`) ONLY for failures before any byte of the streamed response has been observed by the consumer, and MUST NOT retry once streaming has started -- a retry after partial output would duplicate or corrupt what the consumer already has. `ai/openaicompat` additionally honours a 429 response's `Retry-After` header (seconds or HTTP-date, capped, ctx-aware) before its own backoff runs.

#### REQ: http-error-mapping

Every HTTP-backed adapter (`ai/openaicompat`, `ai/anthropic`, `ai/cloud`) MUST map HTTP 401/403 to `ai.ErrCodeAuth`, 429 to `ai.ErrCodeRateLimited` (retryable), 5xx to `ai.ErrCodeUpstream` (retryable), and other non-2xx to `ai.ErrCodeInvalid`. `ai/cloud` additionally treats 429/5xx as retryable from the STATUS CODE even when a decoded `cloudproto.ErrorResponse` body left its own `retryable` field false/absent.

### Decision chain

#### REQ: chain-semantics

`decision.Chain` MUST run its `Providers` in order and return the first provider's decision that is valid (`decision.Validate` against the request's `Taxonomy`) and meets `MinConfidence` for both `Module` and (when set) `Intent`. `MinConfidence`'s zero value defaults to 0.7; a POSITIVE value is used as given; a NEGATIVE value means "accept any confidence" (no floor at all). A provider that errors, times out, abstains, returns an invalid decision, or returns a low-confidence decision MUST be recorded in `Trace.Attempts` and the chain MUST fall through to the next provider. Timeout detection MUST use `errors.Is(err, context.DeadlineExceeded)` (not `==`), so a provider that wraps `ctx.Err()` still classifies as `timeout`. Each provider's timeout is `Chain.Timeout` (default 1500ms) UNLESS the provider itself implements the optional `interface{ DecisionTimeout() time.Duration }`, in which case ITS value is used instead -- `ai/decision/rules.Provider` reports 300ms, `ai/cloud`'s decider and `ai/decision/llmdecider.Decider` both report 4s, reflecting how much slower a remote/inference call reasonably is than a local rule match. A chain where every provider falls through is not an error: `Chain.Decide` returns `ok=false`, and the product's main-LLM path classifies and answers in a single inference (there is deliberately no dedicated "no decision" LLM decider step distinct from the product's normal chat path).

#### REQ: rules-friendly-validation

`decision.Validate` requires `Interaction` to be a known, non-empty value. For the module-optional interactions (`confirmation`, `rejection`, `cancellation`, `undo`) an EMPTY `Module`/`Intent` is valid and skipped in both `Validate`'s module/intent lookup and `Chain`'s confidence check -- "Yes", "Cancel", "Undo that" answer the PREVIOUS turn's pending action and have nothing module-shaped to report. `Validate` also checks `Reference.Kind` against `Taxonomy.EntityTypes` and each `RequiredData` entry against `Taxonomy.DataKinds` when those lists are non-empty (empty list means "not constrained", same as the existing scope/presentation checks). `ai/decision/rules.Provider` fills `Module.Confidence`/`Intent.Confidence` to `1.0` when a matched rule leaves either at its zero value (with a non-empty `.Value`) -- a deterministic exact-phrase match that fired IS certain, and this spares every product `Rule` author the boilerplate of writing `Confidence: 1`.

#### REQ: rules-provider

`ai/decision/rules.Provider` MUST run its `Rule`s in declared order against text normalised by `Normalize` (lowercased, whitespace-collapsed, trailing `.!?` trimmed) and session state, returning the first rule's decision. It MUST NOT use a general-purpose regex/NLP engine — matching is exact-phrase (`Phrases`) or hand-written Go in each `Rule.Match`. Products own their own `Rule` sets; this package supplies only the runner and the two normalisation helpers.

#### REQ: llmdecider-single-inference

`ai/decision/llmdecider.Decider` MUST make exactly one `ai.LLMProvider.Stream` call per `Decide`, with `ResponseSchema` set to the static, tested, STRICT-VALID JSON Schema of `decision.Decision` (see REQ: llmdecider-strict-schema), a system prompt that lists the request's `Taxonomy` (modules/intents/scopes/presentations/data kinds/entity types) and the product-neutral rules (never invent entity IDs; `reference` is an expression to resolve, not an ID; `requiredScopes` is the minimum; `canHandleDeterministically`/`needsLLM` are only true together when the product can fully answer from data alone), and a context block carrying session state (entity refs and titles only, never rendered data) plus a bounded tail of `Request.Recent` (`Options.MaxRecent`: zero/unset means the default of 8; a NEGATIVE value, not zero, means "no limit") and `Now`/`TZ`. It MUST parse the decision from the `EventStructured` event, falling back to extracting a JSON object from the collected text (tolerating a fenced block) when no structured event arrived; malformed JSON in either path MUST return an error (which `Chain` treats as a fall-through, per REQ: chain-semantics). This is the product-neutral implementation a cloud-hosted decision service ("Jev") can host directly — nothing in it is Sneat- or DataTug-specific.

#### REQ: llmdecider-strict-schema

The wire JSON Schema `ai/decision/llmdecider` sends is written to satisfy OpenAI's strict structured-output rules, recursively: every object sets `additionalProperties: false`, and `required` lists EVERY key in `properties` (an optional field's optionality is expressed as a `["T","null"]` type union, never by omission from `required`). Because strict mode has no open-map type, `decision.Decision.Slots` (a `map[string]string`) is represented on the wire as an ARRAY of `{name, value}` objects and folded back into the map after parsing (`wireDecision.toDecision`) -- the wire shape intentionally does not mirror `decision.Decision`'s own JSON tags 1:1. A schema-walking test asserts the strict-mode rules hold recursively, not just at the top level.

#### REQ: decision-schema-evolvable

The `decision.Decision` JSON Schema embedded in `ai/decision/llmdecider` is intentionally evolvable: new fields are additive, and both `decision.Validate` and any consumer MUST treat unknown `module`/`intent`/`presentation` values as "not decided" rather than erroring the whole turn. A schema change is a reviewed diff to a static Go constant, not a struct-reflection surprise (`TestDecisionSchema_MatchesFields` pins the field-name correspondence).

### Context Manager

#### REQ: ctxmgr-retain-then-required

`ctxmgr.Manager.Select(required, available, pinnedScopes)` MUST always include every scope in `required` (an empty, non-nil slice is a valid "this decision needs no scopes"), and MUST additionally retain every static scope already sent earlier in the conversation even when no longer required, so the cached prefix does not shrink or shift on a turn that needs less context than a previous one. When there was NO decision at all (Jev off, or every provider abstained/errored), the product MUST call `Manager.SelectAll(available, pinnedScopes)` instead -- a distinct method, not `Select(nil, ...)` -- which includes every available static scope (and its dynamic blocks) so the main-LLM path gets full cached context for a single classify-and-answer inference. Collapsing "no decision" into a nil/empty `required` slice on `Select` itself would make "a decision that needs nothing" indistinguishable from "no decision happened", so the two are separate call shapes.

#### REQ: ctxmgr-stable-order

Static blocks in the output MUST appear in first-sent order (the order scopes were first included across the conversation), with newly-added scopes appended after all previously-sent ones, so the leading prompt bytes stay stable turn to turn for a provider-side prefix cache (e.g. Anthropic's `cache_control: ephemeral`) to actually hit.

#### REQ: ctxmgr-budget-compaction

Only when the estimated token count (a coarse `len(text)/4` estimate, summed over the currently-selected static blocks) exceeds `Policy.BudgetTokens` (default 12000) MUST the Manager drop retained-but-not-required static scopes FROM THE TAIL of the stable order INWARD (never from the middle or front), so whatever survives is still a stable PREFIX of what was sent before -- dropping is never allowed to disturb it, only shorten it from the end. A `required` scope MUST NEVER be dropped regardless of budget. A scope dropped this way MUST also be forgotten from the Manager's "sent" memory (not just excluded from this turn's output), so it is not implicitly retained again on a later turn just because it appeared once before budget pressure removed it -- it only returns if required again or naturally re-added. `Report.Compacted` and `Report.Dropped` MUST reflect exactly what was dropped for this call.

#### REQ: ctxmgr-dynamic-scoping

Dynamic blocks MUST be included only for scopes in `required` or `pinnedScopes` (or, under `SelectAll`, every available static scope's dynamic blocks too) — dynamic content is per-turn and never cached, so there is no reason to resend an unrelated scope's dynamic data.

### Cloud protocol and client

#### REQ: cloudproto-sse-framing

`cloudproto.WriteEvent`/`ReadEvents` MUST frame each `ai.Event` as a standard SSE `event:`/`data:`/blank-line frame with single-line JSON; `ReadEvents` MUST join multi-line `data:` fields with `\n` before parsing, tolerate bare-`\r` (old Mac-style) line endings in addition to `\n`/`\r\n`, ignore SSE comment lines (`:` prefix) and unknown event names, stop iterating after `response.completed` or a fatal error (see REQ: fatal-error-contract), and stop promptly if the consumer breaks out of the range loop early.

#### REQ: cloud-client-endpoints

`ai/cloud.Client` MUST implement `ai.LLMProvider` (Name `"cloud"`) by `POST {BaseURL}ai/chat` with `Accept: text/event-stream`, a bearer token from `Config.Token`, and the `X-AI-Product` header set from `Config.Product`, parsing the response with `cloudproto.ReadEvents` and relaying its events (already fatal-error-contract-conformant) unchanged; `Client.Decider()` returns a SEPARATE `decision.Provider` value (Name `"cloud-decision"`) by `POST {BaseURL}ai/decision`, treating `decided=false` as abstention; and `Usage(ctx)` by `GET {BaseURL}ai/usage`. `Config.BaseURL` MUST be normalised to always end with `/` and carries no default of its own -- the product supplies it (`ai/aiconfig.Deps.CloudBaseURL` or `Config.Cloud.BaseURL`). A non-2xx response at any of the three endpoints MUST be decoded as `cloudproto.ErrorResponse` into an `*ai.Error`, and all three retry 429/5xx before the first byte like the BYOK adapters.

#### REQ: cloud-decider-is-a-separate-value

`*Client` cannot itself satisfy both `ai.LLMProvider` (Name `"cloud"`) and `decision.Provider` (Name `"cloud-decision"`) through one `Name()` method -- Go dispatches exactly one `Name()` per concrete type regardless of which interface a caller holds it through. `Client.Decider()` resolves this literally: it returns a small unexported `decision.Provider` wrapping the same `*Client`, with its own `Name()` returning `"cloud-decision"` and a `DecisionTimeout() time.Duration` of 4s. `ai/aiconfig.Build` wires `c.Decider()` into the decision chain, never `c` itself.

### Config and BYOK

#### REQ: config-independent-llm-and-decision

`aiconfig.Config` MUST let `LLM.Provider` (`cloud` | `byok`) and `Decision.Provider` (`auto` | `cloud` | `disabled`; any other value is a `Build` error) vary independently: a BYOK chat LLM combined with a cloud-hosted decision provider MUST work, and a cloud chat LLM combined with `Decision.Provider: disabled` MUST work. `Decision.Provider: auto` with no cloud token source SILENTLY skips cloud decision (best-effort); `Decision.Provider: cloud` with no cloud token source or no base URL is a `Build` ERROR (an explicit request that can't be honoured is a misconfiguration, not something to swallow). `aiconfig.Build(cfg, deps)` MUST run the product's `deps.ExtraDecision` providers before `c.Decider()` in the returned chain, wiring the decision provider role, never the LLM `*cloud.Client` itself (see REQ: cloud-decider-is-a-separate-value). `Build` applies `deps.Getenv`/`deps.EnvPrefix` env overrides on top of `cfg` itself (via `Config.ApplyEnv`), so a product only needs to `Load` once.

#### REQ: byok-direct-connection

A BYOK adapter (`ai/openaicompat` or `ai/anthropic`, selected by `BYOK.Protocol`, default `openai-compatible`) MUST connect directly to `BYOK.Endpoint` using the API key read from the environment variable named by `BYOK.APIKeyEnv`. It MUST NEVER route through `Cloud.BaseURL` or any cloud boundary, which this package has no default value for at all -- the product supplies it via `Deps.CloudBaseURL` (or `Config.Cloud.BaseURL` to override). An empty `BYOK.Endpoint` MUST default to `https://api.openai.com/v1/` or `https://api.anthropic.com/v1/` per `BYOK.Protocol`. `BYOK.APIKeyEnv` left unset is valid (some endpoints need no key); naming a variable that `deps.Getenv` resolves to `""` MUST be a `Build` error, since that is almost always a forgotten export rather than an intentional no-auth setup. `aiconfig.Config.String()` MUST print the env var NAME for diagnostics and MUST NEVER print a key value (which `Config` never stores in the first place).

#### REQ: aiconfig-env-prefix

`Config.ApplyEnv(getenv, prefix)` MUST read each `AI_*` variable as `prefix + suffix` (e.g. prefix `"SNEAT_"` reads `SNEAT_AI_LLM_PROVIDER`), so multiple products sharing an environment don't collide on the same variable names. `Deps.EnvPrefix` supplies the prefix `Build` uses automatically.

### Diagnostics

#### REQ: diag-turn-record

`diag.Turn` MUST record which `Path` handled a turn (`deterministic` | `decision` | `llm` | `llm-fallback`), the decision `Trace` when one ran, module/intent, required vs. actual context scopes, whether the LLM was skipped, provider/model, usage (including `CacheWriteTokens` and `Allowance`), and per-stage latency — and MUST NEVER carry raw user message text, API key values, or raw upstream error bodies. `Turn.Errors` entries MUST be short CLASSIFIERS built with `diag.ErrorCode(err)` (an `*ai.Error.Code`, or `"error"` for anything else), never `err.Error()` text, which can carry arbitrary provider-internal or user-adjacent detail. `diag.Log` MUST emit at `slog.LevelDebug` so it is invisible by default and available on demand.

## Acceptance Criteria

### AC: llmprovider-event-order
**Requirements:** ai-layer#req:llmprovider-contract

**Given** a fake `ai.LLMProvider` that yields `EventStarted`, two `EventTextDelta`, `EventUsage`, then `EventCompleted`
**When** `ai.Collect` drains the stream
**Then** it returns the concatenated text, the last usage, and a nil error, and a manual `range` over the same stream that breaks after 2 events causes the generator to stop producing further events

### AC: dynamic-context-prefixes-last-message-not-system
**Requirements:** ai-layer#req:context-rendering-order

**Given** an `ai.ChatRequest` with one static and one dynamic `ContextBlock` and a trailing user message
**When** `ai/openaicompat` or `ai/anthropic` builds the outbound request
**Then** the static block's text is in the leading system prompt/blocks, the dynamic block's text is NOT there at all, and it instead appears as a delimited prefix of the last message's text

### AC: fatal-pair-shape-and-no-further-yields
**Requirements:** ai-layer#req:fatal-error-contract

**Given** a provider (`ai/openaicompat`, `ai/anthropic`, or `ai/cloud`) hitting a fatal condition (bad chunk JSON, in-stream error payload, or a 5xx after retries exhaust)
**When** the stream is ranged to completion
**Then** the very last yield is `(ai.Event{Type: ai.EventError, Error: e}, e)` with a non-nil `e`, and no event follows it; a context-cancelled mid-body stream yields `Code: ai.ErrCodeCanceled` with `Retryable: false`

### AC: truncation-and-max-tokens-are-fatal-only-with-schema
**Requirements:** ai-layer#req:truncation-is-fatal

**Given** a server that ends the SSE body without `[DONE]`/`message_stop`, and separately one that reports `finish_reason: "length"`/`stop_reason: "max_tokens"`
**When** the request had no `ResponseSchema`
**Then** the truncated-without-marker case is still fatal (`ai.ErrCodeUpstream`), but the `length`/`max_tokens` case completes normally; with `ResponseSchema` set, the `length`/`max_tokens` case is ALSO fatal

### AC: structured-output-round-trips
**Requirements:** ai-layer#req:structured-output

**Given** a `ChatRequest.ResponseSchema` and a provider that streams a ` ```json ` fenced JSON object as text deltas
**When** the stream completes
**Then** exactly one `EventStructured` event is emitted carrying the parsed JSON object

### AC: retry-only-before-streaming
**Requirements:** ai-layer#req:retry-before-first-byte

**Given** a test server that returns HTTP 503 twice then 200 with an SSE body
**When** `ai/openaicompat.Provider.Stream` (or `ai/anthropic.Provider.Stream`) is collected
**Then** the request is retried up to the third attempt before any event is yielded, and the final text is the 200 response's content

### AC: http-status-mapping
**Requirements:** ai-layer#req:http-error-mapping

**Given** a test server returning 401, 429, and 500 in turn
**When** each is streamed
**Then** the resulting `*ai.Error` has `Code` `auth`, `rate_limited`, `upstream` respectively, with `Retryable` true only for 429 and 500

### AC: chain-first-confident-wins
**Requirements:** ai-layer#req:chain-semantics

**Given** a `Chain` with a first provider that abstains, a second that times out, a third that returns an invalid decision, a fourth that returns a low-confidence decision, and a fifth that returns a valid, confident decision
**When** `Chain.Decide` runs
**Then** it returns the fifth provider's decision, `ok=true`, and `Trace.Attempts` records outcomes `abstained`, `timeout`, `invalid`, `low_confidence`, `decided` in order, with `DecidedBy` set to the fifth provider's name

### AC: chain-no-decider-is-not-an-error
**Requirements:** ai-layer#req:chain-semantics

**Given** a `Chain` whose only provider abstains
**When** `Chain.Decide` runs
**Then** it returns `ok=false` and a nil-equivalent (empty `DecidedBy`) `Trace` with no error, signalling the product should fall through to its main-LLM path

### AC: chain-honours-provider-decision-timeout-and-min-confidence-modes
**Requirements:** ai-layer#req:chain-semantics

**Given** a provider implementing `DecisionTimeout() time.Duration` returning 4s, run under a `Chain{Timeout: 50ms}`
**When** `Chain.Decide` calls it
**Then** the context it receives has a deadline close to 4s, not 50ms; separately, `MinConfidence: -1` accepts a 0.01-confidence decision that `MinConfidence: 0` (default 0.7) would reject

### AC: module-optional-interactions-validate-and-decide
**Requirements:** ai-layer#req:rules-friendly-validation

**Given** a `Decision{Interaction: InteractionConfirmation}` with empty `Module`/`Intent`, and separately a `rules.Rule` that returns a decision with `Module.Value` set but `Confidence` left at zero
**When** `Validate` and `Chain.Decide` run the first, and `rules.Provider.Decide` runs the second
**Then** the confirmation decision validates and is accepted regardless of confidence, and the rule's zero confidence is filled to `1.0`

### AC: rules-exact-phrase-match
**Requirements:** ai-layer#req:rules-provider

**Given** an `ai/decision/rules.Provider` with a rule matching the normalised phrase "cancel"
**When** `Decide` is called with text "Cancel!" and, separately, with text "please cancel this"
**Then** the first call decides and the second abstains (Phrases is exact-match, not substring)

### AC: llmdecider-parses-structured-and-falls-back
**Requirements:** ai-layer#req:llmdecider-single-inference

**Given** a fake `ai.LLMProvider` that emits an `EventStructured` decision in one test and only fenced-JSON text deltas (no `EventStructured`) in another
**When** `llmdecider.Decider.Decide` runs each case
**Then** both return the same parsed `decision.Decision` with `ok=true`, and a third case with unparseable text returns `ok=false` and a non-nil error

### AC: llmdecider-schema-is-strict-valid
**Requirements:** ai-layer#req:llmdecider-strict-schema

**Given** the `decisionSchema` constant
**When** it is walked recursively
**Then** every object node has `additionalProperties: false` and every key in its `properties` also appears in its `required`, and a `Decision` with `Slots` set round-trips through the wire array-of-`{name,value}` shape back into the same map

### AC: ctxmgr-select-vs-selectall
**Requirements:** ai-layer#req:ctxmgr-retain-then-required

**Given** a `Manager` and a set of static scopes
**When** `Select([]string{}, ...)` is called (an explicit empty-but-non-nil required list)
**Then** it returns NO static blocks, unlike `SelectAll(...)` on the same available blocks, which returns every static scope

### AC: ctxmgr-retains-across-turns
**Requirements:** ai-layer#req:ctxmgr-retain-then-required

**Given** a `Manager` that selected scope `calendar` as required on turn 1
**When** turn 2 requires only scope `tasks`
**Then** `Select` returns both `calendar` (retained) and `tasks` (required) static blocks, and `Report.Retained` includes `calendar`

### AC: ctxmgr-compacts-from-tail-and-forgets-dropped
**Requirements:** ai-layer#req:ctxmgr-budget-compaction

**Given** a `Manager` that sent scopes `[first, second, third]` in that order on turn 1, then a tiny budget on turn 2 with nothing required
**When** `Select` compacts
**Then** only a PREFIX of `[first, second, third]` survives (never a dropped scope followed by a kept one), and on a THIRD turn the dropped scope is not implicitly retained again just because it was sent before

### AC: cloudproto-round-trip
**Requirements:** ai-layer#req:cloudproto-sse-framing

**Given** a sequence of `ai.Event` values written with `WriteEvent`
**When** the resulting bytes are read back with `ReadEvents`
**Then** the same events come back in order, an interleaved unknown `event:` name and a `:`-prefixed comment line are both silently skipped, an `error` event frame yields the fatal pair and stops the stream, and bare-`\r` line endings parse the same as `\n`

### AC: cloud-client-endpoints-and-errors
**Requirements:** ai-layer#req:cloud-client-endpoints

**Given** a test server implementing `ai/chat`, `ai/decision` (once with `decided:false`), and `ai/usage`, and separately a server returning a non-2xx `ErrorResponse` and one returning 503 twice then 200
**When** `cloud.Client`'s methods (`Stream`, `Decider().Decide`, `Usage`) are called against each
**Then** chat streams events normally, decision reports `ok=false` for the abstain case, usage decodes the allowance, the error case surfaces as an `*ai.Error` with the response's `Code`/`Message`, and the 503-then-200 case retries before the first byte

### AC: byok-never-touches-cloud-base-url
**Requirements:** ai-layer#req:byok-direct-connection

**Given** an `aiconfig.Config` with `LLM.Provider: byok`, a `BYOK.Endpoint`, and `Deps.CloudBaseURL` pointing at a different host
**When** `aiconfig.Build` constructs the LLM provider
**Then** the returned provider is an `*openaicompat.Provider` or `*anthropic.Provider` configured with `BYOK.Endpoint`, and no request path in the built provider ever references the cloud base URL; separately, an empty `BYOK.Endpoint` with `Protocol: anthropic` resolves to the Anthropic default endpoint

### AC: config-independent-cloud-decision-with-byok-llm
**Requirements:** ai-layer#req:config-independent-llm-and-decision

**Given** `LLM.Provider: byok` and `Decision.Provider: cloud` with a working `Deps.CloudToken` and `Deps.CloudBaseURL`
**When** `aiconfig.Build` runs
**Then** `Providers.LLM` is the BYOK adapter and `Providers.Decision` contains a provider named `cloud-decision`; separately, `Decision.Provider: cloud` with NO token source is a `Build` error, while the default `auto` silently produces an empty decision chain in the same situation

### AC: diag-turn-never-carries-user-text
**Requirements:** ai-layer#req:diag-turn-record

**Given** a FULLY populated `diag.Turn` (every field set, `Errors` built from `diag.ErrorCode`) containing a sentinel string standing in for private user text
**When** it is JSON-marshalled and logged via `diag.Log`
**Then** the sentinel appears in neither the marshalled JSON nor the log output, no field name matches `text`, `message`, or a raw key/secret pattern, and `diag.Log` emits at `slog.LevelDebug`

## Open Questions

- `ai/ctxmgr`'s token estimate is `len/4`; if a product finds this consistently over/under-shoots its real tokenizer badly enough to mis-budget, a provider-supplied estimator hook may be worth adding.
- `ai/decision/llmdecider`'s `MaxRecent` trims `Request.Recent` client-side; whether that trimming should instead be the product's responsibility (so it can prioritise which turns matter) is open.
- Whether `ai/anthropic` should adopt native structured-output support instead of the system-prompt-instruction approach is open pending live-API verification of current support/stability for the target models; the instruction approach is kept for now since it is already tested and working.

---
*This document follows the https://specscore.md/feature-specification*
