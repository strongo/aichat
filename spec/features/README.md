---
format: https://specscore.md/features-index-specification
---

# Features

Feature specifications for this project.

## Index

| Feature | Status | Description |
|---------|--------|-------------|
| [AI Layer](ai-layer/README.md) | Draft | The shared, product-neutral AI provider layer under `github.com/strongo/aichat`: the streaming event model and `LLMProvider` contract (`ai`), a decision chain of pluggable `decision.Provider`s including a deterministic rule engine (`ai/decision/rules`) and a single-inference LLM decider (`ai/decision/llmdecider`), three concrete providers (`ai/openaicompat`, `ai/anthropic`, `ai/cloud`), a context manager that keeps a provider-cacheable prompt prefix stable (`ai/ctxmgr`), product-facing config that wires cloud vs. BYOK independently for chat and decision (`ai/aiconfig`), and diagnostics (`ai/diag`). It is consumed first by Sneat's chat MVP and by DataTug. |

## Open Questions

None at this time.

---
*This document follows the https://specscore.md/features-index-specification*
