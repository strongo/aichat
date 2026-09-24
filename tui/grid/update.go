package grid

import (
	tea "charm.land/bubbletea/v2"

	"github.com/strongo/aichat/tui"
	"github.com/strongo/aichat/tui/transcript"
)

// Update implements transcript.Block. Key handling order: the filter input
// (while focused) always wins; then the active ExtraView's own Update (if
// any); then the product KeyHandler (see WithKeyHandler), which can claim
// any key including Enter, digits or Tab; then the grid's own defaults:
// h/l select a column (scrolling it into view), up/k and down move the row,
// digit keys switch views (1 is always the table), Tab toggles focus between
// the table and a split secondary view, s sorts by the selected column,
// Enter emits RowActivatedMsg, + emits tui.AddToSidebarMsg, / opens the
// built-in filter.
func (m *Model) Update(msg tea.Msg) (transcript.Block, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	if m.table.GetIsFilterInputFocused() {
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		return m, cmd
	}
	if i := int(m.view) - firstExtraView; i >= 0 && i < len(m.extraViews) && m.extraViews[i].Update != nil {
		if cmd, handled := m.extraViews[i].Update(m, keyMsg); handled {
			return m, cmd
		}
	}
	if m.keyHandler != nil {
		if cmd, handled := m.keyHandler(m, keyMsg); handled {
			return m, cmd
		}
	}
	switch keyMsg.String() {
	case "left", "h":
		if m.selectedColumn > 0 {
			m.SelectColumn(m.selectedColumn - 1)
		}
		return m, nil
	case "right", "l":
		if m.selectedColumn+1 < len(m.columns) {
			m.SelectColumn(m.selectedColumn + 1)
		}
		return m, nil
	case "up", "k":
		// bubble-table's own RowUp always wraps to the last row; the grid
		// itself does not, matching h/l's clamped column navigation above.
		if m.view == ViewTable {
			if i := m.CurrentIndex(); i > 0 {
				m.SelectRow(i - 1)
			}
			return m, nil
		}
	case "down":
		if m.view == ViewTable {
			if i := m.CurrentIndex(); i >= 0 && i+1 < len(m.rows) {
				m.SelectRow(i + 1)
			}
			return m, nil
		}
	case "1":
		m.SetView(ViewTable)
		return m, nil
	case "tab":
		m.ToggleSecondaryFocusIfSplit()
		return m, nil
	case "enter":
		if row, ok := m.CurrentRow(); ok {
			return m, func() tea.Msg { return RowActivatedMsg{Row: row} }
		}
		return m, nil
	case "+":
		if ref := m.Current(); ref != nil {
			return m, func() tea.Msg { return tui.AddToSidebarMsg{Ref: *ref} }
		}
		return m, nil
	case "s":
		m.Sort(m.selectedColumn)
		return m, nil
	}
	if n := extraViewKeyIndex(keyMsg.String()); n >= 0 && n < len(m.extraViews) {
		m.SetView(View(firstExtraView + n))
		return m, nil
	}
	if m.view != ViewTable && m.secondaryFocus {
		// A registered ExtraView with no Update of its own does not consume
		// navigation keys while it holds secondary focus.
		return m, nil
	}
	if m.view == ViewTable {
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		return m, cmd
	}
	return m, nil
}
