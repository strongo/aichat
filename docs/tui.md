# `tui/*` keyboard and sidebar reference

This is a short, product-neutral reference for the Bubble Tea chat kit in
`tui/`. It documents the keybindings and sidebar model shared by every
`tui/chatshell`-based screen (`sneat chat`, `datatug chat`, ...). See
`spec/features/tui-kit` for the full feature spec.

## Focus ring (`tui/focus`)

The screen has three focus zones: **Input** (composer), **Transcript**
(an ordered list of "stops" — result grids, user messages, any focusable
`transcript.Block`), and **Sidebar** (pinned entities).

| Key | From | Effect |
|---|---|---|
| `Shift+Up` | Input (empty) | Focus the latest (bottom-most) transcript stop |
| `Shift+Up` | Transcript stop N | Focus stop N-1 |
| `Shift+Down` | Transcript stop N | Focus stop N+1, or Input past the last stop |
| `Shift+Right` | Input or Transcript | Focus the sidebar (remembers where focus was) |
| `Shift+Left` | Sidebar | Return focus to where it was before `Shift+Right` |
| `Esc` | anywhere | Return focus to Input, unless the focused Block captures Esc (see below) |

`Shift+Right`/`Shift+Left` only move focus when the sidebar is visible and
the pane is split (terminal width ≥ 104 columns). A key that a zone does not
consume (e.g. `Shift+Left` with nowhere to return to, or `Shift+Up` in the
transcript with the input non-empty) is never silently swallowed: it falls
through to that zone's normal handling (composer cursor movement, the
focused Block's own `Update`, etc.).

If the transcript's focused `Block` implements the optional
`transcript.EscCapturer` interface (`CapturesEsc() bool`) and returns true
(e.g. a grid whose "/" filter box currently has input focus), `Esc` is
routed to the Block's own `Update` instead of returning focus to the
composer — so the Block can close its own input mode first. `Shift+Left`
also clamps its remembered transcript stop to the current number of stops,
in case entries were removed while the sidebar had focus.

`F6` hides the sidebar even while it holds focus; when that happens, focus
returns to wherever it was before `Shift+Right` (or the input).

## Composer (`tui/chatshell`)

| Key | Effect |
|---|---|
| `Enter` | Submit the message (calls `Handler.Submit`); no-op while busy or empty |
| `Shift+Enter` | Insert a newline without submitting |
| `/` at start of input | Opens the slash-command menu |
| `↑`/`↓` (menu open) | Move the menu selection |
| `Enter`/`Tab` (menu open) | Insert the selected command |
| `Esc` (menu open) | Closes the menu (stays closed until the input value changes); takes priority over every other Esc behaviour |
| `F6` | Toggle sidebar visibility (moves focus back first if the sidebar held it) |
| `Ctrl+Left`/`Ctrl+Right` | Shrink/grow the chat pane's split share (40–75%) |
| `Esc` / `Ctrl+C` (busy, 1st press) | Cancel the in-flight stream or `SetBusy(true)` phase; renders `(stopped)` — not an error — in the transcript |
| `Ctrl+C` (busy, 2nd consecutive press) | Quit anyway, even if the cancellation from the first press hasn't finished yet |
| `Ctrl+C` (idle) | Quit |

The composer stops accepting keystrokes entirely while `Busy()` is true
(during `StartStream`, or a product's own `SetBusy(true)` phase such as a
decision/query lookup); the spinner runs in its place. Esc's priority order
is: close an open slash-command menu, then cancel if busy, then let a
focused Block capture it (`EscCapturer`), then the default focus-ring Esc.

### Attachment chips (`tui/chatshell`)

`chatshell.WithChips([]Chip{...})` / `(m *Model) SetChips(chips []Chip)` /
`Chips() []Chip` render a product's attachments as removable pills
(`"[Label ×]"`) in wrapped rows directly above the input — e.g. DataTug's
tables/files staged as context for the next turn (ported from
datatug-cli#291's attachment chips). `Chip{ID, Label string; Ref
*session.EntityRef}` is product-neutral: `Ref` carries the product's own
entity identity when the chip has one. The transcript viewport's height
shrinks by exactly the number of rendered chip rows, and grows back as
chips are removed, so the layout never overflows.

| Key | Effect |
|---|---|
| `Tab`/`Shift+Tab` (chips present) | Cycle chip focus: input → chip 0 → … → last chip → input (reverse for Shift+Tab) |
| `Left`/`Right` (a chip focused) | Move focus to the adjacent chip, clamped at the first/last |
| `Backspace`/`Delete` (a chip focused) | Remove the focused chip |
| `Shift+Esc` | Restore the chip list to how it stood before the most recent run of removals (one unit, not one chip at a time); no-op with nothing to restore |
| Left-click on a chip's `×` (mouse enabled) | Remove that chip; any other click falls through to chatshell's normal unhandled-message path |

All chip interaction is a no-op while there are no chips or while `Busy()`
is true. Submitting a message (`Enter`) clears any pending Shift+Esc
snapshot. `chatshell.ChipObserver{ OnChipsChange(chips []Chip) tea.Cmd }` is
an optional `Handler` capability notified after every chip-list change
chatshell itself performs (a removal or a Shift+Esc restore) — it is NOT
called from `WithChips`/`SetChips`, since those calls already come from the
product, and `SetChips` never clears a pending Shift+Esc snapshot for the
same reason.

## Result grid (`tui/grid`)

One grid implementation, used by DataTug and Sneat Chat alike — this package
now owns the table rendering, per-cell column selection, scrollbar, style
presets and footer/stats that used to be duplicated in DataTug's own
`gridState`; DataTug's `gridState` is a thin wrapper embedding a
`*grid.Model`.

| Key | Effect |
|---|---|
| `↑`/`↓`, `k` | Move the highlighted row (no `j` binding — reserved for a product's own use, e.g. DataTug's join-candidate navigation) |
| `←`/`→`, `h`/`l` | Select a column, auto-scrolling it into view (`Model.SelectedColumn()`) |
| `1` | Switch to the table (always view 0) |
| `2`.. | Switch to a `WithExtraViews`/`SetExtraViews`-registered view, in registration order |
| `Tab` | Toggle focus between the table and a split secondary view (`Model.ToggleSecondaryFocusIfSplit()`) |
| `s` | Sort (toggle ascending/descending) by the selected column |
| `Enter` | Emit `grid.RowActivatedMsg` for the highlighted row |
| `+` | Emit `tui.AddToSidebarMsg` for the highlighted row's `Ref` |
| `/` | Open bubble-table's built-in filter |

`grid.WithKeyHandler(fn)` registers a product hook checked *first*, for every
key the filter input isn't consuming; it can claim any key above (DataTug's
Enter opens a cell-detail dialog instead of emitting `RowActivatedMsg`) plus
its own actions with no generic-grid meaning (DataTug's
c/r/a/d/b/B/e/q/space workspace keys).

`Model.CapturesEsc()` reports true while the filter input is focused, so a
surrounding chatshell should let Esc clear/blur the filter before treating
Esc as its own (e.g. closing the block).

`grid.Row.Values` is **positional** (`[]any`, aligned with the `Columns`
slice a `Model` was built from), not a map keyed by column name — two
columns sharing a name (e.g. `SELECT a.id, b.id`) each keep their own value.
Use `grid.Absent` for a cell with no value at all (a sparse selection); it
renders differently from an explicit `nil` ("NULL"). A value may be a raw Go
value (formatted by `grid.FormatValue`) or a product's own pre-formatted
display string.

There is no fixed Card/Inspector view: the table is always view 0, and
everything else is a `WithExtraViews`/`SetExtraViews`-registered `ExtraView`,
in whatever order a product wants. `grid.CardView`/`grid.InspectorView` are
ready-made `ExtraView` constructors (a formatted field list and a raw-value
dump of the highlighted row) for a product that wants one — DataTug
registers `CardView("Current row")` third, after its own Charts view.

A large result is capped to `grid.DefaultMaxVisibleRows` rows per page
(override with `grid.WithMaxVisibleRows`) so it never renders fully into a
scrolling transcript.

A product registers its own secondary views (DataTug's Charts, Raw response,
Headers) with `grid.WithExtraViews(...)`, and a table/secondary-view
split-pane policy — generalising DataTug's `chooseRecordsetLayout` — with
`grid.WithSplitLayout(...)`; `Model.NaturalWidth()` gives a `LayoutFunc` the
table's natural (unclipped) content width to compare against the pane's
total width.

`grid.WithStyle`/`Model.SetStyle` pick a border/header color preset
(`grid.StyleLines`/`StyleSoft`/`StyleMinimal`; `grid.ParseStyle` recovers one
by name). `grid.WithFooterHook` lets a product append text (e.g. a
save-status badge) to the built-in stats footer (row/column range, sort
indicator).

## Sidebar (`tui/sidebar`)

An ordered list of `session.EntityRef`, rendered by a product-supplied
`Renderer`. Reachable via `Shift+Right`/`Shift+Left`, or `+` in a grid pins
an entity there directly.

| Key | Effect |
|---|---|
| `↑`/`↓`, `j`/`k` | Move the cursor |
| `x`, `Delete`, `Backspace` | Remove the highlighted entry (`sidebar.RemoveMsg`) |
| `Enter` | Open the highlighted entry (`sidebar.OpenMsg`) |

`tui/chatshell` adds every pinned/removed ref to `Handler.OnSidebarChange`
when the product's `Handler` implements it (`chatshell.SidebarObserver`).

`Model.SelectionRefs()` and `Model.SidebarRefs()` are distinct: `SelectionRefs`
is the **transcript** selection (the focused Block's `Current()`), while
`SidebarRefs` is the sidebar's pins. `Model.FocusedRef()` follows whichever
zone has focus — the transcript's `Current()`, or the sidebar cursor's ref
when the sidebar zone is focused — and is nil from the composer.

## Streaming (`tui/stream`)

`stream.Start(ctx, id, seq)` pumps an `ai.LLMProvider.Stream` result into
`stream.EventMsg`/`stream.DoneMsg` without buffering the response, per the
event contract: a fatal error is exactly one final yield
`(ai.Event{Type: EventError, Error: e}, e)` (arrives as `DoneMsg{Err: e}`,
not an `EventMsg`); `EventError` with a nil Go error is non-fatal (arrives as
an `EventMsg`, and the stream continues); a user-initiated cancellation
surfaces as `ai.ErrCodeCanceled` once the `ai/` provider has translated it,
or as `context.Canceled` from tui/stream's own ctx-race before that.

`chatshell.Model.StartStream(id string, open func(ctx context.Context)
iter.Seq2[ai.Event, error]) tea.Cmd` wires this into the transcript: text
deltas append progressively to the streaming entry, and a spinner runs until
the stream produces its first delta or completes.

The **Model owns the per-stream context**: it creates a cancellable child of
its own context (`WithContext`) and passes it to `open`, which must build
the actual provider call from it —
`open := func(ctx context.Context) iter.Seq2[ai.Event, error] { return
provider.Stream(ctx, req) }` — so a cancellation reaches the live request,
not just chatshell's local pump: an adapter observing `ctx.Done()` aborts
its own call and yields `ai.ErrCodeCanceled`. The context is cancelled
automatically when a new `StartStream` call supersedes this one, or by
`Esc`/`Ctrl+C` while busy; the cancellation renders as a `(stopped)`
transcript entry rather than an error (`chatshell.isCanceled` recognises
both `ai.ErrCodeCanceled` and tui/stream's own `context.Canceled` ctx-race).

An optional `chatshell.StreamObserver` on the product's `Handler`:

```go
type StreamObserver interface {
	OnStreamEvent(id string, ev ai.Event) tea.Cmd
	OnStreamDone(id string, err error) tea.Cmd
}
```

`OnStreamEvent` receives every event (Started/TextDelta/Structured/Usage/
Completed/Error) in addition to chatshell's own built-in handling — e.g. to
track usage or diagnostics. `OnStreamDone` fires exactly once per
`StartStream` call, whatever the outcome (success, a fatal error, or a
cancellation) — including for a stream superseded by a later `StartStream`
before it finished — so a product can roll back speculative state or record
diagnostics for that stream `id`.

`Model.SetBusy(bool) tea.Cmd` marks a product-driven pre-stream phase (a
decision chain, a deterministic query) busy the same way as a stream:
composer disabled, spinner running. `Model.SetBusyCancel(cancel func())`
registers that phase's own cancel func (e.g. a `context.CancelFunc`);
`Esc`/`Ctrl+C` while busy and no stream is active call it directly and
render `(stopped)` synchronously (unlike a stream, there is no `DoneMsg` to
report it asynchronously). A **second, immediately-following** `Ctrl+C`
always quits, whether or not the first press's cancellation has finished;
any other key re-arms it back to "cancel on the next Ctrl+C".

**A `SetBusy(true)` phase has no identity of its own** — unlike a stream,
which `StartStream` keys by `id` and whose `DoneMsg` always names that `id`
back, `SetBusy`/`SetBusyCancel` carry no per-call token at all. chatshell
itself doesn't need one: cancelling just calls whatever func is currently
registered and clears busy. But a PRODUCT whose own async work outlives a
cancelled phase (e.g. a decision-chain goroutine that keeps running briefly
after `SetBusyCancel`'s func fires, or a query that was already in flight)
gets no signal from chatshell distinguishing "this result belongs to the
phase that's still current" from "this result belongs to a phase the user
already cancelled". A product with that shape must track its own identity
across a `SetBusy(true)`/cancel/`SetBusy(false)` cycle — e.g. increment a
local phase token when starting a phase and again when cancelling it, and
have the async work's completion handler compare its captured token against
the model's current one before acting on the result (exactly the pattern
`StartStream`'s `id` gives for free on the streaming path). `SetBusy(false)`
itself only clears `busy` and forgets the registered `SetBusyCancel` func —
it does not, and cannot, retroactively invalidate a result already in
flight from a phase that was never explicitly cancelled; that's on the
product's own token check, same as the cancelled case.

An optional `chatshell.MsgHandler` (`OnMsg(msg tea.Msg) tea.Cmd`) receives
every message chatshell does not itself recognise — e.g. a product message,
or a Block message such as `grid.RowActivatedMsg` — in addition to that
message being broadcast to the transcript's Blocks (see `transcript.Targeted`
for routing a message to one entry by ID instead of every Block).

## Mouse (`tui/chatshell`)

Mouse reporting is OFF by default (a terminal's own native text
selection/copy keeps working). A product opts in with
`chatshell.WithMouse(chatshell.MouseCellMotion)` at construction, or toggles
it at runtime with `(m *Model) SetMouseEnabled(bool)` / `MouseEnabled()
bool` — e.g. DataTug's `F2` capture toggle, since native text selection is
unusable while mouse reporting is on, so a screen offering both needs a key
to flip between them. Once enabled, the mouse wheel scrolls the transcript
viewport (`tea.MouseWheelUp`/`tea.MouseWheelDown`); every other mouse event
(click, release, motion) is still delivered to chatshell's `Update` but has
no built-in effect. While an `Overlay` is on the stack, a wheel event is
captured by the overlay like any other input and never reaches the
transcript.

## Extension points (`tui/chatshell`)

- **`WithSidePanel(p SidePanel)`** replaces the default sidebar END TO END:
  `F6` visibility, `Ctrl+Left`/`Ctrl+Right` split-percent resizing (still
  clamped 40–75%), `Shift+Right`/`Shift+Left` focus-ring participation and
  rendering all route through `p` instead of `tui/sidebar` once set. A
  `SidePanel` is `Title() string`, `View(width, height int, focused bool)
  string`, `Update(msg tea.Msg) (SidePanel, tea.Cmd)` — e.g. a DataTug
  workspace pane with its own tabs, explorer and bookmarks.
- **`PushOverlay(o Overlay) tea.Cmd`** pushes a modal dialog. The TOP overlay
  on the stack captures every message chatshell would otherwise handle
  itself — including keys that would submit the composer — until its
  `Update` returns `done: true`, and renders centred over the screen. An
  `Overlay` is `View(width, height int) string`, `Update(msg tea.Msg) (o
  Overlay, cmd tea.Cmd, done bool)`.
- **`WithGlobalKeys(func(tea.KeyPressMsg) (tea.Cmd, bool))`** is checked
  BEFORE chatshell's own key handling (so a product can claim `F3`/`F4`
  pickers or any other key ahead of chatshell's defaults); returning
  `consumed: true` stops chatshell handling that key at all this cycle.
- **`WithTopBar(func(width int) string)`** / **`WithStatusBar(func(width
  int) string)`** replace the default bold title line / `SetStatus`-driven
  status line(s) with product-rendered content.
- **`Model.ReplaceBlock(entryID string, b transcript.Block)`** swaps a
  transcript entry's `Block` in place (same position, same ID) — e.g. to
  refresh or re-run a grid. **`SetComposerText(s string)`** sets the
  composer text and moves the cursor to the end (an edit-previous-message
  flow). **`ClearTranscript()`** empties the transcript and returns focus to
  the composer (`/clear`, a session switch).
