# aichat

Shared AI provider layer and Bubble Tea chat kit for Sneat, DataTug and future
products. Apache-2.0.

| Package | What it owns |
|---|---|
| `ai` | Chat request, normalised streaming event model, `LLMProvider`, tool calling (`Tool`/`ToolCall`/`ToolResult`) and reasoning |
| `ai/agent` | Tool-calling agent `Loop` over an `ai.LLMProvider`: executes `Handler`s, feeds results back, itself an `ai.LLMProvider` |
| `ai/decision` | `Decision` schema (evolvable), `Provider`, the first-decider-wins `Chain` |
| `ai/session` | Entity refs, focus/selection/sidebar, pending/previous action |
| `ai/cloudproto` | Product-neutral wire protocol + SSE codec for an AI cloud boundary (e.g. `api.sneat.cloud`) |
| `ai/openaicompat` | `LLMProvider` over the OpenAI Chat Completions streaming API (plain net/http) |
| `ai/anthropic` | `LLMProvider` over the Anthropic Messages streaming API (plain net/http, prompt caching) |
| `ai/cloud` | `LLMProvider` client for the `ai/cloudproto` cloud boundary; `Decider()` returns its separate `decision.Provider` role, plus `Usage` |
| `ai/decision/rules` | Deterministic, table-driven `decision.Provider` — no regex/NLP engine |
| `ai/decision/llmdecider` | `decision.Provider` backed by one structured LLM inference; the product-neutral "Jev" implementation |
| `ai/ctxmgr` | Context Manager: token-budgeted, cache-stable `ai.ContextBlock` selection |
| `ai/aiconfig` | Product config (`cloud`/`byok`, decision chain) and `Build` wiring |
| `ai/diag` | Per-turn diagnostics record (`diag.Turn`) and Debug-level logging |
| `ai/internal/retry` | Unexported retry-before-first-byte helper (+ Retry-After honouring) shared by the HTTP adapters |
| `ai/internal/sse` | Unexported SSE line-scanner (LF/CRLF/bare-CR) shared by every SSE reader in the module |
| `tui` | Messages shared across the `tui/*` packages (e.g. `AddToSidebarMsg`) |
| `tui/grid` | Sortable/filterable result-grid transcript block (table/card/inspector views), extracted from DataTug chat's `GridModel`/`gridState` |
| `tui/transcript` | Scrolling chat history: plain messages, streamed text, rich `Block` entries |
| `tui/focus` | The Shift+Arrow focus ring shared by every chatshell (input / transcript stops / sidebar) |
| `tui/sidebar` | The right-hand working-context panel: pinned `session.EntityRef`s |
| `tui/stream` | Pumps an `ai.LLMProvider` stream into Bubble Tea messages without buffering |
| `tui/chatshell` | The reusable chat screen composing the packages above; a product plugs in a `Handler`, and optionally a `SidePanel` (replaces the sidebar), `Overlay`s (modal dialogs), `GlobalKeys`, and product-rendered top/status bars |

Products own scopes, intents, prompts, actions and controls. This module owns
only what every product needs to talk to models and render chat the same way.

See [`docs/tui.md`](docs/tui.md) for the `tui/*` keyboard map and sidebar
reference.
