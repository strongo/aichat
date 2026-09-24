# aichat

Shared AI provider layer and Bubble Tea chat kit for Sneat, DataTug and future
products. Apache-2.0.

| Package | What it owns |
|---|---|
| `ai` | Chat request, normalised streaming event model, `LLMProvider` |
| `ai/decision` | `Decision` schema (evolvable), `Provider`, the first-decider-wins `Chain` |
| `ai/session` | Entity refs, focus/selection/sidebar, pending/previous action |
| `ai/cloudproto` | Product-neutral wire protocol + SSE codec for an AI cloud boundary (e.g. `api.sneat.cloud`) |
| `tui` | Messages shared across the `tui/*` packages (e.g. `AddToSidebarMsg`) |
| `tui/grid` | Sortable/filterable result-grid transcript block (table/card/inspector views), extracted from DataTug chat's `GridModel`/`gridState` |
| `tui/transcript` | Scrolling chat history: plain messages, streamed text, rich `Block` entries |
| `tui/focus` | The Shift+Arrow focus ring shared by every chatshell (input / transcript stops / sidebar) |
| `tui/sidebar` | The right-hand working-context panel: pinned `session.EntityRef`s |
| `tui/stream` | Pumps an `ai.LLMProvider` stream into Bubble Tea messages without buffering |
| `tui/chatshell` | The reusable chat screen composing the packages above; a product plugs in a `Handler` |

Products own scopes, intents, prompts, actions and controls. This module owns
only what every product needs to talk to models and render chat the same way.

See [`docs/tui.md`](docs/tui.md) for the `tui/*` keyboard map and sidebar
reference.
