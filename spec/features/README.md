---
format: https://specscore.md/features-index-specification
---

# Features

Feature specifications for this project.

## Index

| Feature | Status | Description |
|---------|--------|-------------|
| [AI Layer](ai-layer/README.md) | Draft | The shared, product-neutral AI provider layer under `github.com/strongo/aichat`: the streaming event model and `LLMProvider` contract (`ai`), tool calling and extended-reasoning support across the adapters plus a tool-calling agent loop (`ai/agent`), a decision chain of pluggable `decision.Provider`s including a deterministic rule engine (`ai/decision/rules`) and a single-inference LLM decider (`ai/decision/llmdecider`), three concrete providers (`ai/openaicompat`, `ai/anthropic`, `ai/cloud`), a context manager that keeps a provider-cacheable prompt prefix stable (`ai/ctxmgr`), product-facing config that wires cloud vs. BYOK independently for chat and decision (`ai/aiconfig`), and diagnostics (`ai/diag`). It is consumed first by Sneat's chat MVP and by DataTug (whose chat agent and tool-calling migrated onto `ai/agent` and the adapters' tool support). |
| [TUI Kit](tui-kit/README.md) | Draft | The product-neutral Bubble Tea (charm.land/bubbletea/v2) chat kit under `github.com/strongo/aichat/tui`: a focus-ring state machine (`tui/focus`), a scrolling transcript with pluggable rich `Block` entries (`tui/transcript`), a result-grid block ported from DataTug (`tui/grid`), a working-context sidebar (`tui/sidebar`), a non-buffering stream pump bridging `ai.LLMProvider.Stream` into Bubble Tea messages (`tui/stream`), the reusable chat screen that wires all of the above together (`tui/chatshell`), and the small cross-package message vocabulary in the `tui` package root. It is consumed first by Sneat's chat MVP and by DataTug's CLI, which is migrating its own hand-rolled chat UI onto this kit. |

## Open Questions

None at this time.

---
*This document follows the https://specscore.md/features-index-specification*
