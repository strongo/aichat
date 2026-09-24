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

// Column is the UI-ready description of a result column.
type Column struct {
	Name    string
	Numeric bool
}

// Row is one grid row. Ref, when set, lets the row be pinned to the sidebar
// or resolved as the "current" entity (transcript.EntityBlock).
type Row struct {
	Key    string
	Values map[string]any
	Ref    *session.EntityRef
}

// View selects what the grid's secondary area shows.
type View int

const (
	// ViewTable is the sortable/filterable table (the default).
	ViewTable View = iota
	// ViewCard shows the highlighted row as a vertical field list.
	ViewCard
	// ViewInspector shows the highlighted row's raw values.
	ViewInspector
)

func (v View) label() string {
	switch v {
	case ViewCard:
		return "Card"
	case ViewInspector:
		return "Inspector"
	default:
		return "Table"
	}
}

// RowActivatedMsg is emitted on Enter over the highlighted row (table view).
type RowActivatedMsg struct{ Row Row }

// Option configures a Model at construction time.
type Option func(*Model)

// WithTitle sets the grid's header title (defaults to "Result").
func WithTitle(title string) Option {
	return func(m *Model) { m.title = title }
}

// Model is a transcript.EntityBlock: a result grid with table/card/inspector
// views, sort, filter and a footer/scrollbar, driven by bubble-table.
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
}

// New builds a grid from columns and rows. Row order is preserved until the
// user sorts.
func New(columns []Column, rows []Row, opts ...Option) *Model {
	m := &Model{
		columns:    append([]Column(nil), columns...),
		rows:       append([]Row(nil), rows...),
		title:      "Result",
		sortColumn: -1,
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
		data := make(table.RowData, len(columns))
		for c := range columns {
			if c < len(cells[i]) {
				data[columnKey(c)] = cells[i][c]
			}
		}
		out[i] = table.NewRow(data)
	}
	return out
}

func formatRows(columns []Column, rows []Row) [][]string {
	cells := make([][]string, len(rows))
	for i, row := range rows {
		line := make([]string, len(columns))
		for c, col := range columns {
			if v, ok := row.Values[col.Name]; ok {
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

// CurrentIndex returns the display index of the highlighted row, or -1 when
// there are no rows.
func (m *Model) CurrentIndex() int {
	if len(m.rows) == 0 {
		return -1
	}
	return m.table.GetHighlightedRowIndex()
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
	var body string
	switch m.view {
	case ViewTable:
		body = m.table.View()
	case ViewCard:
		body = currentRowContent(m.columns, m.rows, m.CurrentIndex(), width, false)
	case ViewInspector:
		body = currentRowContent(m.columns, m.rows, m.CurrentIndex(), width, true)
	}
	return header + "\n" + body + "\n" + m.footer()
}

func (m *Model) headerLine(width int) string {
	views := []View{ViewTable, ViewCard, ViewInspector}
	styled := make([]string, len(views))
	for i, v := range views {
		style := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
		if v == m.view {
			style = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231"))
		}
		styled[i] = style.Render(strconv.Itoa(i+1) + " " + v.label())
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
		if v, ok := row.Values[col.Name]; ok {
			if v == nil {
				value = "NULL"
			} else if raw {
				value = sanitize(fmt.Sprintf("%#v", v))
			} else {
				value = sanitize(FormatValue(v))
			}
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
	if m.view == ViewTable {
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		return m, cmd
	}
	return m, nil
}
