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

The module's `tui/*` tree MUST be organised as: `tui` (the message vocabulary shared across sub-packages, e.g. `tui.AddToSidebarMsg`, so a leaf component like `tui/grid` can ask the shell to do something without importing it), `tui/focus` (the pure focus-ring state machine), `tui/transcript` (the scrolling history + `Block`/`EntityBlock` contract), `tui/grid` (the result-grid `Block` implementation), `tui/sidebar` (the working-context panel), `tui/stream` (the `ai.LLMProvider.Stream` → Bubble Tea message pump), and `tui/chatshell` (the assembled chat screen). `tui/*` packages MAY depend on `ai/*` (the event model, `session.EntityRef`) but MUST NOT depend on anything product-specific; products depend on `tui/*`, never the other way around. This mirrors REQ: package-boundaries in `spec/features/ai-layer/README.md` for the `ai/*` tree, which this module also contains.

### Focus ring

#### REQ: focus-ring-zones-and-moves

`tui/focus.Ring` MUST track exactly one `Zone` (`ZoneInput`, `ZoneTranscript`, or `ZoneSidebar`) and, when `ZoneTranscript`, which transcript "stop" (an index into the transcript's ordered focusable entries) is focused. `ShiftUp`/`ShiftDown` MUST move between transcript stops, with `ShiftUp` from an empty input focusing the latest (bottom-most) stop and `ShiftDown` past the last stop returning focus to the input. `ShiftRight` MUST move focus to the sidebar while remembering the prior zone/stop so `ShiftLeft` from the sidebar returns to exactly that spot. `Esc` MUST always return focus to the input, unconditionally. `Ring` MUST hold no rendering state and know nothing about `tea.Msg` -- callers (chatshell) decide which key presses invoke which method.

### Transcript

#### REQ: transcript-block-contract

`tui/transcript.Block` MUST be the extension point for a rich transcript entry (e.g. a `tui/grid.Model`): `View(width int, focused bool) string`, `Update(msg tea.Msg) (Block, tea.Cmd)` (the Bubble Tea value-model convention: `Update` returns the possibly-new `Block` value, never mutates in place), and `Focusable() bool`. A `Block` that also implements `EntityBlock` (`Current() *session.EntityRef`) additionally reports which entity is under its cursor, which `chatshell.Model.FocusedRef` and "Add to sidebar" use.

#### REQ: transcript-streamed-append

`transcript.Model` MUST support appending a streamed entry by `ID` once (`Append` with a non-empty `ID`) and then progressively extending its text (`AppendDelta(id, text)`) as further deltas arrive, without re-rendering or re-flowing entries that aren't changing -- a streamed response must not require buffering its full text before any of it can be displayed.

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

`chatshell.WithSidePanel(p SidePanel)` (`SidePanel`: `Title() string`, `View(width, height int, focused bool) string`, `Update(msg tea.Msg) (SidePanel, tea.Cmd)`) MUST REPLACE the default sidebar end-to-end for the whole sidebar zone: F6 visibility toggling, `Ctrl+←/→` split-percent resizing (clamped to `sidebar.MinChatPercent`/`MaxChatPercent`, 40/75, same as the default sidebar), `Shift+Right`/`Shift+Left` focus-ring participation, and rendering all route through the installed `SidePanel` instead of `sidebar.Model` once set. It starts visible, matching the default sidebar's own start state.

#### REQ: chatshell-overlay

`chatshell.Model.PushOverlay(o Overlay) tea.Cmd` (`Overlay`: `View(width, height int) string`, `Update(msg tea.Msg) (o Overlay, cmd tea.Cmd, done bool)`) MUST push a modal dialog onto an overlay stack. While the stack is non-empty, the TOP overlay MUST capture every message chatshell would otherwise handle itself (including keys that would submit the composer or trigger chatshell's own shortcuts) until its `Update` returns `done: true`, at which point it is popped; an OLDER overlay beneath it MUST NOT receive any message while a newer one is on top. The top overlay MUST be rendered centred over the rest of the screen. A `tea.WindowSizeMsg` MUST still resize the shell even while an overlay is open.

#### REQ: chatshell-global-keys

`chatshell.WithGlobalKeys(func(tea.KeyPressMsg) (tea.Cmd, bool))` MUST be checked BEFORE chatshell's own key handling (Ctrl+C, Esc, F6, Shift+arrows, the composer, ...) on every `tea.KeyPressMsg` chatshell would otherwise process (i.e. when no `Overlay` is capturing it) -- returning `consumed: true` stops chatshell from handling that key at all this cycle; `consumed: false` lets chatshell's normal handling proceed as if the hook were absent.

#### REQ: chatshell-product-bars

`chatshell.WithTopBar(func(width int) string)` and `WithStatusBar(func(width int) string)` MUST, when set, REPLACE chatshell's default bold-title top line and default `SetStatus`-driven status line(s) respectively in `View()`'s rendered output.

#### REQ: chatshell-transcript-ops

`chatshell.Model.ReplaceBlock(entryID string, b transcript.Block)` MUST replace the `Block` of the transcript entry identified by `entryID` IN PLACE (same position, same ID) -- e.g. to refresh or re-run a grid -- and MUST be a no-op when no entry has that ID. `SetComposerText(s string)` MUST set the composer's text and move the cursor to the end (an edit-previous-message flow). `ClearTranscript()` MUST remove every transcript entry and return focus to the composer (`/clear`, a session switch).

### Sidebar

#### REQ: sidebar-pin-and-notify

`chatshell.Model.PinToSidebar`/`UnpinFromSidebar` MUST add/remove a `session.EntityRef` from the sidebar and, when the `Handler` implements the optional `SidebarObserver` capability (`OnSidebarChange(refs []session.EntityRef)`), notify it with a defensive copy of the current pinned refs -- ONLY when the sidebar actually changed (a duplicate pin or a not-present unpin is a no-op, no notification). `sidebar.Model`'s split-pane width MUST stay within `MinChatPercent`/`MaxChatPercent` (40/75), matching DataTug's existing Ctrl+←/→ clamp, so the chat pane never gets crushed to unusability by resizing the split.

### Grid

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

### AC: global-keys-checked-before-shell-defaults
**Requirements:** tui-kit#req:chatshell-global-keys

**Given** a `chatshell.Model` built `WithGlobalKeys` a hook that claims `F3` (`consumed: true`) and passes every other key through
**When** `F3` is sent, then Enter with composer text is sent
**Then** the hook fires for `F3` and chatshell does not additionally treat it as any of its own shortcuts, while the unclaimed Enter still submits normally

### AC: replace-block-updates-entry-in-place
**Requirements:** tui-kit#req:chatshell-transcript-ops

**Given** a transcript entry with `ID: "grid-1"` holding one `transcript.Block`
**When** `Model.ReplaceBlock("grid-1", newBlock)` is called
**Then** the entry at that position now renders `newBlock`, its `ID` and position are unchanged, and `ClearTranscript()` afterward empties the transcript and returns focus to the composer

## Open Questions

- `tui/chatshell`'s `StreamObserver`/`SidebarObserver`/`MsgHandler` are all optional capability interfaces detected via type assertion on the single `Handler`; whether a product large enough to want several unrelated observers should instead get first-class multi-observer registration is open, deferred until a real product needs more than one.
- `tui/grid`'s port from DataTug's `datatug-cli/pkg/chat/grid.go` is close to 1:1 for the table itself; DataTug's own migration onto this package (replacing its `GridModel`/`gridState` wrapper with a thin embedding of `*grid.Model`) is tracked as follow-on product work, not part of this Feature.
- Whether `tui/stream`'s pump should expose a buffered-channel variant for a product that wants to decouple event production from Bubble Tea's message loop (rather than always re-arming per event) is open; no product has asked for it yet.

---
*This document follows the https://specscore.md/feature-specification*
