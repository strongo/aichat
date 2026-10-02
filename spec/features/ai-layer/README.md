---
format: https://specscore.md/feature-specification
status: Draft
---

# Feature: AI Layer

> [SpecScore.**Studio**](https://specscore.studio): | [Explore](https://specscore.studio/app/github.com/strongo/aichat/spec/features/ai-layer?op=explore) | [Edit](https://specscore.studio/app/github.com/strongo/aichat/spec/features/ai-layer?op=edit) | [Ask question](https://specscore.studio/app/github.com/strongo/aichat/spec/features/ai-layer?op=ask) | [Request change](https://specscore.studio/app/github.com/strongo/aichat/spec/features/ai-layer?op=request-change) |
**Status:** Draft
**Source Ideas:** —

## Summary

The shared, product-neutral AI provider layer under `github.com/strongo/aichat`: the streaming event model and `LLMProvider` contract (`ai`), tool calling and extended-reasoning support across the adapters plus a tool-calling agent loop (`ai/agent`), a decision chain of pluggable `decision.Provider`s including a deterministic rule engine (`ai/decision/rules`), a single-inference LLM decider (`ai/decision/llmdecider`, an uncalibrated fallback or emulator for decisions and scored questions), a real decision-model client for TypeSafe AI's System One API (`ai/decision/typesafe`, calibrated probabilities, model id required), a selection policy and engine combinators (`ai/decision`, `ai/decision/compose`: fallback, hedged, race, circuit breaker), three concrete providers (`ai/openaicompat`, `ai/anthropic`, `ai/cloud`), a context manager that keeps a provider-cacheable prompt prefix stable (`ai/ctxmgr`), product-facing config that wires cloud vs. BYOK independently for chat and decision (`ai/aiconfig`), and diagnostics (`ai/diag`). It is consumed first by Sneat's chat MVP and by DataTug (whose chat agent and tool-calling migrated onto `ai/agent` and the adapters' tool support).

## Problem

Sneat and DataTug both need conversational AI: a chat surface backed by a streaming LLM, a way to route a user's turn deterministically when possible (cheaper, faster, more predictable than an LLM call) and fall back to an LLM otherwise, and a way to keep per-request prompt cost down as context grows. Building this per-product duplicates the event model, the streaming/parsing/error-mapping code for each provider's wire protocol, and the judgment calls around what to cache and what to always resend.

Putting it in `strongo/aichat` (Apache-2.0, dependency-free of any product) means:

- one streaming event model and one `LLMProvider` contract every adapter and every product's UI renders the same way;
- one place that gets OpenAI-compatible and Anthropic SSE parsing, HTTP error mapping, and retry-before-first-byte right, instead of N places;
- a decision chain that lets a product register its own fast, deterministic rules ahead of a decision model, an LLM decider or a cloud-hosted one, with a single, uniform fallback: no decider (or none confident) means the product's main LLM path classifies and answers in one inference;
- decision engines that are swappable by configuration and combinable (fallback, hedged, race, circuit breaker), so a real decision model can be primary and an LLM decider its backup, and a decision always says which engine answered;
- a context manager whose caching policy (retain the sent static prefix, only compact under budget pressure) is written and tested once, not re-derived per product;
- BYOK (bring your own key) that always talks to the provider directly — the cloud is never a required relay — while a cloud LLM and a cloud-hosted decision service can still be used independently of each other.

## Behavior

### Package boundaries

#### REQ: package-boundaries

The module MUST be organised as: `ai` (event model, `LLMProvider`, `Collect`), `ai/session` (conversational working state), `ai/decision` (the `Provider` contract, `Chain`, `Validate`), `ai/cloudproto` (the wire protocol between clients and a cloud AI boundary), `ai/openaicompat`, `ai/openairesponses`, `ai/anthropic`, `ai/cloud` (four `LLMProvider`/`decision.Provider` adapters), `ai/decision/rules` (deterministic table-driven provider), `ai/decision/llmdecider` (single-inference LLM provider), `ai/decision/compose` (engine combinators and circuit breaker), `ai/decision/typesafe` (TypeSafe System One client), `ai/ctxmgr` (context selection), `ai/aiconfig` (config + wiring), `ai/diag` (turn diagnostics), and `ai/internal/{retry,sse}` (unexported HTTP-adapter helpers: bounded retry-before-first-byte, and the shared SSE line-scanner). No package outside `ai/internal/...` MAY depend on another product's code; `strongo/aichat` has no Sneat- or DataTug-specific types anywhere.

This module also contains a second, independent tree, `tui/*` (the Bubble Tea chat kit: `tui`, `strongo-tui/pkg/focus`, `tui/transcript`, `strongo-tui/pkg/grid`, `tui/sidebar`, `tui/stream`, `tui/chatshell`), specified separately in `spec/features/tui-kit/README.md`. `tui/*` MAY depend on `ai/*` (the event model, `session.EntityRef`) but the reverse is never true, and `ai/*` remains fully usable — by a non-interactive product, a server, a script — without ever importing `tui/*`.

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

Adapters (`ai/openaicompat`, `ai/openairesponses`, `ai/anthropic`, `ai/cloud`) MUST bound retries with backoff and jitter (`ai/internal/retry`) ONLY for failures before any byte of the streamed response has been observed by the consumer, and MUST NOT retry once streaming has started -- a retry after partial output would duplicate or corrupt what the consumer already has. `ai/openaicompat` and `ai/openairesponses` additionally honour a 429 response's `Retry-After` header (seconds or HTTP-date, capped, ctx-aware) before their own backoff runs.

#### REQ: http-error-mapping

Every HTTP-backed adapter (`ai/openaicompat`, `ai/openairesponses`, `ai/anthropic`, `ai/cloud`) MUST map HTTP 401/403 to `ai.ErrCodeAuth`, 429 to `ai.ErrCodeRateLimited` (retryable), 5xx to `ai.ErrCodeUpstream` (retryable), and other non-2xx to `ai.ErrCodeInvalid`. `ai/cloud` additionally treats 429/5xx as retryable from the STATUS CODE even when a decoded `cloudproto.ErrorResponse` body left its own `retryable` field false/absent.

#### REQ: openairesponses-adapter

`ai/openairesponses.Provider` implements `ai.LLMProvider` (Name `"openai-responses"`) over OpenAI's Responses API (`POST {Config.BaseURL}/responses`, `stream: true`), with the SAME `Config` shape as `ai/openaicompat` (`BaseURL`, `APIKey`, `Model`, `Headers`, `HTTPClient`) so products can switch between the two adapters without reshaping their wiring. It MUST map `ChatRequest.System` plus `ContextStatic` blocks to the Responses API's `instructions` field (the leading, stable, cacheable prefix -- see REQ: context-rendering-order, whose dynamic-context splice rule applies identically here: dynamic context is spliced onto the last "user" TEXT input item, never a `function_call`/`function_call_output` item). `ChatRequest.Messages` MUST render as `input` items: a `{role, content}` message item per `RoleUser`/`RoleAssistant` message, one `{type:"function_call", call_id, name, arguments}` item per `ToolCall` on an assistant message (`arguments` defaulting to `"{}"` when empty, per REQ: tool-messages-on-the-wire), and one `{type:"function_call_output", call_id, output}` item per `ToolResult` on a `RoleTool` message (`output` prefixed `"Error: "` when `IsError`). A message item's `content` and a function_call_output item's `output` MUST be sent EVEN WHEN THE EMPTY STRING (`""`) -- the Responses API requires the key present on those item types regardless of value, so this adapter builds each item kind through its own constructor/marshal path (`messageItem`/`functionCallItem`/`functionCallOutputItem`, dispatched by `inputItem.MarshalJSON`) rather than one struct with `omitempty` on every field, which would silently drop an empty value instead of sending it. `ChatRequest.Tools` MUST render as `{type:"function", name, description, parameters, strict:false}`; `ToolChoice` maps to a bare `"auto"`/`"none"`/`"required"` string or, for a named tool, the FLAT `{type:"function", name}` shape (unlike Chat Completions' nested `function` object). `ChatRequest.Reasoning` maps to `reasoning:{effort}` (omitted when empty); `MaxTokens` maps to `max_output_tokens`; `ResponseSchema` maps to `text:{format:{type:"json_schema", schema, strict}}` with the same `StrictSchema` default as REQ: structured-output. Every request sends `store: false` UNCONDITIONALLY -- this adapter never relies on OpenAI's server-side conversation state (no `previous_response_id` chaining: a privacy property worth stating explicitly, since it means no turn of a conversation is ever persisted on OpenAI's servers by this adapter) -- and, UNLESS this model has already been learned unsupported (see NB2 below), `include: ["reasoning.encrypted_content"]`, which is what makes a reasoning item's `encrypted_content` arrive inline in the stream so it can be captured and replayed (see the ProviderState paragraph below) without ever turning `store` on.

On a 400 whose error message NAMES THE `reasoning` PARAMETER specifically (e.g. mentioning `reasoning.effort`, or "unsupported parameter" together with `'reasoning'`) -- NEVER a 400 that merely mentions "reasoning" in an unrelated, item-shaped context (e.g. "missing required 'reasoning' item"), which MUST NOT trigger this at all -- the adapter MUST retry the SAME request once, before any byte of a response was seen, WITHOUT that field, and remember (per MODEL, not per `*Provider` instance, mirroring `ai/openaicompat`'s identical `reasoning_effort` fallback -- REQ: reasoning-maps-to-provider-knob) not to send it again for that model on later `Stream` calls.

NB2 (r2 review): `include: ["reasoning.encrypted_content"]` ITSELF 400s on a model that does not support encrypted reasoning content at all (e.g. "Encrypted content is not supported with this model"). On such a 400, the adapter MUST retry the SAME request once, before any byte of a response was seen, WITHOUT `include` AND with every reasoning item stripped from `input` (a captured reasoning item from an earlier turn is equally unreplayable on a model that rejects `include` -- sending it anyway would just trigger the identical rejection a second time), and remember (per MODEL) to omit BOTH `include` and any reasoning item on every later `Stream` call for that model.

The adapter MUST assemble one `EventToolCall` per function-call item from `response.output_item.added` (registers the item's `call_id`/`name`), `response.function_call_arguments.delta`/`.done` (accumulates/finalises `arguments`), and `response.output_item.done` (the AUTHORITATIVE source for the item's final `call_id`/`name`/`arguments`, overriding whatever the delta buffer accumulated), emitted before the terminal `EventCompleted`, per REQ: tool-calling-additive-contract's non-empty-`ID` requirement. `response.output_text.delta` events map to `EventTextDelta`; `response.refusal.delta` text MUST NOT be surfaced as `EventTextDelta` at all (per `ai.StopReasonRefusal`'s doc, a refusal is not an `ai.Error` -- the response completed normally with no usable content), and its only effect is `StopReason: "refusal"` on the terminal `EventCompleted` when no tool call was produced (a tool call, if any, still takes priority over a same-response refusal for `StopReason`). The Responses API has no `[DONE]` sentinel; its own terminal events are `response.completed` (success -- `StopReason: "tool_calls"` when any function-call item was produced, else `"refusal"` when a refusal was seen, else `"end"`) and `response.incomplete` (truncated -- `StopReason: "length"` when `incomplete_details.reason` is `"max_output_tokens"`, `"content_filter"` when it is `"content_filter"`, else `"end"`), and either maps to exactly one `ai.EventCompleted` (REQ: llmprovider-contract); an EOF with neither is a fatal truncation (REQ: truncation-is-fatal's rule applied to this wire shape). `response.incomplete` is instead ALWAYS FATAL (`ai.ErrCodeUpstream`), regardless of `ResponseSchema`, whenever ANY tool call is still open or was merely assembled (registered via `output_item.added`, even one that already reached `output_item.done`) at the time it arrives -- a truncated stream leaves no safe way to know the model's full intended action set, so no `EventToolCall` is ever emitted for that response; separately, `response.incomplete` with `"max_output_tokens"` while `ResponseSchema` was requested (and no tool call is open) is ALSO fatal, same as `ai/openaicompat`'s `finish_reason: "length"` case. `response.failed`'s `response.error` and a top-level `error` event both map through a shared classifier: `rate_limit_exceeded` -> `ai.ErrCodeRateLimited` (retryable), `insufficient_quota` -> `ai.ErrCodeQuota`, `server_error` -> `ai.ErrCodeUpstream` (retryable), any other/absent code -> `ai.ErrCodeUpstream`, with a non-empty fallback message when the wire event carries none. Usage maps `usage.input_tokens`/`usage.output_tokens` to `InputTokens`/`OutputTokens`, `usage.input_tokens_details.cached_tokens` to `CacheReadTokens`, and `usage.output_tokens_details.reasoning_tokens` to `ReasoningTokens` -- the same INFORMATIONAL-SUBSET relationship to `InputTokens`/`OutputTokens` as `ai/openaicompat` (see REQ: usage-billable-tokens-per-adapter). Every other Responses API SSE event type (`response.created`, `response.in_progress`, `response.output_text.done`, `response.content_part.*`, reasoning-summary/MCP/web-search progress events, ...) MUST be silently ignored.

`Event.ProviderState` (REQ: tool-calling-additive-contract) captures EVERY output item -- reasoning (with its `encrypted_content`), message, function_call, and any other type -- from `response.output_item.done`, in stream order, as a JSON array, on the terminal `EventCompleted`, with two adjustments made to each captured item before it is stored (NEVER a byte-for-byte capture, unlike `ai/anthropic`'s thinking-block replay):

- NB1 (r2 review, BLOCKER): the item's top-level `id` and `status` fields are ALWAYS stripped. With `store: false`, OpenAI never persists an item server-side, and replaying an item's own `id` verbatim on a later request 400s ("Item with id '...' not found. Items are not persisted when store is set to false"). Every other field, including a reasoning item's `encrypted_content`, is kept untouched.
- minor (r2 review): for a `function_call` item whose provider-sent `call_id` was empty (this adapter then surfaces a SYNTHESIZED `"call_N"` as that call's `ai.ToolCall.ID` -- REQ: tool-calling-additive-contract's non-empty-`ID` requirement), the captured item's `call_id` is overwritten with that SAME synthesized id before storage. Without this, a call's replayed `function_call` item would carry an empty (or provider-omitted) `call_id` while the `function_call_output` item `buildInput` builds from that call's `ai.ToolResult.CallID` (itself derived from the synthesized `ai.ToolCall.ID`) would carry a DIFFERENT value -- an orphaned `function_call`/`function_call_output` pair on the wire. A call whose provider-sent `call_id` was already non-empty is unaffected (the synthesized id equals the real one in that case).

`buildInput`, when rendering an assistant message AFTER the last genuine `ai.RoleUser` message (i.e. within the CURRENT tool-calling loop) that carries a non-empty, well-formed `Message.ProviderState`, MUST replay those captured (and `id`/`status`-stripped, `call_id`-patched) items onto the wire VERBATIM in place of the normal message/function_call reconstruction -- mirroring `ai/anthropic.buildMessages`' current-loop-only `ProviderState` scoping (REQ: anthropic-thinking-block-replay): an assistant turn from an EARLIER loop is always rebuilt from `Text`/`ToolCalls` instead, even if it also carries a `ProviderState`. A `ProviderState` that fails to unmarshal, or unmarshals to a zero-length array, falls back to the legacy `Text`/`ToolCalls` reconstruction rather than emitting nothing. This is what lets a reasoning item's `encrypted_content` survive across the steps of one `ai/agent.Loop` run (which already carries `Event.ProviderState` onto the assistant message it appends) without this adapter ever needing OpenAI's server-side `Store`.

### Tool calling, reasoning and the agent loop

#### REQ: tool-calling-additive-contract

`ai.Tool`/`ai.ToolCall`/`ai.ToolResult`, `ai.RoleTool`, `Message.ToolCalls`/`Message.ToolResults`/`Message.ProviderState`, `ChatRequest.Tools`/`ChatRequest.ToolChoice`/`ChatRequest.Reasoning`, `EventToolCall`/`EventToolResult`, `Event.StopReason`/`Event.ProviderState`, and `Usage.ReasoningTokens` MUST be additive to the existing `ai` contract: every field is new or `omitempty`, so a caller that never sets `Tools` sees byte-identical request/event shapes to before this feature. `EventToolCall` MUST carry one FULLY ASSEMBLED `ai.ToolCall` with a NON-EMPTY `ID` -- an adapter that streamed no id (or an empty one) for a call MUST synthesize a stable, unique one rather than pass `""` through (Handler dispatch and `ToolResult.CallID` pairing both key off it). A response that ends with tool calls MUST still end with a single `EventCompleted`, additionally carrying `StopReason: "tool_calls"`; `StopReason` also carries `"refusal"`/`"pause_turn"` when a provider reports them (`ai/anthropic`'s `stop_reason`) rather than folding them into `"end"`.

#### REQ: tool-call-streaming-assembly

`ai/openaicompat` MUST assemble `delta.tool_calls` chunks by array `index` (id/name arrive on the first chunk for that index, `arguments` arrive concatenated across subsequent chunks) into one `EventToolCall` per call, emitted after the last content chunk and before the terminal `EventCompleted`. Some OpenAI-compatible providers omit `index` entirely on `delta.tool_calls` chunks; for those, `ai/openaicompat`'s assembler MUST key on a synthetic per-call slot instead, starting a NEW call whenever a chunk carries a non-empty `id` that differs from the call currently being assembled in that no-index slot, and otherwise (empty `id`, or the same `id` repeated) continuing to append `arguments` to it -- so two parallel no-index calls never collapse into one just because both default to index 0. `ai/anthropic` MUST assemble `content_block_start` (`type: tool_use`, carrying `id`/`name`) plus `input_json_delta` chunks on `content_block_delta` the same way, by block `index` (the Messages API always sends an explicit index, so no no-index fallback is needed there). A response that stops with reason `"length"` (truncated) while at least one tool call is still open MUST be reported as a FATAL error instead of emitting that call's (possibly incomplete/invalid-JSON) `EventToolCall` -- a truncated argument fragment is not safe to hand to any consumer.

#### REQ: tool-messages-on-the-wire

`ai/openaicompat` MUST render an assistant `Message.ToolCalls` as `tool_calls` on an `assistant` wire message and a `RoleTool` message's `ToolResults` as one `role:"tool"` wire message PER result (`tool_call_id` set, content prefixed `"Error: "` when `IsError`). `ai/anthropic` MUST render `Message.ToolCalls` as `tool_use` content blocks on an `assistant` wire message and `ToolResults` as `tool_result` content blocks on a `user` wire message, MERGING every consecutive `RoleTool` source message into ONE wire `user` message (Anthropic requires all `tool_result` blocks answering one assistant turn to arrive together). Either adapter, rendering a `ToolCall` whose `Arguments` is empty, MUST send `"{}"` (an empty JSON object), never an empty string. Dynamic context (`ai.ContextDynamic` blocks) MUST be spliced onto the LAST genuine "user" TEXT message specifically -- NEVER a `role:"tool"` (`ai/openaicompat`) or a tool_result-carrying `role:"user"` (`ai/anthropic`) message, which would both corrupt that message and make the splice point drift with every step of a multi-step tool-calling turn (breaking prompt-cache stability); with no such message, dynamic context is dropped rather than corrupting whatever IS last.

#### REQ: reasoning-maps-to-provider-knob

`ai/anthropic` MUST derive the request's thinking mode from the MODEL ID: Claude 4.6+ family models (Opus 4.6/4.7/4.8/5/5.5, Sonnet 4.6/5, Fable 5/5.1; an unrecognised/future id defaults the same way) use ADAPTIVE thinking (`thinking: {type: "adaptive"}` plus `output_config: {effort: <ChatRequest.Reasoning>}`, no `budget_tokens`); Haiku 4.5, any pre-4.6 Sonnet/Opus, and every Claude 3.x id (m4, r2 review: `claude-3-7-sonnet`, `claude-3-5-haiku`, `claude-3-opus`, `claude-3-5-sonnet`, `claude-3-sonnet`, `claude-3-haiku`, and any other `claude-3[-<minor>]-<family>`-shaped id -- these put the family AFTER the version, so they never match the "claude-<family>-<major>[-<minor>]" id shape the 4.6+ detection uses, and MUST NOT fall through to that detection's "unrecognised id" adaptive default, since adaptive thinking never existed for Claude 3.x) use the LEGACY form (`thinking: {type: "enabled", budget_tokens: 1024/4096/16000}` for low/medium/high). `ai/openaicompat` maps `ChatRequest.Reasoning` to `reasoning_effort` (set only when non-empty). Neither form may ever raise `MaxTokens` above what the CALLER explicitly set (`ChatRequest.MaxTokens != 0`) -- only when the caller left it unset may the adapter pick a larger default. When the caller leaves `MaxTokens` unset, `ai/anthropic`'s default MUST be raised in two cases: (1) thinking is REQUESTED (`ChatRequest.Reasoning` names a budget level) -- 16000 for adaptive thinking, and for the legacy form, the requested budget + 4096 capped at 16000 (except the cap MUST NOT drop the default AT or below the budget itself, since Anthropic requires `budget_tokens < max_tokens` -- a "high" budget of 16000 therefore gets `budget + 1024` even though that exceeds the nominal cap); and (2) the model is in the ADAPTIVE-THINKING family (N3 remainder, r3 review: `thinkingModeAdaptive(model)` true) REGARDLESS of `Reasoning` -- these models (Opus 5/5.5, Sonnet 5, Fable 5/5.1, and any other id this adapter classifies as adaptive) think BY DEFAULT even when `Reasoning` is `""`; omitting `thinking` does not disable it on them, it only leaves depth at the API's own default. This second case is NOT gated on `Reasoning` naming a budget level -- `reasoningBudgets` has no `""` entry, so a naive "only when thinking is requested" reading would leave `Reasoning: ""` on an adaptive model at `defaultMax` (2048), almost no room to answer after the model's default-on reasoning. On the legacy form, when the caller's `MaxTokens` would not clear the requested budget, `ai/anthropic` MUST shrink the budget to fit (floored at 1024) rather than raise `MaxTokens`; when the caller's `MaxTokens` is set below 2048 (too little room for a useful budget under that ceiling), thinking is DISABLED entirely instead. Thinking/signature deltas from `ai/anthropic` MUST NOT be emitted as `EventTextDelta` -- they are dropped from the text stream (they are still captured -- see REQ: anthropic-thinking-block-replay). `ai/openaicompat`, on a 400 whose error message specifically NAMES the `reasoning_effort` field (snake_case or camelCase, case-insensitive -- m3, r2 review: NOT the generic phrase "unsupported parameter", which is too broad and would misattribute an unrelated 400 to `reasoning_effort`), MUST retry the SAME request once, before any byte of a response was seen, WITHOUT that field, and remember (per MODEL, not per `*Provider` instance -- m3: a `Provider` can be reused across requests naming different models, and support for the field is a property of the model/deployment) not to send it again for that model on later `Stream` calls.

#### REQ: anthropic-thinking-block-replay

Anthropic requires the ENTIRE content array of the LAST assistant turn that contains `tool_use` -- every block, in the order the API returned them (thinking/redacted_thinking/text/tool_use, however they were interleaved) -- to be replayed BYTE-FAITHFUL on the next request that includes that turn; this applies to plain extended thinking, not only the interleaved-thinking beta, and omitting or reordering it 400s the following request. `ai.Message` gains an ADDITIVE, opaque `ProviderState json.RawMessage` field for this: `ai/anthropic.Provider.Stream` MUST capture EVERY content block it streams (not only thinking/redacted_thinking -- text and tool_use too), including a `thinking` block's `thinking` text with NO `omitempty` (a `display:"omitted"` block's empty `""` text MUST round-trip as an explicit empty string, never be dropped from the captured or replayed JSON), and surface them, JSON-encoded in stream order, as `ai.Event.ProviderState` on the terminal `EventCompleted`. A captured `tool_use` block's `input` MUST default to `{}` (X1, r2 review) both when captured (a no-argument tool call streams zero `input_json_delta` chunks, leaving the accumulated arguments empty) AND when replayed (defensively, for a `ProviderState` captured before this fix or relayed from a foreign origin) -- omitting `input` from the wire entirely 400s, since Anthropic requires the key even for an empty object. A captured/replayed `text` block with empty text is dropped rather than replayed (N1, r2 review): Anthropic rejects `{"type":"text","text":""}`.

`ai/anthropic.buildMessages`, when an assistant `ai.Message.ProviderState` is set, unmarshals to at least one block of a KNOWN type (`thinking`, `redacted_thinking`, `text`, `tool_use` -- anything else is filtered out, e.g. a foreign block relayed through `ai/cloud` from an origin this adapter doesn't fully trust), AND the message is WITHIN THE CURRENT TOOL-CALLING LOOP (N2 ruling, r2 review: strictly AFTER the last `ai.RoleUser` message in `ChatRequest.Messages` -- an assistant turn from an EARLIER loop, even one still carrying a `ProviderState`, is rebuilt from `Text`/`ToolCalls` instead, WITHOUT its thinking blocks), MUST use that `ProviderState` AS the ENTIRE wire content for that message VERBATIM, replacing (not merging with) whatever would otherwise have been rebuilt from `Text`/`ToolCalls`; a `ProviderState` that fails to unmarshal, unmarshals to zero known blocks, or belongs to an earlier loop, falls back to that legacy reconstruction instead. Anthropic's replay requirement is scoped to the turn that led to the CURRENT `tool_use`, not the entire conversation history, so this scoping also keeps a later turn's dynamic-context edit from ever landing ahead of (or "inside") an earlier turn's replayed thinking block. `ai/openaicompat` MUST ignore `Message.ProviderState`/`Event.ProviderState` entirely (it never reads or writes the field, so the current-loop-only scoping does not apply to it). Whoever appends an assistant tool-call message to a transcript across steps (`ai/agent.Loop`) MUST carry `Event.ProviderState` from that step's terminal `EventCompleted` onto the `ai.Message.ProviderState` it appends -- EXCEPT for a genuinely empty final turn (N1, r2 review: no text, no tool calls, and no `ProviderState` payload -- e.g. a refusal, or an empty `end_turn`), which MUST NOT be appended to the transcript at all, and a message with no surviving content after these rules (N1) MUST be dropped from the wire entirely rather than sent with an empty content array.

#### REQ: usage-billable-tokens-per-adapter

`ai.Usage`'s `CacheReadTokens`/`CacheWriteTokens`/`ReasoningTokens` fields have a DIFFERENT relationship to `InputTokens`/`OutputTokens` on each adapter, and naively summing all of them double-counts on one adapter and undercounts on the other -- this MUST be documented precisely on `Usage` itself, per field, not left to be discovered from adapter source. On `ai/openaicompat`: `usage.prompt_tokens_details.cached_tokens` (-> `CacheReadTokens`) and `usage.completion_tokens_details.reasoning_tokens` (-> `ReasoningTokens`) are INFORMATIONAL SUBSETS already counted inside `usage.prompt_tokens`/`usage.completion_tokens` (-> `InputTokens`/`OutputTokens`) -- OpenAI's totals include cached and reasoning tokens, the `*_details` objects only break out how many of the total were which; ai/openaicompat MUST NOT add them to `InputTokens`/`OutputTokens`. `ai/openairesponses` follows the IDENTICAL subset convention from `usage.input_tokens_details.cached_tokens`/`usage.output_tokens_details.reasoning_tokens` (see REQ: openairesponses-adapter). On `ai/anthropic`: `usage.cache_read_input_tokens`/`usage.cache_creation_input_tokens` (-> `CacheReadTokens`/`CacheWriteTokens`) are billed SEPARATELY, at their own per-token rates, and are NOT included in `usage.input_tokens` (-> `InputTokens`) -- they ARE additive. `ai.Usage.BillableTokens(provider string) int64` MUST sum correctly for a named adapter (`provider` matching that adapter's `ai.LLMProvider.Name()`): `InputTokens + OutputTokens` for `"openai-compatible"` AND `"openai-responses"` (no addition of the subset fields, both adapters); `InputTokens + OutputTokens + CacheReadTokens + CacheWriteTokens` for `"anthropic"` and any unrecognised provider name (the additive formula is the fallback, since silently dropping a populated `Cache*Tokens` field undercounts real spend more than the (currently nonexistent) case of a future adapter where adding it would double-count).

#### REQ: cloudproto-and-cloud-pass-tools-through

`ai/cloud` MUST pass `ChatRequest.Tools`/`ToolChoice`/`Reasoning` and `Message.ToolCalls`/`ToolResults`/`ProviderState` through unchanged (it marshals the whole `ai.ChatRequest`/`ai.Event`). `cloudproto.ReadEvents` MUST treat `tool.call` and `tool.result` as known event types (not silently dropped as unknown).

#### REQ: agent-loop-contract

`ai/agent.Loop` (`Provider ai.LLMProvider`, `Handlers map[string]Handler`, `MaxSteps` default 8, `MaxToolCalls` default 16) MUST hold NO mutable run state of its own -- a `Loop` VALUE (not just a pointer) MUST be safely copyable and independently reusable across concurrent `Run`/`RunWithTranscript` calls (no embedded `sync.Mutex`, no shared transcript field). It MUST itself implement `ai.LLMProvider` (`Name() "agent"`). `Loop.Run` MUST stream every step's events in order (text deltas, `tool.call`, `tool.result`, usage), append the assistant's tool-call message (its own accumulated `Text` alongside `ToolCalls`, plus `ProviderState`) and a `RoleTool` result message to its OWN copy of `req.Messages` between steps, and end with EXACTLY ONE final `EventCompleted` carrying the SUMMED usage across every step -- never a `EventCompleted` per step. A step with no tool calls ends the run, and its OWN text/`ProviderState` MUST be appended as a final assistant message too (not silently dropped from the transcript). A forced `ChatRequest.ToolChoice` (`"required"`, or a specific tool name) MUST apply ONLY to the first step; every later step resets it to `"auto"` so the model can naturally stop calling tools. Multiple tool calls returned in one step (parallel calls) MUST be executed SEQUENTIALLY, in the order the model returned them. `Loop.RunWithTranscript(ctx, req)` returns `(iter.Seq2[ai.Event, error], func() []ai.Message)`: the accessor returns the full transcript (including tool-call/tool-result messages, and the final assistant message) accumulated by THAT run so far, safe to call concurrently with draining the iterator; `Loop.Run` is `RunWithTranscript` with the accessor discarded, for callers that only need the events.

#### REQ: agent-loop-error-handling

A `Handler` returning a non-nil error is an infrastructure failure and MUST abort the `Run`: it is reported as a FATAL `ai.Error` pair (`ai.ErrCodeUpstream`, wrapping the handler's error), exactly like exceeding `MaxSteps`/`MaxToolCalls` (`ai.Error{Code: "limit"}`) or a cancelled `ctx` (checked before each step and before/after each tool call, yielding the fatal `ai.ErrCodeCanceled` pair) -- see the fatal-pair contract, REQ: fatal-error-contract. In all three abort cases, every tool call already issued in that step -- the one that failed/was cut off, and every call after it that never ran -- MUST still be synthesized as an `ai.ToolResult{IsError: true}` and appended as one `RoleTool` message before the fatal pair is yielded, so the persisted transcript stays a valid, replayable conversation even though the `Run` itself does not continue.

By contrast, a TOOL-level failure -- no handler registered for the call's tool name, or a `Handler` panicking (recovered) -- is NOT an infrastructure failure and MUST NOT abort the `Run`: it becomes an `ai.ToolResult{IsError: true}` fed back to the model, and the loop continues to the next step. Invalid JSON in a tool call's `Arguments` is likewise non-fatal: it becomes an `IsError` result without ever invoking the handler.

### Decision chain

#### REQ: chain-semantics

`decision.Chain` MUST run its `Providers` in order and return the first provider's decision that is valid (`decision.Validate` against the request's `Taxonomy`) and meets `MinConfidence` for both `Module` and (when set) `Intent`. `MinConfidence`'s zero value defaults to 0.7; a POSITIVE value is used as given; a NEGATIVE value means "accept any confidence" (no floor at all). A provider that errors, times out, abstains, returns an invalid decision, or returns a low-confidence decision MUST be recorded in `Trace.Attempts` and the chain MUST fall through to the next provider. Timeout detection MUST use `errors.Is(err, context.DeadlineExceeded)` (not `==`), so a provider that wraps `ctx.Err()` still classifies as `timeout`. Each provider's timeout is `Chain.Timeout` (default 1500ms) UNLESS the provider itself implements the optional `interface{ DecisionTimeout() time.Duration }`, in which case ITS value is used instead -- `ai/decision/rules.Provider` reports 300ms, `ai/cloud`'s decider and `ai/decision/llmdecider.Decider` both report 4s, reflecting how much slower a remote/inference call reasonably is than a local rule match. A chain where every provider falls through is not an error: `Chain.Decide` returns `ok=false`, and the product's main-LLM path classifies and answers in a single inference (there is deliberately no dedicated "no decision" LLM decider step distinct from the product's normal chat path).

#### REQ: rules-friendly-validation

`decision.Validate` requires `Interaction` to be a known, non-empty value. For the module-optional interactions (`confirmation`, `rejection`, `cancellation`, `undo`) an EMPTY `Module`/`Intent` is valid and skipped in both `Validate`'s module/intent lookup and `Chain`'s confidence check -- "Yes", "Cancel", "Undo that" answer the PREVIOUS turn's pending action and have nothing module-shaped to report. `Validate` also checks `Reference.Kind` against `Taxonomy.EntityTypes` and each `RequiredData` entry against `Taxonomy.DataKinds` when those lists are non-empty (empty list means "not constrained", same as the existing scope/presentation checks). `ai/decision/rules.Provider` fills `Module.Confidence`/`Intent.Confidence` to `1.0` when a matched rule leaves either at its zero value (with a non-empty `.Value`) -- a deterministic exact-phrase match that fired IS certain, and this spares every product `Rule` author the boilerplate of writing `Confidence: 1`.

#### REQ: rules-provider

`ai/decision/rules.Provider` MUST run its `Rule`s in declared order against text normalised by `Normalize` (lowercased, whitespace-collapsed, trailing `.!?` trimmed) and session state, returning the first rule's decision. It MUST NOT use a general-purpose regex/NLP engine — matching is exact-phrase (`Phrases`) or hand-written Go in each `Rule.Match`. Every matched decision MUST be declared deterministic (`decision.Deterministic`; REQ: decision-provenance), so a chain with a `Policy`, including `DurablePolicy`, accepts it as `deterministic` without calling the engines behind the rules. Products own their own `Rule` sets; this package supplies only the runner and the two normalisation helpers.

#### REQ: llmdecider-single-inference

`ai/decision/llmdecider.Decider` MUST make exactly one `ai.LLMProvider.Stream` call per `Decide`, with `ResponseSchema` set to the static, tested, STRICT-VALID JSON Schema of `decision.Decision` (including `interactionConfidence`) (see REQ: llmdecider-strict-schema), a system prompt that lists the request's `Taxonomy` (modules/intents/scopes/presentations/data kinds/entity types) and the product-neutral rules (never invent entity IDs; `reference` is an expression to resolve, not an ID; `requiredScopes` is the minimum; `canHandleDeterministically`/`needsLLM` are only true together when the product can fully answer from data alone), and a context block carrying session state (entity refs and titles only, never rendered data) plus a bounded tail of `Request.Recent` (`Options.MaxRecent`: zero/unset means the default of 8; a NEGATIVE value, not zero, means "no limit") and `Now`/`TZ`. It MUST parse the decision from the `EventStructured` event, falling back to extracting a JSON object from the collected text (tolerating a fenced block) when no structured event arrived; malformed JSON in either path MUST return an error (which `Chain` treats as a fall-through, per REQ: chain-semantics). It is an LLM decider, not a calibrated decision model: its confidences, and the `interactionConfidence` the schema asks for (0 when the model leaves it out), are the model's self-report (`Decision.Calibrated` stays false; provenance `self_reported`), so a `SelectionPolicy` never thresholds them as probabilities and never accepts its side-effectful interactions without the opt-in (REQ: selection-policy). It is useful as a fallback engine or an emulator behind a real one (REQ: engine-combinators). Nothing in it is Sneat- or DataTug-specific.

#### REQ: llmdecider-strict-schema

The wire JSON Schema `ai/decision/llmdecider` sends is written to satisfy OpenAI's strict structured-output rules, recursively: every object sets `additionalProperties: false`, and `required` lists EVERY key in `properties` (an optional field's optionality is expressed as a `["T","null"]` type union, never by omission from `required`). Because strict mode has no open-map type, `decision.Decision.Slots` (a `map[string]string`) is represented on the wire as an ARRAY of `{name, value}` objects and folded back into the map after parsing (`wireDecision.toDecision`) -- the wire shape intentionally does not mirror `decision.Decision`'s own JSON tags 1:1. A schema-walking test asserts the strict-mode rules hold recursively, not just at the top level.

#### REQ: decision-schema-evolvable

The `decision.Decision` JSON Schema embedded in `ai/decision/llmdecider` is intentionally evolvable: new fields are additive, and both `decision.Validate` and any consumer MUST treat unknown `module`/`intent`/`presentation` values as "not decided" rather than erroring the whole turn. A schema change is a reviewed diff to a static Go constant, not a struct-reflection surprise (`TestDecisionSchema_MatchesFields` pins the field-name correspondence).

### Scored decisions, policy and engines

#### REQ: decision-additive-scores

`decision.Decision` MUST gain only additive fields: `Scores` (probability per option of the Choice that picked the intent, keyed by option id, `"module/intent"` for a multi-module taxonomy), `Calibrated` (true only when the numbers are calibrated probabilities; false for an LLM decider's self-reported confidence and for rule matches), `Provenance()` (a method layered over `Calibrated`: `calibrated`, `self_reported`, or `deterministic`; see REQ: decision-provenance), `Outcome` (the verdict as a record, set by a `Chain` with or without a `Policy`, by `SelectionPolicy.JudgeDecision` and by an engine built with `compose.WithPolicy`; INFORMATIONAL: kept on the wire for traces and telemetry, never believed when it comes from a provider, a remote engine or storage), `InteractionConfidence` (the engine's confidence in `Interaction`, 0 when it reports none), `InteractionScores` (a calibrated engine's probabilities for the options of the Choice that picked `Interaction`, keyed by `Interaction` value; the evidence a calibrated claim about a side-effectful interaction needs, REQ: selection-policy) and `Model` (the model id the engine reported, empty when it reports none; `decision.Trace` and `decision.Report` carry it too, so every answer says which model produced it). ONE rule MUST say whether a caller may act on an answer, and `Decision.Actionable()` and `Selection.Actionable()` MUST agree: it is true only for an explicit, positive VERDICT that this package's judges stamped on the decision in an UNEXPORTED field (so no JSON, remote engine, provider or stored decision can set it; the exported `Outcome` is not consulted): `selected` or `several` (a policy SELECTED a calibrated answer), `accepted` (the caller's policy explicitly opted in to an uncalibrated decision at a stated bar, see REQ: selection-policy), `deterministic` (REQ: decision-provenance) or `floor` (a `Chain` without a `Policy` accepted the answer at its `MinConfidence` floor, the caller's explicit bar); `uncertain`, `none`, `unscored`, `invalid` and the EMPTY `Outcome` are never actionable. An answer is therefore never actionable by omission: the zero `Decision` is not actionable, and neither is a decision returned by a provider or engine used directly (`typesafe.Client.Decide`, `compose.Single`, an LLM decider) at any confidence, until a `Chain` (which stamps `floor`, `deterministic` or the policy's verdict on every answer it accepts) or an engine built with `compose.WithPolicy` judged it. A `Chain` without a `Policy` MUST ALWAYS stamp `floor` (or `deterministic`, for a decision declared with `decision.Deterministic`) on an answer that passes its floor, whatever `Outcome` the provider wrote, so a provider can never claim `selected`, `accepted` or `deterministic` for itself; the only provider verdict it honours is a NON-actionable one stamped by an engine built with a policy (`uncertain`, `none`, `unscored`). `Validate` also rejects an `InteractionConfidence` or an `InteractionScores` value outside [0,1]. A decision that was marshalled and read back (a stored row, a trace, a cloud response) is NOT actionable and has lost its provenance class, a replayed rule decision included (`self_reported`); the package documentation, the README and this specification MUST say so, and `Chain.Rejudge(d, taxonomy)` MUST be the explicit way back: it discards any claimed `Outcome`, judges the decision as the chain would judge a provider's answer (its policy or floor, and the side-effect gate), can never make it deterministic, and reports whether the result is actionable; a product that must replay a rule decision as deterministic re-runs its rules. `decision.Request` MUST gain optional `Context` (JSON-able names and public metadata only, never row data, credentials or user identifiers) and `decision.Taxonomy` optional `Descriptions` (one line per taxonomy entry, keyed by module name, `"module/intent"`, presentation, data kind or entity type). Everything placed in a `Request` or `ScoreRequest` (text, recent turns, context, descriptions) is sent verbatim to the engine's operator, and the package documentation of `ai/decision` and `ai/decision/typesafe` MUST say so and tell callers to send metadata, never row data. Existing decisions, requests and `Chain` behaviour with no `Policy` MUST be unchanged, apart from the explicit `floor` verdict, the closed side-effect gap (REQ: selection-policy) and the stop options (REQ: chain-stops-on-quota).

#### REQ: decision-provenance

A `decision.Decision` has one of three provenances, `Decision.Provenance()`: `calibrated` (`Calibrated` is true: calibrated probabilities from a real decision model), `self_reported` (the default: an LLM emulator's own confidence, a proposal and not a probability) and `deterministic` (exact logic with no model in the loop, such as a rule table). `Calibrated` stays, unchanged on the wire, and the provenance layers over it, so decisions, requests and `cloudproto` bodies stay compatible. The deterministic class MUST be declared only through `decision.Deterministic(d)`, which marks the decision, sets `Outcome` to `deterministic` and clears everything that would claim a model stood behind it (`Calibrated`, `Scores`, `Model`); `ai/decision/rules.Provider` MUST call it for every matched rule, and a product's own deterministic provider (a lookup table, a command parser) MAY. The marker MUST live in an unexported field, so it can never be set by JSON, an LLM's structured output or a stored decision, and it survives being copied through engines and breakers; a decision that merely carries `Outcome: deterministic` is judged afresh by `Chain` (which stamps its own verdict), and once marshalled and read back a deterministic decision is `self_reported` and not actionable (REQ: decision-additive-scores, `Chain.Rejudge`). A cloud or other remote response MUST NOT be able to claim `deterministic`: protocol version 1 has no wire form for it, `ai/cloud` ignores the `outcome` of a server's decision entirely (a server refusal, such as `uncertain` over calibrated scores, is advisory input: the local policy judges, and the trace shows only the local verdict), and a server answer is therefore at most `calibrated` and otherwise `self_reported`; a product that needs the deterministic class keeps its own rules in front of the server (`aiconfig.Deps.ExtraDecision` runs first). Every `SelectionPolicy` MUST judge a deterministic decision `deterministic` (REQ: selection-policy), so a chain with a `Policy`, `DurablePolicy` included, accepts a rule match without calling any engine behind the rules and without the match depending on `AcceptUncalibratedAt`. `Trace.Provenance` MUST echo the answering decision's provenance.

#### REQ: chain-stops-on-quota

`decision.Chain` MUST stop, instead of trying its next provider, when a provider reports an exhausted allowance (an error matching `decision.ErrQuota`, attempt outcome `quota`), a spent spending cap (`decision.ErrBudget`, outcome `budget`, REQ: engine-budget) or a misconfigured engine (`decision.ErrMisconfigured`, outcome `misconfigured`), so exhaustion and misconfiguration are loud and a paid backup behind them is never silently billed. They are controlled by `Chain.StopOnQuota` (quota and budget) and `Chain.StopOnMisconfigured` (type `decision.StopPolicy`), whose zero value is `decision.StopChain` (stop); the explicit, named opt-out is `decision.FallThrough`. `StopPolicy` marshals as the string `stop` or `fall_through` and refuses any other text. When an engine joins several legs' errors (a `Fallback` whose primary was unavailable and whose backup was out of budget) the chain and the combinators MUST classify the loud condition (`quota`, `budget`, `misconfigured`) before the quiet ones (`unavailable`, `auth`, `rejected`, a timeout), so the quiet one cannot hide it, and an abstention elsewhere in the call MUST NOT swallow a refusal the engine does not absorb. A stopped chain returns `ok=false` like any chain that decided nothing, with `Trace.StoppedBy` set to the outcome and `Trace.Err()` returning an error that matches `ErrQuota`, `ErrBudget` or `ErrMisconfigured`, names the provider and states the provider's condition once, so a product can tell "nobody decided" from "stopped": a product MUST check `Trace.StoppedBy` before treating `ok=false` as "use the paid main-LLM path", because that path is what the stop protects; the package documentation, the README and `aiconfig` MUST say so. A stop also skips every provider after the one that stopped, deterministic ones included, so `Chain`'s documentation MUST say to list deterministic providers (rules) FIRST; `decision.DeterministicProvider` (implemented by `ai/decision/rules.Provider`) marks them, and `aiconfig` MUST reject a deterministic provider listed in `decision.engines` after another engine (rules belong in `Deps.ExtraDecision`, which runs first). An engine configured with `compose.OnQuota` has already chosen to fail over and reports no quota error when its backup answers. `aiconfig` MUST mirror the switches (`decision.stopOnQuota`, `decision.stopOnMisconfigured`, default true, also `AI_DECISION_STOP_ON_QUOTA` and `AI_DECISION_STOP_ON_MISCONFIGURED` as booleans, a malformed value being a `Build` error) and `Providers.Chain()` MUST build the configured chain; `decision.fallbackOn: [quota]` together with an explicit `stopOnQuota: true` MUST be a `Build` error (fail over inside the engine or stop the chain, never an unstated precedence). Existing chains that listed a quota-refusing provider ahead of another provider now stop at it unless they opt out.

#### REQ: engine-budget

`compose.NewBudget(engine, BudgetOptions{MaxCalls, Per, Clock})` MUST cap how often any engine is called, because no engine's own errors can bound the cost of a paid backup: TypeSafe documents one 429 error (a rate limit) and no way to tell a spent account from it, so every 429 stays a transient failure, a `Fallback` hands every call to the backup while the primary keeps failing, and nothing else bounds the bill (a TypeSafe 402 is not documented either and is not mapped to `ErrQuota`). A `Budget` MUST admit at most `MaxCalls` calls (decisions and scored questions together, one each, counted on admission; a call to an engine that does not score is `unsupported` and not counted) per fixed `Per` window (`Per` 0: the cap never resets), and beyond the cap MUST NOT call the engine and MUST return an error matching `decision.ErrBudget` (attempt outcome `budget`), distinct from `ErrQuota`, whose message says the cap. A `MaxCalls` below 1 or a negative `Per` MUST fail every call with an error matching `decision.ErrMisconfigured` instead of allowing everything or nothing; a nil engine returns `compose.ErrNoEngine`. `ErrBudget` is a quota-class refusal: a `Breaker` ignores it, `Fallback`, `Hedged` and `Race` treat it exactly as they treat `ErrQuota` (no backup unless `OnQuota`; a `Hedged` primary's or any `Race` engine's refusal ends the call), and `decision.Chain` stops at it by default (REQ: chain-stops-on-quota). The `Budget` MUST be placed inside the `Breaker` (`NewBreaker(NewBudget(engine, ...))`), so a call an open breaker blocked is not counted. `aiconfig` MUST expose it as `decision.backupBudget: {maxCalls, per}` (and `AI_DECISION_BACKUP_BUDGET_MAX_CALLS`, `_PER`), applied to the BACKUP engine of strategy `fallback` or `hedged`, and a `backupBudget` without engines, with another strategy, with `maxCalls` below 1 or with a malformed or negative `per` MUST be a `Build` error. The `README`, the `aiconfig` documentation and the `compose` package documentation MUST state the hole and the guard, and recommend that anonymous or public traffic run the calibrated engine alone (no paid backup) or with a budgeted backup.

#### REQ: scored-candidates

`decision.ScoredProvider` MUST answer a `ScoreRequest` (a state text, optional context, and one or more `Question`s) with a `ScoreResult` holding one `Answer` per question: a probability per candidate, sorted best first, plus the engine name and the model id the engine reported. A `Question` is `KindChoice` (exactly one candidate is right; probabilities sum to 1; may name a `NoneID`) or `KindRelevance` (each candidate is judged independently; probabilities need not sum to 1). An `Answer` carries `Confidence` only when the engine reports one (`HasConfidence`) and `Calibrated` (true for a real decision model, false for an LLM emulator). `ValidateScoreRequest` and `ValidateScoreResult` MUST reject a malformed request (no questions, duplicate or empty ids, unknown kind, a `NoneID` that is not a candidate of a choice question) and a result that does not answer every question with known candidates and finite probabilities in [0,1] (a choice answer must also sum to about 1).

#### REQ: selection-policy

`decision.SelectionPolicy` MUST turn an `Answer` into an `Outcome` (`selected`, `several`, `uncertain`, `none`, `unscored`; a `Decision` can also be `accepted`, `deterministic` or `invalid`) so no caller compares a probability to a number of its own. An uncalibrated answer to a scored question MUST be `unscored`, never a selection, but it MUST carry a PROPOSAL so a read-only narrowing still works with an LLM engine: `Selection.Proposals` (not `Picks`) holds, for a relevance answer, the candidates at or above `MinProbability` (best first, capped by `MaxPicks`) and, for a choice answer, its top candidate (none when that is the `NoneID`); `Picks`, `Strong` and `Potential` stay empty and `Selection.Actionable()` is false, so a caller that sees `len(Picks) > 0` is looking at policy-selected, calibrated picks only. An uncalibrated DECISION (an LLM emulator's self-reported confidence) is acted on only through the policy's explicit opt-in `AcceptUncalibratedAt`: when both its module and intent confidences reach it (the module confidence is exempt for a module-optional interaction) the verdict is `accepted` (actionable; `Calibrated` stays false), otherwise `unscored` (not actionable). A DETERMINISTIC decision (a rule match, REQ: decision-provenance) is NOT governed by `AcceptUncalibratedAt`: every valid policy, `DurablePolicy` included, judges it `deterministic` (reason `deterministic_rule`, actionable) whatever the policy's numbers, and an LLM self-reporting 1.0 is never `deterministic`. A side-effectful interaction (`confirmation`, `rejection`, `correction`, `cancellation`, `undo`; `decision.SideEffectful`) from an uncalibrated engine MUST NOT be `accepted` without the separate, explicit opt-in `AcceptUncalibratedSideEffects` (default false for both named policies; reason `side_effect_uncalibrated` otherwise, whatever the confidences and under any `AcceptUncalibratedAt`), and even then only when its `InteractionConfidence` is above 0 and reaches the larger of `AcceptUncalibratedAt` and `DurableMinConfidence` (reason `interaction_low_confidence`): an engine that reports no interaction confidence is never accepted for these kinds, so an interaction a calibrated engine abstained on is never handed to a weaker gate. A CALIBRATED decision whose `Interaction` is side-effectful is gated too, because a remote engine's `Calibrated` flag is only a claim: it needs `InteractionScores` that make `Interaction` their top option (otherwise it counts as a self-report and needs the opt-in above), its `InteractionConfidence` above 0 and at least the larger of the policy's `MinConfidence` and `DurableMinConfidence` (`interaction_low_confidence`, `uncertain`), and its top interaction clear of the runner-up by at least the larger of `MinGap` and the durable gap (`interaction_narrow_gap`, `uncertain`); `InteractionScores` that contradict `Interaction` are `invalid` (`interaction_not_top`). `SelectionPolicy.JudgeDecision(d)` MUST return `d` with the verdict stamped and the `Selection`; it is the judge `Chain` and `compose.WithPolicy` use. A calibrated decision with `Scores` MUST be judged on its OWN module/intent, never on whichever option tops its `Scores`: an own option missing from `Scores` (`decision_not_scored`), scoring below another option (`decision_not_top`) or a non-finite or out-of-range score (`bad_scores`) is `invalid` (not actionable, recorded as an `invalid` attempt and falling through even under `KeepNonSelected`), and a tie reads as a zero gap (`uncertain`, `narrow_gap`). `AcceptUncalibratedAt` is 0, never, by default and for `DurablePolicy`, and 0.70 for `NarrowingPolicy` (the floor a chain applies without a policy; a narrowing decision is a proposal for choosing what to examine, never durable knowledge); a calibrated decision without `Scores` is judged like an uncalibrated one. Only a calibrated `selected` or `several` selection, an `accepted` decision and a `deterministic` decision (and a policy-less chain's `floor`) are actionable. `SelectionPolicy.AtLeast` MUST return the stricter of two policies (each threshold the larger, an uncalibrated decision accepted only if both accept it, side-effectful ones only if both opt in). A `KindChoice` answer MUST be `none` when the top option is the question's `NoneID`, `uncertain` when the confidence is below `MinConfidence` (the engine's own, or, when it reports none, the top probability) or the top option leads the runner-up by less than `MinGap`, and `selected` otherwise (so a lone candidate at probability 0.1 is not selected). A `KindRelevance` answer MUST select every candidate at or above `MinProbability` (best first, capped by `MaxPicks`), mark those at or above `StrongProbability` as strong, keep those between `PotentialProbability` and `MinProbability` as potential, and be `selected` (one pick), `several` (more than one), `uncertain` (only potential candidates) or `none`. `Validate` MUST reject a threshold outside [0,1], an unordered set, a negative `MaxPicks`, and a `MinConfidence` or `MinProbability` of 0, so the zero value (which would select everything) is invalid, and `Evaluate` and `EvaluateDecision` MUST select nothing (`uncertain`, reason `invalid_policy`) with an invalid policy; `Validate` MUST also reject an `AcceptUncalibratedAt` outside [0,1]. The defaults live in named constants and two named policies: `NarrowingPolicy` (narrowing a set of candidates: confidence 0.50, gap 0.20, select 0.60, strong 0.85, potential 0.30) and `DurablePolicy` (answers stored and reused as fact: confidence 0.90, gap 0.20, select 0.90, strong 0.95, potential 0.60). These numbers are PROVISIONAL: they come from a single small measurement against one model version, are not a calibration, and MUST be re-measured on the product's own corpus and for the exact model id in use; the code, the configuration documentation and this specification MUST say so. `0.96/0.91/0.72` independent relevance probabilities MUST be `several`; `0.38/0.35/0.33` MUST be `uncertain` as a choice and as a relevance list.

#### REQ: chain-policy

`decision.Chain` MAY carry a `Policy`. With a policy set the policy alone owns the bar for every answer, and `MinConfidence` is not applied: a calibrated decision with `Scores` is judged by its probabilities, a deterministic decision is `deterministic` (REQ: decision-provenance), and any other decision (uncalibrated, or calibrated without scores) is `accepted` only if the policy opts in through `AcceptUncalibratedAt` (and, for a side-effectful interaction, `AcceptUncalibratedSideEffects`) and is `unscored` otherwise (REQ: selection-policy), so a durable chain never silently drops its bar to an LLM's self-reported confidence when the calibrated engine is down. Only an actionable verdict stops the chain: any other MUST be recorded in `Trace.Attempts` as `uncertain` (with the verdict and reason as detail, for example `uncertain: low_confidence`, `unscored: not_calibrated`) and the chain MUST fall through to the next provider. A chain with `KeepNonSelected` set MUST instead return the non-actionable answer with `ok=true`, `Decision.Outcome` and `Trace.Outcome` set, and because `ok=true` is then not enough to act on, a caller MUST check `Decision.Actionable()` (false for `uncertain`, `none` and `unscored`) before acting; the `Chain` and `ai/decision` documentation MUST say so. A provider answer that fails `Validate`, or whose verdict is `invalid` (it contradicts its own scores), MUST still fall through, even with `KeepNonSelected`. With no `Policy` the chain MUST behave as before (`MinConfidence` floor) and MUST stamp the explicit `floor` verdict (or `deterministic`) on an answer it accepts instead of leaving the `Outcome` empty; a provider's own non-actionable verdict (`uncertain`, `none` or `unscored`, from an engine built with a policy) is honoured as `uncertain` and never acted on by omission; and for ANY non-deterministic side-effectful interaction, calibrated or not, module-less or not, with or without a policy, the chain MUST require an `InteractionConfidence` above 0 and at least the larger of its floor and `DurableMinConfidence`, even with `MinConfidence` negative (attempt `low_confidence`, detail `interaction=<value> (<reason>)`); a calibrated claim additionally needs `InteractionScores` that back it (contradicting or narrow-gap scores are refused), and without them it counts as a self-report, so a server answering `{"interaction":"undo","calibrated":true}` with no scores and no confidence is refused under every chain; a provider's own claimed `Outcome` is never believed (the chain stamps `floor` or its policy's verdict). `Trace` MUST carry the answer's `Provenance`.

#### REQ: llmdecider-scores

`llmdecider.Decider` MUST also be a `decision.ScoredProvider`, so a `Fallback` from a decision model to an LLM decider answers scored questions (table narrowing is a scored question): `Score` MUST make ONE structured inference for every question of the `ScoreRequest` (a strict JSON Schema of `{answers: [{questionId, scores: [{id, probability}]}]}`), and return a `ScoreResult` that passes `ValidateScoreResult` with `Calibrated` false and `HasConfidence` false on every answer. A choice answer MUST be normalised to sum to 1, probabilities clamped to [0,1], candidates the model left out scored 0 and ids it invented dropped; a question the model did not answer, or a choice with no probability on any candidate, MUST be an error matching `llmdecider.ErrBadScores` that quotes nothing; a request that fails `ValidateScoreRequest` MUST be refused with an error matching `decision.ErrInvalidRequest` that quotes no id. Under a `SelectionPolicy` such an answer is `unscored` with a proposal (REQ: selection-policy), never a selection.

#### REQ: engine-combinators

`ai/decision/compose` MUST provide `Single`, `Fallback(primary, backup)`, `Hedged(primary, backup, after)` and `Race(providers)`, each a `decision.Provider`, a `decision.TracedProvider` and a `decision.ScoredProvider` (a provider that is not scored is recorded `unsupported`), so they nest inside `decision.Chain` and inside each other. An engine's answer is accepted only when valid (`decision.Validate`, or `ValidateScoreResult`). `Fallback` MUST start the backup only when the primary errors, times out, is unavailable (open breaker), is unsupported, is rejected or unauthorised, or returns an invalid answer; it MUST NOT start it for an exhausted allowance (`decision.ErrQuota`, outcome `quota`, or a spent budget, `decision.ErrBudget`, outcome `budget`: the error is surfaced to the caller, because a backup is typically a paid engine) unless `WithFallbackOn(OnQuota)` says so, and MUST NOT start it for a misconfigured endpoint (`decision.ErrMisconfigured`, outcome `misconfigured`) under any option, so a configuration error is loud instead of absorbed by a backup forever. `Hedged` MUST start the primary, start the backup only after the latency budget passes or as soon as the primary fails, accept the first valid answer and cancel the other through its context. `Race` MUST start every engine at once, accept the first valid answer and cancel the rest. An abstention or an uncertain answer is an answer: by default it MUST NOT start the backup in any strategy (`WithFallbackOn(OnAbstain|OnUncertain)` opts in; `OnUncertain` needs `WithPolicy`). `Hedged` MUST apply ONE rule to them whatever the timing: when the primary abstains or answers uncertain and `WithFallbackOn` names that outcome, the backup is used (started at once if the hedge had not fired, waited for if it was running); otherwise the primary's abstention or uncertain answer stands and a still-running backup is cancelled. In `Hedged` and `Race`, which run engines at once, an exhausted allowance (unless `OnQuota`) or a misconfigured endpoint reported by the `Hedged` primary (even after the hedge fired and the backup is running) or by ANY `Race` engine MUST end the call: the other engines are cancelled, no answer is returned (not even an abstention or uncertain answer another engine had given) and the error matches `decision.ErrQuota` or `decision.ErrMisconfigured`, so a started backup cannot take over a metered caller's traffic; a `Hedged` backup's own refusal is just a failed backup. Each engine MUST run under its own timeout (its `DecisionTimeout()`, else `WithDefaultTimeout`), enforced by the combinator even for an engine that ignores its context; a combinator MUST report its own `DecisionTimeout()` (single/race: the longest, none: 0; fallback: the sum; hedged: the longer of the primary and the hedge delay plus the backup). A cancelled loser MUST be recorded `cancelled`, must not count as a failure, and the combinator MUST NOT wait for it; the one exception is a `Hedged` primary still running when its hedge answered, which is cancelled with the cause `compose.ErrSuperseded` so a `Breaker` tallies it as SLOW, apart from failures (REQ: circuit-breaker); the documentation MUST say to set the hedge delay above the primary's p99. With `WithPolicy` every decision an engine returns MUST carry the policy's stamped verdict (`Outcome`, and the judged state `Actionable` reads) and only an actionable one counts as decided (an answer judged uncertain, none or unscored is recorded `uncertain` and, as an answer, returned with `ok=true` and a non-actionable `Outcome` when nothing better exists); without `WithPolicy` an engine has no bar to judge by and returns the decision with whatever verdict the engine behind it stamped (none for a plain provider: NOT actionable, whatever `Outcome` the provider wrote) for the `Chain` that owns the bar; with a policy, a deterministic decision is accepted as `deterministic` and a decision that contradicts its own scores is recorded `invalid`. Each answered scored attempt MUST carry its own `Attempt.Usage`. An invalid score request MUST return an error matching `decision.ErrInvalidRequest` that quotes no id, only positions and counts. No goroutine of a context-honouring engine may outlive a call. A constructor MUST NOT panic: an engine built with no providers, a nil provider, or as a zero value MUST return an error matching `compose.ErrNoEngine` from its first call, and `DecisionTimeout()` MUST then be 0. Time is read through a `Clock` so tests are deterministic. There are no retries: trying another engine is the only retry.

#### REQ: decision-trace-engine

Every decision MUST say which engine answered, and which model. `decision.Report` (strategy, answering leaf engine, its model id, every attempt with role and latency, `FallbackFired`, `HedgeFired`) is returned by a `TracedProvider`; `decision.Chain` MUST use it so `Trace.Attempts` lists every engine tried rather than the outermost wrapper, set `Trace.Engine`, `Strategy`, `FallbackFired`, `HedgeFired`, `Calibrated`, `Provenance`, `Outcome` and `Model`, and relabel the answering attempt when the chain itself rejects the answer (`invalid`, `low_confidence`). `Attempt.Outcome` gains `unavailable` (a circuit breaker was open; an error wrapping `decision.ErrUnavailable` is recorded so), `cancelled`, `uncertain`, `unsupported`, `rejected` (an error matching `decision.ErrInvalidRequest`), `auth` (matching `decision.ErrAuth`), `quota` (matching `decision.ErrQuota`: the allowance is exhausted), `budget` (matching `decision.ErrBudget`: a spending cap the caller set is used up) and `misconfigured` (matching `decision.ErrMisconfigured`). `Attempt.Latency` is a wall-clock duration whose wire form is the integer `latencyMs` (milliseconds, rounded down); the legacy `latency` (nanoseconds) is still written and read when `latencyMs` is absent. `Attempt.Usage` carries what that attempt consumed (nil, not zero, when its engine reported none) so hedged and fallen-back calls can be metered per engine. `compose.Breaker` MUST be a `TracedProvider` that passes the wrapped engine's `Report` on (an engine that reports nothing is reported by name only), so a breaker never hides an engine's attempts or its abstention reason from the chain's trace. A trace MUST carry no caller-supplied content: the detail of an `invalid` attempt states only the number of validation problems (`decision.InvalidDetail`), never candidate, question or module ids.

#### REQ: circuit-breaker

`compose.NewBreaker(p)` MUST wrap a provider so a down engine is not called on every request: after 5 consecutive engine-health failures within 30 seconds it opens and returns an error wrapping `decision.ErrUnavailable` at once, without calling the engine, for a 30 second cooldown; then it lets exactly one probe through (half-open), closes on its success and reopens on its failure. Only engine-health failures count: a transport error, a timeout (including a `Hedged` primary that outran its own timeout), a server error, a rate limit or overload. These MUST NOT count: a request refused as invalid or an authentication failure (errors matching `decision.ErrInvalidRequest` and `decision.ErrAuth`, which are the caller's fault and must not take a healthy engine out of service for everyone), an exhausted allowance (`decision.ErrQuota`), a spent budget (`decision.ErrBudget`), a misconfigured endpoint (`decision.ErrMisconfigured`), an unsupported operation, an abstention, an invalid answer, and a cancellation by the caller or by a race winner. A `Hedged` primary still running when its backup answered (cancelled with `compose.ErrSuperseded`) is SLOW, tallied apart from failures (`Breaker.Stats`), reset by a call that completes in time, and opens the breaker only when `WithBreakerSlowThreshold(n)` asks for it (default: never), so a healthy primary whose latency sits above the hedge delay is not taken out of service by its own hedge; a primary that also outruns its own timeout is a plain timeout failure. A failure whose error carries a retry delay (`decision.RetryDelay`, for example an HTTP `Retry-After`, capped at 10 minutes) MUST keep the breaker open at least that long. A call that started before the breaker last opened MUST be ignored when it finishes, so a late success cannot close an open breaker. The breaker MUST return as soon as its call's context is done even if the engine ignores cancellation, and MUST bound the half-open probe (`WithBreakerProbeTimeout`, default 10 seconds, even when the caller's context has no deadline): a probe that overruns counts as a failure and the breaker returns to open, so a hung engine cannot hold it half-open. Threshold, window, cooldown and probe timeout are options; a state-change callback lets the host log and count changes. A breaker built with no engine MUST return an error matching `compose.ErrNoEngine` from `Decide` and from `Score` alike. The breaker is transparent in traces (it reports the wrapped engine's name and timeout, and passes on the wrapped engine's `Report`). State is per instance and in memory.

#### REQ: config-selects-engines

`aiconfig.Decision` MUST let configuration (YAML, JSON or `AI_DECISION_ENGINES`, `_STRATEGY`, `_HEDGE_AFTER`, `_POLICY`) choose the decision engines, how they combine and the selection policy, with no code change: `engines` (names resolved against `Deps.Engines`), `strategy` (`single`, `fallback`, `hedged`, `race`; default `single` for one engine and `fallback` for two), `hedgeAfter` (a Go duration, default 600ms), `breaker` (default true: each engine behind a circuit breaker), `breakerSlowThreshold` (consecutive hedge-cancelled calls that open a primary's breaker; default 0, never), `fallbackOn` (`abstain`, `uncertain`, `quota`), `policy` (`narrowing`, `durable`, returned as `Providers.Policy`) and `stopOnQuota` and `stopOnMisconfigured` (default true, also `AI_DECISION_STOP_ON_QUOTA` and `AI_DECISION_STOP_ON_MISCONFIGURED`; REQ: chain-stops-on-quota; returned as `Providers.StopOnQuota`/`StopOnMisconfigured`, and `Providers.Chain()` builds the configured `decision.Chain`) and `backupBudget` (REQ: engine-budget) and `policyValues` (optional overrides of the named policy: `minConfidence`, `minGap`, `minProbability`, `strongProbability`, `potentialProbability`, `maxPicks`, `acceptUncalibratedAt`, `acceptUncalibratedSideEffects`; unset values keep the named policy's, the result is validated with `SelectionPolicy.Validate`, and the policy is renamed `<name>+custom`). Configured engines MUST run after the product's `ExtraDecision` providers and before the cloud decider, and `Decision.Provider: disabled` MUST skip them. An unknown engine, strategy, policy or trigger, a wrong engine count for the strategy, a malformed duration, `policyValues` without a `policy`, an invalid resulting policy, an invalid `backupBudget`, a malformed stop or budget environment value, `fallbackOn: [quota]` with an explicit `stopOnQuota: true`, or a deterministic provider listed after another engine MUST be a `Build` error.

### TypeSafe decision model

#### REQ: typesafe-client

`ai/decision/typesafe` MUST be a client for TypeSafe AI's System One API: `POST {BaseURL}/v1/systemone` (default `https://api.typesafe.ai`) with `Authorization: Bearer <key>`, a JSON body `{state, model, questions}`, questions of type `choice`, `score` or `noul`, and a response `{model, answers, usage}`. `Config.Model` MUST be required (`New` fails without it): thresholds are only valid for the model they were measured on, so a versioned id such as `jev-1.13.0` is pinned in production, and the moving alias `jev-latest` (`typesafe.ModelLatest`) is available only as an explicit choice; the model id the API returns MUST be recorded in every `Decision` and `ScoreResult`. `BaseURL` MUST be `https` (`http` only for a loopback host, for tests) and carry no credentials; the default HTTP client MUST NOT follow redirects (a 307/308 would re-send the state and the key to another host; a 3xx is `ErrUnexpectedStatus`), and a caller-supplied `HTTPDoer` is documented to need the same. HTTP MUST go through an `HTTPDoer` seam. It MUST NOT retry. It MUST NOT log. An error MUST carry only the status, the API's `error_type`, the `x-typesafe-request-id`, the retry delay and counts, never the response body, the submitted state, or any caller-supplied id, and the key MUST never appear in an error or `String()`. Status mapping: 401/403 `ErrAuth` (matches `decision.ErrAuth`); 400 and 422 `ErrInvalidRequest` (documented as 422, observed as 400; matches `decision.ErrInvalidRequest`); 429 `ErrRateLimited` (ALWAYS a transient rate limit, never `decision.ErrQuota`: TypeSafe's documentation, checked 2026-10, defines one 429 error, "the rate limit was exceeded", and no error type, status or body field that tells an exhausted allowance or billing from rate limiting, so the package says so in its documentation and a product that needs a hard allowance stop enforces it behind the cloud boundary, whose protocol carries `quota`); 529 `ErrOverloaded`; other 5xx `ErrServer`; any other non-2xx `ErrUnexpectedStatus`; a malformed 2xx `ErrBadResponse`. `Retry-After` MUST be read in both forms (seconds and HTTP date), capped, and exposed through `decision.RetryDelay`. Requests MUST be checked locally before any call, each failure matching `ErrInvalidRequest`: more than 255 options in a Choice (`ErrTooManyOptions`, on `Decide` as on `Score`), more than 255 questions in one call (`ErrTooManyQuestions`, a local guard: a relevance question costs one per candidate) and an estimated state over the 32k-token budget (`ErrStateTooLarge`; `Config.MaxStateTokens`; the estimate is approximate, bytes/4). A per-call hook MUST report model, usage, latency and status without any content. Published limits not enforced locally: 40 requests and 100K tokens per second, 64k tokens of context per request, price per input token. The package documentation MUST state that everything in the state is sent verbatim to the operator.

#### REQ: typesafe-score-mapping

`typesafe.Client.Score` MUST implement `decision.ScoredProvider` with one API call for all questions. A `KindChoice` question MUST become one Choice (candidate ids as options, descriptions as criteria); a `KindRelevance` question MUST become one Noul per candidate, because a Choice distribution sums to 1 and cannot express several relevant candidates. The state MUST be an object holding the question text, the context and the candidate catalogue of every relevance question, and each Noul MUST refer to its candidate and to the question by path (`state.candidates.q0[3]`, `state.question`): measured against the live API on a catalogue of 11 tables, the relevant ones separated cleanly (0.67-0.78 against at most 0.29) with the catalogue in the state, and did not (0.1-0.4 for all) with a description inside each question. Answers MUST be `Calibrated`; a Choice answer carries the API's confidence and the question's `NoneID`, a Noul answer carries none. A malformed answer MUST be `ErrBadResponse` naming the question by position, not by id.

#### REQ: typesafe-decide-mapping

`typesafe.Client.Decide` MUST implement `decision.Provider` with one API call: one Choice over `"module/intent"` plus `other` (its confidence is both the module and the intent confidence, its probabilities are `Decision.Scores`, `other` abstains); one Noul per scope and per data kind (a scope of the chosen module or a data kind whose probability reaches the policy's select threshold is required); one Choice over entity types plus `none` and one over presentations plus `none`, each used only when the policy selects it; and one Choice over the `Interaction` enum, ALWAYS asked and run through the selection policy like every other Choice: only a SELECTED option becomes `Interaction` (with `InteractionConfidence` and the Choice's probabilities as `InteractionScores`), an interaction that acts on a pending or previous action (`confirmation`, `rejection`, `correction`, `cancellation`, `undo`) must additionally clear the stricter of the policy and `DurablePolicy` (a wrong "yes" is a side effect), and when the policy does not select the interaction the provider ABSTAINS (`ok=false`, no error) rather than hand a confident intent to a caller with a guessed turn kind, so the next provider or the product's main-LLM path classifies the turn (REQ: selection-policy then keeps a weaker uncalibrated engine from accepting what this one abstained on); the abstention MUST say why: `typesafe.Client` is a `TracedProvider` whose report carries one `abstained` attempt whose `Detail` is a fixed, non-sensitive reason code (`intent_other`, `interaction_low_confidence`, `interaction_gap`, `interaction_below_durable`, or `interaction_<reason>`) and whose `Usage` is what the call billed, so the interaction-abstention rate can be measured from `Trace.Attempts`; an answer whose top option (by the `choice` field or the probabilities) is outside the enum is `ErrBadResponse`, and no interaction is ever assumed; a request whose single module is named `choose:<role>` MUST become exactly one Choice over its intents, with `Interaction` `question` by construction. More than 255 options in the intent Choice MUST be refused locally. `Request.InteractionID`, `ClientContext`, `Now` and `TZ` MUST NEVER be forwarded. The decision is `Calibrated`, carries the API's model id in `Decision.Model`, has `NeedsLLM` set, and validates against the taxonomy; called directly it is unjudged (empty `Outcome`, not actionable) until a chain or policy judges it on its own intent.

#### REQ: typesafe-live-test-opt-in

The package MUST include live integration tests that make real API calls only when `JEV_API_KEY` is set AND `AICHAT_LIVE_JEV=1` (the model is `JEV_MODEL`, else `ModelLatest`, an explicit choice there), and are skipped otherwise; they MUST log probabilities, the model id, latency and token usage only, never the key, and use a small invented fixture (a lending-library schema), never real or third-party data.

### Context Manager

#### REQ: ctxmgr-retain-then-required

`ctxmgr.Manager.Select(required, available, pinnedScopes)` MUST always include every scope in `required` (an empty, non-nil slice is a valid "this decision needs no scopes"), and MUST additionally retain every static scope already sent earlier in the conversation even when no longer required, so the cached prefix does not shrink or shift on a turn that needs less context than a previous one. When there was NO decision at all (decision off, or every provider abstained/errored), the product MUST call `Manager.SelectAll(available, pinnedScopes)` instead -- a distinct method, not `Select(nil, ...)` -- which includes every available static scope (and its dynamic blocks) so the main-LLM path gets full cached context for a single classify-and-answer inference. Collapsing "no decision" into a nil/empty `required` slice on `Select` itself would make "a decision that needs nothing" indistinguishable from "no decision happened", so the two are separate call shapes.

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

`ai/cloud.Client` MUST implement `ai.LLMProvider` (Name `"cloud"`) by `POST {BaseURL}ai/chat` with `Accept: text/event-stream`, a bearer token from `Config.Token`, and the `X-AI-Product` header set from `Config.Product`, parsing the response with `cloudproto.ReadEvents` and relaying its events (already fatal-error-contract-conformant) unchanged; `Client.Decider()` returns a SEPARATE `decision.Provider` value (Name `"cloud-decision"`) by `POST {BaseURL}ai/decision`, treating `decided=false` as abstention and treating the response as a CLAIM, not a verdict: the calibrated flag, scores and interaction scores are kept, the `outcome` in the body is ignored entirely (a server refusal is advisory input; the local policy judges) and a response can never make a decision actionable or deterministic (REQ: decision-provenance); and `Usage(ctx)` by `GET {BaseURL}ai/usage`. `Config.BaseURL` MUST be normalised to always end with `/` and carries no default of its own -- the product supplies it (`ai/aiconfig.Deps.CloudBaseURL` or `Config.Cloud.BaseURL`). A non-2xx response at any of the three endpoints MUST be decoded as `cloudproto.ErrorResponse` into an `*ai.Error` (carrying the `Retry-After` it asked for, in both header forms, capped at 10 minutes, as `RetryAfterMs`), and all three retry 429/5xx before the first byte like the BYOK adapters. On the decision routes (`ai/decision`, `ai/score`) the error MUST also match the engine-neutral sentinel it stands for, so the circuit breaker and the combinators classify the cloud engine exactly as they classify a TypeSafe one: 401/403 or code `auth` (and a failing token source) `decision.ErrAuth`; 400/422 or code `invalid` `decision.ErrInvalidRequest`; 429 with code `quota` `decision.ErrQuota` (allowance exhausted: not transient, never retried, breaker-neutral); 404/405 (other than ai/score's "no such route") `decision.ErrMisconfigured`; any other 429 and every 5xx a transient engine fault that counts against the breaker. A response that carries a `Retry-After` MUST NOT be retried by the client itself (a breaker keeps the engine out of service at least that long instead). Every request MUST carry the `X-AI-Protocol` header.

#### REQ: cloud-score-route

The cloud boundary MUST offer a scoring route so a client can reach table narrowing through a hosted decision endpoint: `POST {base}ai/score` with the bearer token, the `X-AI-Product` header and the `X-AI-Protocol` header (the protocol version the client speaks, `cloudproto.ProtocolVersion`, in addition to the version prefix of `{base}`), body `cloudproto.ScoreRequest` (the `decision.ScoreRequest` fields `product`, `text`, `context`, `questions[{id, kind, instructions, candidates[{id, description}], noneId}]`, plus `interactionId` and `clientContext` as on a decision request), and a 2xx JSON body `cloudproto.ScoreResponse`: `answers` (an object keyed by question id, each a `decision.Answer` with `scores[{id, probability}]`, optional `confidence` with `hasConfidence`, `calibrated`, `noneId`), `engine` (the answering leaf engine), `model` (its model id), `strategy` (`single`, `fallback`, `hedged`, `race` or empty), `calibrated` (true only when the answering engine is calibrated), `attempts` (each engine tried, as `decision.Attempt`: `provider`, `outcome`, `role`, `latencyMs` in integer milliseconds, and the attempt's own `usage` when its engine reported it, so hedged and fallen-back calls are metered per engine), optional `usage` (the answering engine's) and `protocol` (the version the server answered in, informational). The route is additive within the API version: unknown JSON fields are ignored on both sides and old clients never call it. A server that predates it SHOULD answer 501; a 404 or 405 is accepted. A client MUST treat as `decision.ErrUnsupported` (not retried, neutral for a circuit breaker, so a combinator moves to its next engine) ONLY an unambiguous "route not implemented": a 501, or a 404/405 whose body is not a protocol `ErrorResponse` AND whose base URL answers `GET ai/usage` in the protocol (a 2xx JSON object of the shape of `cloudproto.UsageResponse`: a non-empty `product` string and, when present, an `allowance` that is an object or null; any other JSON object, such as a catch-all 200 from a web host, does not count; or any status with an `ErrorResponse` body). It MUST remember that finding for `Config.CapabilityTTL` (default 5 minutes; a negative value means until `Client.ResetCapabilities`), making no request to ai/score meanwhile, so one stray 404 during a rolling deploy does not disable scoring until restart. Concurrent first calls MUST share ONE probe of `GET ai/usage`, and a late call is answered by the recorded verdict, never a second probe; a follower whose leader was cancelled by the leader's own context MUST probe again rather than inherit that cancellation. Every other 404/405 (an `ErrorResponse` body such as an unknown product, or a base URL that does not speak the protocol) MUST be an error matching `decision.ErrMisconfigured` (outcome `misconfigured`), which a circuit breaker ignores and a `Fallback` does not hide behind its backup, and which the client remembers only briefly (`Config.MisconfiguredTTL`, default 30 seconds, negative: not at all) so a wrong base URL costs one probe per TTL instead of two round trips per call and stays loud (calls in the window fail at once with the same error); an inconclusive probe (a transport failure, a 429 or a 5xx without a protocol body) is an engine fault and remembers nothing. A server MUST therefore use 501 or 404/405 on this route for nothing but "no such route" and MUST put an `ErrorResponse` body on every application error. `cloud.Client.Decider()` MUST return a value that is also a `decision.ScoredProvider` and `decision.TracedScorer` backed by this route: the client refuses an invalid request locally, retries 429 and 5xx before the first byte like every other call, validates the answers against the request, sorts them, reports an answer `Calibrated` only when both the answer and the response say so, and puts the server's engine, strategy, model and attempts in the `Report`. Servers live in other repositories; this requirement is their contract.

#### REQ: cloud-decider-is-a-separate-value

`*Client` cannot itself satisfy both `ai.LLMProvider` (Name `"cloud"`) and `decision.Provider` (Name `"cloud-decision"`) through one `Name()` method -- Go dispatches exactly one `Name()` per concrete type regardless of which interface a caller holds it through. `Client.Decider()` resolves this literally: it returns a small unexported `decision.Provider` wrapping the same `*Client`, with its own `Name()` returning `"cloud-decision"` and a `DecisionTimeout() time.Duration` of 4s. `ai/aiconfig.Build` wires `c.Decider()` into the decision chain, never `c` itself.

### Config and BYOK

#### REQ: config-independent-llm-and-decision

`aiconfig.Config` MUST let `LLM.Provider` (`cloud` | `byok`) and `Decision.Provider` (`auto` | `cloud` | `disabled`; any other value is a `Build` error) vary independently: a BYOK chat LLM combined with a cloud-hosted decision provider MUST work, and a cloud chat LLM combined with `Decision.Provider: disabled` MUST work. `Decision.Provider: auto` with no cloud token source SILENTLY skips cloud decision (best-effort); `Decision.Provider: cloud` with no cloud token source or no base URL is a `Build` ERROR (an explicit request that can't be honoured is a misconfiguration, not something to swallow). `aiconfig.Build(cfg, deps)` MUST run the product's `deps.ExtraDecision` providers before `c.Decider()` in the returned chain, wiring the decision provider role, never the LLM `*cloud.Client` itself (see REQ: cloud-decider-is-a-separate-value). `Build` applies `deps.Getenv`/`deps.EnvPrefix` env overrides on top of `cfg` itself (via `Config.ApplyEnv`), so a product only needs to `Load` once.

#### REQ: byok-direct-connection

A BYOK adapter (`ai/openaicompat` or `ai/anthropic`, selected by `BYOK.Protocol`, default `openai-compatible`) MUST connect directly to `BYOK.Endpoint` using the API key read from the environment variable named by `BYOK.APIKeyEnv`. It MUST NEVER route through `Cloud.BaseURL` or any cloud boundary, which this package has no default value for at all -- the product supplies it via `Deps.CloudBaseURL` (or `Config.Cloud.BaseURL` to override). An empty `BYOK.Endpoint` MUST default to `https://api.openai.com/v1/` (`openai-compatible`) or `https://api.anthropic.com` -- the BARE host, no `/v1` -- (`anthropic`) per `BYOK.Protocol`; `ai/anthropic` appends its own fixed `/v1/messages`, so a default (or a caller-supplied `BYOK.Endpoint`) that already ends in `/v1` or `/v1/` MUST be tolerated by stripping it before appending, rather than doubling into `.../v1/v1/messages`. `BYOK.APIKeyEnv` left unset is valid (some endpoints need no key); naming a variable that `deps.Getenv` resolves to `""` MUST be a `Build` error, since that is almost always a forgotten export rather than an intentional no-auth setup. `aiconfig.Config.String()` MUST print the env var NAME for diagnostics and MUST NEVER print a key value (which `Config` never stores in the first place).

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

### AC: openairesponses-event-assembly-and-terminal-mapping
**Requirements:** ai-layer#req:openairesponses-adapter

**Given** a fixture Responses API SSE body streaming `response.output_text.delta` text, a `response.output_item.added`/`response.function_call_arguments.delta`/`.done`/`response.output_item.done` sequence for one function-call item, then `response.completed` with usage
**When** `ai/openairesponses.Provider.Stream` is ranged to completion
**Then** the text deltas concatenate correctly, exactly one `EventToolCall` is yielded with the `output_item.done` item's `call_id`/`name`/`arguments` (not merely the accumulated delta buffer), and the terminal `EventCompleted` carries `StopReason: "tool_calls"` and the mapped usage (`CacheReadTokens` from `input_tokens_details.cached_tokens`, `ReasoningTokens` from `output_tokens_details.reasoning_tokens`); separately, a fixture ending in `response.incomplete` with `incomplete_details.reason: "max_output_tokens"` (no open tool call) maps to `StopReason: "length"`, the same fixture with `incomplete_details.reason: "content_filter"` maps to `StopReason: "content_filter"`, and the `max_output_tokens` fixture with `ChatRequest.ResponseSchema` set is instead FATAL

### AC: openairesponses-incomplete-with-open-tool-call-is-always-fatal
**Requirements:** ai-layer#req:openairesponses-adapter

**Given** two fixtures: one where a function-call item registered via `response.output_item.added` never reaches `output_item.done` before `response.incomplete` arrives, and one where it DOES reach `output_item.done` (fully assembled) before `response.incomplete` arrives
**When** each is streamed to completion
**Then** both are FATAL (`ai.ErrCodeUpstream`) and neither yields an `EventToolCall` -- reaching `output_item.done` does not make a tool call safe to emit once the response itself is known to be truncated

### AC: openairesponses-empty-content-and-output-still-sent
**Requirements:** ai-layer#req:openairesponses-adapter

**Given** a `ChatRequest` with a user message whose `Text` is `""`, and separately a `RoleTool` message whose one `ToolResult.Content` is `""`
**When** `ai/openairesponses` builds the outbound request body
**Then** the message item's JSON object has a `"content"` key present with value `""` (not omitted), and the function_call_output item's JSON object has an `"output"` key present with value `""` (not omitted)

### AC: openairesponses-reasoning-item-replayed-across-agent-loop-steps
**Requirements:** ai-layer#req:openairesponses-adapter

**Given** an `ai/agent.Loop` over the REAL `ai/openairesponses.Provider` against an httptest server that streams a `reasoning` output item (with `encrypted_content`) followed by a `function_call` item on step 1, then a plain text completion on step 2
**When** the Loop's registered `Handler` answers the tool call and the Loop issues its second request
**Then** the second request's `input` array contains the replayed reasoning item, with its `encrypted_content` byte-identical to what step 1 streamed, positioned BEFORE both the `function_call` item and its `function_call_output`; separately, `buildInput` given an EARLIER loop's assistant turn (before the last `ai.RoleUser` message) with a `ProviderState` set still rebuilds it from `Text`/`ToolCalls` without replaying that earlier reasoning item, and an unparsable or empty-array `ProviderState` on the current turn falls back to the same legacy reconstruction

### AC: openairesponses-replayed-items-never-carry-their-own-id-or-status
**Requirements:** ai-layer#req:openairesponses-adapter

**Given** an `ai/agent.Loop` over the REAL `ai/openairesponses.Provider` against an httptest server whose step-1 fixture uses REALISTIC OpenAI-style ids (`rs_`/`fc_` prefixes, not test-fixture-shaped toy ids) for its reasoning and function_call output items
**When** the Loop issues its second request
**Then** NONE of the replayed items in that request's raw `input` JSON carry an `"id"` or `"status"` key at all (NB1 -- replaying an item's own `id` with `store:false` 400s: "Item with id '...' not found. Items are not persisted when store is set to false"); separately, a `function_call` item whose provider-sent `call_id` was empty has the SAME synthesized `ai.ToolCall.ID` this adapter surfaced written into its replayed `call_id`, so the replayed `function_call` and the `function_call_output` `ai/agent` builds from that `ai.ToolCall.ID` still reference each other

### AC: openairesponses-encrypted-content-rejected-retries-without-include-or-reasoning-item
**Requirements:** ai-layer#req:openairesponses-adapter

**Given** an `ai/openairesponses.Provider`, a `ChatRequest` whose current-loop assistant message carries a `ProviderState` replaying a reasoning item, and a server that 400s the first request naming encrypted content ("Encrypted content is not supported with this model") whenever `include` is set
**When** the `Stream` call is made, and separately a second `Stream` call is made on the SAME `Provider` for the SAME model
**Then** the retry succeeds with `include` OMITTED and the reasoning item DROPPED from `input` entirely (not merely `include` omitted); the second `Stream` call never sends `include` or replays a reasoning item at all (remembered per model), making only one request

### AC: openairesponses-reasoning-rejected-retries-once-remembered-per-model
**Requirements:** ai-layer#req:openairesponses-adapter

**Given** an `ai/openairesponses.Provider`, and a 400 whose message names the `reasoning` PARAMETER (e.g. "unsupported parameter: 'reasoning'") on the first request of a `ChatRequest.Reasoning`-set call, and separately a 400 whose message names an unrelated field, and separately a 400 whose message names an ITEM-related "reasoning" error (e.g. "missing required 'reasoning' item")
**When** each `Stream` call is made, and separately a second `Stream` call is made on the SAME `Provider` for the SAME model after the first succeeded via the fallback
**Then** the parameter-naming 400 triggers exactly one retry without `reasoning` and succeeds; neither the unrelated-field 400 nor the item-related "reasoning" 400 retries -- both surface as a fatal error, and neither disables `reasoning` for later calls on that model/Provider (NM1); the second `Stream` call after the parameter-naming success never sends `reasoning` at all (remembered per model), making only one request

### AC: openairesponses-stream-error-code-classification
**Requirements:** ai-layer#req:openairesponses-adapter

**Given** a `response.failed` event and separately a top-level `error` event, each with `code` set to `rate_limit_exceeded`, `insufficient_quota`, `server_error`, and an unrecognised value in turn, and one with no `message` at all
**When** each is streamed
**Then** the resulting `*ai.Error` has `Code` `rate_limited` (retryable), `quota`, `upstream` (retryable), and `upstream` respectively, and the no-`message` case still carries a non-empty fallback message

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

### AC: policy-worked-examples
**Requirements:** ai-layer#req:selection-policy

**Given** the narrowing policy and independent relevance probabilities loans 0.96, loan_items 0.91, members 0.72, and separately a choice 0.38/0.35/0.33 and a relevance list 0.38/0.35/0.33
**When** each is evaluated
**Then** the first is `several` with the strong subset loans and loan_items, and both of the others are `uncertain`, never "A wins"

### AC: policy-uncalibrated-is-unscored
**Requirements:** ai-layer#req:selection-policy

**Given** a choice answer 0.99/0.01 that is not calibrated, and a relevance answer 0.95/0.7/0.2 that is not calibrated
**When** the narrowing policy evaluates them
**Then** both are `unscored` with `Proposals` (the top candidate; the candidates at or above 0.60), no `Picks`, nothing strong and not actionable, the same relevance numbers calibrated are an actionable `several` with `Picks` and no `Proposals`, the zero-value policy and a policy with a zero threshold are invalid and select nothing (`invalid_policy`), and a calibrated one-candidate choice with no confidence at probability 0.1 is `uncertain`

### AC: policy-chain-falls-through-on-uncertain
**Requirements:** ai-layer#req:chain-policy, ai-layer#req:decision-additive-scores

**Given** a `Chain` with the narrowing policy and a provider returning a calibrated decision whose scores are 0.38/0.35/0.33, followed by a second provider that selects
**When** `Chain.Decide` runs
**Then** the first attempt is recorded `uncertain` with detail `uncertain: low_confidence`, the chain falls through and the second provider decides; with no second provider the chain returns `ok=false`; with `KeepNonSelected` it returns `ok=true` with `Decision.Outcome == uncertain` and `Actionable()` false; the same chain with a nil `Policy` rejects the first as `low_confidence`

### AC: chain-uncalibrated-needs-an-explicit-opt-in
**Requirements:** ai-layer#req:chain-policy, ai-layer#req:selection-policy, ai-layer#req:decision-additive-scores

**Given** a `Chain` and a provider returning an uncalibrated decision (an LLM emulator's self-reported confidence), and separately a calibrated decision without `Scores`
**When** the chain runs under the narrowing policy (`AcceptUncalibratedAt` 0.70) with confidences 0.95, 0.72 and 0.60, under the durable policy (`AcceptUncalibratedAt` 0) with 0.72 and 0.99, and under a durable policy that opts in at 0.80 with 0.85 and 0.75
**Then** narrowing accepts 0.95 and 0.72 as `accepted` (actionable, `Calibrated` false, attempt `decided`, detail `accepted: accepted_uncalibrated`) and records 0.60 as `uncertain` with the verdict `unscored: low_confidence`; durable never accepts, records `uncertain` with `unscored: not_calibrated` and falls through (so a durable chain whose calibrated engine is down does not act on the LLM backup's 0.72), and with `KeepNonSelected` returns it `unscored` and not actionable; the calibrated decision without scores is `unscored: no_scores`; the opt-in policy accepts 0.85 and rejects 0.75; and for every case `Decision.Actionable()` equals `Selection.Actionable()` of the same policy

### AC: rules-are-deterministic-under-any-policy
**Requirements:** ai-layer#req:decision-provenance, ai-layer#req:selection-policy, ai-layer#req:chain-policy, ai-layer#req:rules-provider

**Given** a `Chain` of a rules provider followed by an LLM-backed provider, with `DurablePolicy`, the narrowing policy, or `DurablePolicy` with `AcceptUncalibratedAt` 1.0, and a rule that matches
**When** the chain runs
**Then** the answer is the rule's, with `Outcome == deterministic`, `Actionable()` true, `Trace.Provenance == deterministic`, attempt detail `deterministic: deterministic_rule`, and the LLM-backed provider is never called; a module-less "yes" rule is accepted the same way; a decision that merely claims `Outcome: deterministic`, or `Calibrated` with a one-option `Scores`, is not accepted under durable; an LLM self-reporting 1.0 is `accepted` only at an explicit bar and its provenance stays `self_reported`; a decision decoded from JSON that says `deterministic` is `self_reported` and not actionable; a cloud response claiming `deterministic` or any `outcome` (a refusal included) comes back `self_reported` with an empty `Outcome`, and a server `uncertain` over calibrated scores is judged by the local policy; and `aiconfig` with the rules in `ExtraDecision` and `policy: durable` accepts a rule match without calling the configured engine

### AC: nothing-is-actionable-by-omission
**Requirements:** ai-layer#req:decision-additive-scores, ai-layer#req:chain-policy

**Given** the zero `Decision`, an LLM decision at confidence 0.05 returned by a provider or by a policy-less `compose.Single`, `Fallback`, `Breaker` or `Budget`, a `typesafe.Client.Decide` answer at intent confidence 0.05, decisions unmarshalled from JSON whose `outcome` is `deterministic`, `selected`, `several`, `accepted` or `floor`, and a custom provider that writes one of those outcomes into an answer at confidence 0.2
**When** `Actionable()` is read, and the provider runs inside a `Chain` with and without a policy
**Then** it is false for all of them, with provenance `self_reported` for the JSON ones; the provider's claimed outcome is never believed (a 0.2 answer is rejected `low_confidence`, a 0.9 one is stamped `floor` whatever it claimed); a policy-less `Chain` that accepts a 0.9 answer stamps `Outcome == floor` (actionable) and honours an engine's non-actionable (`uncertain`, `none`, `unscored`) verdict; an inner engine's policy verdict survives `Single`, `Breaker` and `Budget` wrappers; the verdict is the unexported judged state, `Outcome` stays on the wire (`"outcome":"floor"`) and no other field leaks into the JSON; and `Outcome.Actionable()` and `Selection.Actionable()` are true for `selected`, `several`, `accepted`, `deterministic` and `floor` and false for the empty outcome, `uncertain`, `none`, `unscored`, `invalid` and an unknown string

### AC: replayed-decisions-are-rejudged
**Requirements:** ai-layer#req:decision-additive-scores, ai-layer#req:decision-provenance

**Given** a rule decision (`decision.Deterministic`) marshalled to JSON and read back, and a stored decision at confidence 0.1 whose `outcome` says `selected`, and one that fails validation
**When** each is read, and judged with `Chain.Rejudge` under a policy-less chain, under `DurablePolicy` and under a chain with `KeepNonSelected`
**Then** the replayed rule decision is not actionable and `self_reported`; `Rejudge` of it under the policy-less chain is `floor` and `self_reported` (never deterministic again, not even from the in-process value), under `DurablePolicy` it is `unscored` and not actionable; the 0.1 decision is refused whatever its claimed outcome; the invalid one is returned `invalid`; `KeepNonSelected` never makes a refusal actionable

### AC: side-effectful-interactions-need-their-own-opt-in
**Requirements:** ai-layer#req:selection-policy, ai-layer#req:chain-policy, ai-layer#req:llmdecider-single-inference, ai-layer#req:decision-additive-scores

**Given** an uncalibrated module-less `confirmation` (and `rejection`, `correction`, `cancellation`, `undo`) at interaction confidence 0, a chain of an abstaining calibrated engine followed by it under the narrowing policy, an LLM decider whose output carries no `interactionConfidence`, a cloud answer `{"interaction":"undo","calibrated":true}` with no module, scores or confidence, and calibrated module-ful decisions whose `InteractionScores` back, contradict or do not back the interaction, with narrow gaps and low or zero interaction confidence
**When** each is evaluated under the narrowing policy, under a policy with `AcceptUncalibratedSideEffects`, under `DurablePolicy`, by a policy-less chain and by a chain with `MinConfidence` -1
**Then** the narrowing policy refuses the uncalibrated one (`unscored: side_effect_uncalibrated`) at any module or intent confidence; the opt-in policy refuses it at an interaction confidence of 0 or below 0.90 (`interaction_low_confidence`) and accepts it at 0.95; `chat` is unaffected; the policy-less chain, with or without a floor, rejects every side-effectful interaction below 0.90 or at 0 as `low_confidence` with detail `interaction=<value> (interaction_low_confidence)`, and accepts one at 0.90; the cloud answer is refused under every chain and policy, with or without `KeepNonSelected`; a calibrated decision backed by `InteractionScores` at 0.95 is `selected`, one whose scores contradict or omit its interaction is `invalid` (`interaction_not_top`), a narrow gap is `uncertain` (`interaction_narrow_gap`), a low or zero confidence `uncertain` (`interaction_low_confidence`), and one without `InteractionScores` is judged as a self-report; a deterministic "yes" is unaffected; and the LLM decider reports its `interactionConfidence`, 0 when absent

### AC: decision-is-judged-on-its-own-intent
**Requirements:** ai-layer#req:selection-policy

**Given** a calibrated decision with intent `i` whose `Scores` are `m/j` 0.9 and `m/i` 0.1, one whose own option is missing from `Scores`, one with a NaN score, a tie between its own option and another, and decisions keyed by a bare intent or a bare module
**When** the durable policy evaluates them and a `Chain` (with and without `KeepNonSelected`) and a `compose.WithPolicy` engine judge the first
**Then** the first is `invalid` with reason `decision_not_top` (the missing one `decision_not_scored`, the NaN one `bad_scores`), never `selected`; the chain records an `invalid` attempt with detail `invalid: decision_not_top` and falls through even with `KeepNonSelected`; the engine records `invalid`; the tie is `uncertain: narrow_gap` whichever id sorts first; and the bare-key decisions are judged on their own option

### AC: chain-stops-at-quota-and-misconfiguration
**Requirements:** ai-layer#req:chain-stops-on-quota, ai-layer#req:engine-combinators, ai-layer#req:config-selects-engines

**Given** a `Chain` of a provider that refuses with `decision.ErrQuota`, `ErrBudget` or `ErrMisconfigured` (also joined with an unavailable, auth or timeout error) followed by a paid provider, a `Hedged` engine whose primary refuses after the hedge fired while its backup runs, and a `Race` of a refusing engine and a slow one
**When** each runs with the defaults, with `decision.FallThrough` for the matching switch, with `compose.OnQuota`, and through `aiconfig` (`Providers.Chain()` with `stopOnQuota`/`stopOnMisconfigured`, also from `AI_DECISION_STOP_ON_QUOTA`)
**Then** the default chain stops without calling the paid provider, returns `ok=false`, `Trace.StoppedBy == quota` (or `budget`) and `Trace.Err()` matching `ErrQuota` (or `ErrBudget`) and stating the condition once; the loud condition wins over a joined quiet one; each switch is independent and `FallThrough` lets the next provider answer; `StopPolicy` marshals as `stop`/`fall_through` and refuses other text; the hedged call ends with the error, the backup `cancelled` and never accepted; the race ends with the error and cancels the slow engine (also on a scored call, and over an abstention or uncertain answer another racer gave); `OnQuota` lets the backup or the other racers answer (a misconfigured endpoint halts regardless); a hedge backup's own refusal does not end the call; `aiconfig` mirrors the defaults and the opt-outs and refuses `fallbackOn: [quota]` with an explicit `stopOnQuota: true`, a malformed environment boolean, and a deterministic provider listed after another engine

### AC: backup-budget-bounds-the-bill
**Requirements:** ai-layer#req:engine-budget, ai-layer#req:chain-stops-on-quota, ai-layer#req:config-selects-engines

**Given** `Fallback(Breaker(primary), Breaker(Budget(paid, 7 per hour)))` inside a `Chain`, a primary that fails with a transient error on every call (a TypeSafe 429), 50 turns, and the same through `aiconfig` with `backupBudget: {maxCalls: 6, per: 1h}` (strategies `fallback` and `hedged`) and from the environment
**When** the 50 turns run, and the clock passes the window
**Then** without a budget all 50 turns call the paid engine (the documented hole); with it exactly 7 (6) paid calls decide, every later turn stops with `Trace.StoppedBy == budget` and `Trace.Err()` matching `ErrBudget`, the breaker stays closed (a spent budget is not a fault) and does not count calls it blocked, the next window admits calls again, scored and decided calls share the cap, a spent primary does not start its backup without `OnQuota`, a spent backup that an abstention started (`OnAbstain`) is an error and not an abstention, `MaxCalls` below 1 or a negative `Per` is `ErrMisconfigured` on every call, and every invalid `backupBudget` (no engines, strategy `single` or `race`, `maxCalls` below 1, a bad or negative `per`) is a `Build` error

### AC: typesafe-abstention-says-why
**Requirements:** ai-layer#req:typesafe-decide-mapping, ai-layer#req:decision-trace-engine, ai-layer#req:circuit-breaker

**Given** a `typesafe.Client` whose answers make it abstain because the intent is `other`, the interaction confidence is low, the interaction gap is narrow, a side-effectful interaction is below the durable bar, or the policy is invalid, run directly, inside a `Chain`, and behind a `compose.Breaker` inside an engine
**When** `DecideTraced` runs or the chain decides
**Then** the attempt is `abstained` with `Detail` `intent_other`, `interaction_low_confidence`, `interaction_gap`, `interaction_below_durable` or `interaction_invalid_policy`, carries what the call billed in `Usage`, and holds no caller text; a decided answer or an error leaves the report without attempts; the breaker and the engine pass the attempt on to `Trace.Attempts`; and a 429 whatever its body says is `ErrRateLimited`, never `decision.ErrQuota`

### AC: cloud-capability-probe-is-hardened
**Requirements:** ai-layer#req:cloud-score-route

**Given** a server whose ai/score answers 404 with a non-protocol body and whose `GET ai/usage` answers a `UsageResponse`, an unrelated JSON object, an empty `product`, a non-object `allowance` or HTML; a clock the test moves; eight concurrent first callers; and a leader whose context is cancelled while followers wait
**When** scores are asked
**Then** only a `UsageResponse`-shaped body counts as speaking the protocol (the others are `ErrMisconfigured`); the unsupported verdict is remembered for `CapabilityTTL` (default 5 minutes, a 501 included, a negative value until `ResetCapabilities`) and then asked again, so a deployed route works; a wrong base URL is `ErrMisconfigured`, remembered for `MisconfiguredTTL` (default 30 seconds) with no round trips in the window and loud throughout; eight concurrent callers make one `GET ai/usage`; a late call is answered by the recorded verdict; a follower survives a cancelled leader and an impatient follower returns `canceled`

### AC: scored-request-and-result-validation
**Requirements:** ai-layer#req:scored-candidates

**Given** a `ScoreRequest` with a duplicate candidate id, a duplicate question id, an unknown kind, an empty id and a `NoneID` that is no candidate (all using distinctive ids), and a `ScoreResult` missing the answer to one question
**When** `ValidateScoreRequest` and `ValidateScoreResult` run
**Then** both return errors, the request error matches `decision.ErrInvalidRequest` and names problems by question and candidate position, never by id, and a well-formed pair passes

### AC: fallback-only-on-failure
**Requirements:** ai-layer#req:engine-combinators

**Given** `Fallback(jev, llm)` where `jev` and `llm` would answer differently
**When** `jev` answers, and separately when `jev` errors, times out, reports an open breaker, is rejected or unauthorised, or returns an invalid answer, and when it refuses for an exhausted allowance or a misconfigured endpoint
**Then** the first call never calls `llm` and uses `jev`'s answer; each failing case uses `llm`'s answer with `FallbackFired` true and the attempts `jev:<outcome>, llm:decided` (`rejected` and `auth` for the caller faults); an abstention or an uncertain answer from `jev` does not start `llm` unless `WithFallbackOn` opts in; a `quota` refusal does not start `llm` (the error matching `decision.ErrQuota` is returned) unless `WithFallbackOn(OnQuota)` opts in; and a `misconfigured` refusal never starts `llm`, with any option

### AC: engine-answers-carry-their-verdict
**Requirements:** ai-layer#req:engine-combinators, ai-layer#req:decision-trace-engine

**Given** engines built with `WithPolicy` over a calibrated clear answer, a calibrated uncertain answer (0.38/0.35/0.33), an uncalibrated 0.72 decision under the narrowing and the durable policy, and a policy-less engine; and scored attempts, some answered with usage, from a `Fallback` whose primary failed, from an uncertain primary followed by an `OnUncertain` backup, and from a nested engine
**When** each is called directly, and the decision engines run inside a durable `Chain`
**Then** every decision from a policy engine carries a stamped verdict (`Outcome`) (`selected`, `uncertain`, `accepted` at 0.72 under narrowing, `unscored` under durable) and `Actionable()` is true only for the first and third; a non-actionable one is recorded `uncertain` and returned (an uncertain answer is an answer) rather than acted on; a durable chain around a `Fallback(jev down, llm 0.72)` returns `ok=false` with `llm` recorded `uncertain` (`unscored: not_calibrated`); a policy-less engine returns the decision unjudged (empty `Outcome`, not actionable) and a `Chain` without a policy still honours an engine's stamped non-actionable (`uncertain`, `none`, `unscored`) verdict; each answered score attempt carries its own `Usage` (nil when none was reported, never zero) and a failed or cancelled one carries nil

### AC: hedged-starts-backup-after-budget
**Requirements:** ai-layer#req:engine-combinators

**Given** `Hedged(jev, llm, 600ms)` over a fake clock, with `jev` blocked
**When** the clock advances 600ms and `llm` answers
**Then** `llm`'s answer wins with `HedgeFired` true, `jev` is recorded `cancelled` with 600ms latency and exits, and when `jev` fails outright the backup starts at once with `FallbackFired` true and `HedgeFired` false

### AC: race-first-valid-wins-and-cancels-losers
**Requirements:** ai-layer#req:engine-combinators

**Given** `Race` over three engines, one of which returns an invalid answer and two of which wait on their context
**When** the third answers validly
**Then** that answer wins, the others are `cancelled`, the call returns without waiting for them, and no goroutine outlives the test

### AC: combinator-enforces-engine-timeout
**Requirements:** ai-layer#req:engine-combinators

**Given** an engine that ignores its context and a fake clock
**When** the clock advances past the engine's `DecisionTimeout()`
**Then** the combinator returns with outcome `timeout` and an error matching `context.DeadlineExceeded`

### AC: combinators-nest-and-trace
**Requirements:** ai-layer#req:decision-trace-engine, ai-layer#req:engine-combinators

**Given** a `Chain` holding `Fallback(Hedged(jev, llm), rules)` where `jev` errors
**When** `Chain.Decide` runs
**Then** `Trace.Attempts` lists `jev:error, llm:decided`, `Trace.Engine` is `llm`, `Trace.DecidedBy` is the outer combinator's name, and a winner the chain rejects is relabelled `low_confidence`

### AC: breaker-opens-probes-and-recovers
**Requirements:** ai-layer#req:circuit-breaker

**Given** a `Breaker` over an engine that fails, with a fake clock
**When** 5 consecutive calls fail, then a call is made, then 30 seconds pass and a call is made while another probe is in flight, then the probe succeeds
**Then** the sixth call returns `ErrUnavailable` without calling the engine; after the cooldown exactly one probe is let through and the concurrent caller gets `ErrUnavailable`; success closes the breaker; a failed probe reopens it and a cancelled one does not count as a failure; and failures more than 30 seconds apart do not accumulate

### AC: breaker-counts-only-engine-health
**Requirements:** ai-layer#req:circuit-breaker

**Given** a `Breaker` with threshold 1 over an engine that returns, in turn, an error matching `decision.ErrInvalidRequest`, one matching `decision.ErrAuth`, one matching `decision.ErrQuota`, one matching `decision.ErrMisconfigured`, and a 429 carrying `Retry-After: 90s`, and a probe that hangs ignoring its context
**When** the calls are made with a fake clock, and a late result from a call that started before the breaker opened arrives
**Then** the first four never open it; the fifth (with threshold 2 and two such failures) keeps it open past the 30 second cooldown until 90 seconds have passed; a late success or failure from a call that started before the opening neither closes the breaker nor extends its cooldown; and the hung probe is abandoned when `WithBreakerProbeTimeout` passes, counts as a failure and returns the breaker to open, after which a later probe is let through

### AC: hedged-hung-primary-opens-its-breaker
**Requirements:** ai-layer#req:circuit-breaker, ai-layer#req:engine-combinators

**Given** `Hedged(Breaker(jev), llm, 100ms)` over a fake clock where `jev` hangs until cancelled, once with the default breaker and once with `WithBreakerSlowThreshold(3)`, and a healthy primary that merely answers after the hedge delay
**When** twelve calls are made, each answered by `llm` after the hedge fires
**Then** each hedged-out `jev` attempt is recorded `cancelled` with detail "superseded by hedge" and counted as SLOW (`Breaker.Stats().Slow`), never as a failure; the default breaker stays closed after twelve (a healthy primary above the hedge delay is not taken out of service by its own hedge); with the slow threshold the breaker is open after the third and the fourth call records `jev:unavailable, llm:decided` with `FallbackFired`; a call that completes in time resets the slow run; a primary that outruns its OWN timeout is a plain timeout failure that opens the default breaker at its failure threshold; and the same hung engine as a `Race` loser is never counted at all

### AC: hedged-abstain-rule-is-independent-of-timing
**Requirements:** ai-layer#req:engine-combinators

**Given** `Hedged(jev, llm, 100ms)` over a fake clock where `jev` abstains (and, separately, answers uncertain under a policy) only after the hedge fired and `llm` is still running
**When** `jev` finishes
**Then** the abstention (or uncertain answer) stands, `llm` is recorded `cancelled`, and the result equals the one when `jev` finishes before the hedge fires; with `WithFallbackOn(OnAbstain)` (or `OnUncertain`) the same late outcome hands the call to `llm`, whose answer wins

### AC: attempt-wire-form-carries-milliseconds-and-usage
**Requirements:** ai-layer#req:decision-trace-engine, ai-layer#req:cloud-score-route

**Given** an `Attempt` with a latency of 1234ms and its own usage, and JSON with only the legacy `latency` (nanoseconds), with both fields, and with only `latencyMs`
**When** each is marshalled and unmarshalled, inside a `Report` and a `cloudproto.ScoreResponse`
**Then** the wire form has the integer `latencyMs` 1234 and (for older readers) `latency` in nanoseconds, `latencyMs` wins when both are present, the legacy field alone still reads, and `usage` round-trips per attempt and is absent when the engine reported none

### AC: engines-are-safe-to-build-empty
**Requirements:** ai-layer#req:engine-combinators, ai-layer#req:circuit-breaker

**Given** `Race(nil)`, `Single(nil)`, `Hedged(nil, x, d)`, a zero-value `Engine` and `NewBreaker(nil)`
**When** their names and `DecisionTimeout()` are read and each is called
**Then** nothing panics, `Race(nil).DecisionTimeout()` is 0, and every call, `Decide` and `Score` alike (the breaker's included), returns an error matching `compose.ErrNoEngine`

### AC: config-builds-engine-chain
**Requirements:** ai-layer#req:config-selects-engines

**Given** `decision: {engines: [jev, llm-decider], strategy: hedged, hedgeAfter: 600ms, policy: narrowing}` and a `Deps.Engines` registry
**When** `Build` runs
**Then** the chain is the product's rules, then one hedged engine over breaker-wrapped `jev` and `llm-decider`, then the cloud decider; `Providers.Policy` is the narrowing policy; `provider: disabled` drops the engine; and an unknown engine name, strategy, policy or a three-engine `fallback` is a `Build` error (REQ: engine-budget adds `backupBudget` to the engine layout)

### AC: config-policy-values
**Requirements:** ai-layer#req:config-selects-engines

**Given** `decision: {policy: narrowing, policyValues: {minProbability: 0.7, maxPicks: 3}}`, and separately `policyValues` without a `policy`, a probability above 1, a zero threshold and an unordered set
**When** `Build` runs
**Then** `Providers.Policy` is the narrowing policy with those two values replaced and named `narrowing+custom`, and each of the invalid cases is a `Build` error

### AC: typesafe-request-and-error-mapping
**Requirements:** ai-layer#req:typesafe-client

**Given** a fake `HTTPDoer` and a client with a test key
**When** a call is made, and then calls are answered 401, 400, 422, 429 with `Retry-After: 7`, 529, 500 and 404 with bodies that echo the state
**Then** the request is a `POST` to `/v1/systemone` with a bearer header and the `{state, model, questions}` body; each status maps to its sentinel with the request id, error type and retry delay; no error string contains the body or the key; exactly one HTTP call is made per `Ask`; and the call hook reports model, usage and status without content

### AC: typesafe-refuses-unsafe-requests-and-redirects
**Requirements:** ai-layer#req:typesafe-client

**Given** a client built with no model, with `http://api.example.com`, with credentials in the URL, and a default client calling a loopback server that answers 307 to a second server; and requests with 256 options, 256 questions and a state of about 40k tokens
**When** `New` and `Ask` run
**Then** the first three `New` calls fail without echoing the URL, the redirect is not followed (the second server sees nothing; the error is `ErrUnexpectedStatus`), and each oversized request fails with `ErrTooManyOptions`, `ErrTooManyQuestions` or `ErrStateTooLarge` (all matching `ErrInvalidRequest` and `decision.ErrInvalidRequest`) before any HTTP call, while a request just under the budget goes through and `MaxStateTokens` below 0 disables the check

### AC: errors-carry-no-caller-content
**Requirements:** ai-layer#req:typesafe-client, ai-layer#req:decision-trace-engine, ai-layer#req:engine-combinators

**Given** a response that answers with an option the request never had, a request with a duplicate candidate id, and a decision that fails validation, all using a distinctive id
**When** `Score`, `Decide`, a `compose` engine's `Score` and a `Chain` run
**Then** no returned error and no `Attempt.Detail` contains the id, the question id or the model's own words; they carry only counts and positions, and the request error from a combinator still matches `decision.ErrInvalidRequest`

### AC: typesafe-retry-after-and-model
**Requirements:** ai-layer#req:typesafe-client

**Given** 429 responses carrying `Retry-After` as `7`, as an HTTP date 30 seconds ahead, as a date in the past and as 864000, and a decide response from model `jev-1.13.0`
**When** `Ask` and `Decide` run
**Then** `decision.RetryDelay` reads 7s, 30s, 0 and 10 minutes, and the `Decision`, its `Trace` and the `ScoreResult` all carry `jev-1.13.0`

### AC: typesafe-score-maps-relevance-to-nouls
**Requirements:** ai-layer#req:typesafe-score-mapping

**Given** a `ScoreRequest` with one relevance question over 4 tables and one choice question
**When** `Score` runs against a recorded response
**Then** exactly one call carries 5 questions (4 Nouls, 1 Choice), the state holds the question text and the table catalogue, and the answers are `Calibrated` with the Noul probabilities per table and the Choice's confidence and `NoneID`; malformed or mismatched answers are `ErrBadResponse`

### AC: typesafe-decide-folds-into-a-validating-decision
**Requirements:** ai-layer#req:typesafe-decide-mapping

**Given** a taxonomy with two modules, scopes, a data kind, entity types and presentations, and a recorded response
**When** `Decide` runs, and again with `other` as the top intent
**Then** one call carries every question, the decision carries module, intent, scores, required scopes and data, reference and presentation, validates, is `Calibrated`, and the request body contains neither the interaction id, the client context, the time zone nor `now`; `other` abstains; a `choose:<role>` request sends exactly one Choice

### AC: typesafe-decide-always-asks-the-interaction
**Requirements:** ai-layer#req:typesafe-decide-mapping

**Given** a request with an empty state and no recent turns, and recorded responses whose interaction Choice has `chat` on top at confidence 0.9, `chat` on top at confidence 0.1 or with a 0.05 lead over the runner-up, `confirmation` on top at p=0.19 with confidence 0.04 beside a confident intent (0.97), a `confirmation`, `rejection`, `correction`, `cancellation` or `undo` on top at confidence 0.80 and at 0.97, and one whose top is outside the enum (by the `choice` field and, separately, by the probabilities)
**When** `Decide` runs, alone and inside a `Chain` with the durable policy
**Then** the interaction question is sent; `chat` at 0.9 is the `Interaction` with `InteractionConfidence` 0.9; every doubtful case, including the probed `confirmation`, makes the provider abstain (`ok=false`, no error, the chain's attempt is `abstained`) instead of carrying a guessed interaction; a side-effectful interaction at 0.80 abstains even under the narrowing policy and is used at 0.97, while a harmless one at 0.80 is used under narrowing and not under the durable policy; the out-of-enum answers are `ErrBadResponse` quoting nothing; and no interaction is assumed when the question is not answered

### AC: typesafe-live-test-skips-without-opt-in
**Requirements:** ai-layer#req:typesafe-live-test-opt-in

**Given** `JEV_API_KEY` is unset or `AICHAT_LIVE_JEV` is not `1`
**When** `go test ./ai/decision/typesafe` runs
**Then** the live tests are skipped and no network call is made

### AC: llmdecider-scores-in-one-inference
**Requirements:** ai-layer#req:llmdecider-scores

**Given** a fake LLM that answers a two-question `ScoreRequest` (one relevance, one choice) with one structured result
**When** `Score` runs, and again with unnormalised and out-of-range numbers, with an invented id, with a question left unanswered, and with a choice that has no probability anywhere
**Then** one inference carries both questions and the strict schema; the result validates with `Calibrated` and `HasConfidence` false; choices are normalised, numbers clamped, invented ids dropped and missing candidates scored 0; the unanswered and no-probability cases are `ErrBadScores` without any id in the text; the narrowing policy turns the answer into `unscored` with a proposal

### AC: scored-questions-have-an-llm-backup
**Requirements:** ai-layer#req:llmdecider-scores, ai-layer#req:engine-combinators, ai-layer#req:circuit-breaker

**Given** `Fallback`, `Hedged` and `Race` over `Breaker(typesafe)` and `Breaker(llmdecider)`, the TypeSafe client over a fake transport, and a scored table-narrowing request
**When** `Score` runs with Jev answering HTTP 500, with Jev hanging until its timeout (or hedge delay), and with Jev's breaker open
**Then** the LLM decider answers every time with the attempts `jev:error|timeout|cancelled|unavailable, <llm>:decided`, an open breaker never calls Jev, and the narrowing policy yields a proposal (relevance: ids at or above the floor; choice: the top id) that is not actionable

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

### AC: cloud-score-route-and-old-servers
**Requirements:** ai-layer#req:cloud-score-route, ai-layer#req:cloud-client-endpoints

**Given** a test server implementing `ai/score` (with an unsorted, partly uncalibrated response carrying per-attempt usage and `latencyMs`), servers that predate the route (ai/usage answering, ai/score answering 404 with a proxy HTML page or plain text, 405, or 501), a server where every route answers 404 HTML (a mistyped base URL), one answering a JSON `ErrorResponse` 404 (an unknown product), probes of `GET ai/usage` that answer a 401 `ErrorResponse`, an HTML 200, JSON `null` or a 502, one answering a JSON `ErrorResponse` with 502, and one answering malformed or invalid results
**When** the cloud decider's `Score` and `ScoreTraced` run, also inside a `Fallback` over a `Breaker`, five times in a row against each old server, and again after `ResetCapabilities`
**Then** the request carries the product, text, questions, interaction id, client context and the `X-AI-Protocol` header; the answers come back sorted, validated and `Calibrated` only when both flags say so, with the server's engine, model, strategy, attempts, per-attempt usage and latency in the `Report`; the 501 server gives `decision.ErrUnsupported` after one call and the 404/405 servers after one `ai/score` call and one `ai/usage` probe, the finding is remembered (no request on the next four calls) until `ResetCapabilities`, the `Fallback` moves to its backup and the breaker stays closed; the mistyped base URL and the JSON 404 are `decision.ErrMisconfigured` (never `ErrUnsupported`), recorded `misconfigured`, not hidden by the `Fallback`, not remembered, never opening the breaker, and quoting none of the body; a probe that answers a 401 `ErrorResponse` counts as the protocol speaking, an HTML 200 or JSON `null` does not, and a 502 is an inconclusive engine fault that remembers nothing; the 502 is an `*ai.Error` retried before the first byte; malformed or invalid responses are errors that quote none of the body or ids; an invalid request is refused locally with no call

### AC: cloud-errors-map-to-engine-outcomes
**Requirements:** ai-layer#req:cloud-client-endpoints, ai-layer#req:cloud-score-route, ai-layer#req:circuit-breaker

**Given** a server answering `ai/score` and `ai/decision` with 400, 422, 401, 403, a 429 with code `quota`, a 429 with code `rate_limited`, a 503 with `Retry-After` as `90`, as an HTTP date, as junk, as a past date and as 100000000, a body-only `retryAfterMs`, and a token source that fails
**When** the cloud decider runs behind `Breaker(threshold 1)` and inside `Fallback` with a paid backup, six times each
**Then** 400/422 match `decision.ErrInvalidRequest` (outcome `rejected`) and 401/403 and the failing token source match `decision.ErrAuth` (outcome `auth`), none opens the breaker and none is retried (one request per call); the quota refusal matches `decision.ErrQuota` (outcome `quota`), is not retried, does not open the breaker, is returned to the caller and does not start the paid backup unless `WithFallbackOn(OnQuota)` opts in; the plain 429 is retried before the first byte and opens the breaker; `*ai.Error` stays reachable through `errors.As` in every case; `Retry-After` is carried as `ai.Error.RetryAfterMs` (`decision.RetryDelay` reads 90 seconds, 0 for junk or a past date, and at most 10 minutes), the response is not retried by the client, and the breaker stays open past its 30 second cooldown until the delay has passed

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

### AC: tool-calls-assembled-from-multi-chunk-parallel-deltas
**Requirements:** ai-layer#req:tool-call-streaming-assembly

**Given** a fixture streaming two parallel tool calls, each with its arguments split across multiple delta chunks (by index)
**When** `ai/openaicompat.Provider.Stream` or `ai/anthropic.Provider.Stream` is ranged to completion
**Then** exactly two `EventToolCall` events are yielded, each carrying a complete `ai.ToolCall` with concatenated `Arguments`, before the terminal `EventCompleted` carrying `StopReason: "tool_calls"`

### AC: tool-calls-without-index-keyed-by-id-assemble-separately
**Requirements:** ai-layer#req:tool-call-streaming-assembly

**Given** a fixture streaming two parallel tool calls' `delta.tool_calls` chunks with NO `index` field at all, distinguished only by each call's first chunk carrying a distinct non-empty `id`
**When** `ai/openaicompat.Provider.Stream` is ranged to completion
**Then** exactly two `EventToolCall` events are yielded, each with the correct `Name` and concatenated `Arguments` for its own call -- they are never merged into one

### AC: tool-messages-round-trip-in-request-body
**Requirements:** ai-layer#req:tool-messages-on-the-wire

**Given** an `ai.ChatRequest` whose `Messages` include an assistant message with `ToolCalls` and a `RoleTool` message with `ToolResults`
**When** `ai/openaicompat` or `ai/anthropic` builds the outbound request body
**Then** the wire body carries the tool call(s) and result(s) in that adapter's native shape (openaicompat: `tool_calls` + one `role:"tool"` message per result; anthropic: `tool_use` + `tool_result` content blocks, with consecutive `RoleTool` source messages merged into one wire message)

### AC: reasoning-sets-thinking-budget-above-max-tokens
**Requirements:** ai-layer#req:reasoning-maps-to-provider-knob

**Given** `ChatRequest.Reasoning: "medium"` and `MaxTokens` left UNSET (the caller gave no explicit ceiling)
**When** `ai/anthropic` builds the request
**Then** `thinking` is `{type: "enabled", budget_tokens: 4096}` and the request's `MaxTokens` is raised strictly above 4096 (per N3, to `4096 + 4096 = 8192`); separately, `ai/openaicompat` sets `reasoning_effort: "medium"` and omits the field entirely when `Reasoning` is unset. (When the caller DOES set an explicit `MaxTokens`, see legacy-thinking-never-raises-caller-max-tokens-shrinks-budget-instead -- it is never raised, only the budget shrinks to fit.)

### AC: billable-tokens-sums-correctly-per-adapter
**Requirements:** ai-layer#req:usage-billable-tokens-per-adapter

**Given** an `ai.Usage{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 20, CacheWriteTokens: 5, ReasoningTokens: 10}`
**When** `BillableTokens("openai-compatible")`, `BillableTokens("openai-responses")`, `BillableTokens("anthropic")`, and `BillableTokens("some-unrecognised-provider")` are each called
**Then** `"openai-compatible"` and `"openai-responses"` both return 150 (`InputTokens + OutputTokens` only -- the subset fields are not added); `"anthropic"` and the unrecognised provider both return 175 (`InputTokens + OutputTokens + CacheReadTokens + CacheWriteTokens`); separately, with `CacheReadTokens`/`CacheWriteTokens` both zero, all provider names agree exactly

### AC: legacy-thinking-default-max-tokens-is-budget-plus-4096-capped-16000
**Requirements:** ai-layer#req:reasoning-maps-to-provider-knob

**Given** `ai/anthropic` targeting a legacy (Haiku 4.5 / pre-4.6 / Claude 3.x) model with `Reasoning` set and `MaxTokens` left unset
**When** the request is built for low/medium/high `Reasoning`
**Then** `MaxTokens` is the budget (1024/4096/16000) plus 4096, capped at 16000, EXCEPT that the cap never drops `MaxTokens` at or below the budget itself (so "high" gets `budget + 1024` = 17024, not the nominal 16000 cap); separately, adaptive thinking with `MaxTokens` left unset defaults `MaxTokens` to 16000

### AC: adaptive-model-defaults-max-tokens-even-with-reasoning-unset
**Requirements:** ai-layer#req:reasoning-maps-to-provider-knob

**Given** `ai/anthropic` targeting `claude-opus-5` (adaptive-thinking family) with `Reasoning` left `""` and `MaxTokens` left unset
**When** the request is built
**Then** `MaxTokens` is 16000 (not `defaultMax`'s 2048) and no explicit `thinking`/`output_config` is sent, since `Reasoning` was never requested -- only the `MaxTokens` default changes

### AC: reasoning-effort-retry-matches-field-name-only-remembered-per-model
**Requirements:** ai-layer#req:reasoning-maps-to-provider-knob

**Given** an `ai/openaicompat.Provider`, and separately a 400 whose message names `reasoning_effort`/`reasoningEffort` and one whose message names an unrelated field (e.g. "unsupported parameter: 'frequency_penalty'")
**When** each `Stream` call is made, and separately two `Stream` calls on the same `Provider` name different models
**Then** only the `reasoning_effort`-naming 400 triggers the one-time retry-without-it (the unrelated 400 does not, and is not retried); the "don't send it again" memory is keyed per model, so a rejection learned for one model does not suppress `reasoning_effort` on a different model

### AC: claude-3x-ids-use-legacy-thinking-not-the-unrecognised-id-default
**Requirements:** ai-layer#req:reasoning-maps-to-provider-knob

**Given** a Claude 3.x model id in the older "claude-3[-<minor>]-<family>" shape (`claude-3-7-sonnet`, `claude-3-5-haiku`, `claude-3-opus`, `claude-3-5-sonnet`, `claude-3-sonnet`, `claude-3-haiku`, with or without a dated snapshot suffix)
**When** `thinkingModeAdaptive` classifies it
**Then** every one of them reports `false` (legacy budget_tokens form) -- none fall through to the "unrecognised id" adaptive default, which only applies to ids that don't match EITHER the 4.6+ shape or the Claude 3.x shape

### AC: thinking-block-replayed-before-tool-use-with-signature-intact
**Requirements:** ai-layer#req:anthropic-thinking-block-replay

**Given** `ai/agent.Loop` over the REAL `ai/anthropic.Provider` against an httptest server that, with `Reasoning: "medium"` requested, streams a `thinking` block (with a `signature_delta`) followed by a `tool_use` call on step 1, then a plain text completion on step 2
**When** the Loop's registered `Handler` answers the tool call and the Loop issues its second request
**Then** the second request's assistant message content is `[thinking, tool_use, ...]` in that order, the `thinking` block's `signature` and `thinking` text are byte-identical to what step 1 streamed, and `thinking: {type: "enabled", budget_tokens: 4096}` is still set on that request

### AC: no-arg-tool-call-replays-non-empty-input
**Requirements:** ai-layer#req:anthropic-thinking-block-replay

**Given** `ai/agent.Loop` over the REAL `ai/anthropic.Provider` against an httptest server that streams a `tool_use` call with NO `input_json_delta` chunks at all (a no-argument call) on step 1, then a plain text completion on step 2
**When** the Loop issues its second request
**Then** the second request's replayed `tool_use` block carries `"input": {}` -- never a missing or empty `input` key

### AC: empty-final-turn-and-empty-text-blocks-dropped
**Requirements:** ai-layer#req:anthropic-thinking-block-replay

**Given** separately: (a) an `ai/agent.Loop` final step with no text, no tool calls and no `ProviderState`, and (b) an `ai.ChatRequest` whose `Messages` include an assistant message with empty `Text` and no `ToolCalls`, and (c) a `ProviderState` whose captured content array includes a `"text"` block with empty text
**When** (a) the Loop completes and its transcript is read, and (b)/(c) `ai/anthropic.buildMessages` builds the wire body
**Then** (a) the empty final turn is not appended to the transcript at all; (b) the empty assistant turn is dropped from the wire entirely rather than sent as `{"type":"text","text":""}`; (c) the empty text block is filtered out of the replay

### AC: provider-state-replay-scoped-to-current-loop-only
**Requirements:** ai-layer#req:anthropic-thinking-block-replay

**Given** an `ai.ChatRequest` with an earlier, already-resolved tool-calling loop (assistant turn carrying a `ProviderState` with a thinking block, then a tool result, then a plain assistant reply) followed by a new user message and a current in-progress tool call that ALSO carries a `ProviderState` with a thinking block
**When** `ai/anthropic.buildMessages` builds the wire body
**Then** the EARLIER loop's assistant turn is rebuilt from `Text`/`ToolCalls` with NO thinking block, while the CURRENT loop's assistant turn replays its `ProviderState` verbatim, thinking block and signature intact

### AC: agent-loop-two-step-tool-use-sums-usage
**Requirements:** ai-layer#req:agent-loop-contract

**Given** an `ai/agent.Loop` over a fake provider that first returns a tool call and then a final text-only completion, with a registered `Handler`
**When** `Loop.Run` is drained
**Then** the handler is called once, exactly one `EventToolResult` and exactly one final `EventCompleted` are seen (never a `EventCompleted` per step), its `Usage` is the sum of both steps', and `Loop.Messages()` returns the user/assistant-tool-call/tool-result transcript

### AC: agent-loop-limits-and-error-resilience
**Requirements:** ai-layer#req:agent-loop-error-handling

**Given** a `Loop` with `MaxSteps: 2` over a provider that always returns another tool call (never stops), and separately a `Loop` whose `Handler` returns an error, and separately a `Loop` whose `Handler` panics
**When** each is run
**Then** the first yields a fatal `ai.Error{Code: "limit"}`; the second (Handler error) also aborts with a fatal `ai.Error` (`ai.ErrCodeUpstream`) after synthesizing `IsError` results for the failed call and any unanswered calls in the same step; the third (panic, recovered) does NOT abort -- it reports an `IsError` `ai.ToolResult` for the panicking call and the run continues

## Open Questions

- `typesafe`'s relevance questions depend on wording: the live API separated a catalogue of 11 tables with the catalogue in the state and a bare question, and not with a description repeated in each question (REQ: typesafe-score-mapping). The wording, and every number of `NarrowingPolicy` and `DurablePolicy` (provisional, from a single small sample), should be re-measured on a product's own corpus whenever a model version is pinned or moved; until then they are starting values, overridable through `decision.policyValues`.
- Per-product and per-decision-type engine overrides (a different strategy for anonymous traffic, for one decision type) are not part of `aiconfig` yet; a host that needs them selects the configured `decision.Provider` per request itself.
- The `ai/score` route is specified here (REQ: cloud-score-route) but its server side lives in other repositories and is not implemented by this module; until a server implements it, a client sees `decision.ErrUnsupported` and its combinator falls over to a local engine.
- `ai/ctxmgr`'s token estimate is `len/4`; if a product finds this consistently over/under-shoots its real tokenizer badly enough to mis-budget, a provider-supplied estimator hook may be worth adding.
- `ai/decision/llmdecider`'s `MaxRecent` trims `Request.Recent` client-side; whether that trimming should instead be the product's responsibility (so it can prioritise which turns matter) is open.
- Whether `ai/anthropic` should adopt native structured-output support instead of the system-prompt-instruction approach is open pending live-API verification of current support/stability for the target models; the instruction approach is kept for now since it is already tested and working.
- N2's current-loop-only `ProviderState` replay scoping (REQ: anthropic-thinking-block-replay) is a deliberate trade-off, not a free fix, and it is worth naming explicitly (m3, r3 review): (1) it discards cross-turn reasoning continuity -- on the models with Anthropic's "preserved thinking" mechanism (Claude Opus 5.5, Claude Fable 5.1), an EARLIER loop's thinking blocks are intentionally rebuilt away once a new user turn starts a new loop, even though those models are specifically designed to carry reasoning forward across edited/continued history; this adapter does not attempt to distinguish "preserved thinking"-capable models from the rest and always rebuilds earlier loops the same way. (2) it causes CACHED-PREFIX CHURN: the moment a loop's turns become "earlier" (a new user message arrives), `buildMessages`' wire bytes for those turns change shape -- from a byte-identical replay of what Anthropic's prompt cache saw on the previous request, to a rebuilt Text/ToolCalls rendering with the thinking blocks stripped -- so the NEXT request's history prefix no longer matches the cached one and that cache entry is paid for again, uncached, even though the CONTENT (ignoring thinking) is unchanged. The root cause both problems share is that dynamic context (REQ: tool-messages-on-the-wire) is spliced INTO the last user message's existing text, which is what makes it unsafe to keep replaying an earlier turn's `ProviderState` past the loop boundary in the first place (an edit landing ahead of a byte-pinned replayed thinking block is exactly what 400s). The long-term fix under consideration is to stop splicing dynamic context into an existing message's text at all, and instead carry it as its own distinct, clearly-delimited slot (e.g. a dedicated per-turn message, or a `mid-conversation system message` per the claude-api skill) that changes turn to turn WITHOUT mutating any earlier message's bytes -- which would let every earlier loop's `ProviderState` replay byte-identically indefinitely, eliminating both the reasoning-continuity loss and the cache churn. Not implemented yet: it changes the wire shape of every adapter that renders dynamic context, not just `ai/anthropic`, and needs its own design pass.

---
*This document follows the https://specscore.md/feature-specification*
