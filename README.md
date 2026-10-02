# aichat

Shared AI provider layer and Bubble Tea chat kit for Sneat, DataTug and future
products. Apache-2.0.

| Package | What it owns |
|---|---|
| `ai` | Chat request, normalised streaming event model, `LLMProvider`, tool calling (`Tool`/`ToolCall`/`ToolResult`) and reasoning |
| `ai/agent` | Tool-calling agent `Loop` over an `ai.LLMProvider`: executes `Handler`s, feeds results back, itself an `ai.LLMProvider` |
| `ai/decision` | `Decision` schema (evolvable) with three provenances (calibrated, self-reported, deterministic), `Provider`, the first-decider-wins `Chain` (stops at an exhausted allowance, a spent budget or a misconfigured engine by default); scored candidates (`ScoredProvider`), the named `SelectionPolicy` with one `Actionable` rule: a decision is actionable only with a positive verdict stamped by the package's own judges (a calibrated selection, a deterministic rule match, an uncalibrated decision accepted by the policy's explicit `AcceptUncalibratedAt`, or a policy-less chain's floor), never by omission, never from the wire or storage (`Chain.Rejudge`), and never a side-effectful interaction without positive, backed confidence; and `Report`/`Trace` of which engine answered, with per-attempt usage and latency. The default thresholds are PROVISIONAL: re-measure them on your corpus and the exact model id |
| `ai/session` | Entity refs, focus/selection/sidebar, pending/previous action |
| `ai/cloudproto` | Product-neutral wire protocol + SSE codec for an AI cloud boundary (e.g. `api.sneat.cloud`) |
| `ai/openaicompat` | `LLMProvider` over the OpenAI Chat Completions streaming API (plain net/http) |
| `ai/openairesponses` | `LLMProvider` over OpenAI's Responses API (plain net/http, item-based input/output, same `Config` shape as `ai/openaicompat`) |
| `ai/anthropic` | `LLMProvider` over the Anthropic Messages streaming API (plain net/http, prompt caching) |
| `ai/cloud` | `LLMProvider` client for the `ai/cloudproto` cloud boundary; `Decider()` returns its separate `decision.Provider` role (also a `ScoredProvider`, over `POST ai/score`), plus `Usage`. Decision-route errors map to the engine-neutral sentinels (invalid, auth, quota, misconfigured) and carry `Retry-After`; "no such route" is told from a mistyped base URL and remembered (`ResetCapabilities`) |
| `ai/decision/rules` | Deterministic, table-driven `decision.Provider` — no regex/NLP engine; a match is declared deterministic (`decision.Deterministic`), so it is always actionable under any selection policy, durable included, and never costs an engine call |
| `ai/decision/llmdecider` | `decision.Provider` backed by one structured LLM inference: an LLM decider with uncalibrated confidences (decisions and scored candidates), a fallback or emulator behind a real decision model |
| `ai/decision/typesafe` | Client for TypeSafe AI's System One API (the Jev decision model): `decision.Provider` and `decision.ScoredProvider` with calibrated probabilities; the model id is required (pin a version) |
| `ai/decision/compose` | Engine combinators over `decision.Provider`: `Single`, `Fallback`, `Hedged`, `Race`, a circuit `Breaker` (hedge-cancelled primaries are tallied slow, not failed; a quota refusal never fails over to a paid backup unless `OnQuota`) and `Budget`, a cap on how often a paid backup is called (`ErrBudget`) |
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

## Using the decision chain safely

Four things a product must know before it acts on `ai/decision` answers:

- **Only a judged decision is actionable.** `Decision.Actionable()` is true only
  when the package's own judges (`Chain`, `compose.WithPolicy`,
  `decision.Deterministic`) stamped a positive verdict in an unexported field. The
  exported `Outcome` is informational and is never believed from a provider, a
  remote engine or storage. A side-effectful interaction (confirmation, rejection,
  correction, cancellation, undo) needs, with or without a policy, an
  `InteractionConfidence` above 0 and at least the durable bar (0.90); a calibrated
  claim also needs `InteractionScores`, because a remote engine's `calibrated` flag
  is only a claim.
- **A stored or replayed decision is NOT actionable** and has lost its provenance
  class (a replayed rule decision reads `self_reported`). Re-judge it with
  `Chain.Rejudge(d, taxonomy)` (it is then an ordinary decision under your policy
  or floor, never deterministic), or re-run your rules.
- **A stopped chain is not "nobody decided".** At an exhausted allowance
  (`ErrQuota`), a spent budget (`ErrBudget`) or a misconfigured engine the chain
  returns `ok=false` with `Trace.StoppedBy` and `Trace.Err()` set, so a paid
  backup is never billed silently. Check `Trace.StoppedBy` BEFORE escalating to
  your paid main LLM. Put rules (deterministic providers) before the engines: a
  stop skips everything after it.
- **A paid backup needs a cap.** TypeSafe documents one 429 (a rate limit) and no
  way to tell a spent Jev account from it, so every 429 stays transient and a
  `Fallback(Breaker(jev), Breaker(llm))` sends all traffic to the LLM once the
  account is spent (a probe: 5 Jev calls and 50 paid LLM calls in 50 turns). Wrap
  the backup in `compose.NewBudget` (inside its `Breaker`), or set
  `decision.backupBudget: {maxCalls, per}` in `aiconfig`. For anonymous or public
  traffic run the calibrated engine alone (no paid backup) or with a budgeted
  backup.

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
