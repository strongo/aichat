---
format: https://specscore.md/feature-specification
status: Draft
---

# Feature: AI Layer

> [SpecScore.**Studio**](https://specscore.studio): | [Explore](https://specscore.studio/app/github.com/strongo/aichat/spec/features/ai-layer?op=explore) | [Edit](https://specscore.studio/app/github.com/strongo/aichat/spec/features/ai-layer?op=edit) | [Ask question](https://specscore.studio/app/github.com/strongo/aichat/spec/features/ai-layer?op=ask) | [Request change](https://specscore.studio/app/github.com/strongo/aichat/spec/features/ai-layer?op=request-change) |
**Status:** Draft
**Source Ideas:** —

## Summary

The shared, product-neutral AI provider layer under `github.com/strongo/aichat`: the streaming event model and `LLMProvider` contract (`ai`), tool calling and extended-reasoning support across the adapters plus a tool-calling agent loop (`ai/agent`), a decision chain of pluggable `decision.Provider`s including a deterministic rule engine (`ai/decision/rules`) and a single-inference LLM decider (`ai/decision/llmdecider`), three concrete providers (`ai/openaicompat`, `ai/anthropic`, `ai/cloud`), a context manager that keeps a provider-cacheable prompt prefix stable (`ai/ctxmgr`), product-facing config that wires cloud vs. BYOK independently for chat and decision (`ai/aiconfig`), and diagnostics (`ai/diag`). It is consumed first by Sneat's chat MVP and by DataTug (whose chat agent and tool-calling migrated onto `ai/agent` and the adapters' tool support).

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

The module MUST be organised as: `ai` (event model, `LLMProvider`, `Collect`), `ai/session` (conversational working state), `ai/decision` (the `Provider` contract, `Chain`, `Validate`), `ai/cloudproto` (the wire protocol between clients and a cloud AI boundary), `ai/openaicompat`, `ai/openairesponses`, `ai/anthropic`, `ai/cloud` (four `LLMProvider`/`decision.Provider` adapters), `ai/decision/rules` (deterministic table-driven provider), `ai/decision/llmdecider` (single-inference LLM provider), `ai/ctxmgr` (context selection), `ai/aiconfig` (config + wiring), `ai/diag` (turn diagnostics), and `ai/internal/{retry,sse}` (unexported HTTP-adapter helpers: bounded retry-before-first-byte, and the shared SSE line-scanner). No package outside `ai/internal/...` MAY depend on another product's code; `strongo/aichat` has no Sneat- or DataTug-specific types anywhere.

This module also contains a second, independent tree, `tui/*` (the Bubble Tea chat kit: `tui`, `tui/focus`, `tui/transcript`, `tui/grid`, `tui/sidebar`, `tui/stream`, `tui/chatshell`), specified separately in `spec/features/tui-kit/README.md`. `tui/*` MAY depend on `ai/*` (the event model, `session.EntityRef`) but the reverse is never true, and `ai/*` remains fully usable — by a non-interactive product, a server, a script — without ever importing `tui/*`.

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

`ai/openairesponses.Provider` implements `ai.LLMProvider` (Name `"openai-responses"`) over OpenAI's Responses API (`POST {Config.BaseURL}/responses`, `stream: true`), with the SAME `Config` shape as `ai/openaicompat` (`BaseURL`, `APIKey`, `Model`, `Headers`, `HTTPClient`) so products can switch between the two adapters without reshaping their wiring. It MUST map `ChatRequest.System` plus `ContextStatic` blocks to the Responses API's `instructions` field (the leading, stable, cacheable prefix -- see REQ: context-rendering-order, whose dynamic-context splice rule applies identically here: dynamic context is spliced onto the last "user" TEXT input item, never a `function_call`/`function_call_output` item). `ChatRequest.Messages` MUST render as `input` items: a `{role, content}` message item per `RoleUser`/`RoleAssistant` message, one `{type:"function_call", call_id, name, arguments}` item per `ToolCall` on an assistant message (`arguments` defaulting to `"{}"` when empty, per REQ: tool-messages-on-the-wire), and one `{type:"function_call_output", call_id, output}` item per `ToolResult` on a `RoleTool` message (`output` prefixed `"Error: "` when `IsError`). A message item's `content` and a function_call_output item's `output` MUST be sent EVEN WHEN THE EMPTY STRING (`""`) -- the Responses API requires the key present on those item types regardless of value, so this adapter builds each item kind through its own constructor/marshal path (`messageItem`/`functionCallItem`/`functionCallOutputItem`, dispatched by `inputItem.MarshalJSON`) rather than one struct with `omitempty` on every field, which would silently drop an empty value instead of sending it. `ChatRequest.Tools` MUST render as `{type:"function", name, description, parameters, strict:false}`; `ToolChoice` maps to a bare `"auto"`/`"none"`/`"required"` string or, for a named tool, the FLAT `{type:"function", name}` shape (unlike Chat Completions' nested `function` object). `ChatRequest.Reasoning` maps to `reasoning:{effort}` (omitted when empty); `MaxTokens` maps to `max_output_tokens`; `ResponseSchema` maps to `text:{format:{type:"json_schema", schema, strict}}` with the same `StrictSchema` default as REQ: structured-output. Every request also sends `store: false` and `include: ["reasoning.encrypted_content"]` UNCONDITIONALLY -- this adapter never relies on OpenAI's server-side conversation state (no `previous_response_id` chaining: a privacy property worth stating explicitly, since it means no turn of a conversation is ever persisted on OpenAI's servers by this adapter), and the `include` value is what makes a reasoning item's `encrypted_content` arrive inline in the stream so it can be captured and replayed (see the ProviderState paragraph below) without ever turning `store` on.

On a 400 whose error message names the `reasoning` field, the adapter MUST retry the SAME request once, before any byte of a response was seen, WITHOUT that field, and remember (per MODEL, not per `*Provider` instance, mirroring `ai/openaicompat`'s identical `reasoning_effort` fallback -- REQ: reasoning-maps-to-provider-knob) not to send it again for that model on later `Stream` calls; a 400 naming an unrelated field MUST NOT trigger this retry.

The adapter MUST assemble one `EventToolCall` per function-call item from `response.output_item.added` (registers the item's `call_id`/`name`), `response.function_call_arguments.delta`/`.done` (accumulates/finalises `arguments`), and `response.output_item.done` (the AUTHORITATIVE source for the item's final `call_id`/`name`/`arguments`, overriding whatever the delta buffer accumulated), emitted before the terminal `EventCompleted`, per REQ: tool-calling-additive-contract's non-empty-`ID` requirement. `response.output_text.delta` events map to `EventTextDelta`; `response.refusal.delta` text MUST NOT be surfaced as `EventTextDelta` at all (per `ai.StopReasonRefusal`'s doc, a refusal is not an `ai.Error` -- the response completed normally with no usable content), and its only effect is `StopReason: "refusal"` on the terminal `EventCompleted` when no tool call was produced (a tool call, if any, still takes priority over a same-response refusal for `StopReason`). The Responses API has no `[DONE]` sentinel; its own terminal events are `response.completed` (success -- `StopReason: "tool_calls"` when any function-call item was produced, else `"refusal"` when a refusal was seen, else `"end"`) and `response.incomplete` (truncated -- `StopReason: "length"` when `incomplete_details.reason` is `"max_output_tokens"`, `"content_filter"` when it is `"content_filter"`, else `"end"`), and either maps to exactly one `ai.EventCompleted` (REQ: llmprovider-contract); an EOF with neither is a fatal truncation (REQ: truncation-is-fatal's rule applied to this wire shape). `response.incomplete` is instead ALWAYS FATAL (`ai.ErrCodeUpstream`), regardless of `ResponseSchema`, whenever ANY tool call is still open or was merely assembled (registered via `output_item.added`, even one that already reached `output_item.done`) at the time it arrives -- a truncated stream leaves no safe way to know the model's full intended action set, so no `EventToolCall` is ever emitted for that response; separately, `response.incomplete` with `"max_output_tokens"` while `ResponseSchema` was requested (and no tool call is open) is ALSO fatal, same as `ai/openaicompat`'s `finish_reason: "length"` case. `response.failed`'s `response.error` and a top-level `error` event both map through a shared classifier: `rate_limit_exceeded` -> `ai.ErrCodeRateLimited` (retryable), `insufficient_quota` -> `ai.ErrCodeQuota`, `server_error` -> `ai.ErrCodeUpstream` (retryable), any other/absent code -> `ai.ErrCodeUpstream`, with a non-empty fallback message when the wire event carries none. Usage maps `usage.input_tokens`/`usage.output_tokens` to `InputTokens`/`OutputTokens`, `usage.input_tokens_details.cached_tokens` to `CacheReadTokens`, and `usage.output_tokens_details.reasoning_tokens` to `ReasoningTokens` -- the same INFORMATIONAL-SUBSET relationship to `InputTokens`/`OutputTokens` as `ai/openaicompat` (see REQ: usage-billable-tokens-per-adapter). Every other Responses API SSE event type (`response.created`, `response.in_progress`, `response.output_text.done`, `response.content_part.*`, reasoning-summary/MCP/web-search progress events, ...) MUST be silently ignored.

`Event.ProviderState` (REQ: tool-calling-additive-contract) captures EVERY output item -- reasoning (with its `encrypted_content`), message, function_call, and any other type -- from `response.output_item.done`, VERBATIM and in stream order, as a JSON array, on the terminal `EventCompleted`. `buildInput`, when rendering an assistant message AFTER the last genuine `ai.RoleUser` message (i.e. within the CURRENT tool-calling loop) that carries a non-empty, well-formed `Message.ProviderState`, MUST replay those captured items onto the wire VERBATIM in place of the normal message/function_call reconstruction -- mirroring `ai/anthropic.buildMessages`' current-loop-only `ProviderState` scoping (REQ: anthropic-thinking-block-replay): an assistant turn from an EARLIER loop is always rebuilt from `Text`/`ToolCalls` instead, even if it also carries a `ProviderState`. A `ProviderState` that fails to unmarshal, or unmarshals to a zero-length array, falls back to the legacy `Text`/`ToolCalls` reconstruction rather than emitting nothing. This is what lets a reasoning item's `encrypted_content` survive across the steps of one `ai/agent.Loop` run (which already carries `Event.ProviderState` onto the assistant message it appends) without this adapter ever needing OpenAI's server-side `Store`.

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

### AC: openairesponses-reasoning-rejected-retries-once-remembered-per-model
**Requirements:** ai-layer#req:openairesponses-adapter

**Given** an `ai/openairesponses.Provider`, and a 400 whose message names the `reasoning` field on the first request of a `ChatRequest.Reasoning`-set call, and separately a 400 whose message names an unrelated field
**When** each `Stream` call is made, and separately a second `Stream` call is made on the SAME `Provider` for the SAME model after the first succeeded via the fallback
**Then** the `reasoning`-naming 400 triggers exactly one retry without `reasoning` and succeeds; the unrelated-field 400 does not retry and surfaces as a fatal error; the second `Stream` call never sends `reasoning` at all (remembered per model), making only one request

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

- `ai/ctxmgr`'s token estimate is `len/4`; if a product finds this consistently over/under-shoots its real tokenizer badly enough to mis-budget, a provider-supplied estimator hook may be worth adding.
- `ai/decision/llmdecider`'s `MaxRecent` trims `Request.Recent` client-side; whether that trimming should instead be the product's responsibility (so it can prioritise which turns matter) is open.
- Whether `ai/anthropic` should adopt native structured-output support instead of the system-prompt-instruction approach is open pending live-API verification of current support/stability for the target models; the instruction approach is kept for now since it is already tested and working.
- N2's current-loop-only `ProviderState` replay scoping (REQ: anthropic-thinking-block-replay) is a deliberate trade-off, not a free fix, and it is worth naming explicitly (m3, r3 review): (1) it discards cross-turn reasoning continuity -- on the models with Anthropic's "preserved thinking" mechanism (Claude Opus 5.5, Claude Fable 5.1), an EARLIER loop's thinking blocks are intentionally rebuilt away once a new user turn starts a new loop, even though those models are specifically designed to carry reasoning forward across edited/continued history; this adapter does not attempt to distinguish "preserved thinking"-capable models from the rest and always rebuilds earlier loops the same way. (2) it causes CACHED-PREFIX CHURN: the moment a loop's turns become "earlier" (a new user message arrives), `buildMessages`' wire bytes for those turns change shape -- from a byte-identical replay of what Anthropic's prompt cache saw on the previous request, to a rebuilt Text/ToolCalls rendering with the thinking blocks stripped -- so the NEXT request's history prefix no longer matches the cached one and that cache entry is paid for again, uncached, even though the CONTENT (ignoring thinking) is unchanged. The root cause both problems share is that dynamic context (REQ: tool-messages-on-the-wire) is spliced INTO the last user message's existing text, which is what makes it unsafe to keep replaying an earlier turn's `ProviderState` past the loop boundary in the first place (an edit landing ahead of a byte-pinned replayed thinking block is exactly what 400s). The long-term fix under consideration is to stop splicing dynamic context into an existing message's text at all, and instead carry it as its own distinct, clearly-delimited slot (e.g. a dedicated per-turn message, or a `mid-conversation system message` per the claude-api skill) that changes turn to turn WITHOUT mutating any earlier message's bytes -- which would let every earlier loop's `ProviderState` replay byte-identically indefinitely, eliminating both the reasoning-continuity loss and the cache churn. Not implemented yet: it changes the wire shape of every adapter that renders dynamic context, not just `ai/anthropic`, and needs its own design pass.

---
*This document follows the https://specscore.md/feature-specification*
