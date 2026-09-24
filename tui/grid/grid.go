package grid

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/evertras/bubble-table/table"

	"github.com/strongo/aichat/ai/session"
)

// sourceKey is a hidden bubble-table RowData key (it matches no column, so it
// never renders) carrying the row's position in Model.rows/Model.cells. It is
// how CurrentIndex recovers the right row when a filter is active: bubble-table's
// cursor indexes GetVisibleRows() (post-filter), not the unfiltered row slice.
const sourceKey = "_src"

// Column is the UI-ready description of a result column.
type Column struct {
	Name    string
	Numeric bool
}

// Row is one grid row. Values is positional, aligned with the Columns slice
// the Row was built against — not a map keyed by column name — so two
// columns sharing a name (e.g. `SELECT a.id, b.id`) each keep their own
// value. A product may pass either raw Go values (formatted by FormatValue)
// or its own pre-formatted display strings (e.g. DataTug's date-only
// formatting) — both are valid Row.Values entries. Ref, when set, lets the
// row be pinned to the sidebar or resolved as the "current" entity
// (transcript.EntityBlock). Key, when set to a stable identifier (e.g. the
// row's original/source index), survives Sort — IndexForKey resolves it back
// to a display index.
type Row struct {
	Key    string
	Values []any
	Ref    *session.EntityRef
}

// Absent is the sentinel Row.Values entry meaning "no value was supplied for
// this column" — distinct from an explicit nil ("NULL"). Product adapters use
// it for sparse selections (e.g. DataTug's cell-range picks) where only some
// columns have a value for a given row.
var Absent any = absentType{}

type absentType struct{}

// value returns the row's value for columns[i], or Absent when the row has
// fewer values than columns (a sparse/partial row).
func (r Row) value(i int) any {
	if i < 0 || i >= len(r.Values) {
		return Absent
	}
	return r.Values[i]
}

// View selects what the grid's secondary area shows: the built-in table
// (ViewTable, always index 0), or a product-registered ExtraView (index
// 1..len(extraViews), in registration order). Unlike an earlier revision,
// there is no fixed Card/Inspector view: use the CardView/InspectorView
// constructors below to register one (or both, in whatever order) as an
// ExtraView, alongside a product's own (Charts, Raw response, ...).
type View int

// ViewTable is the sortable/filterable table (the default, always index 0).
const ViewTable View = 0

// firstExtraView is the View value of the first registered ExtraView.
const firstExtraView = 1

// ExtraView is a product-registered secondary view — DataTug's Charts, Raw
// response, Headers and current-row views are ExtraViews — shown alongside
// the table and selected the same way (number keys, cycling through the
// header). A grid stays the one generic component; products supply their
// own panes (or the CardView/InspectorView helpers) instead of building a
// competing grid.
type ExtraView struct {
	// Label is shown in the view switcher header, e.g. "Charts".
	Label string
	// Render draws the view's body at the given content width/height.
	Render func(m *Model, width, height int) string
	// Update optionally handles a key press while this view is active and
	// focused (e.g. arrow keys moving between chart candidates). It returns
	// the command to run (if any) and whether it handled the message; when
	// it returns false the grid's own key handling still runs.
	Update func(m *Model, msg tea.KeyPressMsg) (tea.Cmd, bool)
}

// CardView returns an ExtraView rendering the highlighted row as a formatted
// vertical field list (Column name / FormatValue'd value). label defaults to
// "Current row" when empty. Ported from DataTug's recordset_views.go
// currentRowContent (raw=false).
func CardView(label string) ExtraView {
	if label == "" {
		label = "Current row"
	}
	return ExtraView{Label: label, Render: func(m *Model, width, _ int) string {
		return currentRowContent(m.columns, m.rows, m.CurrentIndex(), width, false)
	}}
}

// InspectorView is CardView's raw-value counterpart: it renders each field's
// Go value (%#v) instead of FormatValue's terminal-safe text. label defaults
// to "Inspector" when empty.
func InspectorView(label string) ExtraView {
	if label == "" {
		label = "Inspector"
	}
	return ExtraView{Label: label, Render: func(m *Model, width, _ int) string {
		return currentRowContent(m.columns, m.rows, m.CurrentIndex(), width, true)
	}}
}

// KeyHandler lets a product own specific key presses (e.g. DataTug's
// c/r/a/d/b/s/B/e actions) instead of the grid's own defaults. It is checked
// first, for every key press the filter input isn't consuming; returning
// handled=false falls through to the grid's built-in handling (column/row
// navigation, view switching, sort, Enter, +, /).
type KeyHandler func(m *Model, msg tea.KeyPressMsg) (tea.Cmd, bool)

// FooterHook lets a product append extra stats to the grid's own footer
// (row/column range, sort indicator), e.g. a version badge or a save-status
// note. It receives the built-in footer text and returns the final text.
type FooterHook func(m *Model, builtin string) string

// RowActivatedMsg is emitted on Enter over the highlighted row (table view)
// when no KeyHandler claims "enter" first.
type RowActivatedMsg struct{ Row Row }

// SplitLayout is the result of a LayoutFunc: whether the secondary (non-table)
// view should share the pane with the table, and at what widths.
type SplitLayout struct {
	Split          bool
	PrimaryWidth   int
	SecondaryWidth int
}

// LayoutFunc chooses, for the active non-table view, whether to split the
// pane between the table and that view. totalWidth is the grid's full
// width; naturalWidth is the table's natural (unclipped) content width, from
// Model.NaturalWidth(). Generalises DataTug's chooseRecordsetLayout so a
// product's split-pane policy is a plugged-in function, not a second grid.
type LayoutFunc func(totalWidth, naturalWidth int, view View) SplitLayout

// Option configures a Model at construction time.
type Option func(*Model)

// WithTitle sets the grid's header title (defaults to "Result").
func WithTitle(title string) Option {
	return func(m *Model) { m.title = title }
}

// WithExtraViews registers product-specific secondary views (e.g. DataTug's
// Charts/Current-row/Raw/Headers) after the built-in table view, in the
// given order. They are selected the same way: number keys and the header
// switcher. See also Model.SetExtraViews for registering them after
// construction (e.g. once an HTTP response becomes available).
func WithExtraViews(views ...ExtraView) Option {
	return func(m *Model) { m.extraViews = append(m.extraViews, views...) }
}

// WithSplitLayout registers the policy used to decide whether a non-table
// view shares the pane with the table (side by side) or takes the full
// width. Without it, a non-table view always takes the full pane, matching
// prior behaviour.
func WithSplitLayout(fn LayoutFunc) Option {
	return func(m *Model) { m.layout = fn }
}

// WithMaxVisibleRows caps how many rows the table view renders per page so a
// large result (e.g. 1000 rows) never renders fully into a scrolling
// transcript. Defaults to DefaultMaxVisibleRows; pass 0 to disable paging.
func WithMaxVisibleRows(n int) Option {
	return func(m *Model) { m.maxVisibleRows = n }
}

// WithStyle sets the grid's initial border/header color preset (see Style).
// Defaults to StyleLines.
func WithStyle(s Style) Option {
	return func(m *Model) { m.style = s }
}

// WithKeyHandler registers the product key-handler hook (see KeyHandler).
func WithKeyHandler(fn KeyHandler) Option {
	return func(m *Model) { m.keyHandler = fn }
}

// WithFooterHook registers the product footer hook (see FooterHook).
func WithFooterHook(fn FooterHook) Option {
	return func(m *Model) { m.footerHook = fn }
}

// DefaultMaxVisibleRows is the page size a Model uses when WithMaxVisibleRows
// is not supplied.
const DefaultMaxVisibleRows = 12

// Model is a transcript.EntityBlock: a result grid with a sortable/filterable
// table, per-cell column selection, a scrollbar, style presets, and slots for
// a product's own secondary views (ExtraView) and split-pane layout. Ported
// and generalised from DataTug's GridModel/gridState, recordset_ui.go,
// recordset_views.go and table_style.go.
type Model struct {
	columns []Column
	rows    []Row // display order
	cells   [][]string
	title   string
	view    View
	focused bool
	width   int

	table          table.Model
	sortColumn     int
	sortDesc       bool
	selectedColumn int

	extraViews     []ExtraView
	layout         LayoutFunc
	maxVisibleRows int
	keyHandler     KeyHandler
	footerHook     FooterHook
	style          Style
	secondaryFocus bool
}

// New builds a grid from columns and rows. Row order is preserved until the
// user sorts.
func New(columns []Column, rows []Row, opts ...Option) *Model {
	m := &Model{
		columns:        append([]Column(nil), columns...),
		rows:           append([]Row(nil), rows...),
		title:          "Result",
		sortColumn:     -1,
		maxVisibleRows: DefaultMaxVisibleRows,
		style:          StyleLines,
	}
	for _, opt := range opts {
		opt(m)
	}
	m.cells = formatRows(m.columns, m.rows)
	m.width = 80
	m.rebuildTable()
	return m
}

func gridKeyMap() table.KeyMap {
	km := table.DefaultKeyMap()
	// DataTug owns column navigation (h/l select a column; the grid
	// auto-scrolls it into view) and row navigation is up/down/k only — "j"
	// is reserved for a product's own use (DataTug's join-candidate
	// navigation). The grid has no pages (WithNoPagination-equivalent: one
	// page), Enter is reserved for the grid/product (RowActivatedMsg or a
	// KeyHandler), and the filter's own bindings are unlabelled internals.
	km.RowUp = key.NewBinding(key.WithKeys("up", "k"))
	km.RowDown = key.NewBinding(key.WithKeys("down"))
	km.PageUp = key.Binding{}
	km.PageDown = key.Binding{}
	km.PageFirst = key.Binding{}
	km.PageLast = key.Binding{}
	km.ScrollLeft = key.Binding{}
	km.ScrollRight = key.Binding{}
	km.RowSelectToggle = key.Binding{}
	return km
}

// rebuildTable reconstructs the inner bubble-table from the current
// columns/rows/cells/selectedColumn/style, preserving horizontal scroll
// offset. Ported from DataTug's gridState.rebuild.
func (m *Model) rebuildTable() {
	previousOffset := m.table.GetHorizontalScrollColumnOffset()
	columns := make([]table.Column, len(m.columns))
	for i := range m.columns {
		style := columnStyle(m.columns[i], i == m.selectedColumn)
		columns[i] = table.NewColumn(columnKey(i), m.header(i), m.columnWidth(i)).WithStyle(style).WithFiltered(true)
	}
	rows := make([]table.Row, len(m.rows))
	for i := range m.rows {
		data := make(table.RowData, len(m.columns)+1)
		for c := range m.columns {
			value := ""
			if c < len(m.cells[i]) {
				value = m.cells[i][c]
			}
			style := columnStyle(m.columns[c], c == m.selectedColumn)
			data[columnKey(c)] = table.NewStyledCell(value, style)
		}
		data[sourceKey] = i
		rows[i] = table.NewRow(data)
	}
	focused := m.focused
	highlighted := m.table.GetHighlightedRowIndex()
	newTable := table.New(columns).
		WithRows(rows).
		WithBaseStyle(m.style.dividerStyle()).
		WithBorderForeground(m.style.BorderColor).
		HeaderStyle(m.style.HeaderStyle).
		WithMaxTotalWidth(m.tableWidth()).
		WithPaginationWrapping(false).
		WithOuterBorder(false).
		WithRowBorder(false).
		WithFooterVisibility(false).
		WithHeaderVisibility(true).
		Filtered(true).
		WithKeyMap(gridKeyMap()).
		Focused(focused && !m.secondaryFocus).
		WithRowStyleFunc(func(input table.RowStyleFuncInput) lipgloss.Style {
			// Read the highlighted row dynamically (not a value captured at
			// rebuild time): plain row-up/row-down navigation updates
			// m.table's cursor directly, via bubble-table's own Update,
			// without a rebuildTable call.
			if input.Index != m.table.GetHighlightedRowIndex() {
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
	if m.maxVisibleRows > 0 {
		newTable = newTable.WithPageSize(m.maxVisibleRows)
	}
	if len(rows) > 0 {
		highlighted = max(0, min(highlighted, len(rows)-1))
		newTable = newTable.WithHighlightedRow(highlighted)
	}
	m.table = newTable
	for i := 0; i < previousOffset; i++ {
		m.table.ScrollRight()
	}
	m.ensureSelectedColumnVisible()
}

func columnKey(i int) string { return "c" + strconv.Itoa(i) }

func formatRows(columns []Column, rows []Row) [][]string {
	cells := make([][]string, len(rows))
	for i, row := range rows {
		line := make([]string, len(columns))
		for c := range columns {
			if v := row.value(c); v != Absent {
				line[c] = sanitize(FormatValue(v))
			}
		}
		cells[i] = line
	}
	return cells
}

func sanitize(s string) string {
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return ' '
		}
		return r
	}, s)
}

// FormatValue applies basic terminal-safe value formatting, shared with the
// card/inspector views.
func FormatValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "NULL"
	case string:
		return v
	case []byte:
		if utf8.Valid(v) {
			return string(v)
		}
		return "0x" + hex.EncodeToString(v)
	case time.Time:
		return v.Format(time.RFC3339)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// SetWidth resizes the grid and its inner table.
func (m *Model) SetWidth(width int) {
	m.width = max(1, width)
	m.rebuildTable()
}

// Width is the last width passed to SetWidth or View.
func (m *Model) Width() int { return m.width }

// Columns/Rows expose the current (sorted) state for inspection/tests.
func (m *Model) Columns() []Column { return m.columns }
func (m *Model) Rows() []Row       { return m.rows }

// Cell returns the formatted display text for a row/column (the same text
// shown in the table), or "" out of bounds.
func (m *Model) Cell(rowIndex, columnIndex int) string {
	if rowIndex < 0 || rowIndex >= len(m.cells) || columnIndex < 0 || columnIndex >= len(m.cells[rowIndex]) {
		return ""
	}
	return m.cells[rowIndex][columnIndex]
}

// IndexForKey returns the display index of the row whose Key equals key, or
// -1. Row.Key is preserved across Sort, so a product can save a row's Key
// (e.g. a stable source-record index) and restore the selection after a
// sort or a data refresh.
func (m *Model) IndexForKey(key string) int {
	for i, row := range m.rows {
		if row.Key == key {
			return i
		}
	}
	return -1
}

// SelectRow highlights the row at the given display index, clamped to
// bounds. It does not change SelectedColumn.
func (m *Model) SelectRow(index int) {
	if len(m.rows) == 0 {
		return
	}
	index = max(0, min(index, len(m.rows)-1))
	m.table = m.table.WithHighlightedRow(index)
}

// SelectColumn selects a column directly (as h/l do interactively),
// clamping to bounds and scrolling it into view.
func (m *Model) SelectColumn(index int) {
	if len(m.columns) == 0 {
		return
	}
	m.selectedColumn = max(0, min(index, len(m.columns)-1))
	m.rebuildTable()
}

// SelectedColumn is the column h/l (or SelectColumn) currently has selected.
func (m *Model) SelectedColumn() int { return m.selectedColumn }

// NaturalWidth is the table's unclipped content width (sum of column widths
// plus borders), for a LayoutFunc to compare against the pane's total width —
// the same quantity DataTug's chooseRecordsetLayout compares against.
func (m *Model) NaturalWidth() int {
	width := 2 // left border and scrollbar/right border
	for i := range m.columns {
		columnWidth := ansi.StringWidth(m.header(i))
		for _, row := range m.cells {
			if i < len(row) {
				columnWidth = max(columnWidth, ansi.StringWidth(row[i]))
			}
		}
		width += min(28, max(6, columnWidth))
		if i+1 < len(m.columns) {
			width++
		}
	}
	return width
}

// columnWidth is DataTug's gridState.columnWidth, generalised over cells.
func (m *Model) columnWidth(columnIndex int) int {
	if columnIndex < 0 || columnIndex >= len(m.columns) {
		return 1
	}
	width := lipgloss.Width(m.header(columnIndex))
	for _, row := range m.cells {
		if columnIndex < len(row) && lipgloss.Width(row[columnIndex]) > width {
			width = lipgloss.Width(row[columnIndex])
		}
	}
	return max(1, min(m.tableWidth()-1, min(28, max(6, width))))
}

func (m *Model) tableWidth() int { return max(2, m.width-2) }

// visibleColumnWindow mirrors bubble-table's no-outer-border width rules:
// each non-final rendered column consumes its content width plus the right
// divider, while the final source column has no trailing divider. A
// horizontal overflow view reserves two cells for the marker column. Ported
// from DataTug's gridState.visibleColumnWindow.
func (m *Model) visibleColumnWindow() (int, int) {
	if len(m.columns) == 0 {
		return 0, -1
	}
	offset := m.table.GetHorizontalScrollColumnOffset()
	used := 0
	if offset > 0 {
		used = 2 // bubble-table's left overflow marker and divider
	}
	last := offset - 1
	for i := offset; i < len(m.columns); i++ {
		targetWidth := m.tableWidth() - 2 // reserve the right overflow marker
		finalColumn := i == len(m.columns)-1
		if finalColumn {
			targetWidth = m.tableWidth()
		}
		renderedWidth := m.columnWidth(i)
		if !finalColumn {
			renderedWidth++ // non-final cell plus right divider
		}
		if used+renderedWidth > targetWidth {
			break
		}
		used += renderedWidth
		last = i
	}
	return offset, last
}

func (m *Model) visibleColumnRange() (int, int) {
	offset, last := m.visibleColumnWindow()
	if last < offset {
		return 0, 0
	}
	return offset + 1, last + 1
}

func (m *Model) ensureSelectedColumnVisible() {
	if len(m.columns) == 0 {
		return
	}
	for m.table.GetHorizontalScrollColumnOffset() > m.selectedColumn {
		before := m.table.GetHorizontalScrollColumnOffset()
		m.table.ScrollLeft()
		if m.table.GetHorizontalScrollColumnOffset() == before {
			break
		}
	}
	_, last := m.visibleColumnWindow()
	for m.selectedColumn > last && m.table.GetHorizontalScrollColumnOffset() < m.selectedColumn {
		before := m.table.GetHorizontalScrollColumnOffset()
		m.table.ScrollRight()
		if m.table.GetHorizontalScrollColumnOffset() == before {
			break
		}
		_, last = m.visibleColumnWindow()
	}
}

// CurrentIndex returns the display index of the highlighted row, or -1 when
// there are no rows. It is filter-aware: bubble-table's cursor indexes
// GetVisibleRows() (the post-filter subset), so the row's hidden sourceKey
// metadata — not the raw cursor index — is what recovers the position in
// Model.rows.
func (m *Model) CurrentIndex() int {
	if len(m.rows) == 0 {
		return -1
	}
	highlighted := m.table.HighlightedRow()
	if src, ok := highlighted.Data[sourceKey].(int); ok && src >= 0 && src < len(m.rows) {
		return src
	}
	return -1
}

// Current implements transcript.EntityBlock: the highlighted row's Ref.
func (m *Model) Current() *session.EntityRef {
	i := m.CurrentIndex()
	if i < 0 || i >= len(m.rows) {
		return nil
	}
	return m.rows[i].Ref
}

// CurrentRow returns the highlighted Row and whether one exists.
func (m *Model) CurrentRow() (Row, bool) {
	i := m.CurrentIndex()
	if i < 0 || i >= len(m.rows) {
		return Row{}, false
	}
	return m.rows[i], true
}

// CapturesEsc reports whether the grid's own filter input is currently
// focused, in which case Esc should clear/blur that filter rather than be
// handled by a surrounding chatshell (e.g. to close the block or the pane).
func (m *Model) CapturesEsc() bool {
	return m.table.GetIsFilterInputFocused()
}

// SetFocused sets the grid's focus state directly, for a caller that renders
// its own width/focus rather than going through the transcript.Block View
// signature (e.g. a modal dialog's own grid).
func (m *Model) SetFocused(focused bool) {
	if m.focused == focused {
		return
	}
	m.focused = focused
	m.rebuildTable()
}

// Focused reports the grid's current focus state.
func (m *Model) Focused() bool { return m.focused }

// SecondaryFocus reports whether keyboard focus is on the active secondary
// (non-table) view rather than the table, when the pane is split. See
// SetSecondaryFocus.
func (m *Model) SecondaryFocus() bool { return m.secondaryFocus }

// SetSecondaryFocus moves keyboard focus to/from the active secondary view.
// It is a no-op (always false) while the table view is active. Ported from
// DataTug's gridState.setSecondaryFocus.
func (m *Model) SetSecondaryFocus(focused bool) {
	if m.view == ViewTable {
		focused = false
	}
	if m.secondaryFocus == focused {
		return
	}
	m.secondaryFocus = focused
	m.rebuildTable()
}

// ToggleSecondaryFocusIfSplit toggles SecondaryFocus when the active
// non-table view is currently sharing the pane with the table (per the
// registered LayoutFunc), and reports whether it did. A product's own Tab
// handling (e.g. falling back to focusing its composer) uses the return
// value to know whether the grid consumed the key.
func (m *Model) ToggleSecondaryFocusIfSplit() bool {
	if m.view == ViewTable {
		return false
	}
	if !m.splitNow() {
		return false
	}
	m.SetSecondaryFocus(!m.secondaryFocus)
	return true
}

func (m *Model) splitNow() bool {
	if m.layout == nil {
		return false
	}
	return m.layout(m.width, m.NaturalWidth(), m.view).Split
}

// CurrentView reports the active view.
func (m *Model) CurrentView() View { return m.view }

// SetView switches the active view (ViewTable or a registered ExtraView
// index), clamped to a valid value. Mirrors DataTug's
// gridState.setRecordsetView, auto-focusing the new secondary view when it
// won't be split with the table.
func (m *Model) SetView(v View) {
	if v != ViewTable && (int(v) < firstExtraView || int(v)-firstExtraView >= len(m.extraViews)) {
		return
	}
	m.view = v
	if v == ViewTable {
		m.SetSecondaryFocus(false)
		return
	}
	if !m.splitNow() {
		m.SetSecondaryFocus(true)
	}
}

// ExtraViews returns the currently registered extra views.
func (m *Model) ExtraViews() []ExtraView { return append([]ExtraView(nil), m.extraViews...) }

// SetExtraViews replaces the registered extra views (e.g. once an HTTP
// response becomes available and a product wants to add Raw/Headers views
// that weren't known at construction time). The active view is reset to
// ViewTable if it no longer resolves.
func (m *Model) SetExtraViews(views ...ExtraView) {
	m.extraViews = append([]ExtraView(nil), views...)
	if m.view != ViewTable && (int(m.view) < firstExtraView || int(m.view)-firstExtraView >= len(m.extraViews)) {
		m.SetView(ViewTable)
	}
}

// SetKeyHandler registers (or replaces) the product key-handler hook after
// construction — useful when the hook's closure needs context only
// available once the Model itself exists (e.g. a dialog capturing its own
// *Model to react to Space/Enter).
func (m *Model) SetKeyHandler(fn KeyHandler) { m.keyHandler = fn }

// SetFooterHook registers (or replaces) the product footer hook after
// construction. See WithFooterHook.
func (m *Model) SetFooterHook(fn FooterHook) { m.footerHook = fn }

// Style is the grid's current border/header color preset.
func (m *Model) Style() Style { return m.style }

// SetStyle changes the grid's border/header color preset.
func (m *Model) SetStyle(s Style) {
	m.style = s
	m.rebuildTable()
}

// SortState reports the column currently sorted (-1 if none) and direction.
func (m *Model) SortState() (column int, desc bool) { return m.sortColumn, m.sortDesc }

// Sort toggles ascending/descending order on column, stably. Ported from
// DataTug's GridModel.Sort (pkg/chat/grid.go).
func (m *Model) Sort(column int) {
	if column < 0 || column >= len(m.columns) {
		return
	}
	if m.sortColumn == column {
		m.sortDesc = !m.sortDesc
	} else {
		m.sortColumn, m.sortDesc = column, false
	}
	order := make([]int, len(m.rows))
	for i := range order {
		order[i] = i
	}
	numeric := m.columns[column].Numeric
	sort.SliceStable(order, func(i, j int) bool {
		li, ri := order[i], order[j]
		left, right := "", ""
		if column < len(m.cells[li]) {
			left = m.cells[li][column]
		}
		if column < len(m.cells[ri]) {
			right = m.cells[ri][column]
		}
		comparison := strings.Compare(left, right)
		if numeric {
			if l, lok := new(big.Rat).SetString(left); lok {
				if r, rok := new(big.Rat).SetString(right); rok {
					comparison = l.Cmp(r)
				}
			}
		}
		if m.sortDesc {
			return comparison > 0
		}
		return comparison < 0
	})
	rows := make([]Row, len(m.rows))
	cells := make([][]string, len(m.cells))
	for i, idx := range order {
		rows[i] = m.rows[idx]
		cells[i] = m.cells[idx]
	}
	m.rows, m.cells = rows, cells
	m.rebuildTable()
}

func (m *Model) header(column int) string {
	name := m.columns[column].Name
	if m.sortColumn != column {
		return name
	}
	if m.sortDesc {
		return name + " ▼"
	}
	return name + " ▲"
}

// Focusable implements transcript.Block: a grid is always a focusable stop.
func (m *Model) Focusable() bool { return true }

// currentRowContent renders the highlighted row as a vertical field list
// (raw=false, CardView) or raw Go values (raw=true, InspectorView). Ported
// from DataTug's recordset_views.go currentRowContent, generalised over
// Row.Values.
func currentRowContent(columns []Column, rows []Row, rowIndex, width int, raw bool) string {
	if rowIndex < 0 || rowIndex >= len(rows) {
		return "No current row."
	}
	width = max(1, width)
	row := rows[rowIndex]
	var lines []string
	for i, col := range columns {
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, ansi.Wrap(sanitize(col.Name), width, " "))
		value := "—"
		switch v := row.value(i); {
		case v == Absent:
			// No value was supplied for this cell at all.
		case v == nil:
			// An explicit nil (e.g. a SQL NULL) is distinct from a value
			// that was never supplied for this row (Absent, above).
			value = "NULL"
		case raw:
			value = sanitize(fmt.Sprintf("%#v", v))
		default:
			value = sanitize(FormatValue(v))
		}
		lines = append(lines, ansi.Wrap(value, width, " "))
	}
	return strings.Join(lines, "\n")
}

// extraViewKeyIndex maps number keys "2".."9" to an ExtraView index
// (0-based), or -1 when key isn't one of those. "1" is always ViewTable.
func extraViewKeyIndex(key string) int {
	if len(key) != 1 || key[0] < '2' || key[0] > '9' {
		return -1
	}
	return int(key[0]-'0') - firstExtraView - 1
}
