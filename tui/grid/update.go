package grid

import (
	tea "charm.land/bubbletea/v2"

	"github.com/strongo/aichat/tui"
	"github.com/strongo/aichat/tui/transcript"
)

// Update implements transcript.Block. Key handling order: the filter input
// (while focused) always wins; then, ONLY while secondary focus holds the
// active non-table view (see SecondaryFocus/SetSecondaryFocus — always true
// for a non-split view, since the table isn't reachable there), that
// ExtraView's own Update; then the product KeyHandler (see WithKeyHandler),
// which can claim any key including Enter, digits or Tab; then the grid's
// own defaults: h/l select a column (scrolling it into view), up/k and down
// move the table row WHILE THE TABLE HAS FOCUS (a split layout's primary
// pane, or the plain table view), digit keys switch views (1 is always the
// table), Tab toggles focus between the table and a split secondary view, s
// sorts by the selected column, Enter emits RowActivatedMsg, + emits
// tui.AddToSidebarMsg, / opens the built-in filter.
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
	if m.secondaryFocus {
		if i := int(m.view) - firstExtraView; i >= 0 && i < len(m.extraViews) && m.extraViews[i].Update != nil {
			if cmd, handled := m.extraViews[i].Update(m, keyMsg); handled {
				return m, cmd
			}
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
		// Gated on !secondaryFocus rather than m.view == ViewTable: a split
		// layout's primary (table) pane keeps its own row cursor reachable
		// even while a non-table view occupies the secondary pane, as long
		// as secondary focus hasn't been Tab'd onto that view.
		//
		// This moves by POSITION within the table's own visible/filtered
		// row set (GetHighlightedRowIndex/WithHighlightedRow), not by
		// CurrentIndex()'s source-row arithmetic: under an active filter,
		// adjacent visible rows are not adjacent source rows, so "current
		// source index minus one" can land on a filtered-out row (or the
		// wrong visible one) instead of the previous visible row.
		if !m.secondaryFocus {
			if pos := m.table.GetHighlightedRowIndex(); pos > 0 {
				m.table = m.table.WithHighlightedRow(pos - 1)
			}
			return m, nil
		}
	case "down", "j":
		if !m.secondaryFocus {
			if pos, n := m.table.GetHighlightedRowIndex(), len(m.table.GetVisibleRows()); pos >= 0 && pos+1 < n {
				m.table = m.table.WithHighlightedRow(pos + 1)
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
	if m.secondaryFocus {
		// A registered ExtraView with no Update of its own does not consume
		// navigation keys while it holds secondary focus.
		return m, nil
	}
	// The table has focus — the plain table view, or a split layout's
	// primary pane while secondary focus hasn't been Tab'd onto the active
	// non-table view — so any bubble-table key not already handled above
	// (e.g. pgup/pgdown) still reaches it.
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}
