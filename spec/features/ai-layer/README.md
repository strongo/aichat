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

Every chat provider MUST implement `ai.LLMProvider`: `Name() string` and `Stream(ctx, ai.ChatRequest) iter.Seq2[ai.Event, error]`. A stream MUST yield `EventStarted` first, then any number of `EventTextDelta` / `EventStructured` / `EventUsage`, then exactly one of `EventCompleted` or a terminal error. Implementations MUST NOT buffer the full response and MUST stop promptly when `ctx` is cancelled or the consumer stops iterating (Go 1.23+ range-over-func semantics: returning `false` from `yield` ends the stream).

#### REQ: context-rendering-order

`ai.ChatRequest.System` plus `Context` blocks MUST be rendered with all `ContextStatic` blocks before all `ContextDynamic` blocks, each kind in the given slice order, so the leading, cacheable portion of the prompt is the same across turns as long as the static block set and order don't change (see REQ: ctxmgr-stable-order).

#### REQ: structured-output

When `ChatRequest.ResponseSchema` is set, an adapter MUST use the provider's native structured-output support where available (OpenAI-compatible: `response_format: json_schema` with `strict: true`) or instruct the model to reply with only matching JSON (Anthropic: appended to the system prompt) where it is not, and MUST emit the parsed final object as a single `EventStructured` event, tolerating a ` ```json ` fence around the model's raw text.

#### REQ: retry-before-first-byte

Adapters MUST bound retries with backoff and jitter (`ai/internal/retry`) ONLY for failures before any byte of the streamed response has been observed by the consumer, and MUST NOT retry once streaming has started — a retry after partial output would duplicate or corrupt what the consumer already has.

#### REQ: http-error-mapping

Every HTTP-backed adapter (`ai/openaicompat`, `ai/anthropic`, `ai/cloud`) MUST map HTTP 401/403 to `ai.ErrCodeAuth`, 429 to `ai.ErrCodeRateLimited` (retryable), 5xx to `ai.ErrCodeUpstream` (retryable), and other non-2xx to `ai.ErrCodeInvalid`.

### Decision chain

#### REQ: chain-semantics

`decision.Chain` MUST run its `Providers` in order and return the first provider's decision that is valid (`decision.Validate` against the request's `Taxonomy`) and meets `MinConfidence` for both `Module` and (when set) `Intent`. A provider that errors, times out (`Chain.Timeout`, default 1500ms per provider), abstains, returns an invalid decision, or returns a low-confidence decision MUST be recorded in `Trace.Attempts` and the chain MUST fall through to the next provider. A chain where every provider falls through is not an error: `Chain.Decide` returns `ok=false`, and the product's main-LLM path classifies and answers in a single inference (there is deliberately no dedicated "no decision" LLM decider step distinct from the product's normal chat path).

#### REQ: rules-provider

`ai/decision/rules.Provider` MUST run its `Rule`s in declared order against text normalised by `Normalize` (lowercased, whitespace-collapsed, trailing `.!?` trimmed) and session state, returning the first rule's decision. It MUST NOT use a general-purpose regex/NLP engine — matching is exact-phrase (`Phrases`) or hand-written Go in each `Rule.Match`. Products own their own `Rule` sets; this package supplies only the runner and the two normalisation helpers.

#### REQ: llmdecider-single-inference

`ai/decision/llmdecider.Decider` MUST make exactly one `ai.LLMProvider.Stream` call per `Decide`, with `ResponseSchema` set to the static, tested JSON Schema of `decision.Decision`, a system prompt that lists the request's `Taxonomy` (modules/intents/scopes/presentations/data kinds/entity types) and the product-neutral rules (never invent entity IDs; `reference` is an expression to resolve, not an ID; `requiredScopes` is the minimum; `canHandleDeterministically`/`needsLLM` are only true together when the product can fully answer from data alone), and a context block carrying session state (entity refs and titles only, never rendered data) plus a bounded tail of `Request.Recent` and `Now`/`TZ`. It MUST parse the decision from the `EventStructured` event, falling back to extracting a JSON object from the collected text (tolerating a fenced block) when no structured event arrived; malformed JSON in either path MUST return an error (which `Chain` treats as a fall-through, per REQ: chain-semantics). This is the product-neutral implementation a cloud-hosted decision service ("Jev") can host directly — nothing in it is Sneat- or DataTug-specific.

#### REQ: decision-schema-evolvable

The `decision.Decision` JSON Schema embedded in `ai/decision/llmdecider` is intentionally evolvable: new fields are additive, and both `decision.Validate` and any consumer MUST treat unknown `module`/`intent`/`presentation` values as "not decided" rather than erroring the whole turn. A schema change is a reviewed diff to a static Go constant, not a struct-reflection surprise (`TestDecisionSchema_MatchesFields` pins the field-name correspondence).

### Context Manager

#### REQ: ctxmgr-retain-then-required

`ctxmgr.Manager.Select(required, available, pinnedScopes)` MUST always include every scope in `required` (or, when `required` is `nil` — no decision was made — every available static scope, so the main-LLM path gets full cached context for a single classify-and-answer inference), and MUST additionally retain every static scope already sent earlier in the conversation even when no longer required, so the cached prefix does not shrink or shift on a turn that needs less context than a previous one.

#### REQ: ctxmgr-stable-order

Static blocks in the output MUST appear in first-sent order (the order scopes were first included across the conversation), with newly-added scopes appended after all previously-sent ones, so the leading prompt bytes stay stable turn to turn for a provider-side prefix cache (e.g. Anthropic's `cache_control: ephemeral`) to actually hit.

#### REQ: ctxmgr-budget-compaction

Only when the estimated token count (a coarse `len(text)/4` estimate, summed over the currently-selected static blocks) exceeds `Policy.BudgetTokens` (default 12000) MUST the Manager drop retained-but-not-required static scopes, largest first, until it fits or runs out of droppable scopes; a `required` scope MUST NEVER be dropped regardless of budget. `Report.Compacted` and `Report.Dropped` MUST reflect exactly what was dropped for this call.

#### REQ: ctxmgr-dynamic-scoping

Dynamic blocks MUST be included only for scopes in `required` or `pinnedScopes` (or, when `required` is `nil`, every available static scope's dynamic blocks too) — dynamic content is per-turn and never cached, so there is no reason to resend an unrelated scope's dynamic data.

### Cloud protocol and client

#### REQ: cloudproto-sse-framing

`cloudproto.WriteEvent`/`ReadEvents` MUST frame each `ai.Event` as a standard SSE `event:`/`data:`/blank-line frame with single-line JSON; `ReadEvents` MUST join multi-line `data:` fields with `\n` before parsing, ignore SSE comment lines (`:` prefix) and unknown event names, stop iterating after `response.completed`, and stop promptly if the consumer breaks out of the range loop early.

#### REQ: cloud-client-endpoints

`ai/cloud.Client` MUST implement `ai.LLMProvider` by `POST {BaseURL}ai/chat` with `Accept: text/event-stream`, a bearer token from `Config.Token`, and the `X-AI-Product` header set from `Config.Product`, parsing the response with `cloudproto.ReadEvents`; `decision.Provider` by `POST {BaseURL}ai/decision`, treating `decided=false` as abstention; and `Usage(ctx)` by `GET {BaseURL}ai/usage`. `Config.BaseURL` MUST be normalised to always end with `/`. A non-2xx response at any of the three endpoints MUST be decoded as `cloudproto.ErrorResponse` into an `*ai.Error`.

#### REQ: cloud-single-name-deviation

`*Client` implements both `ai.LLMProvider` and `decision.Provider`; because Go dispatches `Name()` once per concrete type regardless of which interface a caller holds it through, `Client.Name()` returns `"cloud"` for both roles rather than the `"cloud"` / `"cloud-decision"` split first sketched for the pinned cross-lane API. `diag.Turn.Path` and whether `Turn.Decision` is set already disambiguate which role produced a given diagnostic record, so this is a naming simplification, not a loss of information.

### Config and BYOK

#### REQ: config-independent-llm-and-decision

`aiconfig.Config` MUST let `LLM.Provider` (`cloud` | `byok`) and `Decision.Provider` (`auto` | `cloud` | `disabled`) vary independently: a BYOK chat LLM combined with a cloud-hosted decision provider MUST work, and a cloud chat LLM combined with `Decision.Provider: disabled` (or no cloud token source) MUST work. `aiconfig.Build(cfg, deps)` MUST run the product's `deps.ExtraDecision` providers before any cloud decision provider in the returned chain.

#### REQ: byok-direct-connection

A BYOK adapter (`ai/openaicompat` or `ai/anthropic`, selected by `BYOK.Protocol`) MUST connect directly to `BYOK.Endpoint` using the API key read from the environment variable named by `BYOK.APIKeyEnv`. It MUST NEVER route through `Cloud.BaseURL` or any cloud boundary. `aiconfig.Config.String()` MUST print the env var NAME for diagnostics and MUST NEVER print a key value (which `Config` never stores in the first place).

### Diagnostics

#### REQ: diag-turn-record

`diag.Turn` MUST record which `Path` handled a turn (`deterministic` | `decision` | `llm` | `llm-fallback`), the decision `Trace` when one ran, module/intent, required vs. actual context scopes, whether the LLM was skipped, provider/model, usage, and per-stage latency — and MUST NEVER carry raw user message text or API key values. `diag.Log` MUST emit at `slog.LevelDebug` so it is invisible by default and available on demand.

## Acceptance Criteria

### AC: llmprovider-event-order
**Requirements:** ai-layer#req:llmprovider-contract

**Given** a fake `ai.LLMProvider` that yields `EventStarted`, two `EventTextDelta`, `EventUsage`, then `EventCompleted`
**When** `ai.Collect` drains the stream
**Then** it returns the concatenated text, the last usage, and a nil error, and a manual `range` over the same stream that breaks after 2 events causes the generator to stop producing further events

### AC: context-static-before-dynamic
**Requirements:** ai-layer#req:context-rendering-order

**Given** an `ai.ChatRequest` with one static and one dynamic `ContextBlock`
**When** `ai/openaicompat` or `ai/anthropic` builds the outbound system message/blocks
**Then** the static block's text precedes the dynamic block's text in the rendered system prompt

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

### AC: ctxmgr-retains-across-turns
**Requirements:** ai-layer#req:ctxmgr-retain-then-required

**Given** a `Manager` that selected scope `calendar` as required on turn 1
**When** turn 2 requires only scope `tasks`
**Then** `Select` returns both `calendar` (retained) and `tasks` (required) static blocks, and `Report.Retained` includes `calendar`

### AC: ctxmgr-compacts-under-budget
**Requirements:** ai-layer#req:ctxmgr-budget-compaction

**Given** a `Manager` with a small `BudgetTokens`, a large retained-but-not-required scope, and a small required scope
**When** `Select` is called over budget
**Then** the large scope is dropped, the required scope is kept, and `Report.Compacted` is true with the dropped scope named in `Report.Dropped`

### AC: cloudproto-round-trip
**Requirements:** ai-layer#req:cloudproto-sse-framing

**Given** a sequence of `ai.Event` values written with `WriteEvent`
**When** the resulting bytes are read back with `ReadEvents`
**Then** the same events come back in order, and an interleaved unknown `event:` name and a `:`-prefixed comment line are both silently skipped

### AC: cloud-client-endpoints-and-errors
**Requirements:** ai-layer#req:cloud-client-endpoints

**Given** a test server implementing `ai/chat`, `ai/decision` (once with `decided:false`), and `ai/usage`, and separately a server returning a non-2xx `ErrorResponse`
**When** `cloud.Client`'s three methods are called against each
**Then** chat streams events normally, decision reports `ok=false` for the abstain case, usage decodes the allowance, and the error case surfaces as an `*ai.Error` with the response's `Code`/`Message`

### AC: byok-never-touches-cloud-base-url
**Requirements:** ai-layer#req:byok-direct-connection

**Given** an `aiconfig.Config` with `LLM.Provider: byok`, a `BYOK.Endpoint`, and a `Cloud.BaseURL` pointing at a different host
**When** `aiconfig.Build` constructs the LLM provider
**Then** the returned provider is an `*openaicompat.Provider` or `*anthropic.Provider` configured with `BYOK.Endpoint`, and no request path in the built provider ever references `Cloud.BaseURL`

### AC: config-independent-cloud-decision-with-byok-llm
**Requirements:** ai-layer#req:config-independent-llm-and-decision

**Given** `LLM.Provider: byok` and `Decision.Provider: cloud` with a working `Deps.CloudToken`
**When** `aiconfig.Build` runs
**Then** `Providers.LLM` is the BYOK adapter and `Providers.Decision` contains the cloud decision provider

### AC: diag-turn-never-carries-user-text
**Requirements:** ai-layer#req:diag-turn-record

**Given** a fully populated `diag.Turn`
**When** it is JSON-marshalled
**Then** no field name matches `text`, `message`, or a raw key/secret pattern, and `diag.Log` emits it at `slog.LevelDebug`

## Open Questions

- Should `ai/cloud`'s single shared `Name()` (REQ: cloud-single-name-deviation) be resolved with two small exported wrapper types instead, once a consumer actually needs the literal `"cloud-decision"` string in a diagnostic filter? Deferred until DataTug or Sneat's aiconfig-consuming lane needs it.
- `ai/ctxmgr`'s token estimate is `len/4`; if a product finds this consistently over/under-shoots its real tokenizer badly enough to mis-budget, a provider-supplied estimator hook may be worth adding.
- `ai/decision/llmdecider`'s `MaxRecent` trims `Request.Recent` client-side; whether that trimming should instead be the product's responsibility (so it can prioritise which turns matter) is open.

---
*This document follows the https://specscore.md/feature-specification*
