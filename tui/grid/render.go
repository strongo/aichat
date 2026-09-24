package grid

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/evertras/bubble-table/table"
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
	if m.viewSwitcherHidden {
		// A minimal grid (dock/bookmark/parameter-lookup — see
		// WithoutViewSwitcher) never had a "1 Table" switcher to show;
		// main's own header there is title-only.
		title := ansi.Truncate(sanitize(m.title), max(1, width), "…")
		if m.focused {
			title = activeTitleStyle.Render(title)
		} else {
			title = inactiveTitleStyle.Render(title)
		}
		return padAnsiLine(title, width)
	}
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
// recordsetHeader breakpoints (<62, <42, 24-36, <=24 cells). A view's own
// ShortLabel (e.g. DataTug's Charts → "C", CardView's built-in "Row") is
// used verbatim in the shortened tiers when set — main's own hand-picked
// short forms — falling back to a generic N-character truncation of Label
// only when a view didn't provide one (a product-registered ExtraView
// label is otherwise arbitrary text, not one of a fixed known set).
func (m *Model) viewLabels(width int) []string {
	names := make([]string, 0, 1+len(m.extraViews))
	shortNames := make([]string, 0, 1+len(m.extraViews))
	names = append(names, "Table")
	shortNames = append(shortNames, "")
	for _, v := range m.extraViews {
		names = append(names, v.Label)
		shortNames = append(shortNames, v.ShortLabel)
	}
	shorten := func(n int) []string {
		out := make([]string, len(names))
		for i, name := range names {
			short := shortNames[i]
			if short == "" {
				short = abbreviate(name, n)
			}
			out[i] = strconv.Itoa(i+1) + " " + short
		}
		return out
	}
	full := func() []string {
		out := make([]string, len(names))
		for i, name := range names {
			out[i] = strconv.Itoa(i+1) + " " + name
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
		// Plenty of room: show each view's own full Label, never
		// ShortLabel — that's reserved for the narrower tiers above where
		// abbreviate()'s generic truncation would otherwise risk two
		// labels reading the same.
		return full()
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
//
// A non-table view's own body is always clamped/padded to exactly
// paneHeight() lines here (padOrClampHeight) — not left to card() as
// before — because a split layout's primary pane (the table, stripped of
// its header/data separator via stripHeaderSeparator) is one line TALLER
// than that (its own header line plus paneHeight() data rows), and
// card()'s old single view-independent height clamp (used for every
// non-table view, split or not) truncated that taller combined block down
// to paneHeight() total lines — silently clipping the table's own
// bottommost data row(s), including the highlighted one (B1).
func (m *Model) body(width int) string {
	if m.view == ViewTable {
		return m.viewBody(m.view, width, m.paneHeight())
	}
	if m.layout == nil {
		return m.padOrClampHeight(m.viewBody(m.view, width, m.paneHeight()), m.paneHeight())
	}
	layout := m.layout(width, m.NaturalWidth(), m.view)
	if !layout.Split {
		return m.padOrClampHeight(m.viewBody(m.view, width, m.paneHeight()), m.paneHeight())
	}
	// padLinesToWidth: bubble-table only renders as wide as its columns
	// actually need, up to layout.PrimaryWidth — a table with few/narrow
	// columns can render narrower than the width SplitLayout allotted it.
	// lipgloss.JoinHorizontal positions the secondary pane right after
	// whatever width primary's lines actually are, so an unpadded narrow
	// primary shifts the secondary card left, leaving a gap of blank
	// cells between it and the outer card's own right border instead of
	// the secondary card sitting flush there (m2).
	primary := padLinesToWidth(stripHeaderSeparator(m.tableViewAt(layout.PrimaryWidth)), layout.PrimaryWidth)
	// The secondary pane gets its own bordered card (title, ●/○ focus
	// bullet) so a split layout's two panes are visually distinguishable —
	// the primary (table) side's own focus cue is the outer card m.View
	// wraps everything in, which only reflects overall grid focus, not
	// which of the two panes Tab currently routes keys to. cardWidth
	// reserves 1 cell (gapCol below) between the panes and 2 cells for the
	// secondary card's own left/right border.
	cardWidth := max(1, layout.SecondaryWidth-1)
	contentWidth := max(1, cardWidth-2)
	content := m.padOrClampHeight(m.viewBody(m.view, contentWidth, m.paneHeight()), m.paneHeight())
	secondary := m.secondaryCard(content, cardWidth)
	// Joined line by line rather than via lipgloss.JoinHorizontal: both
	// primary and secondary are already padded to their own fixed widths
	// (padLinesToWidth/secondaryCard's own borderLine/padAnsiLine calls),
	// but JoinHorizontal's own internal width measurement of a styled,
	// ANSI-heavy line (e.g. the table's own styled header row) has proven
	// inconsistent by a cell or two versus a plain content line, visibly
	// misaligning the secondary card's left edge across rows. Manual
	// string concatenation of already-known-width lines has no such
	// ambiguity to resolve.
	primaryLines := strings.Split(primary, "\n")
	secondaryLines := strings.Split(secondary, "\n")
	blankPrimary := strings.Repeat(" ", max(0, layout.PrimaryWidth))
	blankSecondary := strings.Repeat(" ", max(0, cardWidth))
	height := max(len(primaryLines), len(secondaryLines))
	lines := make([]string, height)
	for i := range lines {
		// primary/secondary are already each padded to a known-fixed
		// width (padLinesToWidth; secondaryCard's own borderLine/
		// padAnsiLine calls) — re-running padAnsiLine on an
		// already-correctly-sized, heavily-styled line here previously
		// misjudged its width by a cell or two on some rows and not
		// others, visibly misaligning the secondary card's left edge; a
		// plain blank fallback line avoids re-measuring styled content at
		// all.
		p, s := blankPrimary, blankSecondary
		if i < len(primaryLines) {
			p = primaryLines[i]
		}
		if i < len(secondaryLines) {
			s = secondaryLines[i]
		}
		lines[i] = p + " " + s
	}
	return strings.Join(lines, "\n")
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

// padOrClampHeight blank-pads or truncates content to exactly height lines,
// the sizing a non-table view's own body must have — matching the table's
// own natural height (paneHeight() data rows, after stripHeaderSeparator
// removes its header/data separator) so a split layout's two panes align
// under lipgloss.JoinHorizontal, and so a standalone secondary view fills
// its card the same way the table itself would.
func (m *Model) padOrClampHeight(content string, height int) string {
	lines := strings.Split(content, "\n")
	if content == "" {
		lines = []string{""}
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// padLinesToWidth right-pads every line of content to exactly width
// display cells (ansi-aware, via padAnsiLine) — used to give a split
// layout's primary (table) pane a fixed, predictable width even when
// bubble-table itself rendered narrower than that (m2), so
// lipgloss.JoinHorizontal always positions the secondary pane at the same
// offset rather than wherever the table's actual content happened to end.
func padLinesToWidth(content string, width int) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = padAnsiLine(line, width)
	}
	return strings.Join(lines, "\n")
}

// stripHeaderSeparator removes bubble-table's header/data separator line
// (its View() output's second line) — the same trim card() applies for a
// standalone table view — from the table's own rendering wherever it is
// embedded elsewhere, i.e. a split layout's primary pane (tableViewAt).
// Without it that pane is 2 lines taller than the data-row count alone
// suggests (a header line it keeps plus a separator line it shouldn't),
// which used to throw off the combined pane's expected height and clip the
// table's own bottommost row(s), including the highlighted one (B1).
func stripHeaderSeparator(view string) string {
	lines := strings.Split(view, "\n")
	if len(lines) > 1 {
		lines = append(lines[:1], lines[2:]...)
	}
	return strings.Join(lines, "\n")
}

// tableViewAt renders the bare table at a specific width — used for the
// primary pane of a split layout, typically narrower than m.width — WITHOUT
// disturbing the Model's own width, table, or filter focus. It builds a
// throwaway table.Model via buildTable rather than temporarily mutating
// m.width/m.table and rebuilding twice (once at the new width, once back):
// that used to run on every single View() call for a split-active grid
// (layout.PrimaryWidth is almost never equal to m.width), and each
// rebuildTable call blurs the real filter input (bubble-table v0.23.0's
// WithFilterInputValue always does), so a user typing into the filter under
// a split layout had it silently blurred on the very next render — the next
// keystroke fell through to the grid's own key handling instead of
// extending the filter text. View must be side-effect free; this is.
func (m *Model) tableViewAt(width int) string {
	if width == m.width {
		return m.table.View()
	}
	highlightedSource := m.CurrentIndex()
	filterText := m.table.GetCurrentFilter()
	highlighted := -1 // set below, once the throwaway table's visible set is known; the row-style closure reads it by reference at render time.
	t := m.buildTable(width, func(input table.RowStyleFuncInput) lipgloss.Style {
		// A snapshot, unlike rebuildTable's dynamic m.table.GetHighlightedRowIndex()
		// read: this table is rendered once, right here, and discarded —
		// it is never separately navigated — so there is no "later" state
		// for a dynamic read to need to catch up with.
		if input.Index != highlighted {
			if m.focused {
				return lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Background(lipgloss.Color("235"))
			}
			return lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Background(lipgloss.Color("232"))
		}
		if m.focused {
			return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("229")).Background(lipgloss.Color("57"))
		}
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("250")).Background(lipgloss.Color("239"))
	})
	if filterText != "" {
		// This throwaway copy's own filter focus doesn't matter — it's
		// rendered once and discarded — only the REAL m.table's filter
		// input (never touched here) determines whether the next keypress
		// reaches it.
		t = t.WithFilterInputValue(filterText)
	}
	if len(m.rows) > 0 {
		pos := findVisiblePositionForTableViewAt(t.GetVisibleRows(), highlightedSource)
		if pos < 0 {
			pos = 0
		}
		highlighted = pos
		t = t.WithHighlightedRow(pos)
	}
	t = m.scrollColumnIntoView(t, width)
	return t.View()
}

// findVisiblePositionForTableViewAt is tableViewAt's seam over
// visiblePositionForSource. The throwaway table t is built from the same
// rows/filter as the real m.table read moments earlier, so there is no
// legitimate sequence through the public API where the real highlighted
// source row is absent from t's visible set — the "pos < 0" fallback above
// is defensive. Swapped out in
// TestTableViewAtFallsBackToFirstRowWhenSourceMissing to drive it directly.
var findVisiblePositionForTableViewAt = visiblePositionForSource

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

// secondaryCard wraps a split layout's secondary-pane content in its own
// bordered card: a title bar (the active ExtraView's Label) with a ●/○
// focus bullet and border color reflecting m.secondaryFocus specifically
// (not the grid's own overall Focused, which the primary table's outer
// m.card() already shows) — so a user can see at a glance which of the two
// panes Tab currently routes keys to. Ported from DataTug's (pre-adoption)
// recordsetCard.
func (m *Model) secondaryCard(content string, width int) string {
	label := ""
	if i := int(m.view) - firstExtraView; i >= 0 && i < len(m.extraViews) {
		label = m.extraViews[i].Label
	}
	focused := m.focused && m.secondaryFocus
	titleStyle, borderStyle, bullet := inactiveTitleStyle, inactiveBorderStyle, "○ "
	if focused {
		titleStyle, borderStyle, bullet = activeTitleStyle, activeBorderStyle, "● "
	}
	title := titleStyle.Render(bullet) + titleStyle.Render(label)
	innerWidth := max(1, width-2)
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines)+2)
	out = append(out, padAnsiLine(borderStyle.Render(borderLine("╭", title, "╮", width)), width))
	for _, line := range lines {
		out = append(out, padAnsiLine(borderStyle.Render("│")+padAnsiLine(line, innerWidth)+borderStyle.Render("│"), width))
	}
	out = append(out, padAnsiLine(borderStyle.Render(borderLine("╰", "", "╯", width)), width))
	return strings.Join(out, "\n")
}

// card wraps content in a bordered card with a title bar and a stats footer
// in the bottom border, with a scrollbar down the right edge. Ported from
// DataTug's gridState.viewWithTitle.
func (m *Model) card(label, content string) string {
	cardWidth := max(1, m.width)
	innerWidth := m.tableWidth()
	// bubble-table always emits a header/data separator with its outer
	// border disabled. The card's own title bar already distinguishes the
	// two regions. body() already strips this for a split layout's primary
	// (table) sub-pane (stripHeaderSeparator) and already pads/clamps any
	// non-table view's own body to its final height (padOrClampHeight), so
	// this is the one remaining case: a standalone table view's content,
	// straight off bubble-table's own View().
	if m.view == ViewTable {
		content = stripHeaderSeparator(content)
	}
	rawLines := strings.Split(content, "\n")
	if content == "" {
		rawLines = []string{""}
	}
	lines := make([]string, 0, len(rawLines)+2)
	// label (headerLine's output) is already padded to its own width with
	// trailing spaces (padAnsiLine); borderLine below re-truncates title to
	// fit the bullet's 2 extra cells, and truncating a padded string can
	// leave "… " (an ellipsis followed by dangling padding) right against
	// the top-right corner — trim the padding first, like main did, so a
	// re-truncation (if still needed) ends on real content or a clean "…".
	title := strings.TrimRight(label, " ")
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
