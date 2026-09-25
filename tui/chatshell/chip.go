package chatshell

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/strongo/aichat/ai/session"
	"github.com/strongo/aichat/tui/theme"
)

// Chip is a product-neutral attachment shown as a removable pill above the
// composer input -- e.g. a table, file, or other entity the user has staged
// as context for the next turn. Ported from DataTug's own
// ContextReference-backed attachment chips (datatug-cli#291): Ref carries
// the product's own entity identity (nil when a chip has none, e.g. a
// free-form label), Label is what renders in the pill, and ID is the
// product's stable identifier for the chip itself -- used only to let a
// product address a specific chip (SetChips) across renders; chatshell
// never interprets it.
type Chip struct {
	ID    string
	Label string
	Ref   *session.EntityRef
}

// ChipObserver is an optional Handler capability: when implemented,
// chatshell calls OnChipsChange after every chip-list change IT ITSELF
// performs -- a chip removed via Backspace/Delete/mouse click, or a
// Shift+Esc restore -- so a product can keep its own attachment/context
// state (e.g. a session's working set) in sync. It is deliberately NOT
// called from WithChips or SetChips: those calls already come FROM the
// product, so echoing them back would be redundant, and -- because a
// product's own SetChips call in response to an earlier OnChipsChange lands
// as a separate, later Update (OnChipsChange returns a tea.Cmd, not a
// synchronous call) -- calling it there too could race a product's SetChips
// echo against a still-pending Shift+Esc undo. See SetChips's doc.
type ChipObserver interface {
	OnChipsChange(chips []Chip) tea.Cmd
}

// WithChips sets the composer's initial attachment chips (see SetChips).
func WithChips(chips []Chip) Option {
	return func(m *Model) { m.chips = append([]Chip(nil), chips...) }
}

// SetChips replaces the composer's attachment chips wholesale, e.g. when the
// product attaches a new entity from its own workspace/sidebar UI. The
// focused chip index is preserved when it still falls within the new list,
// and reset to "no chip focused" (returning keyboard focus to the input)
// when it doesn't -- e.g. the list shrank.
//
// SetChips does NOT clear a pending Shift+Esc/Ctrl+Y undo snapshot (see
// snapshotComposerUndo): the snapshot exists to let the user recover from a
// change THEY just made, and a product-driven SetChips call (adding or
// syncing chips for an unrelated reason) shouldn't silently discard that
// recovery option out from under them. When a restore does eventually
// happen, any chip present now that wasn't part of the snapshot is kept,
// not discarded -- see mergeRestoredChips.
func (m *Model) SetChips(chips []Chip) {
	m.chips = append([]Chip(nil), chips...)
	m.clampChipFocus()
	m.resize()
}

// Chips returns a defensive copy of the composer's current attachment
// chips.
func (m *Model) Chips() []Chip {
	return append([]Chip(nil), m.chips...)
}

// RemoveChip removes the chip with the given ID -- a product-facing
// equivalent of Backspace/Delete on a focused chip or a mouse click on its
// ×, e.g. a "remove attachment" control the product renders elsewhere in
// its own UI. It snapshots the pre-removal draft first, same as any other
// removal (so Shift+Esc/Ctrl+Y can undo it), and reports (r2 review, m3)
// via the second return value whether a chip with that ID was found and
// removed; found is false (cmd is nil) for an unknown ID, a documented
// no-op.
func (m *Model) RemoveChip(id string) (cmd tea.Cmd, found bool) {
	for i, c := range m.chips {
		if c.ID == id {
			return m.removeChipAt(i), true
		}
	}
	return nil, false
}

// ClearChips detaches every chip -- a product-facing equivalent of Esc's
// second step (clearComposerStep) -- e.g. a toolbar "clear attachments"
// button. It snapshots the pre-clear draft first (so Shift+Esc/Ctrl+Y can
// undo it) and is a no-op (returns nil) when there are no chips to clear.
func (m *Model) ClearChips() tea.Cmd {
	if len(m.chips) == 0 {
		return nil
	}
	m.snapshotComposerUndo()
	return m.doClearChips()
}

// doClearChips empties the chip list WITHOUT snapshotting -- callers
// (clearComposerStep, ClearChips) snapshot first themselves, since one
// (Esc's second step) only wants to snapshot when it's actually about to
// act, and the other (ClearChips) already checked len(m.chips) > 0 before
// calling this.
func (m *Model) doClearChips() tea.Cmd {
	m.chips = nil
	m.chipFocus = -1
	m.input.Focus()
	m.resize()
	return m.notifyChipsChange()
}

// clampChipFocus resets chipFocus to "no chip focused" (and returns
// keyboard focus to the input) when it no longer indexes a live chip --
// shared by SetChips, removeChipAt and restoreComposerDraft.
func (m *Model) clampChipFocus() {
	if m.chipFocus >= len(m.chips) {
		m.chipFocus = -1
		m.input.Focus()
	}
}

// notifyChipsChange calls the Handler's ChipObserver.OnChipsChange, if
// implemented, with the current chip list.
func (m *Model) notifyChipsChange() tea.Cmd {
	if obs, ok := m.handler.(ChipObserver); ok {
		return obs.OnChipsChange(m.Chips())
	}
	return nil
}

// composerDraft is a snapshot of the composer's text and chip list, taken
// immediately before the first change (of either) since the last
// successful Shift+Esc/Ctrl+Y restore, message submit, or composer text
// edit -- see snapshotComposerUndo and restoreComposerDraft.
type composerDraft struct {
	text  string
	chips []Chip
}

// snapshotComposerUndo remembers the composer's text and chip list as they
// stood before the FIRST change since the last successful restore/submit/
// text-edit (mirrors DataTug's rememberComposerDraft): a run of several
// changes (e.g. several chip removals) with nothing resetting the snapshot
// in between is undone as ONE unit by Shift+Esc/Ctrl+Y, not one change at a
// time. A no-op once a snapshot is already pending.
func (m *Model) snapshotComposerUndo() {
	if m.composerUndo == nil {
		m.composerUndo = &composerDraft{
			text:  m.input.Value(),
			chips: append([]Chip(nil), m.chips...),
		}
	}
}

// removeChipAt removes the chip at index (a no-op, returning nil, for an
// out-of-range index), snapshotting the pre-removal draft first, and
// returns the ChipObserver notification command.
func (m *Model) removeChipAt(index int) tea.Cmd {
	if index < 0 || index >= len(m.chips) {
		return nil
	}
	m.snapshotComposerUndo()
	next := make([]Chip, 0, len(m.chips)-1)
	next = append(next, m.chips[:index]...)
	m.chips = append(next, m.chips[index+1:]...)
	m.clampChipFocus()
	m.resize()
	return m.notifyChipsChange()
}

// clearComposerStep implements Esc's two-step clear in the input zone
// (DataTug's own clearComposerStep, generalised to text+chips): the FIRST
// Esc, while there's text, clears the composer TEXT ONLY -- even while a
// chip is focused, since chip focus and "there's text to clear" are
// independent; the SECOND Esc, once the text is already empty, detaches
// EVERY chip. Either step snapshots the pre-clear draft first (so
// Shift+Esc/Ctrl+Y can undo it). Reports cleared=false (nothing to do, so
// the caller should fall through to Esc's normal focus-ring behaviour) when
// the input is already empty and there are no chips.
func (m *Model) clearComposerStep() (tea.Cmd, bool) {
	if m.input.Value() == "" && len(m.chips) == 0 {
		return nil, false
	}
	m.snapshotComposerUndo()
	if m.input.Value() != "" {
		m.input.Reset()
		m.commandMenuIndex = 0
		m.commandMenuDismissed = ""
		return nil, true
	}
	return m.doClearChips(), true
}

// restoreComposerDraft implements Shift+Esc/Ctrl+Y: restores the composer
// text exactly as snapshotted, and the chip list as the snapshot MERGED
// with any chip added since (matched by ID -- see mergeRestoredChips), so a
// chip the product attached AFTER the snapshot was taken isn't silently
// discarded by an unrelated undo. Reports ok=false (a no-op) when there is
// nothing to restore.
func (m *Model) restoreComposerDraft() (tea.Cmd, bool) {
	if m.composerUndo == nil {
		return nil, false
	}
	draft := m.composerUndo
	m.composerUndo = nil
	m.input.SetValue(draft.text)
	m.input.CursorEnd()
	m.chips = mergeRestoredChips(draft.chips, m.chips)
	m.chipFocus = -1
	m.input.Focus()
	m.resize()
	return m.notifyChipsChange(), true
}

// chipKey identifies a chip for mergeRestoredChips's union: its ID when it
// has one, or its Label when it doesn't (a chip with no ID has no other
// stable identity chatshell knows about, so two chips sharing the same
// empty ID are only ever treated as "the same chip" when their Label also
// matches).
func chipKey(c Chip) string {
	if c.ID != "" {
		return "id\x00" + c.ID
	}
	return "lbl\x00" + c.Label
}

// mergeRestoredChips implements restoreComposerDraft's chip merge (m2, r1
// review): the result is the snapshot's own chips, in their original
// order, UNION any chip in current that ISN'T already represented in the
// snapshot (by chipKey) -- appended after the snapshot's chips, in
// current's own order. A chip removed since the snapshot was taken (it's
// in snapshot but not in current) is still restored, since the whole point
// of Shift+Esc/Ctrl+Y is to bring it back; a chip added since (it's in
// current but not in snapshot) is kept rather than silently dropped by an
// undo that was never about it.
func mergeRestoredChips(snapshot, current []Chip) []Chip {
	seen := make(map[string]bool, len(snapshot)+len(current))
	merged := make([]Chip, 0, len(snapshot)+len(current))
	for _, c := range snapshot {
		merged = append(merged, c)
		seen[chipKey(c)] = true
	}
	for _, c := range current {
		key := chipKey(c)
		if !seen[key] {
			merged = append(merged, c)
			seen[key] = true
		}
	}
	return merged
}

// cycleChipFocus implements Tab (forward: true) / Shift+Tab (forward:
// false): it cycles chipFocus through every chip index plus one extra "no
// chip focused" state, matching DataTug's attachmentFocus cycle
// ((focus+1)%(count+1), wrapping): Tab from the input focuses the first
// chip; Tab from the last chip returns focus to the input; Shift+Tab is the
// same cycle in reverse. It blurs/focuses the composer's textarea to match,
// since chip focus and input focus are mutually exclusive. A no-op when
// there are no chips.
func (m *Model) cycleChipFocus(forward bool) {
	count := len(m.chips)
	if count == 0 {
		return
	}
	if forward {
		m.chipFocus = (m.chipFocus + 1) % (count + 1)
	} else {
		m.chipFocus = (m.chipFocus + count) % (count + 1)
	}
	if m.chipFocus == count {
		m.chipFocus = -1
		m.input.Focus()
	} else {
		m.input.Blur()
	}
}

// chipCell is one rendered chip pill within a wrapped row: index into
// m.chips, the pill's rendered text (including its "×" close glyph), and x,
// the column (within the row's own width) where a mouse click removes it --
// the position of the "×" glyph itself.
type chipCell struct {
	index int
	text  string
	x     int
}

// chipCloseGlyph is appended, space-separated, to a chip's (possibly
// truncated) label to form its pill text, e.g. "[Customer ×]".
const chipCloseGlyph = "×"

// chipRows lays out the current chips into rows that wrap at width (one
// space between adjacent pills on the same row; a pill that would overflow
// starts a new row instead), mirroring DataTug's attachmentRows. A single
// chip wider than width still gets its own row (truncated, close glyph
// preserved) rather than being dropped. Returns nil when there are no
// chips.
func (m *Model) chipRows(width int) [][]chipCell {
	if len(m.chips) == 0 {
		return nil
	}
	available := max(1, width)
	rows := [][]chipCell{{}}
	used := 0
	for i, c := range m.chips {
		label := ansi.Truncate(c.Label, max(0, available-4), "")
		text := "[" + label + " " + chipCloseGlyph + "]"
		w := ansi.StringWidth(text)
		gap := 0
		if used > 0 {
			gap = 1
		}
		if used+gap+w > available && used > 0 {
			rows = append(rows, []chipCell{})
			used, gap = 0, 0
		}
		used += gap
		rows[len(rows)-1] = append(rows[len(rows)-1], chipCell{
			index: i,
			text:  text,
			x:     used + ansi.StringWidth(label) + 2,
		})
		used += w
	}
	return rows
}

// chipsHeight is the number of rows chipRows(width) lays out -- how many
// extra lines the chip row(s) take above the input, so historyHeight can
// shrink to make room and grow back as chips are removed.
func (m *Model) chipsHeight(width int) int {
	return len(m.chipRows(width))
}

// chipStyle/chipFocusedStyle render a chip pill's text: muted normal,
// FocusSurfaceColors-highlighted when focused (Tab-cycled or about to be
// removed) — the SAME accent every other focused/selected element uses
// (founder 2026-09-25). Functions, not package vars: theme.Dark can change
// at runtime (theme.SetDark), and a memoised colour would keep rendering
// the stale variant forever after.
func chipStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(theme.MutedColor())
}
func chipFocusedStyle() lipgloss.Style {
	bg, fg := theme.FocusSurfaceColors()
	return lipgloss.NewStyle().Bold(true).Foreground(fg).Background(bg)
}

// chipsView renders the current chips as one or more wrapped rows, joined
// with a blank line above nothing (each row is newline-joined; a space
// separates pills on the same row). Returns "" when there are no chips.
func (m *Model) chipsView(width int) string {
	rows := m.chipRows(width)
	if len(rows) == 0 {
		return ""
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		parts := make([]string, 0, len(row))
		for _, cell := range row {
			style := chipStyle()
			if cell.index == m.chipFocus {
				style = chipFocusedStyle()
			}
			parts = append(parts, style.Render(cell.text))
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	return strings.Join(lines, "\n")
}

// chipsTopY returns the Y coordinate (chatshell's own top-left-origin
// coordinate frame, matching tea.Mouse's) of the first chip row, so
// handleMouseClick can translate a click's Y into a row index: the
// rendered top bar's height, plus the transcript's fixed viewport height
// (historyHeight), plus the slash-command menu's height when it's
// currently showing -- exactly the content View() stacks above the chip
// row(s), in order (see historyHeight's own doc for why each of these is
// measured rather than assumed).
func (m *Model) chipsTopY() int {
	return m.topBarHeight() + m.historyHeight() + m.menuHeight()
}

// handleMouseClick handles a tea.MouseClickMsg: a click on a chip's "×"
// glyph removes that chip (see chipCloseClick); anything else takes
// chatshell's normal unhandled-message path (dispatchUnhandled), same as
// before this case existed (mouse clicks had no chatshell-specific
// handling and fell to the default branch of Update's switch).
func (m *Model) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if cmd, ok := m.chipCloseClick(msg); ok {
		return m, cmd
	}
	return m, m.dispatchUnhandled(msg)
}

// chipCloseClick reports whether msg is a left-button click landing exactly
// on a chip's "×" glyph and, if so, removes that chip and returns the
// resulting ChipObserver notification command. It reports ok=false (no
// removal) while mouse reporting is off, the shell is busy (the composer,
// chips included, is disabled while busy -- see handleInputKey), there are
// no chips, msg isn't a left-button click, or msg's coordinates don't land
// on any chip's close glyph.
func (m *Model) chipCloseClick(msg tea.MouseClickMsg) (tea.Cmd, bool) {
	if !m.mouseEnabled || m.busy || len(m.chips) == 0 || msg.Button != tea.MouseLeft {
		return nil, false
	}
	rows := m.chipRows(m.chatWidth())
	row := msg.Y - m.chipsTopY()
	if row < 0 || row >= len(rows) {
		return nil, false
	}
	for _, cell := range rows[row] {
		if msg.X == cell.x {
			return m.removeChipAt(cell.index), true
		}
	}
	return nil, false
}
