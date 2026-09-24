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
	"github.com/strongo/aichat/tui"
	"github.com/strongo/aichat/tui/transcript"
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
// value. Ref, when set, lets the row be pinned to the sidebar or resolved as
// the "current" entity (transcript.EntityBlock).
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

// View selects what the grid's secondary area shows: the built-in
// Table/Card/Inspector views, or a product-registered ExtraView.
type View int

const (
	// ViewTable is the sortable/filterable table (the default).
	ViewTable View = iota
	// ViewCard shows the highlighted row as a vertical field list.
	ViewCard
	// ViewInspector shows the highlighted row's raw values.
	ViewInspector
)

// ExtraView is a product-registered secondary view — DataTug's Charts, Raw
// response and Headers views are ExtraViews — shown alongside the built-in
// Table/Card/Inspector views and selected the same way (number keys, cycling
// through the header). A grid stays the one generic component; products
// supply their own panes instead of building a competing grid.
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

func (v View) label(extra []ExtraView) string {
	switch v {
	case ViewCard:
		return "Card"
	case ViewInspector:
		return "Inspector"
	case ViewTable:
		return "Table"
	default:
		if i := int(v) - firstExtraView; i >= 0 && i < len(extra) {
			return extra[i].Label
		}
		return "Table"
	}
}

// firstExtraView is the View value of the first ExtraView, right after the
// three built-in views. Declared as a plain int (not View) so it mixes
// freely with int arithmetic at its call sites.
const firstExtraView = int(ViewInspector) + 1

// RowActivatedMsg is emitted on Enter over the highlighted row (table view).
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
// Charts/Raw/Headers) after the built-in Table/Card/Inspector views. They are
// selected the same way: number keys and the header switcher.
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

// DefaultMaxVisibleRows is the page size a Model uses when WithMaxVisibleRows
// is not supplied.
const DefaultMaxVisibleRows = 12

// Model is a transcript.EntityBlock: a result grid with table/card/inspector
// (plus any registered ExtraViews) views, sort, filter and a footer/scrollbar,
// driven by bubble-table.
type Model struct {
	columns []Column
	rows    []Row // display order
	cells   [][]string
	title   string
	view    View
	focused bool
	width   int

	table      table.Model
	sortColumn int
	sortDesc   bool
	// sortTarget is the column Tab cycles and "s" sorts by.
	sortTarget int

	extraViews     []ExtraView
	layout         LayoutFunc
	maxVisibleRows int
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
	}
	for _, opt := range opts {
		opt(m)
	}
	m.cells = formatRows(m.columns, m.rows)
	m.table = table.New(m.tableColumns()).
		WithRows(tableRows(m.columns, m.rows, m.cells)).
		Filtered(true).
		WithKeyMap(gridKeyMap()).
		Focused(true)
	if m.maxVisibleRows > 0 {
		m.table = m.table.WithPageSize(m.maxVisibleRows)
	}
	return m
}

func gridKeyMap() table.KeyMap {
	km := table.DefaultKeyMap()
	// DataTug uses h/j/k/l plus arrows for row/column movement, not paging;
	// the grid has no pages (WithNoPagination-equivalent: one page).
	km.PageUp = key.Binding{}
	km.PageDown = key.Binding{}
	km.PageFirst = key.Binding{}
	km.PageLast = key.Binding{}
	km.ScrollLeft = key.NewBinding(key.WithKeys("left", "h"))
	km.ScrollRight = key.NewBinding(key.WithKeys("right", "l"))
	km.RowSelectToggle = key.Binding{} // Enter is handled by Model, not row-select
	return km
}

// tableColumns builds the inner table's column headers, including the sort
// arrow (see Model.header) on the sorted column.
func (m *Model) tableColumns() []table.Column {
	out := make([]table.Column, len(m.columns))
	for i := range m.columns {
		out[i] = table.NewFlexColumn(columnKey(i), m.header(i), 1).WithFiltered(true)
	}
	return out
}

func columnKey(i int) string { return "c" + strconv.Itoa(i) }

func tableRows(columns []Column, rows []Row, cells [][]string) []table.Row {
	out := make([]table.Row, len(rows))
	for i := range rows {
		data := make(table.RowData, len(columns)+1)
		for c := range columns {
			if c < len(cells[i]) {
				data[columnKey(c)] = cells[i][c]
			}
		}
		// Hidden metadata: bubble-table keeps any key that doesn't match a
		// column attached to the row without rendering it. It is how
		// CurrentIndex resolves the right row while a filter is active.
		data[sourceKey] = i
		out[i] = table.NewRow(data)
	}
	return out
}

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
	m.table = m.table.WithTargetWidth(m.width)
}

// Columns/Rows expose the current (sorted) state for inspection/tests.
func (m *Model) Columns() []Column { return m.columns }
func (m *Model) Rows() []Row       { return m.rows }

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
	m.table = m.table.WithColumns(m.tableColumns()).WithRows(tableRows(m.columns, m.rows, m.cells))
}

func (m Model) header(column int) string {
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

// View implements transcript.Block.
func (m *Model) View(width int, focused bool) string {
	m.focused = focused
	if width != m.width {
		m.SetWidth(width)
	}
	m.table = m.table.Focused(focused)
	header := m.headerLine(width)
	body := m.body(width)
	return header + "\n" + body + "\n" + m.footer()
}

// body renders the active view's content, applying the split-pane layout
// (see WithSplitLayout) when one is registered and the active view isn't the
// table itself.
func (m *Model) body(width int) string {
	if m.view == ViewTable || m.layout == nil {
		return m.viewBody(m.view, width)
	}
	layout := m.layout(width, m.NaturalWidth(), m.view)
	if !layout.Split {
		return m.viewBody(m.view, width)
	}
	primary := m.tableViewAt(layout.PrimaryWidth)
	secondary := m.viewBody(m.view, layout.SecondaryWidth)
	return lipgloss.JoinHorizontal(lipgloss.Top, primary, secondary)
}

// tableViewAt renders the table at a specific width without disturbing the
// Model's own width (used for the primary pane of a split layout).
func (m *Model) tableViewAt(width int) string {
	previousWidth := m.width
	if width != m.width {
		m.SetWidth(width)
	}
	view := m.table.View()
	if width != previousWidth {
		m.SetWidth(previousWidth)
	}
	return view
}

func (m *Model) viewBody(view View, width int) string {
	switch view {
	case ViewTable:
		return m.table.View()
	case ViewCard:
		return currentRowContent(m.columns, m.rows, m.CurrentIndex(), width, false)
	case ViewInspector:
		return currentRowContent(m.columns, m.rows, m.CurrentIndex(), width, true)
	default:
		if i := int(view) - firstExtraView; i >= 0 && i < len(m.extraViews) {
			return m.extraViews[i].Render(m, width, 0)
		}
		return m.table.View()
	}
}

func (m *Model) headerLine(width int) string {
	views := []View{ViewTable, ViewCard, ViewInspector}
	for i := range m.extraViews {
		views = append(views, View(firstExtraView+i))
	}
	styled := make([]string, len(views))
	for i, v := range views {
		style := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
		if v == m.view {
			style = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231"))
		}
		styled[i] = style.Render(strconv.Itoa(i+1) + " " + v.label(m.extraViews))
	}
	title := sanitize(m.title)
	if m.focused {
		title = lipgloss.NewStyle().Bold(true).Render(title)
	}
	line := title + " │ " + strings.Join(styled, " · ")
	return ansi.Truncate(line, max(1, width), "…")
}

// footer mirrors DataTug's gridState.footer(): current position and key hints.
func (m *Model) footer() string {
	total := len(m.rows)
	pos := "0/0"
	if total > 0 {
		pos = fmt.Sprintf("%d/%d", m.CurrentIndex()+1, total)
	}
	hints := "↑↓ move · 1-3 view · Enter detail · + sidebar · / filter · s sort"
	return lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render(pos + "  " + hints)
}

// currentRowContent renders the highlighted row as a vertical field list
// (Card) or raw values (Inspector). Ported from DataTug's
// recordset_views.go currentRowContent, generalised over Row.Values.
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

// Update implements transcript.Block.
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
	switch keyMsg.String() {
	case "1":
		m.view = ViewTable
		return m, nil
	case "2":
		m.view = ViewCard
		return m, nil
	case "3":
		m.view = ViewInspector
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
		m.Sort(m.sortTarget)
		return m, nil
	case "tab":
		if len(m.columns) > 0 {
			m.sortTarget = (m.sortTarget + 1) % len(m.columns)
		}
		return m, nil
	}
	if n := extraViewKeyIndex(keyMsg.String()); n >= 0 && n < len(m.extraViews) {
		m.view = View(firstExtraView + n)
		return m, nil
	}
	if m.view == ViewTable {
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		return m, cmd
	}
	return m, nil
}

// extraViewKeyIndex maps number keys "4".."9" to an ExtraView index (0-based,
// right after the three built-in views), or -1 when key isn't one of those.
func extraViewKeyIndex(key string) int {
	if len(key) != 1 || key[0] < '4' || key[0] > '9' {
		return -1
	}
	return int(key[0]-'0') - firstExtraView - 1
}
