---
format: https://specscore.md/feature-specification
status: Draft
---

# Feature: TUI Kit

> [SpecScore.**Studio**](https://specscore.studio): | [Explore](https://specscore.studio/app/github.com/strongo/aichat/spec/features/tui-kit?op=explore) | [Edit](https://specscore.studio/app/github.com/strongo/aichat/spec/features/tui-kit?op=edit) | [Ask question](https://specscore.studio/app/github.com/strongo/aichat/spec/features/tui-kit?op=ask) | [Request change](https://specscore.studio/app/github.com/strongo/aichat/spec/features/tui-kit?op=request-change) |
**Status:** Draft
**Source Ideas:** —

## Summary

The product-neutral Bubble Tea (charm.land/bubbletea/v2) chat kit under `github.com/strongo/aichat/tui`: a focus-ring state machine (`tui/focus`), a scrolling transcript with pluggable rich `Block` entries (`tui/transcript`), a result-grid block ported from DataTug (`tui/grid`), a working-context sidebar (`tui/sidebar`), a non-buffering stream pump bridging `ai.LLMProvider.Stream` into Bubble Tea messages (`tui/stream`), the reusable chat screen that wires all of the above together (`tui/chatshell`), and the small cross-package message vocabulary in the `tui` package root. It is consumed first by Sneat's chat MVP and by DataTug's CLI, which is migrating its own hand-rolled chat UI onto this kit.

## Problem

Sneat and DataTug both need a terminal chat screen: a scrolling transcript that can render both plain messages and rich results (tables, cards), a streamed-response pump that never buffers a full response into memory, a working-context sidebar the user can pin entities to, and keyboard navigation between all of that (composer, transcript entries, sidebar) that behaves the same way in both products. DataTug's existing chat UI (`datatug-cli/pkg/chat/*.go`) already solved these problems once, product-specifically; building each piece again for Sneat would duplicate the same focus-ring bugs, the same streaming-cancellation races, and the same grid rendering work.

Putting it in `strongo/aichat/tui` means:

- one focus-ring state machine, tested once, that both products' screens share instead of reimplementing DataTug's `gridFocused`/`messageFocused`/`workspaceFocused` boolean soup;
- one stream pump with one cancellation contract (`ctx` flows from the chat screen through to the actual `ai.LLMProvider.Stream` call), so "Esc cancels a live response" works identically everywhere and adapters see real `ctx.Done()` cancellation, not just a local UI stop;
- one grid component (ported from DataTug, generalised with product extension points: `ExtraView`, `WithKeyHandler`, `WithSplitLayout`, `WithFooterHook`) so DataTug's own chat migrates onto it rather than keeping two competing grids;
- one busy/cancel model (`SetBusy`/`SetBusyCancel`) that covers non-streaming phases too (a decision chain, a deterministic query) with the same Esc/Ctrl+C behavior as a live stream.

Like `ai/*`, this package owns only the product-neutral mechanics; products own their own slash commands, handler logic, entity types and rendering choices (`sidebar.Renderer`, `grid.ExtraView`, transcript `Block` implementations).

## Behavior

### Package boundaries

#### REQ: tui-package-boundaries

The module's `tui/*` tree MUST be organised as: `tui` (the message vocabulary shared across sub-packages, e.g. `tui.AddToSidebarMsg`, so a leaf component like `tui/grid` can ask the shell to do something without importing it), `tui/theme` (the centralised colour/style leaf package — see Theme below), `tui/focus` (the pure focus-ring state machine), `tui/transcript` (the scrolling history + `Block`/`EntityBlock` contract), `tui/grid` (the result-grid `Block` implementation), `tui/mdrender` (the shared glamour-backed `transcript.MarkdownRenderer`), `tui/sidebar` (the working-context panel), `tui/stream` (the `ai.LLMProvider.Stream` → Bubble Tea message pump), and `tui/chatshell` (the assembled chat screen). `tui/*` packages MAY depend on `ai/*` (the event model, `session.EntityRef`) but MUST NOT depend on anything product-specific; products depend on `tui/*`, never the other way around. `tui/theme` is a LEAF within `tui/*` itself: every other `tui/*` package (and `tui/chatshell`) MAY depend on `tui/theme`, but `tui/theme` MUST depend on none of them — this is what makes it safe for `tui/transcript` (a `tui/chatshell` dependency) to also read colours from `tui/theme` without a cycle. This mirrors REQ: package-boundaries in `spec/features/ai-layer/README.md` for the `ai/*` tree, which this module also contains.

### Theme

#### REQ: theme-centralised-look

`tui/theme` MUST be the SOLE place any `tui/*` package (or, transitively, any product built on `tui/chatshell`) constructs a colour, border, or padding decision for aichat's shared chat chrome — no other `tui/*` package MAY call `lipgloss.NewStyle().Foreground(...)`/`.Background(...)` with a hard-coded colour literal (founder ruling, 2026-09-25: "UI styling should be unified across apps"). A product supplies CONTENT ONLY — message text, `[]theme.Hint`, `[]theme.MenuItem`, a title/context string — never a colour: every render path `tui/transcript`, `tui/sidebar` and `tui/chatshell` expose (including their DEFAULTS, reachable with zero product options set) MUST route through a `tui/theme` colour or render helper, so the polished look is what every product gets automatically, not something opted into.

`tui/theme.Dark` (default `true`) MUST select which of each role's light/dark colour pair every helper below resolves against, read fresh on every call (not memoised at package init) so `theme.SetDark` takes effect on the very next render; `theme.Detect()` MUST report the real terminal's apparent background (via `lipgloss.HasDarkBackground`) without ever panicking, falling back to `true` for a non-terminal (a test, CI, a piped run).

#### REQ: theme-card-rendering

Founder ruling (2026-09-25) REPLACED the original bordered-card design: "There is unnecessary border around message card and grid. The card defined not by border but by background." `theme.Card(role Role, header, body string, width int, focused bool) string` MUST render body (with header, when non-empty, bold above it) as a FILLED BACKGROUND BLOCK — NO box-drawing border — with `theme.CardPaddingRows`/`CardPaddingCols` (1 row, 2 columns) of padding, whose OUTER width is exactly `width`; `theme.InnerWidth(width)` MUST report the content width available inside such a card. Each `Role` (`RoleUser`, `RoleAssistant`, `RoleSystem`, `RoleError`, `RoleBlock`) MUST get a DISTINCT background/foreground pair, legible in both `Dark` variants (REQ: theme-contrast). `focused: true` MUST (a) shift the WHOLE fill to `theme.FocusSurfaceColors()` (the same pair `theme.SelectedRow`'s selected state and a grid's highlighted row use) and (b) add a 1-column left accent bar in `theme.FocusColor()` — the bar is the non-text focus indicator; the background shift is the reading cue. A card's OUTER width MUST be identical whether or not it is focused (the bar column is always reserved, coloured the card's own background — invisible — when unfocused), so a transcript's cards never shift width as focus moves between them.

Grids are the ONE exception (REQ: transcript-card-rendering, REQ: grid-self-framed): a `tui/grid.Model` keeps its own drawn border (inline title/view-switcher in the top border, inline footer in the bottom border, a scrollbar in the right border — REQ: grid-inline-frame) and is NEVER wrapped in a `theme.Card` fill.

#### REQ: theme-bars-and-chrome

`theme.Bar(width int, content string) string` MUST pad content to exactly `width` columns per line, filling the remainder with the shared bar background — content that already carries its own nested lipgloss styling (e.g. `theme.RenderHints`' per-hint colouring) MUST keep that styling; `Bar` itself sets only the background/width frame around it. `theme.TopBar(width int, title, context string, items []MenuItem) string` MUST compose a title/context/menu-items triple through `Bar`, with an active `MenuItem` visually distinguished (bold+underline). `theme.RenderHints(width int, hints []Hint, segments ...string) string` MUST compose a segment-list/hint-list pair through `Bar`, WRAPPING onto as many lines as needed to fit `width` (a segment/hint that would overflow the current line starts a new one, ported from DataTug's own `wrapStatusSegments`) rather than silently truncating, each `Hint`'s key rendered in `theme.AccentColor()` against its label in `theme.MutedColor()`. `theme.ComposerFrame(width int, content string, focused bool) string` and `theme.PanelFrame(width int, content string, focused bool) string` MUST render their content as a FILLED background (no border): `ComposerFrame` mirrors `Card`'s focus language exactly (a `theme.FocusSurfaceColors()` fill shift plus a 1-column `theme.FocusColor()` left accent bar while focused, `theme.SurfaceColors()` and an invisible bar column otherwise); `PanelFrame` draws only a SINGLE 1-column divider (never a full box — founder 2026-09-25: "no boxes around boxes") between the transcript and the panel, in `theme.BorderColor(focused)`. `theme.ComposerFrameSize()`/`theme.PanelFrameSize() (cols, rows int)` MUST report exactly how many extra columns/rows each frame adds (1 column, 0 rows for both — a filled background needs no extra rows), so a caller (`chatshell`'s `resize`/`historyHeight`/panel sizing) can size the wrapped content and reserve the right amount of screen space without hard-coding either frame's dimensions in two places. `theme.SelectedRow(text string, selected bool) string` MUST render a side-panel/sidebar row with the SAME `theme.FocusSurfaceColors()` fill a focused card uses when `selected`, and an indented, unstyled row otherwise.

#### REQ: theme-contrast

Founder ruling (2026-09-25): "Make sure we have good contrast and texts are readable" — measurable, not eyeballed. `theme.ContrastPairs() []ContrastPair` MUST enumerate every (foreground, background) combination this package paints together, for the CURRENT `Dark` variant, each with a `MinimumRatio`: `theme.bodyTextMinRatio` (4.5, WCAG AA normal text) for every body/muted/selected/header text pair — "muted" (`theme.MutedColor()`) MUST be read at the SAME threshold as any other text, never below it, with de-emphasis coming from weight/saturation instead; `theme.nonTextMinRatio` (3.0, WCAG AA non-text contrast) for a focus border/accent (`theme.FocusColor()`) against its surrounding background. `theme.Contrast(a, b color.Color) float64` MUST compute the WCAG 2.x relative-luminance contrast ratio between two colours. A test (`TestContrastMeetsWCAG`) MUST check every `ContrastPairs()` entry against its `MinimumRatio`, for BOTH `Dark` variants, so a future colour change that regresses readability fails a test instead of shipping.

### Focus ring

#### REQ: focus-ring-zones-and-moves

`tui/focus.Ring` MUST track exactly one `Zone` (`ZoneInput`, `ZoneTranscript`, or `ZoneSidebar`) and, when `ZoneTranscript`, which transcript "stop" (an index into the transcript's ordered focusable entries) is focused. `ShiftUp`/`ShiftDown` MUST move between transcript stops, with `ShiftUp` from an empty input focusing the latest (bottom-most) stop and `ShiftDown` past the last stop returning focus to the input. `ShiftRight` MUST move focus to the sidebar while remembering the prior zone/stop so `ShiftLeft` from the sidebar returns to exactly that spot. `Esc` MUST always return focus to the input, unconditionally. `Ring` MUST hold no rendering state and know nothing about `tea.Msg` -- callers (chatshell) decide which key presses invoke which method.

### Transcript

#### REQ: transcript-block-contract

`tui/transcript.Block` MUST be the extension point for a rich transcript entry (e.g. a `tui/grid.Model`): `View(width int, focused bool) string`, `Update(msg tea.Msg) (Block, tea.Cmd)` (the Bubble Tea value-model convention: `Update` returns the possibly-new `Block` value, never mutates in place), and `Focusable() bool`. A `Block` that also implements `EntityBlock` (`Current() *session.EntityRef`) additionally reports which entity is under its cursor, which `chatshell.Model.FocusedRef` and "Add to sidebar" use; one that implements `WheelConsumer` (`ConsumesWheel(msg tea.MouseWheelMsg) bool`) can claim a mouse wheel event for itself while focused instead of chatshell scrolling the transcript viewport for it (see REQ: chatshell-mouse-support). One that implements `Titled` (`Title() string`) additionally supplies the header shown atop the card `transcript.Model` wraps its `View` output in (REQ: transcript-card-rendering); a `Block` that doesn't implement it renders in an untitled card.

#### REQ: transcript-card-rendering

Every `transcript.Model` entry — a plain user/assistant/system message, an "error: ..." system message, a markdown-rendered assistant message, or a `Block` — MUST render as a card via `theme.Card` (founder ruling, 2026-09-25: "Message should be like a card in chat of any app"), UNLESS the entry is a `Block` implementing `SelfFramed` and reporting `true` (REQ: grid-self-framed), in which case its `View` is rendered DIRECTLY with no `theme.Card` fill at all. Never a bespoke `lipgloss.NewStyle()` literal in `tui/transcript` itself. The role→`theme.Role` mapping for a Card-wrapped entry MUST be: a user entry → `RoleUser`; a `Markdown`-flagged or plain assistant entry → `RoleAssistant` (its header, from `theme.HeaderFor`, is used as-is); a system entry whose `Text` has the `"error:"` prefix (how `chatshell.AppendSystem` reports both a stream/command error and a plain status note — the prefix is the only signal available here to distinguish them) → `RoleError`; any other system entry → `RoleSystem`; a non-self-framed `Block` entry → `RoleBlock` by default, or a `Role()` an optional `Roled` capability reports instead (e.g. a product's own focusable/editable user-message `Block` that still wants the `RoleUser` accent a plain user message gets), with its own `View` rendered at `theme.InnerWidth(width)` (so its content lines up exactly inside the card `theme.Card` then wraps it in) and its header taken from an optional `Titled` capability (empty otherwise). `focused` MUST be forwarded to `theme.Card` exactly as `Rebuild` already computes it per entry (the focus-ring's current stop), so a focused card's fill/accent-bar visibly differs from an unfocused one, matching every other focus/selection indicator in the product (REQ: theme-card-rendering).

`transcript.SelfFramed` (`SelfFramed() bool`) MUST be checked before `Titled`/`Roled` for any `Block` entry: reporting `true` means the `Block` draws its OWN complete frame already (a `tui/grid.Model`'s bordered card — REQ: grid-self-framed — or a product's own Block combining one with additional plain-text content, e.g. DataTug's `JoinBlock`) and transcript MUST render its `View(width, focused)` output UNCHANGED, at the entry's FULL `width` (not `theme.InnerWidth(width)`), with no `theme.Card` fill wrapped around it — wrapping a `SelfFramed` Block in a Card fill as well would be a SECOND frame around the first (founder 2026-09-25: "Grids are the exception ... NO surrounding card fill or second frame").

#### REQ: transcript-streamed-append

`transcript.Model` MUST support appending a streamed entry by `ID` once (`Append` with a non-empty `ID`) and then progressively extending its text (`AppendDelta(id, text)`) as further deltas arrive, without re-rendering or re-flowing entries that aren't changing -- a streamed response must not require buffering its full text before any of it can be displayed.

Every path that can add or change transcript content (`Append`, `AppendDelta`, `ReplaceBlock`, `InvalidateAndRebuild`) auto-scrolls to the bottom ONLY when the viewport was already at the bottom (r4 review, folding in a pre-existing gap): focus being unset (`focusIndex < 0`, i.e. focus is on the composer or sidebar, not any transcript stop) MUST NOT by itself be treated as a reason to follow -- an UNFOCUSED viewport the user has manually wheel-scrolled up MUST NOT be yanked back to the bottom by an arriving delta, same as a focused-on-an-earlier-stop viewport already wasn't. This is `shouldAutoFollow`'s single rule (`viewport.AtBottom()`), shared with `SetSize`'s own follow-unless-at-bottom behaviour (REQ: transcript-setsize-idempotent-and-preserves-scroll) -- a resize with no new content and a streamed delta that IS new content now agree on exactly when following is appropriate.

#### REQ: transcript-setsize-idempotent-and-preserves-scroll

`transcript.Model.SetSize(width, height int)` MUST be a NO-OP (no re-`Rebuild`, no viewport mutation) when NEITHER dimension actually changed from the Model's current size (r3 review, B1). This matters because a caller may legitimately call `SetSize` with the SAME dimensions on every single render -- `chatshell.Model.View()` does exactly this (REQ: chatshell-composer-chips's `historyHeight`/`resize` note) -- and unconditionally re-`Rebuild`ing on every such call had two user-visible regressions: it silently scrolled a manually-scrolled-up viewport back to the bottom on every no-op call, and it re-ran `ensureBlockVisible` for a focused entry on every no-op call too, both on top of the wasted re-render cost of resizing to the size it already was.

When the size DOES change, `SetSize` MUST preserve the viewport's current scroll position UNLESS it was already at the bottom (`viewport.AtBottom()`, checked BEFORE applying the new dimensions), in which case it keeps following -- the same "at the bottom" rule `shouldAutoFollow` applies for `Append`/`ReplaceBlock`/`AppendDelta` (REQ: transcript-streamed-append, r4 review): a genuine resize with no NEW content to show has no reason to jump a scrolled-up viewport back to the bottom the way new content arriving does.

### Stream pump

#### REQ: stream-pump-contract

`tui/stream.Start(ctx, id string, seq iter.Seq2[ai.Event, error]) tea.Cmd` MUST drain `seq` in a goroutine, NEVER buffering the whole response, and return the `tea.Cmd` producing the first `EventMsg`. Each subsequent `EventMsg` MUST carry a `Next tea.Cmd` that a handler returns to receive the following message -- the pump only advances when re-armed, so a consumer that stops returning `Next` (e.g. because the model quit) does not leak a runaway goroutine. Exactly one `DoneMsg{ID, Err}` MUST end the sequence: `Err` nil on a fully-drained stream, the yielded error on a fatal stream error (per `ai.LLMProvider`'s fatal-error contract in `spec/features/ai-layer/README.md`), or `ctx.Err()` on cancellation. `ctx` cancellation MUST stop the goroutine promptly. For a cancellation to reach the underlying provider request (not just stop the local pump), the `iter.Seq2` passed to `Start` MUST itself have been built from that same `ctx` (e.g. `provider.Stream(ctx, req)`), so an adapter observing `ctx.Done()` aborts its own call and yields `ai.ErrCodeCanceled`.

### Chatshell

#### REQ: chatshell-start-stream

`chatshell.Model.StartStream(id string, open func(ctx context.Context) iter.Seq2[ai.Event, error]) tea.Cmd` MUST: cancel any in-flight stream first; derive a cancellable child of the Model's own context and pass it to `open` (so `open` can build `provider.Stream(ctx, req)` and have a later cancellation actually reach the request); mark the model busy; append an empty streamed transcript entry keyed by `id`; and start the `tui/stream` pump plus a spinner tick. The per-stream context MUST be cancelled automatically when a later `StartStream` call supersedes this one, or when the user cancels (Esc/Ctrl+C while busy). A cancelled stream MUST end with a `"(stopped)"` transcript entry, never an error entry.

#### REQ: chatshell-stream-observer

`chatshell.StreamObserver` (an optional `Handler` capability: `OnStreamEvent(id string, ev ai.Event) tea.Cmd` and `OnStreamDone(id string, err error) tea.Cmd`) MUST have `OnStreamEvent` called for every event a `StartStream`-driven stream produces, alongside chatshell's own built-in handling (rendering text deltas, appending non-fatal `EventError` events to the transcript). `OnStreamDone` MUST be called EXACTLY ONCE per `StartStream` call, whatever the outcome (success, a fatal error, or a cancellation) -- including for a stream that was superseded by a later `StartStream` call before it finished, so a product can roll back speculative state or record diagnostics for work that never got to render. A superseded (stale) `DoneMsg` MUST still fire `OnStreamDone` but MUST NOT touch the model's current busy state or append a transcript entry for the CURRENT stream.

#### REQ: chatshell-busy-and-cancel

`chatshell.Model.SetBusy(bool)` MUST mark a non-streaming product-driven phase (e.g. a decision chain, a deterministic query) as in flight the same way a live stream does: the composer stops accepting input and the spinner runs. `SetBusyCancel(cancel func())` MUST register the cancel func for the CURRENT `SetBusy(true)` phase; `SetBusy(false)` MUST clear both `busy` and the registered `SetBusyCancel` func (it does not, and cannot, retroactively invalidate a result already in flight from that phase -- see below). Esc/Ctrl+C while busy MUST cancel whatever is busy: a live stream through its context (its `DoneMsg` reports the cancellation asynchronously and renders `"(stopped)"`), or a bare `SetBusy(true)` phase by calling its registered `SetBusyCancel` func directly and appending `"(stopped)"` immediately, since there is no `DoneMsg` for that path. A first Ctrl+C while busy MUST cancel; an immediately-following second Ctrl+C MUST always quit, whether or not the first cancellation has finished.

A `SetBusy(true)` phase has NO IDENTITY of its own -- unlike `StartStream`, which is keyed by `id` and whose `DoneMsg` always names that `id` back, `SetBusy`/`SetBusyCancel` carry no per-call token, and chatshell itself needs none (cancelling just invokes whatever func is currently registered). A product whose own async work can outlive a cancelled (or superseded) `SetBusy(true)` phase gets NO signal from chatshell telling it "this result belongs to the phase that's still current" versus "this result belongs to a phase the user already cancelled or that was replaced by a newer one". Such a product MUST track its own phase identity across a `SetBusy(true)`/cancel-or-finish/`SetBusy(false)` cycle (e.g. a locally incremented phase token compared at the async completion handler) -- chatshell provides none for this path, by design, the same way `StartStream`'s `id` exists precisely because the streaming path needed one.

#### REQ: chatshell-side-panel

`chatshell.WithSidePanel(p SidePanel)` (`SidePanel`: `Title() string`, `View(width, height int, focused bool) string`, `Update(msg tea.Msg) (SidePanel, tea.Cmd)`) MUST REPLACE the default sidebar end-to-end for the whole sidebar zone: F6 visibility toggling, `Ctrl+←/→` split-percent resizing (clamped to `sidebar.MinChatPercent`/`MaxChatPercent`, 40/75, same as the default sidebar), `Shift+Right`/`Shift+Left` focus-ring participation, and rendering all route through the installed `SidePanel` instead of `sidebar.Model` once set. It starts visible, matching the default sidebar's own start state. It MUST also receive `tea.WindowSizeMsg` (so it can lay itself out) and every message chatshell does not itself recognise (the same messages `dispatchUnhandled` gives the transcript and an optional `MsgHandler`).

An optional `SidePanelPinner` capability (`PinRef(ref session.EntityRef) bool`, `UnpinRef(ref session.EntityRef) bool`, `Refs() []session.EntityRef`) routes `PinToSidebar`/`UnpinFromSidebar`/`SidebarRefs`/`AddToSidebarMsg` to the `SidePanel` instead of the (now-hidden) default sidebar's own ref list. A `SidePanel` that does NOT implement it makes those calls a documented no-op: `PinToSidebar`/`UnpinFromSidebar` do nothing (no `OnSidebarChange` notification) and `SidebarRefs` returns `nil` — chatshell MUST NOT silently fall back to exposing the default sidebar's own (invisible) state in that case.

#### REQ: chatshell-overlay

`chatshell.Model.PushOverlay(o Overlay) tea.Cmd` (`Overlay`: `View(width, height int) string`, `Update(msg tea.Msg) (o Overlay, cmd tea.Cmd, done bool)`) MUST push a modal dialog onto an overlay stack. While the stack is non-empty, the TOP overlay MUST capture user INPUT ONLY — key presses, paste, and mouse events — until its `Update` returns `done: true`, at which point it is popped; an OLDER overlay beneath it MUST NOT receive any message while a newer one is on top. Every OTHER message (stream pump events, the spinner tick, sidebar/product messages, ...) takes chatshell's NORMAL path even while an overlay is open, so e.g. a stream keeps completing and clears `Busy()` behind an open dialog. A `tea.WindowSizeMsg` MUST still resize the shell (and forward to an active `SidePanel`) even while an overlay is open. The top overlay MUST be rendered centred over the rest of the screen, CLAMPED to the box it was asked to render into (`View`'s `width`/`height` arguments) regardless of what it actually draws.

`Overlay` implementations MUST be POINTER types (r3 review, minor 1): `updateOverlay` stores back whatever value `Update` returns, so a value receiver's own mutations are preserved regardless, but `CloseOverlay`'s identity match (below) can only ever find a pointer, since Go interface equality on a struct value compares fields, not "is this the same logical dialog" — a value `Overlay` a product still holds a copy of never `==` the (possibly re-wrapped) value `Update` last returned.

`chatshell.Model.PopOverlay() tea.Cmd` MUST close the TOP overlay PROGRAMMATICALLY, without waiting for its own `Update` to report `done: true`, and MUST be a no-op (return `nil`) when the overlay stack is empty. PopOverlay is TOP-ONLY: it closes whatever happens to be on top, regardless of which overlay a caller "meant" — correct for the common single-overlay case, but WRONG (r3 review, MAJOR) for the ASYNC-SAFE overlay pattern below once a SECOND overlay can be stacked on top of the first before its async result arrives (dialog A's submit is in flight, the user opens dialog B on top of it, A's result lands: `PopOverlay` would close B, not A).

`chatshell.Model.CloseOverlay(o Overlay) bool` MUST remove `o` from the overlay stack WHEREVER IT IS (not only if it's on top), matching by POINTER IDENTITY, and report whether it was found and removed. A non-pointer `Overlay`, or a nil pointer, MUST NOT match anything (`CloseOverlay` returns `false`) rather than panicking — identity MUST be extracted via reflection (or an equivalent safe mechanism) specifically to avoid a runtime panic comparing two interface values whose dynamic type is non-comparable (e.g. one holding a slice or map field), which a direct `==` comparison would risk.

Async-safe overlay pattern: an `Overlay` that must stay open ACROSS an async round trip (e.g. a form whose submission posts to a server before the dialog can close) returns `done: false` plus a product `tea.Cmd` from its own `Update` on submit, exactly like any other command chatshell dispatches. The product's own async RESULT message, once it arrives, is NOT itself overlay input (`isOverlayInputMsg` classifies only key/paste/mouse messages) — it takes chatshell's normal `Update` path and reaches an optional `MsgHandler.OnMsg` EVEN WHILE THE OVERLAY IS STILL OPEN (even while ANOTHER overlay has since been stacked on top of it), the same as every other non-input message this REQ already routes around an open overlay. From there the product calls `CloseOverlay(o)` with the SAME `*T` it passed to `PushOverlay` on success (`PopOverlay` is unsafe here once more than one overlay might be open at once), or updates the overlay in place to show an error while keeping the user's draft (e.g. an optional `interface{ OnResult(any) }` capability the product's own `Overlay` implements, or simply because the product holds that same pointer and can mutate it directly) — `chatshell` itself defines no such result-routing interface; it only guarantees the message reaches `OnMsg` and that `CloseOverlay`/`PopOverlay` work.

#### REQ: chatshell-focus-accessors

`(m *Model) Zone() focus.Zone` MUST report the focus ring's current zone (`focus.ZoneInput`, `focus.ZoneTranscript`, or `focus.ZoneSidebar` — the sidebar zone covers both the built-in sidebar and an installed `SidePanel`), e.g. for a product's context-specific status hint. `(m *Model) FocusedEntryID() string` MUST report the `id` of the transcript entry currently under focus (`AppendBlockWithID`/`StartStream`'s id), and MUST return `""` when the transcript isn't the focused zone, no entry is focused, or the focused entry was never given an id (a plain `AppendUser`/`AppendAssistant`/`AppendBlock` entry).

#### REQ: chatshell-global-keys

`chatshell.WithGlobalKeys(func(tea.KeyPressMsg) (tea.Cmd, bool))` MUST be checked BEFORE chatshell's own key handling (Ctrl+C, Esc, F6, Shift+arrows, the composer, ...) on every `tea.KeyPressMsg` chatshell would otherwise process (i.e. when no `Overlay` is capturing it) -- returning `consumed: true` stops chatshell from handling that key at all this cycle; `consumed: false` lets chatshell's normal handling proceed as if the hook were absent.

#### REQ: chatshell-product-bars

`chatshell.WithTopBarProvider(func(width int) (title, context string, items []theme.MenuItem))` and `WithHintsProvider(func(width int) (hints []theme.Hint, segments []string))` are the CONTENT-ONLY way a product supplies its top bar and status/hints bar: chatshell renders their return values through `theme.TopBar`/`theme.RenderHints` (REQ: theme-bars-and-chrome) on every render, so the product never touches colour. They take PRIORITY, when set, over the legacy `WithTopBar(func(width int) string)`/`WithStatusBar(func(width int) string)` (which fully replace chatshell's own styling with a product-rendered string, kept for a bar `theme.TopBar`/`theme.RenderHints` cannot express) and the DEFAULT — no options set at all — which MUST ALSO render through `theme.TopBar`/`theme.Bar` (`WithTitle`'s title, and the plain `SetStatus` text, line by line, each wrapped in `theme.Bar`) rather than an unstyled literal: the polished look is the default, not something a product opts into (founder 2026-09-25).

The composer's own rendered view MUST be wrapped in `theme.ComposerFrame`, focused exactly when the focus ring's zone is `focus.ZoneInput` — unconditionally, the same as the top/status bars, with no option to opt out. `historyHeight()`/`resize()` MUST size the composer's content and reserve screen space using `theme.ComposerFrameSize()`'s reported column/row overhead rather than a hard-coded constant, so the frame and the layout math that reserves room for it can never drift apart.

#### REQ: chatshell-transcript-ops

`chatshell.Model.ReplaceBlock(entryID string, b transcript.Block)` MUST replace the `Block` of the transcript entry identified by `entryID` IN PLACE (same position, same ID) -- e.g. to refresh or re-run a grid -- and MUST be a no-op when no entry has that ID. If the CURRENTLY FOCUSED transcript entry (by ID, not raw stop index -- either the one being replaced or a different one whose stop shifted because this swap changed an earlier entry's `Focusable()` answer) is still focusable afterward, focus MUST stay on that same entry -- this includes `chatshell.Model`'s OWN `focusRing` (zone/stop tracker), not just `transcript.Model`'s internal focus index: `ReplaceBlock` MUST resync `focusRing`'s stop to the (possibly shifted) focused entry too, since a later `syncFocus` (a resize, a zone change) reapplies `focusRing`'s stop INTO the transcript and would otherwise silently undo the fix with a stale value. When the swap instead makes the CURRENTLY FOCUSED entry itself non-focusable (m2, r3 review), focus MUST move to the NEAREST remaining focusable stop in the transcript (the old stop index, clamped into the new, smaller stop range), or hand off to the composer (`focus.ZoneInput`) when no focusable entry remains at all -- never silently leave the transcript zone focused on nothing.

`SetComposerText(s string)` MUST set the composer's text and move the cursor to the end (an edit-previous-message flow). `ClearTranscript()` MUST cancel any in-flight stream, remove every transcript entry, and return focus to the composer (`/clear`, a session switch); an `EventMsg` for the just-cancelled stream's ID that arrives afterward MUST NOT mutate the (now-cleared) transcript. A product's own `SetBusy(true)` phase (no stream, e.g. a decision chain) has no `DoneMsg` to cancel it asynchronously, so `ClearTranscript` MUST also invoke the registered `SetBusyCancel` callback directly, same as Esc/Ctrl+C's cancel-while-busy path. `FocusEntry(id string) bool` MUST move focus to the transcript entry identified by `id`, scrolling it into view, and report whether such a focusable entry exists (`false` leaves focus unchanged) -- e.g. DataTug's Ctrl+G "jump to latest grid". `AppendBlockWithID(id string, b transcript.Block) bool` MUST append a `transcript.Block` under a caller-chosen id, same as `StartStream`'s id, so the appended entry is later addressable via `FocusEntry`/`ReplaceBlock` -- `AppendBlock` (no id) remains for blocks a product never needs to address again. It MUST reject (return `false`, append nothing) an empty id, an id already held by a live transcript entry, or an id currently owned by an in-flight `StartStream`/`StartStreamMarkdown` call, since both identify a transcript entry the same way and a collision would corrupt `FocusEntry`/`ReplaceBlock` addressing.

#### REQ: chatshell-markdown-renderer

`chatshell.WithMarkdownRenderer(r transcript.MarkdownRenderer)` MUST configure the renderer `AppendAssistantMarkdown(text string)` — and any `transcript.Entry` with `Markdown` set — uses (e.g. a glamour-backed renderer for agent or HTTP-response markdown), equivalent to `transcript.New(transcript.WithMarkdownRenderer(r))` but settable on a `chatshell.Model`'s already-constructed transcript. `StartStreamMarkdown(id string, open func(ctx context.Context) iter.Seq2[ai.Event, error]) tea.Cmd` MUST behave exactly like `StartStream` except the streaming entry is created with `Markdown` set; with no renderer configured it behaves identically to `StartStream` (the flag is inert).

Every delta's text MUST be accumulated onto the entry immediately (so the final `Text` is always complete), but re-running the renderer over that accumulated text on every single delta is NOT required and MUST be throttled: at most once per a bounded time window (`markdownRenderThrottle`, 100ms), PLUS any delta whose text crosses a newline (a likely-stable rendering point such as a completed list item or paragraph) forces an immediate re-render even inside that window, PLUS the stream's completion MUST always force one final re-render regardless of the window, so the displayed markdown is never stale once the stream ends. A delta that gets throttled out (m1, r3 review) MUST also schedule a ONE-SHOT `tea.Tick` follow-up render `markdownRenderThrottle` later, guaranteeing its trailing fragment still renders even if NO further delta ever arrives (a stalled or slow-trickling stream) -- not only "wait for the next delta or completion"; at most one such follow-up is pending at a time (a delta arriving before it fires does not schedule a second one). `chatshell.Model` exposes no way to disable this throttle; a product that needs every-delta rendering re-renders its own copy of the accumulated `Text` outside `chatshell`.

#### REQ: chatshell-mouse-support

`chatshell.WithMouse(mode MouseMode)` MUST set the chat screen's initial mouse-reporting state: `MouseOff` (the default when `WithMouse` is never called) requests no mouse reporting at all -- the terminal's own native text selection/copy keeps working -- and `MouseCellMotion` requests click/release/wheel events (`tea.MouseModeCellMotion`) from construction. `WithMouse(MouseOff)` MUST NOT overwrite the Model's configured mode down to `MouseOff` -- only an actual enabling mode (`MouseCellMotion`) updates it; `MouseOff` only clears the enabled flag, so a LATER `SetMouseEnabled(true)` still restores `MouseCellMotion` (New's default) rather than silently staying off forever because the mode itself was clobbered. `(m *Model) SetMouseEnabled(enabled bool)` MUST toggle mouse reporting at runtime -- e.g. DataTug's F2 capture toggle, since a terminal's native text selection is unusable while mouse reporting is on, so a product offering both needs a key to flip between them -- taking effect on the next `View()` (chatshell has no way to push a mode change to the terminal outside the normal render cycle); `enabled: true` restores the mode configured via `WithMouse` (`MouseCellMotion` if `WithMouse` was never called OR was only ever called with `MouseOff`, never a silent no-op), `enabled: false` requests `tea.MouseModeNone` regardless of that configured mode. `(m *Model) MouseEnabled() bool` MUST report the current toggle state. `View()` MUST set the returned `tea.View`'s `MouseMode` from this state on every render.

EXCEPT while an `Overlay` is on the stack (where it is already captured as overlay input, per REQ: chatshell-overlay's existing `isOverlayInputMsg` classification, unchanged by this REQ, and never reaches any of the below), a `tea.MouseWheelMsg` routes on whether the pane is split (REQ: chatshell-side-panel's `splitEnabled`) and the event's `X` falls at or past the side/sidebar column (`chatWidth() + 3`, the width of the " │ " divider `View()` draws between the chat column and that column):

- in the SIDE column, the event goes ONLY to the `SidePanel`/sidebar (r4 review: the transcript has NOTHING to do with a wheel tick over the sidebar/SidePanel column -- an earlier revision broadcast it to transcript `Block`s there too, which read backwards). When a `SidePanel` is installed (`WithSidePanel`), the raw event is forwarded to `SidePanel.Update` (free to interpret `X`/`Y`/`Button` itself); otherwise (the BUILT-IN `tui/sidebar`, which has no scroll offset of its own -- it is a cursor list, not a viewport), the event moves its cursor the same way an Up/Down key would: `tea.KeyPressMsg{Code: tea.KeyUp}` for `tea.MouseWheelUp`, `tea.KeyPressMsg{Code: tea.KeyDown}` for `tea.MouseWheelDown`, forwarded to `sidebar.Model.Update`.
- in the CHAT column (the default: not split, or `X` short of that boundary), the FOCUSED transcript `Block` gets first refusal via the optional `transcript.WheelConsumer` capability (`ConsumesWheel(msg tea.MouseWheelMsg) bool`, a pure query with no side effect): `transcript.Model.DeliverWheelToFocusedBlock(msg)` MUST dispatch msg to the focused entry's `Block` ONLY (never a broadcast to every entry) IF that `Block` implements `WheelConsumer` AND `ConsumesWheel(msg)` reports true for it, reporting `consumed: true` and the `Block`'s own returned `tea.Cmd`; chatshell scrolls the transcript viewport itself (`tea.MouseWheelUp`/`tea.MouseWheelDown` calling `transcript.Model.ScrollUp`/`ScrollDown`) IF AND ONLY IF `DeliverWheelToFocusedBlock` reported `consumed: false` -- no Block is focused, the focused `Block` doesn't implement `WheelConsumer`, or it declined this particular event. This is what prevents "wheel double-move" (r3 review, minor 2): a focused `Block` that handles `tea.MouseWheelMsg` itself (scrolling its own internal view, e.g. a grid's row list) is dispatched to EXACTLY ONCE and the transcript viewport does NOT also move for the same tick; a `Block` that declines, or doesn't implement the capability at all, is never dispatched to, and the viewport scrolls exactly as if no `Block` were focused.

The transcript receives a wheel event through AT MOST ONE mechanism per tick, never both: the focused-`Block` dispatch OR the direct viewport scroll in the chat column (never simultaneously), and NEITHER in the side column. Regardless of which column applies, a `tea.MouseWheelMsg` MUST ALSO always be forwarded to an optional `MsgHandler.OnMsg` (same as `dispatchUnhandled` forwards every other message chatshell does not itself fully own), and any non-nil `tea.Cmd` from the focused-`Block` dispatch, the `SidePanel`/sidebar routing, or `MsgHandler` MUST be included in the returned batch.

#### REQ: chatshell-composer-chips

`chatshell.Chip{ID, Label string; Ref *session.EntityRef}` is a product-neutral attachment shown as a removable pill above the composer input (ported from DataTug's `ContextReference`-backed attachment chips, datatug-cli#291, matched step for step against DataTug's own `TestComposerAttachmentChipsCanBeFocusedClearedAndRestored`, `origin/main:pkg/chat/workspace_test.go`). `WithChips(chips []Chip)` sets the composer's initial chips; `(m *Model) SetChips(chips []Chip)`/`Chips() []Chip` replace/read the current list at runtime (both take/return defensive copies); `RemoveChip(id string) (tea.Cmd, bool)` (the bool reports whether a chip with that ID was found and removed; false, cmd nil, for an unknown ID) and `ClearChips() tea.Cmd` are product-facing equivalents of a focused-chip removal and Esc's chip-clear step (below), for a product's own UI controls -- both snapshot the pre-change draft exactly like the built-in key/mouse paths, and `ClearChips` on an already-empty list is a no-op. Chips render as one or more WRAPPED rows directly above the input -- each pill `"[Label ×]"`, truncated when a single label would overflow the available width -- and `historyHeight()` (the transcript viewport's fixed height) MUST shrink by exactly the number of chip rows currently rendered, and grow back as chips are removed, so the total layout never overflows `m.height`. `historyHeight()` MUST ALSO account for the rendered top bar's own height (1 line for the default title, or however many a product's `WithTopBar` renders), the open slash-command menu's height, and -- while `Busy()` -- the spinner's own trailing line (`View()` appends `"\n" + spinner + " thinking…"` after the transcript while busy; that line MUST be reserved, not left to silently grow the total by one), so `View()`'s total rendered height equals `m.height` exactly in every combination -- not just the chip-free, single-line-top-bar, no-menu, idle case the constant it replaces silently assumed. Because `historyHeight()` depends on state that can change WITHOUT any of resize()'s traditional call sites (`WindowSizeMsg`, `F6`, `Ctrl+Left`/`Right`, a chip-list change) running -- typing `/` opens the menu, `SetStatus` changes the status segment's height, `SetBusy(true)`/`StartStream` reserves the spinner line -- `View()` MUST re-apply `resize()` (which re-applies `historyHeight()` to the transcript's actual viewport size, among other sizes) at the START of every call, unconditionally, rather than relying on some other event handler to have called it already (r2 review, B1: without this, the transcript's actual on-screen size drifts stale between the state change and the next WindowSizeMsg/F6/chip change, both over/under-filling the screen AND desyncing `chipsTopY`'s click math -- computed fresh from the CURRENT `historyHeight()` at click time -- from whatever was ACTUALLY drawn by the last, stale render, so a click meant for the menu or elsewhere could be misread as landing on a chip's `×`). Calling `resize()` -- and therefore `transcript.SetSize` -- on EVERY render this way relies on `transcript.SetSize` itself being a safe no-op when nothing actually changed (REQ: transcript-setsize-idempotent-and-preserves-scroll, r3 review): without that guarantee, the per-frame call would itself re-snap a wheel-scrolled-up transcript back to the bottom on every single render.

**Composer draft (b1, r1 review):** chatshell keeps ONE `composerDraft{text, chips}` snapshot at a time, covering BOTH the composer's text and its chip list together (not a chips-only snapshot) -- mirroring DataTug's own `composerUndo`/`rememberComposerDraft`/`restoreComposerDraft`. `snapshotComposerUndo` captures the CURRENT text and chips the first time either changes since the last successful restore, submit, or composer text edit; a run of several changes (several chip removals, or a chip removal following Esc's text-clear step) with nothing resetting the snapshot in between is undone as ONE unit by Shift+Esc, not one change at a time. The snapshot is dropped (set to nil) as soon as ANY of these happen: the composer text is edited (even to the same value being re-typed --any change to `input.Value()` counts, matching DataTug), a message is submitted (`Enter`), or a restore succeeds.

**Esc's two-step clear, in the input zone, checked after the slash-menu and busy checks (b1):** the FIRST Esc, while the composer has text, clears the TEXT ONLY -- even while a chip is currently focused, since "there's text to clear" and "a chip is focused" are independent conditions, and the first Esc always answers the text question first. The SECOND Esc, once the text is already empty, detaches EVERY chip (equivalent to `ClearChips`). When Esc is pressed with NO text already (chips or not), it goes straight to the chip-clearing step -- there is no empty first step. Either step snapshots the draft first (`snapshotComposerUndo`). Esc with nothing to clear (no text, no chips) is a no-op that falls through to the existing default focus-ring Esc behaviour, unchanged.

**Shift+Esc / Ctrl+Y** (both keys, same behaviour) restore the composer draft: the TEXT exactly as snapshotted, and the CHIP LIST as the snapshot's chips UNION any chip present now that ISN'T represented in the snapshot (matched by `ID`, or by `Label` when `ID` is empty) -- appended after the snapshot's own chips, in their current order (m2, r1 review). This means a chip removed since the snapshot is restored (that's the whole point), while a chip the product attached via `SetChips` AFTER the snapshot was taken is KEPT, not silently discarded by an undo that was never about it. Restoring clears chip focus (returns keyboard focus to the input) and clears the snapshot itself. It is a no-op when there is nothing to restore, or while `Busy()` is true.

**Keyboard, in the input zone:** `Tab`/`Shift+Tab` cycle chip focus through every chip index plus one extra "no chip focused" state (`(focus+1)%(count+1)`, matching DataTug's `attachmentFocus` cycle) -- `Tab` from the input focuses the first chip, `Tab` from the last chip returns focus to the input, `Shift+Tab` is the same cycle in reverse; entering/leaving chip focus blurs/focuses the composer's textarea to match, since chip focus and text-input focus are mutually exclusive. While a chip is focused, `Left`/`Right` move focus to the adjacent chip (clamped at the first/last, no wrap), and `Backspace`/`Delete` remove exactly that chip (snapshotting first). `Ctrl+D` removes the LAST chip regardless of which (if any) chip is focused (DataTug's own built-in shortcut), also snapshotting first. `Enter` with a chip focused (m1, r1 review) submits normally AND resets chip focus to "none", returning keyboard focus to the input -- same end state as any other way of leaving the chip row. All chip key handling is a no-op while there are no chips, and the composer (chips, Esc's clear steps, and Shift+Esc/Ctrl+Y included) is disabled while `Busy()` is true, same as the rest of the input.

**Mouse** (only while `MouseEnabled()`): a left-button `tea.MouseClickMsg` landing exactly on a chip's `×` glyph removes that chip (snapshotting first); any other click (wrong button, off-glyph, outside the chip row, or mouse reporting off) falls through to chatshell's normal unhandled-message path (`dispatchUnhandled`) exactly as a click did before this REQ (no chatshell-specific handling existed for `tea.MouseClickMsg` at all).

`ChipObserver{ OnChipsChange(chips []Chip) tea.Cmd }` is an optional `Handler` capability: chatshell calls it after every chip-list change IT PERFORMS (a removal, `ClearChips`/Esc's chip-clear step, or a Shift+Esc/Ctrl+Y restore), so a product can keep its own attachment/context state in sync. It is deliberately NOT called from `WithChips`/`SetChips` -- those calls already come FROM the product. `SetChips` does NOT clear a pending Shift+Esc/Ctrl+Y undo snapshot: the snapshot exists to let the user recover a change THEY just made, and a product-driven `SetChips` call for an unrelated reason (e.g. attaching something new from its own workspace UI) must not silently discard that recovery option -- see the merge behaviour above for how such a newly-attached chip survives a later restore.

`ClearTranscript()` (M1, r1 review) additionally drops any pending composer-draft snapshot and clears chip focus -- e.g. a session switch, where the OLD session's "undo my last change" and chip-row cursor position no longer apply to the NEW session's own chips. It does NOT itself clear the chip list; which chips belong to the new session is the product's own call, made via `SetChips`.

### Sidebar

#### REQ: sidebar-pin-and-notify

`chatshell.Model.PinToSidebar`/`UnpinFromSidebar` MUST add/remove a `session.EntityRef` from the sidebar and, when the `Handler` implements the optional `SidebarObserver` capability (`OnSidebarChange(refs []session.EntityRef)`), notify it with a defensive copy of the current pinned refs -- ONLY when the sidebar actually changed (a duplicate pin or a not-present unpin is a no-op, no notification). `sidebar.Model`'s split-pane width MUST stay within `MinChatPercent`/`MaxChatPercent` (40/75), matching DataTug's existing Ctrl+←/→ clamp, so the chat pane never gets crushed to unusability by resizing the split. `sidebar.Model.View` MUST render its header via `theme.PanelHeader` and each row via `theme.SelectedRow` (the cursor row `selected: true` only while `focused`) rather than a package-local style, so the default sidebar's selection highlight uses the same accent every other focused/selected element in the product does (REQ: theme-card-rendering).

### Grid

#### REQ: grid-self-framed

`grid.Model.SelfFramed() bool` MUST always return `true` (satisfying `transcript.SelfFramed`): a grid draws its OWN complete frame (REQ: grid-inline-frame) and MUST NEVER be wrapped in `theme.Card`'s fill by `tui/transcript` (founder 2026-09-25: "Grids are the exception ... NO surrounding card fill or second frame"). A product `Block` that EMBEDS a `*grid.Model` alongside its own additional content (e.g. DataTug's `JoinBlock`, a grid plus a plain-text join-candidate selector) MUST implement `SelfFramed` itself (returning `true`) rather than relying on the embedded grid's own `SelfFramed` — `transcript` only ever asks the OUTERMOST `Block`.

#### REQ: grid-inline-frame

`Model.View`'s card (`tui/grid/render.go`'s `card`) MUST draw its own single-line border with the title/view-switcher inline in the TOP border and the stats footer inline in the BOTTOM border (never as separate lines outside the border), and a scrollbar in the RIGHT border: a plain track (`theme.BorderColor(focused)`) when every row already fits, or a thumb glyph sized to `visible rows / total rows` and positioned by the highlighted row's offset, in `theme.FocusColor()` (focused) or a dimmer accent (unfocused), otherwise. The border colour MUST be `theme.FocusColor()` while focused, `theme.MutedColor()`/`theme.BorderColor(false)` otherwise — satisfying `theme-contrast`'s `nonTextMinRatio` in both cases. Row backgrounds/foregrounds (the shared `rowStyle` helper, `tui/grid/render.go`) MUST come from `theme.SurfaceColors()` (a non-highlighted row, blending into the grid's own border rather than a separately-coloured patch) or `theme.FocusSurfaceColors()` (the highlighted/selected row while the grid is focused) — the SAME selection pair `theme.Card`'s focused fill and `theme.SelectedRow`'s selected state use, never a colour literal local to `tui/grid` (founder 2026-09-25: "Use the single theme focus/selection colour everywhere").

`Model.DefaultMaxVisibleRows` (`theme.MaxInlineGridRows`, 10) MUST be the page size a grid embedded inline in a transcript uses when `WithMaxVisibleRows` isn't supplied — a product overrides it per grid via that option, never by shadowing the shared constant. Beyond that many rows, the grid's built-in paging (↑/↓, PgUp/PgDn, already `bubble-table`'s own behaviour) MUST scroll the visible window, with the footer's row range and the scrollbar thumb's position both updating to match — the grid never grows taller than `DefaultMaxVisibleRows` (or `WithMaxVisibleRows`'s override) data rows regardless of how many rows its source data has.

#### REQ: grid-positional-row-values

`grid.Row.Values` MUST be positional (index-aligned with the `Columns` slice the `Row` was built against), never a map keyed by column name, so two columns sharing a name (e.g. `SELECT a.id, b.id`) each keep their own independent value. `grid.Absent` MUST be usable as a `Row.Values` entry to mean "no value at all for this row/column" (a sparse selection), rendering differently from an explicit `nil` ("NULL"). `Row.Key`, when set to a stable identifier, MUST survive `Sort`, and `IndexForKey` MUST find a row again by that key after a sort or a data refresh.

#### REQ: grid-filter-and-source-row

Pressing `/` MUST open bubble-table's built-in row filter; while the filter input is focused, `Model.CapturesEsc()` MUST report `true` so a surrounding `chatshell` lets Esc clear/blur the filter -- but NOT before that Esc's higher-priority uses: `chatshell`'s Esc order is (1) close an open slash-command menu, (2) cancel a busy stream/`SetBusy(true)` phase, (3) let a focused Block (e.g. the grid's filter) capture it via `CapturesEsc`, (4) the default focus-ring Esc (return to the composer). A grid is never both busy-cancelling and filter-focused at once in practice, but the ORDER matters when it matters: busy-cancel wins over the filter, not the other way around. The filter itself operates over the grid's already-materialised `Row`s (the "source rows" the `Model` was built or `Set` with) -- filtering MUST NOT mutate or discard the underlying source rows, only the visible subset, so clearing the filter always recovers the full row set.

#### REQ: grid-extension-points

There is no fixed built-in secondary view: view `0` is always the table, and every other view is a product-registered `ExtraView` (`WithExtraViews`/`SetExtraViews`), in whatever order the product wants; digit keys `"2"`..`"9"` switch to the corresponding registered `ExtraView`. `CardView`/`InspectorView` are ready-made `ExtraView` constructors (a formatted vertical field list, and a raw-Go-value dump, of the highlighted row respectively) a product may use as-is. Key dispatch order is: the filter (when focused) first, then -- when the active secondary view is a product-registered `ExtraView` with its own `Update` -- that `ExtraView.Update` gets the key BEFORE `WithKeyHandler` does, since a focused secondary view's own key handling (e.g. scrolling a chart, navigating a card) takes priority over the product's grid-wide hook; only when the `ExtraView` doesn't claim the key (or the table view is active) does `WithKeyHandler` run, and it MAY claim any key the grid would otherwise handle itself (e.g. a product's own Enter behavior instead of `RowActivatedMsg`). `WithSplitLayout` MUST let a product decide, via `Model.NaturalWidth` (the table's unclipped content width) compared against the pane's total width, whether the table and the active `ExtraView` render side by side or the `ExtraView` takes over the full pane. `WithFooterHook` MUST let a product append its own text to the grid's built-in stats footer (row/column range, sort indicator) without replacing it.

## Acceptance Criteria

### AC: focus-ring-shift-right-and-back
**Requirements:** tui-kit#req:focus-ring-zones-and-moves

**Given** a `focus.Ring` with focus on a transcript stop
**When** `ShiftRight()` then `ShiftLeft(stops)` are called
**Then** the zone becomes `ZoneSidebar` after the first call and returns to `ZoneTranscript` at the SAME stop after the second

### AC: focus-ring-esc-always-returns-to-input
**Requirements:** tui-kit#req:focus-ring-zones-and-moves

**Given** a `focus.Ring` in `ZoneSidebar` or at any transcript stop
**When** `Esc()` is called
**Then** the zone is `ZoneInput`, unconditionally

### AC: transcript-block-update-returns-new-value
**Requirements:** tui-kit#req:transcript-block-contract

**Given** a fake `transcript.Block` whose `Update` returns a distinct new value
**When** the transcript dispatches a message to it
**Then** the transcript's stored entry is the NEW `Block` value, not the original, matching the Bubble Tea value-model convention

### AC: transcript-streamed-delta-does-not-auto-follow-when-unfocused-but-scrolled-up
**Requirements:** tui-kit#req:transcript-streamed-append

**Given** a `transcript.Model` with enough streamed text already appended to overflow the viewport (at the bottom, unfocused), then scrolled up (`ScrollUp`, e.g. a mouse wheel tick)
**When** `AppendDelta` is called again with more text for the SAME streaming entry
**Then** the viewport's `YOffset` is UNCHANGED -- the reader is NOT yanked back to the bottom merely because nothing is focused; separately, in a fresh instance still AT the bottom, a further `AppendDelta` keeps the viewport at the bottom (following continues for a reader who hasn't scrolled away)

### AC: transcript-setsize-same-dimensions-preserves-wheel-scroll
**Requirements:** tui-kit#req:transcript-setsize-idempotent-and-preserves-scroll

**Given** a `transcript.Model` sized and filled with enough unfocused content to overflow the viewport, then scrolled up (`ScrollUp`, e.g. a mouse wheel tick)
**When** `SetSize` is called again with the SAME width and height, once or twice in a row
**Then** the viewport's `YOffset` is UNCHANGED by either call -- it is not snapped back to the bottom despite nothing being focused

### AC: transcript-setsize-same-dimensions-does-not-reforce-focused-entry
**Requirements:** tui-kit#req:transcript-setsize-idempotent-and-preserves-scroll

**Given** a `transcript.Model` with several focusable entries, the LAST one focused (which scrolls it into view via `ensureBlockVisible`), then scrolled away from that entry
**When** `SetSize` is called again with the SAME width and height
**Then** the viewport's `YOffset` is UNCHANGED -- the focused entry is not re-forced back into view

### AC: transcript-setsize-height-change-preserves-offset-unless-at-bottom
**Requirements:** tui-kit#req:transcript-setsize-idempotent-and-preserves-scroll

**Given** a `transcript.Model` with enough unfocused content to overflow the viewport, scrolled partway up (not at the bottom, not at the top)
**When** `SetSize` is called with a DIFFERENT height, and separately, in a fresh instance where the viewport IS at the bottom, `SetSize` is called with a different height
**Then** in the first case `YOffset` is unchanged by the resize; in the second case the viewport is still at the bottom after the resize (it keeps following)

### AC: stream-pump-drains-without-buffering-and-rearms
**Requirements:** tui-kit#req:stream-pump-contract

**Given** a fake `iter.Seq2[ai.Event, error]` yielding several events then completing
**When** `stream.Start` is driven by repeatedly returning each `EventMsg.Next`
**Then** every event arrives as a separate `EventMsg` (none buffered/batched), the pump stops advancing if `Next` is not returned, and exactly one `DoneMsg{Err: nil}` ends the sequence

### AC: stream-pump-ctx-cancel-yields-done-with-ctx-err
**Requirements:** tui-kit#req:stream-pump-contract

**Given** a `stream.Start` pump over a slow-yielding sequence
**When** the `ctx` passed to `Start` is cancelled mid-stream
**Then** the goroutine stops promptly and `DoneMsg.Err` is `ctx.Err()`

### AC: chatshell-start-stream-supersedes-and-cancels-prior
**Requirements:** tui-kit#req:chatshell-start-stream

**Given** a `chatshell.Model` with an in-flight `StartStream("a", ...)`
**When** `StartStream("b", ...)` is called before "a" finishes
**Then** "a"'s context is cancelled, and "a"'s eventual `DoneMsg` does not touch the model's current (now "b"'s) busy state or transcript entry

### AC: chatshell-stream-observer-done-fires-once-even-when-superseded
**Requirements:** tui-kit#req:chatshell-stream-observer

**Given** a `Handler` implementing `StreamObserver`, and a `StartStream("a", ...)` immediately superseded by `StartStream("b", ...)`
**When** both streams eventually produce their `DoneMsg`
**Then** `OnStreamDone` is called exactly once for "a" (with whatever error superseding it produces) and exactly once for "b"

### AC: chatshell-second-ctrlc-always-quits
**Requirements:** tui-kit#req:chatshell-busy-and-cancel

**Given** a busy `chatshell.Model` (stream or `SetBusy(true)`)
**When** Ctrl+C is pressed twice in immediate succession
**Then** the first cancels the busy phase (calling `SetBusyCancel`'s func or the stream's context cancel) and the second quits, regardless of whether the first cancellation has completed

### AC: sidebar-duplicate-pin-does-not-notify
**Requirements:** tui-kit#req:sidebar-pin-and-notify

**Given** a `chatshell.Model` with a `SidebarObserver` `Handler` and a ref already pinned
**When** `PinToSidebar` is called again with the same ref
**Then** `OnSidebarChange` is NOT called a second time

### AC: grid-positional-values-and-absent
**Requirements:** tui-kit#req:grid-positional-row-values

**Given** two `Column`s both named `"id"` and a `Row` with distinct values at those two positions
**When** the grid renders or sorts
**Then** each column shows its own value (never the other's), and a `grid.Absent` entry renders distinctly from an explicit `nil`

### AC: grid-filter-preserves-source-rows
**Requirements:** tui-kit#req:grid-filter-and-source-row

**Given** a `grid.Model` with N rows and the built-in filter narrowing the visible set
**When** the filter is cleared
**Then** all N original rows are visible again, and `CapturesEsc()` reported `true` while the filter input held focus

### AC: grid-extraview-registration-and-switching
**Requirements:** tui-kit#req:grid-extension-points

**Given** a `grid.Model` built `WithExtraViews` of two product views
**When** `"2"` and `"3"` are pressed in turn
**Then** `SetView`'s active view switches to the first and then the second registered `ExtraView`, and `"1"` returns to `ViewTable`

### AC: side-panel-replaces-sidebar-in-focus-ring-and-split
**Requirements:** tui-kit#req:chatshell-side-panel

**Given** a `chatshell.Model` built `WithSidePanel(p)` at a width above the split threshold
**When** `Shift+Right` is pressed, then a key while focused, then `F6`, then `Ctrl+Right`
**Then** the focus ring moves to the sidebar zone and `p.Update` receives the key, `F6` hides the panel (`splitEnabled()` becomes false), and `Ctrl+Right` grows `panelChatPercent()` -- all without touching the default `sidebar.Model`

### AC: overlay-captures-keys-until-done-and-stacks
**Requirements:** tui-kit#req:chatshell-overlay

**Given** a `chatshell.Model` with two overlays pushed (`PushOverlay` twice)
**When** a key is sent
**Then** only the TOP overlay's `Update` receives it (not the one beneath, not the composer); when the top overlay's `Update` returns `done: true` it is popped and the one beneath becomes top; once the stack is empty again, keys reach the composer as normal

### AC: overlay-does-not-block-non-input-messages-or-clamp-oversized-content
**Requirements:** tui-kit#req:chatshell-overlay

**Given** a `chatshell.Model` with an in-flight `StartStream` and an `Overlay` pushed mid-stream, and separately an `Overlay` whose `View` renders far larger than the box it's given
**When** the stream's events are drained
**Then** the stream still completes and `Busy()` clears (its `EventMsg`/`DoneMsg` never reached the overlay, only key/paste/mouse would have); and every rendered line of `View()`'s output is no wider than the screen, whatever the oversized overlay tried to draw

### AC: async-overlay-stays-open-on-failure-closes-on-success
**Requirements:** tui-kit#req:chatshell-overlay

**Given** a `chatshell.Model` with an `Overlay` pushed whose own `Update` never itself reports `done: true`, and a `Handler` that also implements `MsgHandler` and holds a reference to the `Model` and the overlay
**When** a product-defined async result message carrying failure is sent through `Update`, and separately one carrying success
**Then** the failure message reaches `MsgHandler.OnMsg` while the overlay is still open (it is not overlay input) and the overlay stays open (state on it can be mutated in place, e.g. to show an error); the success message likewise reaches `OnMsg`, whose handler calls `CloseOverlay(o)` with that same overlay, and the overlay is thereafter removed from the stack; separately, `PopOverlay()`/`CloseOverlay(o)` on an empty overlay stack both return `nil`/`false` respectively and do not panic

### AC: close-overlay-removes-correct-overlay-when-another-is-stacked-on-top
**Requirements:** tui-kit#req:chatshell-overlay

**Given** a `chatshell.Model` with two `*fakeOverlay` pushed in order (A then B, B on top)
**When** `CloseOverlay(A)` is called
**Then** it returns `true`, A is removed from the stack, and B remains (specifically NOT removed, unlike what `PopOverlay()` would have done since B is on top); separately, `CloseOverlay` given an overlay that was never pushed returns `false` and leaves the stack untouched, and `CloseOverlay` given a NON-POINTER `Overlay` value or a nil `*T` pointer also returns `false` without panicking (never matches by Go's `==`, which would risk a panic on a non-comparable underlying type)

### AC: focus-accessors-report-zone-and-focused-entry-id
**Requirements:** tui-kit#req:chatshell-focus-accessors

**Given** a `chatshell.Model` with one `AppendBlockWithID`-appended transcript entry
**When** `Zone()`/`FocusedEntryID()` are read at construction (composer focused), after `Shift+Up` focuses the transcript entry, and after `Shift+Right` moves focus to the sidebar
**Then** they report `(focus.ZoneInput, "")`, `(focus.ZoneTranscript, "<that entry's id>")`, and `(focus.ZoneSidebar, "")` respectively — `FocusedEntryID()` only ever reports non-empty while `Zone() == focus.ZoneTranscript` AND the focused entry was given an id

### AC: global-keys-checked-before-shell-defaults
**Requirements:** tui-kit#req:chatshell-global-keys

**Given** a `chatshell.Model` built `WithGlobalKeys` a hook that claims `F3` (`consumed: true`) and passes every other key through
**When** `F3` is sent, then Enter with composer text is sent
**Then** the hook fires for `F3` and chatshell does not additionally treat it as any of its own shortcuts, while the unclaimed Enter still submits normally

### AC: replace-block-updates-entry-in-place-and-keeps-focus
**Requirements:** tui-kit#req:chatshell-transcript-ops

**Given** a transcript entry with `ID: "grid-1"` holding one `transcript.Block`, currently focused
**When** `Model.ReplaceBlock("grid-1", newBlock)` is called
**Then** the entry at that position now renders `newBlock`, its `ID` and position are unchanged, focus is still on that entry, and `ClearTranscript()` afterward cancels any active stream, empties the transcript, and returns focus to the composer

### AC: replace-block-syncs-focus-ring-stop-not-just-transcript
**Requirements:** tui-kit#req:chatshell-transcript-ops

**Given** two transcript entries where an EARLIER entry's `Focusable()` answer changes (shifting a LATER, currently-focused entry's stop index) and `Model.ReplaceBlock` is called for the earlier entry
**When** a later, unrelated `syncFocus` runs (e.g. a resize) that reapplies `focusRing.Stop()` into the transcript
**Then** focus is still on the same later entry -- `ReplaceBlock` must have updated `focusRing`'s own stop, not only `transcript.Model`'s internal focus index, or the later `syncFocus` would silently revert it

### AC: replace-block-moves-focus-to-nearest-stop-or-composer
**Requirements:** tui-kit#req:chatshell-transcript-ops

**Given** separately: (a) three transcript entries, the middle one focused, and `ReplaceBlock` on the middle one makes it non-focusable while the other two stay focusable; (b) a single focusable transcript entry, focused, and `ReplaceBlock` makes it non-focusable
**When** each `ReplaceBlock` call is made
**Then** (a) focus moves to the nearest remaining focusable entry (still in the transcript zone); (b) focus hands off to the composer (`focus.ZoneInput`), since no focusable entry remains

### AC: clear-transcript-calls-set-busy-cancel
**Requirements:** tui-kit#req:chatshell-transcript-ops

**Given** a `chatshell.Model` with `SetBusy(true)` and a `SetBusyCancel` callback registered, no stream active
**When** `ClearTranscript()` is called
**Then** the registered callback fires and `Busy()` is false afterward

### AC: focus-entry-jumps-to-entry-by-id
**Requirements:** tui-kit#req:chatshell-transcript-ops

**Given** a transcript with a focusable entry `ID: "grid-1"` among others
**When** `Model.FocusEntry("grid-1")` is called
**Then** it returns `true`, the focus ring is on the transcript zone at that entry's stop, and `FocusEntry("nope")` for an unknown id returns `false` and leaves focus unchanged

### AC: sidepanel-pinner-routes-refs-else-documented-noop
**Requirements:** tui-kit#req:chatshell-side-panel

**Given** a `chatshell.Model` built `WithSidePanel` of a panel implementing `SidePanelPinner`, and separately one that does not
**When** `PinToSidebar`/`UnpinFromSidebar` are called and `SidebarRefs()` is read
**Then** with the `SidePanelPinner`, refs route through it and `OnSidebarChange` fires on an actual change; without it, `SidebarRefs()` returns `nil` and `PinToSidebar`/`UnpinFromSidebar` are no-ops (no notification, no fallback to the default sidebar's own state)

### AC: with-markdown-renderer-applies-to-append-assistant-markdown
**Requirements:** tui-kit#req:chatshell-markdown-renderer

**Given** a `chatshell.Model` built `WithMarkdownRenderer(r)`
**When** `AppendAssistantMarkdown(text)` is called and the transcript is rendered
**Then** the entry's `Markdown` field is set and its rendered view is `r`'s output, not the raw text

### AC: append-block-with-id-is-later-focusable
**Requirements:** tui-kit#req:chatshell-transcript-ops

**Given** a `chatshell.Model`
**When** `AppendBlockWithID("grid-1", b)` is called
**Then** it returns `true`, the transcript has one entry with `ID == "grid-1"`, and `FocusEntry("grid-1")` returns `true`

### AC: append-block-with-id-rejects-collisions
**Requirements:** tui-kit#req:chatshell-transcript-ops

**Given** a `chatshell.Model` that already has a transcript entry `ID: "grid-1"` and a `StartStream` call in flight under id `"turn-1"`
**When** `AppendBlockWithID("grid-1", b)`, `AppendBlockWithID("turn-1", b)`, and `AppendBlockWithID("", b)` are each called
**Then** every call returns `false` and appends no entry

### AC: start-stream-markdown-renders-accumulated-text-progressively
**Requirements:** tui-kit#req:chatshell-markdown-renderer

**Given** a `chatshell.Model` built `WithMarkdownRenderer(r)`
**When** `StartStreamMarkdown(id, open)` streams several text deltas to completion
**Then** the entry's `Markdown` field is set, its final `Text` is the full accumulated string, its rendered view is `r`'s output over that accumulated text (not the raw text), and `r` is invoked more than once as deltas arrive; with no renderer configured, `StartStreamMarkdown` behaves identically to `StartStream`

### AC: start-stream-markdown-throttles-re-render
**Requirements:** tui-kit#req:chatshell-markdown-renderer

**Given** a `chatshell.Model` built `WithMarkdownRenderer(r)`
**When** `StartStreamMarkdown` streams many rapid, newline-free deltas followed by completion
**Then** `r` is invoked far fewer times than there are deltas (throttled to the render window plus the guaranteed completion render) while the entry's final `Text` is still the complete accumulated string; a delta whose text crosses a newline forces an extra render even inside the throttle window

### AC: start-stream-markdown-follow-up-tick-renders-trailing-fragment
**Requirements:** tui-kit#req:chatshell-markdown-renderer

**Given** a `chatshell.Model` built `WithMarkdownRenderer(r)`, a fake clock, and a fake `tea.Tick` that the test fires by hand
**When** a delta is rendered immediately, then a second delta arrives that the throttle window suppresses (no newline, no elapsed window) and NO further delta or completion ever arrives
**Then** a one-shot follow-up tick is scheduled for `markdownRenderThrottle` out, and firing it (with no further stream activity) triggers exactly one more render carrying the full accumulated text, including the previously-suppressed fragment

### AC: mouse-wheel-scrolls-transcript-and-toggle-restores-configured-mode
**Requirements:** tui-kit#req:chatshell-mouse-support

**Given** a `chatshell.Model` built `WithMouse(MouseCellMotion)` with enough transcript entries to overflow the viewport
**When** a `tea.MouseWheelMsg{Button: tea.MouseWheelUp}` is sent through `Update`, followed by `tea.MouseWheelMsg{Button: tea.MouseWheelDown}`
**Then** `View().MouseMode` is `tea.MouseModeCellMotion` throughout, the transcript's rendered view changes after the wheel-up and returns to its original rendering after the matching wheel-down; separately, `SetMouseEnabled(false)` then `View()` reports `tea.MouseModeNone`, and a later `SetMouseEnabled(true)` (with no further `WithMouse` call) restores `tea.MouseModeCellMotion` rather than staying off; separately again, pushing an `Overlay` and sending the same wheel-up leaves the transcript's rendered view unchanged (the overlay captures it first)

### AC: mouse-with-mouse-off-does-not-clobber-later-set-mouse-enabled
**Requirements:** tui-kit#req:chatshell-mouse-support

**Given** a `chatshell.Model` built `WithMouse(MouseOff)` (an EXPLICIT off, not the implicit default of never calling `WithMouse` at all)
**When** `SetMouseEnabled(true)` is called afterward, with no further `WithMouse` call
**Then** `View().MouseMode` is `tea.MouseModeCellMotion`, not `tea.MouseModeNone` -- the explicit `MouseOff` must not have overwritten the Model's underlying configured mode

### AC: mouse-wheel-routes-to-sidepanel-column-and-always-reaches-msghandler
**Requirements:** tui-kit#req:chatshell-mouse-support

**Given** a `chatshell.Model` built `WithMouse(MouseCellMotion)` and `WithSidePanel(p)`, split (width ≥ 104), with a `Handler` that also implements `MsgHandler`, and enough transcript entries to overflow the viewport
**When** a `tea.MouseWheelMsg` with `X` at or past the side panel's column is sent through `Update`, and separately one with `X` inside the chat column
**Then** the side-panel-column event reaches `p.Update` (and the transcript's rendered view is unchanged) while the chat-column event scrolls the transcript (and `p.Update` is NOT additionally called for it); in BOTH cases `MsgHandler.OnMsg` is called with the `tea.MouseWheelMsg`, and a non-nil `tea.Cmd` returned by either `p.Update` or `OnMsg` is included in `Update`'s returned command

### AC: mouse-wheel-never-reaches-transcript-in-the-side-column
**Requirements:** tui-kit#req:chatshell-mouse-support

**Given** a `chatshell.Model` built `WithMouse(MouseCellMotion)` with a focused `transcript.Block` implementing `WheelConsumer` (`ConsumesWheel` returning `true`), split (width ≥ 104), no `SidePanel` installed
**When** a `tea.MouseWheelMsg` with `X` at or past the sidebar column is sent through `Update`
**Then** the `Block`'s `Update` is NOT called and the transcript's rendered view is unchanged -- only the built-in sidebar's cursor moves (r4 review: the transcript has nothing to do with a wheel event over the sidebar column, regardless of what the focused `Block` would otherwise consume)

### AC: mouse-wheel-chat-column-focused-block-first-refusal
**Requirements:** tui-kit#req:chatshell-mouse-support

**Given** a `chatshell.Model` built `WithMouse(MouseCellMotion)` with enough transcript entries to overflow the viewport, and separately: (a) a focused `transcript.Block` implementing `WheelConsumer` with `ConsumesWheel` returning `true`, (b) a focused `transcript.Block` implementing `WheelConsumer` with `ConsumesWheel` returning `false`, (c) a focused `transcript.Block` that does not implement `WheelConsumer` at all
**When** a `tea.MouseWheelMsg` with `X` in the chat column is sent through `Update` in each case
**Then** in case (a) the `Block`'s `Update` is called exactly once with the wheel message and the transcript viewport does NOT also scroll; in cases (b) and (c) the `Block`'s `Update` is NEVER called and the transcript viewport DOES scroll, identically to no `Block` being focused at all

### AC: chip-tab-cycle-and-backspace-remove

**Given** a `chatshell.Model` `WithChips` three chips, freshly constructed (no chip focused, the input holds keyboard focus)
**When** `Tab` is pressed three times, then `Backspace`, then `Tab` once more
**Then** the first two `Tab` presses move chip focus to index 0 then 1 (blurring the input each time); the third `Tab` press returns focus to the input (index -1); `Backspace` at that point does nothing (no chip is focused) and `Chips()` is unchanged; the final `Tab` focuses chip index 0 again

### AC: chip-backspace-removes-focused-chip-and-notifies-observer

**Given** a `chatshell.Model` `WithChips` three chips and a `Handler` implementing `ChipObserver`, with chip index 1 focused
**When** `Backspace` is pressed
**Then** `Chips()` has two entries (the middle chip removed, the other two in their original order) and `OnChipsChange` is called exactly once with that two-chip list; removing the LAST remaining chip the same way returns chip focus to -1 and keyboard focus to the input

### AC: chip-ctrl-d-removes-last-chip-and-enter-with-chip-focused-submits-and-resets-focus

**Given** a `chatshell.Model` `WithChips` three chips
**When** `Ctrl+D` is pressed with NO chip focused (the input holds keyboard focus), and separately, in a fresh instance, `Enter` is pressed with composer text set and chip index 1 focused
**Then** `Ctrl+D` removes the LAST chip (snapshotting the pre-removal draft) regardless of chip focus; the `Enter` case submits the composer text to `Handler.Submit` exactly as it would with no chip focused, AND resets chip focus to -1, returning keyboard focus to the input (m1, r1 review)

### AC: chip-esc-two-step-clear-text-then-chips-even-while-a-chip-is-focused

**Given** a `chatshell.Model` `WithChips` three chips, with composer text set and a chip focused (b1, r1 review, replaying DataTug's own `TestComposerAttachmentChipsCanBeFocusedClearedAndRestored`)
**When** `Esc` is pressed once, then again
**Then** the FIRST `Esc` clears the composer text ONLY -- `Chips()` is unchanged despite the chip being focused -- and the SECOND `Esc` (text now empty) detaches every chip; a `Model` with no text already set goes straight to clearing chips on a single `Esc`; `Esc` with neither text nor chips is a no-op that falls through to the existing default focus-ring Esc

### AC: chip-shift-esc-and-ctrl-y-restore-text-and-chips-as-one-unit

**Given** a `chatshell.Model` `WithChips` three chips and composer text set (b1, r1 review)
**When** `Esc` is pressed twice (clearing text, then chips), then `Shift+Esc`; separately, in a fresh instance, two chips are removed one after another via `Backspace` (no restore or submit in between), then `Ctrl+Y`
**Then** `Shift+Esc` restores BOTH the composer text and the full three-chip list in one press; `Ctrl+Y` behaves identically to `Shift+Esc` and restores all chips removed by the run of `Backspace`es as ONE unit, not one chip at a time; a further `Shift+Esc`/`Ctrl+Y` immediately after either case is a no-op (the snapshot was consumed); submitting a message via `Enter`, or editing the composer text, instead of restoring, clears the pending snapshot so a later `Shift+Esc`/`Ctrl+Y` no longer recovers it

### AC: chip-shift-esc-restore-merges-a-chip-attached-since-the-snapshot

**Given** a `chatshell.Model` `WithChips` three chips (IDs `a`, `b`, `c`), one chip removed via `Backspace` (snapshotting the original three), then `SetChips` called with the remaining two PLUS a new chip (ID `d`, not present in the snapshot) (m2, r1 review)
**When** `Shift+Esc` is pressed
**Then** `Chips()` contains all four chips -- the three from the snapshot (including the one `Backspace` removed) UNION the one (`d`) added since, which is kept rather than discarded; a chip with no `ID` set is matched for this union by `Label` instead, so a still-present ID-less chip is not duplicated by the merge

### AC: chip-mouse-click-on-close-glyph-removes-else-falls-through

**Given** a `chatshell.Model` `WithChips` three chips, `WithMouse(MouseCellMotion)`, `SetMouseEnabled(true)`, and a `Handler` implementing `MsgHandler`
**When** a left-button `tea.MouseClickMsg` lands exactly on a chip's `×` glyph -- located by scanning `View()`'s actual rendered output for the glyph, not by calling an internal layout helper directly (m3, r1 review) -- and separately one lands one column to its left (still inside the pill, not on `×`)
**Then** the first click removes that chip (and notifies `ChipObserver` if implemented); the second reaches `MsgHandler.OnMsg` via chatshell's normal unhandled-message path and leaves `Chips()` unchanged, exactly as any other click did before chip support existed

### AC: chip-clear-transcript-drops-composer-undo-and-chip-focus

**Given** a `chatshell.Model` `WithChips` three chips, a chip focused, and a chip removed via `Backspace` (so a composer-draft snapshot is pending) (M1, r1 review)
**When** `ClearTranscript()` is called
**Then** the pending composer-draft snapshot is dropped (a subsequent `Shift+Esc`/`Ctrl+Y` is a no-op) and chip focus resets to -1; `Chips()` itself is UNCHANGED by `ClearTranscript` -- which chips belong to whatever session comes next is the product's own call, made via `SetChips`

### AC: chip-rows-shrink-history-height-and-grow-it-back

**Given** a `chatshell.Model` sized via `WindowSizeMsg`, `historyHeight()` measured before any chips are set
**When** `SetChips` is called with enough chips to wrap across two rows at the current width, then `SetChips(nil)` clears them
**Then** `historyHeight()` after `SetChips` is exactly 2 less than before (one row subtracted per wrapped chip row); `historyHeight()` after clearing returns to its original value

### AC: view-total-rendered-height-equals-terminal-height

**Given** a `chatshell.Model` sized via `WindowSizeMsg{Width: 80, Height: 24}`, in four configurations: (a) the default single-line top bar, no menu, no status, no chips; (b) `WithTopBar` returning a THREE-line string; (c) `WithCommands` with the composer text set so the slash-command menu is open; (d) both a multi-line `SetStatus` and `WithChips` chips wrapping across two rows, combined with an open command menu
**Then** in every configuration, `len(strings.Split(view.Content, "\n"))` (the total rendered line count of `View()`'s output) equals `m.height` EXACTLY -- not merely bounded by it -- because `historyHeight()` measures the top bar's actual rendered height, the menu's actual rendered height (when open), the chip row(s)' height, and the status segment's height (an EMPTY status still occupies one rendered row, same as a one-line one) rather than assuming a fixed "4 rows of chrome" that was only ever correct for the single-line-top-bar, no-menu, no-status, no-chips case

### AC: view-height-stays-exact-after-chrome-changes-that-bypass-resizes-old-call-sites

**Given** a `chatshell.Model` sized via `WindowSizeMsg{Width: 80, Height: 30}` and rendered once (a baseline `View()` call, matching `m.height`), in three separate cases (b1, r2 review): (a) the slash-command menu is then opened by TYPING `/a`, key by key, rather than via `WithCommands`+pre-set text before the first render; (b) `SetBusy(true)` is then called (no `StartStream`); (c) `SetStatus` with a two-line string is then called
**When** `View()` is called again after each state change, with no intervening `WindowSizeMsg`, `F6`, `Ctrl+Left`/`Right`, or chip-list change
**Then** in every case the rendered line count still equals `m.height` EXACTLY -- neither one line short (the pre-fix menu-by-keystroke and post-render `SetStatus` cases) nor one line over (the pre-fix busy case, where the spinner's trailing line was not reserved)

### AC: click-on-a-menu-row-never-removes-a-chip-with-the-menu-open

**Given** a `chatshell.Model` `WithChips` three chips, `WithCommands`, `WithMouse(MouseCellMotion)`, `SetMouseEnabled(true)`, sized via `WindowSizeMsg`, rendered once, then the slash-command menu opened by typing (as above) and rendered again
**When** a left-button `tea.MouseClickMsg` lands on the chip row's `×` (located by scanning the freshly rendered `View()` output, per the mouse-click AC above) -- confirming it still removes a chip with the menu open -- and separately one lands on the MENU's own first row (`topBarHeight() + historyHeight()`, well above the chip row)
**Then** the click on `×` removes exactly one chip; the click on the menu row removes NO chip and `Chips()` is unchanged -- `chipsTopY`'s click math and the actually-drawn chip row stay in agreement because `View()` re-applied `resize()` before the click's preceding render

### AC: theme-card-focused-differs-from-unfocused-for-every-role

**Requirements:** tui-kit#req:theme-card-rendering

**Given** every `theme.Role` (`RoleUser`, `RoleAssistant`, `RoleSystem`, `RoleError`, `RoleBlock`) and both `theme.Dark` variants
**When** `theme.Card(role, header, body, width, focused)` is rendered once with `focused: false` and once with `focused: true`
**Then** the two renderings differ (a visibly different border), and both contain `body`

### AC: theme-bar-truncates-and-pads-to-exact-width

**Requirements:** tui-kit#req:theme-bars-and-chrome

**Given** `content` longer than `width`
**When** `theme.Bar(width, content)` is rendered
**Then** the result's rendered width equals `width` exactly (truncated with an ellipsis, then padded with the bar background)

### AC: theme-composer-frame-size-matches-rendered-overhead

**Requirements:** tui-kit#req:theme-bars-and-chrome

**Given** `theme.ComposerFrameSize()`'s reported `(cols, rows)`
**When** compared against `theme.ComposerFrame(width, content, focused)`'s actual rendered frame
**Then** the frame adds exactly that many extra columns/rows around content, for both `focused` states

### AC: transcript-block-card-uses-titled-capability-else-untitled

**Requirements:** tui-kit#req:transcript-card-rendering

**Given** two `transcript.Block` entries at the same transcript position, one implementing `transcript.Titled` (returning `"My Title"`), one not
**When** `transcript.Model.Rebuild` renders both
**Then** the `Titled` block's card shows `"My Title"` as its header and the other block's card shows no header

### AC: transcript-system-error-prefix-gets-distinct-card-role

**Requirements:** tui-kit#req:transcript-card-rendering

**Given** a `RoleSystem` entry with `Text: "error: boom"` and another with `Text: "(stopped)"`
**When** both are rendered via `transcript.Model.renderEntry`
**Then** they render with visibly different card styling (`RoleError` vs `RoleSystem`)

### AC: chatshell-topbar-provider-and-hints-provider-take-priority

**Requirements:** tui-kit#req:chatshell-product-bars

**Given** a `chatshell.Model` constructed with BOTH a legacy `WithTopBar`/`WithStatusBar` option AND the corresponding `WithTopBarProvider`/`WithHintsProvider` content option
**When** `View()` renders
**Then** the provider's content appears and the legacy option's full string does not

### AC: chatshell-default-bars-render-through-theme-with-no-options-set

**Requirements:** tui-kit#req:chatshell-product-bars

**Given** a `chatshell.Model` constructed with no top-bar/status-bar/hints option at all, `WithTitle` set, and `SetStatus` called with plain text
**When** `View()` renders
**Then** both the title and the status text appear in the output, rendered through `theme.TopBar`/`theme.Bar` (not an unstyled literal) -- the polished default, unconditionally

### AC: sidebar-selected-row-uses-shared-focus-accent

**Requirements:** tui-kit#req:sidebar-pin-and-notify

**Given** a `sidebar.Model` with at least two pinned refs
**When** `View(width, focused: true)` is rendered with the cursor on one of them
**Then** that row's rendering differs from every unselected row's, via `theme.SelectedRow`

## Open Questions

- `tui/chatshell`'s `StreamObserver`/`SidebarObserver`/`MsgHandler` are all optional capability interfaces detected via type assertion on the single `Handler`; whether a product large enough to want several unrelated observers should instead get first-class multi-observer registration is open, deferred until a real product needs more than one.
- `tui/grid`'s port from DataTug's `datatug-cli/pkg/chat/grid.go` is close to 1:1 for the table itself; DataTug's own migration onto this package (replacing its `GridModel`/`gridState` wrapper with a thin embedding of `*grid.Model`) is tracked as follow-on product work, not part of this Feature.
- Whether `tui/stream`'s pump should expose a buffered-channel variant for a product that wants to decouple event production from Bubble Tea's message loop (rather than always re-arming per event) is open; no product has asked for it yet.

---
*This document follows the https://specscore.md/feature-specification*
