# aichat

Shared AI provider layer and Bubble Tea chat kit for Sneat, DataTug and future
products. Apache-2.0.

| Package | What it owns |
|---|---|
| `ai` | Chat request, normalised streaming event model, `LLMProvider`, tool calling (`Tool`/`ToolCall`/`ToolResult`) and reasoning |
| `ai/agent` | Tool-calling agent `Loop` over an `ai.LLMProvider`: executes `Handler`s, feeds results back, itself an `ai.LLMProvider` |
| `ai/decision` | `Decision` schema (evolvable), `Provider`, the first-decider-wins `Chain`; scored candidates (`ScoredProvider`), the named `SelectionPolicy`, and `Report`/`Trace` of which engine answered |
| `ai/session` | Entity refs, focus/selection/sidebar, pending/previous action |
| `ai/cloudproto` | Product-neutral wire protocol + SSE codec for an AI cloud boundary (e.g. `api.sneat.cloud`) |
| `ai/openaicompat` | `LLMProvider` over the OpenAI Chat Completions streaming API (plain net/http) |
| `ai/openairesponses` | `LLMProvider` over OpenAI's Responses API (plain net/http, item-based input/output, same `Config` shape as `ai/openaicompat`) |
| `ai/anthropic` | `LLMProvider` over the Anthropic Messages streaming API (plain net/http, prompt caching) |
| `ai/cloud` | `LLMProvider` client for the `ai/cloudproto` cloud boundary; `Decider()` returns its separate `decision.Provider` role (also a `ScoredProvider`, over `POST ai/score`), plus `Usage` |
| `ai/decision/rules` | Deterministic, table-driven `decision.Provider` — no regex/NLP engine |
| `ai/decision/llmdecider` | `decision.Provider` backed by one structured LLM inference: an LLM decider with uncalibrated confidences (decisions and scored candidates), a fallback or emulator behind a real decision model |
| `ai/decision/typesafe` | Client for TypeSafe AI's System One API (the Jev decision model): `decision.Provider` and `decision.ScoredProvider` with calibrated probabilities; the model id is required (pin a version) |
| `ai/decision/compose` | Engine combinators over `decision.Provider`: `Single`, `Fallback`, `Hedged`, `Race`, and a circuit `Breaker` |
| `ai/ctxmgr` | Context Manager: token-budgeted, cache-stable `ai.ContextBlock` selection |
| `ai/aiconfig` | Product config (`cloud`/`byok`, decision chain) and `Build` wiring |
| `ai/diag` | Per-turn diagnostics record (`diag.Turn`) and Debug-level logging |
| `ai/internal/retry` | Unexported retry-before-first-byte helper (+ Retry-After honouring) shared by the HTTP adapters |
| `ai/internal/sse` | Unexported SSE line-scanner (LF/CRLF/bare-CR) shared by every SSE reader in the module |
| `tui` | Messages shared across the `tui/*` packages (e.g. `AddToSidebarMsg`) |
| Markdown rendering | Moved to [`tuigoff/pkg/mdrender`](https://github.com/tuigoff/tuigoff): tuigoff owns rendering, aichat composes it |
| Transcript, sidebar, grid block | Moved to [`tuigoff/pkg/transcript`, `pkg/sidebar`, `pkg/gridblock`](https://github.com/tuigoff/tuigoff): tuigoff renders, aichat composes. `session.EntityRef` is an alias of tuigoff's `entity.Ref` |
| `tui/stream` | Pumps an `ai.LLMProvider` stream into Bubble Tea messages without buffering |
| `tui/chatshell` | The reusable chat screen composing the packages above; a product plugs in a `Handler`, and optionally a `SidePanel` (replaces the sidebar), `Overlay`s (modal dialogs), `GlobalKeys`, content-only top-bar/hints-bar providers (`WithTopBarProvider`/`WithHintsProvider`), and mouse support (`WithMouse`/`SetMouseEnabled`: wheel-scrolls the transcript) |

Products own scopes, intents, prompts, actions and controls. This module owns
only what every product needs to talk to models and render chat the same way
— **including how it looks**: `github.com/tuigoff/tuigoff/pkg/theme` is the single place colour,
padding and border decisions live, and every `tui/*` package (and every
product built on `tui/chatshell`) gets that look as the *default*, with zero
styling code of its own. A product supplies content only — message text,
hint labels (`[]theme.Hint`), top-bar title/context/menu items
(`[]theme.MenuItem`) — never a colour literal. See
[`spec/features/tui-kit`](spec/features/tui-kit) for the full contract.

See [`docs/tui.md`](docs/tui.md) for the `tui/*` keyboard map and sidebar
reference.
