package chatshell

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/strongo/aichat/ai/session"
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
// SetChips does NOT clear a pending Shift+Esc undo snapshot (see
// snapshotChipUndo): the snapshot exists to let the user recover from a
// removal THEY just performed, and a product-driven SetChips call (adding
// or syncing chips for an unrelated reason) shouldn't silently discard that
// recovery option out from under them.
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

// clampChipFocus resets chipFocus to "no chip focused" (and returns
// keyboard focus to the input) when it no longer indexes a live chip --
// shared by SetChips, removeChip and restoreChipsCmd.
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

// snapshotChipUndo remembers the chip list as it stood before the FIRST
// removal since the last successful Shift+Esc restore or message submit
// (mirrors DataTug's rememberComposerDraft): a run of several removals with
// no restore or submit in between is undone as one unit by Shift+Esc, not
// one chip at a time.
func (m *Model) snapshotChipUndo() {
	if m.chipUndo == nil {
		snap := append([]Chip(nil), m.chips...)
		m.chipUndo = &snap
	}
}

// removeChip removes the chip at index (a no-op, returning nil, for an
// out-of-range index), snapshotting the pre-removal list for Shift+Esc
// first, and returns the ChipObserver notification command.
func (m *Model) removeChip(index int) tea.Cmd {
	if index < 0 || index >= len(m.chips) {
		return nil
	}
	m.snapshotChipUndo()
	next := make([]Chip, 0, len(m.chips)-1)
	next = append(next, m.chips[:index]...)
	m.chips = append(next, m.chips[index+1:]...)
	m.clampChipFocus()
	m.resize()
	return m.notifyChipsChange()
}

// restoreChipsCmd implements Shift+Esc: it reports ok=false (a no-op) when
// there is nothing to restore, and otherwise replaces the current chip list
// with the pre-removal snapshot, clears the snapshot, and returns the
// resulting ChipObserver notification command.
func (m *Model) restoreChipsCmd() (tea.Cmd, bool) {
	if m.chipUndo == nil {
		return nil, false
	}
	m.chips = *m.chipUndo
	m.chipUndo = nil
	m.clampChipFocus()
	m.resize()
	return m.notifyChipsChange(), true
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

// chipStyle/chipFocusedStyle render a chip pill's text: dim/blue normal,
// bold/inverted when focused (Tab-cycled or about to be removed).
var (
	chipStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("111"))
	chipFocusedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("229")).Background(lipgloss.Color("57"))
)

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
			style := chipStyle
			if cell.index == m.chipFocus {
				style = chipFocusedStyle
			}
			parts = append(parts, style.Render(cell.text))
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	return strings.Join(lines, "\n")
}

// chipsTopY returns the Y coordinate (chatshell's own top-left-origin
// coordinate frame, matching tea.Mouse's) of the first chip row, so
// handleMouseClick can translate a click's Y into a row index. It is the
// rendered top bar's height, plus the transcript's fixed viewport height
// (historyHeight), plus the slash-command menu's height when the menu is
// currently showing -- exactly the content View() stacks above the chip
// row(s), in order.
func (m *Model) chipsTopY() int {
	y := strings.Count(m.topBarView(), "\n") + 1
	y += m.historyHeight()
	if menu := m.commandMenuView(); menu != "" {
		y += strings.Count(menu, "\n") + 1
	}
	return y
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
			return m.removeChip(cell.index), true
		}
	}
	return nil, false
}
