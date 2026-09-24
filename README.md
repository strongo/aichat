# aichat

Shared AI provider layer and Bubble Tea chat kit for Sneat, DataTug and future
products. Apache-2.0.

| Package | What it owns |
|---|---|
| `ai` | Chat request, normalised streaming event model, `LLMProvider` |
| `ai/decision` | `Decision` schema (evolvable), `Provider`, the first-decider-wins `Chain` |
| `ai/session` | Entity refs, focus/selection/sidebar, pending/previous action |
| `ai/cloudproto` | Product-neutral wire protocol + SSE codec for an AI cloud boundary (e.g. `api.sneat.cloud`) |
| `ai/openaicompat` | `LLMProvider` over the OpenAI Chat Completions streaming API (plain net/http) |
| `ai/anthropic` | `LLMProvider` over the Anthropic Messages streaming API (plain net/http, prompt caching) |
| `ai/cloud` | `LLMProvider` + `decision.Provider` client for the `ai/cloudproto` cloud boundary, plus `Usage` |
| `ai/decision/rules` | Deterministic, table-driven `decision.Provider` — no regex/NLP engine |
| `ai/decision/llmdecider` | `decision.Provider` backed by one structured LLM inference; the product-neutral "Jev" implementation |
| `ai/ctxmgr` | Context Manager: token-budgeted, cache-stable `ai.ContextBlock` selection |
| `ai/aiconfig` | Product config (`cloud`/`byok`, decision chain) and `Build` wiring |
| `ai/diag` | Per-turn diagnostics record (`diag.Turn`) and Debug-level logging |
| `ai/internal/retry` | Unexported retry-before-first-byte helper shared by the HTTP adapters |

Products own scopes, intents, prompts, actions and controls. This module owns
only what every product needs to talk to models and render chat the same way.
