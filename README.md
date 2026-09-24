# aichat

Shared AI provider layer and Bubble Tea chat kit for Sneat, DataTug and future
products. Apache-2.0.

| Package | What it owns |
|---|---|
| `ai` | Chat request, normalised streaming event model, `LLMProvider` |
| `ai/decision` | `Decision` schema (evolvable), `Provider`, the first-decider-wins `Chain` |
| `ai/session` | Entity refs, focus/selection/sidebar, pending/previous action |
| `ai/cloudproto` | Product-neutral wire protocol + SSE codec for an AI cloud boundary (e.g. `api.sneat.cloud`) |

Products own scopes, intents, prompts, actions and controls. This module owns
only what every product needs to talk to models and render chat the same way.
