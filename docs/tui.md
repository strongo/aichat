# `tui/*` keyboard and sidebar reference

This is a short, product-neutral reference for the Bubble Tea chat kit in
`tui/`. It documents the keybindings and sidebar model shared by every
`tui/chatshell`-based screen (`sneat chat`, `datatug chat`, ...). The
`aichat-ai` lane folds this into the spec tree at landing; until then it
lives here.

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
| `Esc` | anywhere | Return focus to Input |

`Shift+Right`/`Shift+Left` only move focus when the sidebar is visible and
the pane is split (terminal width ≥ 104 columns).

## Composer (`tui/chatshell`)

| Key | Effect |
|---|---|
| `Enter` | Submit the message (calls `Handler.Submit`); no-op while busy or empty |
| `Shift+Enter` | Insert a newline without submitting |
| `/` at start of input | Opens the slash-command menu |
| `↑`/`↓` (menu open) | Move the menu selection |
| `Enter`/`Tab` (menu open) | Insert the selected command |
| `F6` | Toggle sidebar visibility |
| `Ctrl+Left`/`Ctrl+Right` | Shrink/grow the chat pane's split share (40–75%) |

## Result grid (`tui/grid`)

| Key | Effect |
|---|---|
| `↑`/`↓`, `j`/`k` | Move the highlighted row |
| `←`/`→`, `h`/`l` | Scroll columns horizontally |
| `1`/`2`/`3` | Switch view: Table / Card / Inspector |
| `Tab` | Cycle the column `s` sorts by |
| `s` | Sort (toggle ascending/descending) by the Tab-selected column |
| `Enter` | Emit `grid.RowActivatedMsg` for the highlighted row |
| `+` | Emit `tui.AddToSidebarMsg` for the highlighted row's `Ref` |
| `/` | Open bubble-table's built-in filter |

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

## Streaming (`tui/stream`)

`stream.Start(ctx, id, seq)` pumps an `ai.LLMProvider.Stream` result into
`stream.EventMsg`/`stream.DoneMsg` without buffering the response.
`chatshell.Model.StartStream` wires this into the transcript: text deltas
append progressively to the streaming entry, and a spinner runs until the
stream produces its first delta or completes.
