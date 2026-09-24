package grid

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// View implements transcript.Block: a bordered card (title + view switcher,
// content, scrollbar down the right edge, a stats footer in the bottom
// border) around the active view's body — the table by default, or a
// registered ExtraView, optionally split side by side with the table per
// WithSplitLayout. Ported from DataTug's
// gridState.viewWithTitle/recordsetView/recordsetHeader.
func (m *Model) View(width int, focused bool) string {
	if width != m.width || focused != m.focused {
		m.width, m.focused = max(1, width), focused
		m.rebuildTable()
		m.syncSecondaryFocusForLayout()
	}
	// card prepends a 2-cell focus bullet ("● "/"○ ") to the header label
	// before laying it into the border, so the label itself must be built 2
	// cells narrower than the card or the bullet pushes the rightmost view
	// control (e.g. "3") out of the border and it gets clipped.
	return m.card(m.headerLine(max(1, m.width-2)), m.body(m.width))
}

// headerLine is the card's title bar content: the title plus the view
// switcher (numbered labels, the active one highlighted), responsively
// abbreviated as width shrinks. Ported from DataTug's
// gridState.recordsetHeader, generalised over an arbitrary list of views
// instead of the fixed Table/Charts/Current row/Raw/Headers set.
func (m *Model) headerLine(width int) string {
	labels := m.viewLabels(width)
	separator := " · "
	if width < 42 {
		separator = " "
	}
	controls := strings.Join(labels, separator)
	styled := make([]string, len(labels))
	for i, label := range labels {
		style := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
		if View(i) == m.view {
			style = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231"))
		}
		styled[i] = style.Render(label)
	}
	styledControls := strings.Join(styled, separator)
	available := max(1, width-6)
	if ansi.StringWidth(controls) >= available {
		return padAnsiLine(styledControls, width)
	}
	titleWidth := max(1, available-ansi.StringWidth(controls)-3)
	title := ansi.Truncate(sanitize(m.title), titleWidth, "…")
	if m.focused {
		title = activeTitleStyle.Render(title)
	} else {
		title = inactiveTitleStyle.Render(title)
	}
	return padAnsiLine(title+" │ "+styledControls, width)
}

// HeaderLine returns the card's title bar content at a given width — the
// same text embedded in the top border by View — without rendering the
// whole card.
func (m *Model) HeaderLine(width int) string { return m.headerLine(width) }

// viewLabels returns one label per view (Table, then each registered
// ExtraView in order), abbreviated to fit width. Ported from DataTug's
// recordsetHeader breakpoints (<62, <42, 24-36, <=24 cells), generalised: a
// fixed word-count abbreviation replaces DataTug's hand-picked short forms
// ("Charts" → "Chart" → "C"), since a product-registered ExtraView label is
// arbitrary text, not one of a fixed known set.
func (m *Model) viewLabels(width int) []string {
	names := make([]string, 0, 1+len(m.extraViews))
	names = append(names, "Table")
	for _, v := range m.extraViews {
		names = append(names, v.Label)
	}
	shorten := func(n int) []string {
		out := make([]string, len(names))
		for i, name := range names {
			out[i] = strconv.Itoa(i+1) + " " + abbreviate(name, n)
		}
		return out
	}
	switch {
	case width <= 24:
		out := make([]string, len(names))
		for i := range names {
			out[i] = strconv.Itoa(i + 1)
		}
		return out
	case width > 24 && width < 36:
		return shorten(1)
	case width < 42:
		return shorten(4)
	case width < 62:
		return shorten(6)
	default:
		return shorten(len(strings.Join(names, "")) + 1) // no truncation
	}
}

func abbreviate(name string, n int) string {
	r := []rune(name)
	if len(r) <= n {
		return name
	}
	return string(r[:n])
}

// body renders the active view's content, applying the split-pane layout
// (see WithSplitLayout) when one is registered and the active view isn't the
// table itself. Ported from DataTug's gridState.recordsetView.
func (m *Model) body(width int) string {
	if m.view == ViewTable || m.layout == nil {
		return m.viewBody(m.view, width, m.paneHeight())
	}
	layout := m.layout(width, m.NaturalWidth(), m.view)
	if !layout.Split {
		return m.viewBody(m.view, width, m.paneHeight())
	}
	primary := m.tableViewAt(layout.PrimaryWidth)
	secondary := m.viewBody(m.view, layout.SecondaryWidth, m.paneHeight())
	return lipgloss.JoinHorizontal(lipgloss.Top, primary, secondary)
}

// paneHeight bounds a non-table view's rendered height (see the height clamp
// in card), so a view with many lines (e.g. CardView over a wide row) never
// renders taller than the table itself would. Ported from DataTug's fixed
// recordsetPaneHeight = maxGridHeight.
func (m *Model) paneHeight() int {
	if m.maxVisibleRows > 0 {
		return m.maxVisibleRows
	}
	return DefaultMaxVisibleRows
}

// tableViewAt renders the bare table at a specific width without disturbing
// the Model's own width (used for the primary pane of a split layout).
func (m *Model) tableViewAt(width int) string {
	previousWidth := m.width
	if width != m.width {
		m.width = max(1, width)
		m.rebuildTable()
	}
	view := m.table.View()
	if width != previousWidth {
		m.width = previousWidth
		m.rebuildTable()
	}
	return view
}

// ActiveViewContent renders just the active view's own body (table or the
// active ExtraView) at the given size, without any card chrome and without
// the OTHER pane a split layout would show alongside it. A product's test
// wants this instead of the full View() output whenever a split layout could
// put the table's own header/cells within reach of a substring match aimed
// only at the secondary view (e.g. asserting a CardView's scroll position by
// checking which fields are currently rendered).
func (m *Model) ActiveViewContent(width, height int) string {
	return m.viewBody(m.view, width, height)
}

func (m *Model) viewBody(view View, width, height int) string {
	if view == ViewTable {
		return m.table.View()
	}
	if i := int(view) - firstExtraView; i >= 0 && i < len(m.extraViews) {
		return m.extraViews[i].Render(m, width, height)
	}
	return m.table.View()
}

// card wraps content in a bordered card with a title bar and a stats footer
// in the bottom border, with a scrollbar down the right edge. Ported from
// DataTug's gridState.viewWithTitle.
func (m *Model) card(label, content string) string {
	cardWidth := max(1, m.width)
	innerWidth := m.tableWidth()
	rawLines := strings.Split(content, "\n")
	if m.view == ViewTable {
		// bubble-table always emits a header/data separator with its outer
		// border disabled. The card's own title bar already distinguishes
		// the two regions.
		if len(rawLines) > 1 {
			rawLines = append(rawLines[:1], rawLines[2:]...)
		}
	}
	if content == "" {
		rawLines = []string{""}
	}
	if m.view != ViewTable {
		// A non-table view (e.g. CardView over a wide row) is height-bound
		// to paneHeight, same as the table itself; a taller ExtraView owns
		// its own scrolling (see CardView/InspectorView's Update).
		height := m.paneHeight()
		if len(rawLines) > height {
			rawLines = rawLines[:height]
		}
		for len(rawLines) < height {
			rawLines = append(rawLines, "")
		}
	}
	lines := make([]string, 0, len(rawLines)+2)
	title := label
	if m.focused {
		title = activeTitleStyle.Render("● ") + title
	} else {
		title = inactiveTitleStyle.Render("○ ") + title
	}
	topBorder := borderLine("╭", title, "╮", cardWidth)
	if m.focused {
		topBorder = selectedOutlineStyle.Render(topBorder)
	} else {
		topBorder = inactiveBorderStyle.Render(topBorder)
	}
	lines = append(lines, padAnsiLine(topBorder, cardWidth))
	for lineIndex, line := range rawLines {
		scrollbar := m.scrollbarLine(lineIndex, len(rawLines))
		border := inactiveBorderStyle
		if m.focused {
			border = activeBorderStyle
		}
		lines = append(lines, padAnsiLine(border.Render("│")+padAnsiLine(line, innerWidth)+scrollbar, cardWidth))
	}
	footerLabel := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252")).Render(m.footer())
	bottomBorder := borderLine("╰", footerLabel, "╯", cardWidth)
	if m.focused {
		bottomBorder = selectedOutlineStyle.Render(bottomBorder)
	} else {
		bottomBorder = inactiveBorderStyle.Render(bottomBorder)
	}
	lines = append(lines, padAnsiLine(bottomBorder, cardWidth))
	return strings.Join(lines, "\n")
}

// footer mirrors DataTug's gridState.footer(): current row/column range and
// sort indicator, plus any product FooterHook text.
func (m *Model) footer() string {
	builtin := "No rows returned."
	if len(m.rows) > 0 {
		if start, end := m.table.VisibleIndices(); end >= start {
			builtin = fmt.Sprintf("Rows %d–%d of %d returned", start+1, end+1, len(m.rows))
			if firstColumn, lastColumn := m.visibleColumnRange(); firstColumn > 0 {
				builtin += fmt.Sprintf(" • Cols %d–%d of %d", firstColumn, lastColumn, len(m.columns))
			}
			if m.sortColumn >= 0 && m.sortColumn < len(m.columns) {
				direction := "↑"
				if m.sortDesc {
					direction = "↓"
				}
				builtin += " • sort " + m.columns[m.sortColumn].Name + " " + direction
			}
		} else {
			builtin = "No rows returned."
		}
	}
	if m.footerHook != nil {
		return m.footerHook(m, builtin)
	}
	return builtin
}

// Footer returns the grid's current footer text (row/column range, sort
// indicator, plus any WithFooterHook/SetFooterHook text) — the same text
// shown in the card's bottom border.
func (m *Model) Footer() string { return m.footer() }

// scrollbarLine renders one line of the right-edge scrollbar/border. Ported
// from DataTug's gridState.scrollbarLine.
func (m *Model) scrollbarLine(line, trackHeight int) string {
	pageStart, pageEnd := m.table.VisibleIndices()
	visibleRows := max(0, pageEnd-pageStart+1)
	if trackHeight == 0 || visibleRows == 0 || len(m.rows) <= visibleRows {
		if m.focused {
			return selectedOutlineStyle.Render("│")
		}
		return inactiveBorderStyle.Render("│")
	}
	thumbSize := max(1, trackHeight*visibleRows/len(m.rows))
	maxStart := max(0, trackHeight-thumbSize)
	start := m.table.GetHighlightedRowIndex() * maxStart / max(1, len(m.rows)-1)
	if line >= start && line < start+thumbSize {
		if m.focused {
			return lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Render("▐")
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color("242")).Render("▐")
	}
	if m.focused {
		return selectedOutlineStyle.Render("│")
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("237")).Render("│")
}
