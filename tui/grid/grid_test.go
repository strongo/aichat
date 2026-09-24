package grid

import (
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/strongo/aichat/ai/session"
	"github.com/strongo/aichat/tui"
)

func sampleRows() ([]Column, []Row) {
	cols := []Column{{Name: "id", Numeric: true}, {Name: "name"}}
	rows := []Row{
		{Key: "1", Values: []any{2, "Prague"}, Ref: &session.EntityRef{Type: "city", Keys: map[string]string{"id": "2"}}},
		{Key: "0", Values: []any{10, "Vienna"}, Ref: &session.EntityRef{Type: "city", Keys: map[string]string{"id": "10"}}},
	}
	return cols, rows
}

func TestNewAndView(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows, WithTitle("Cities"))
	m.SetWidth(60)
	view := ansi.Strip(m.View(60, true))
	if !strings.Contains(view, "Cities") {
		t.Fatalf("view missing title: %q", view)
	}
	if !strings.Contains(view, "Prague") || !strings.Contains(view, "Vienna") {
		t.Fatalf("view missing rows: %q", view)
	}
}

func TestFormatValue(t *testing.T) {
	if got := FormatValue(nil); got != "NULL" {
		t.Errorf("nil = %q", got)
	}
	if got := FormatValue(1.5); got != "1.5" {
		t.Errorf("float = %q", got)
	}
	if got := FormatValue([]byte("hi")); got != "hi" {
		t.Errorf("utf8 bytes = %q", got)
	}
}

func TestSortTogglesAscDesc(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	m.Sort(0) // ascending by id
	if m.rows[0].Values[0] != 2 {
		t.Fatalf("ascending first row = %+v", m.rows[0])
	}
	m.Sort(0) // descending
	if m.rows[0].Values[0] != 10 {
		t.Fatalf("descending first row = %+v", m.rows[0])
	}
}

func TestCurrentReturnsHighlightedRowRef(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	ref := m.Current()
	if ref == nil || !ref.Same(*rows[0].Ref) {
		t.Fatalf("Current() = %v, want %v", ref, rows[0].Ref)
	}
}

func TestFocusable(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	if !m.Focusable() {
		t.Fatal("grid should be focusable")
	}
}

func TestUpdateEnterEmitsRowActivated(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	_, cmd := m.Update(tea.KeyPressMsg{Text: "", Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter did not return a command")
	}
	msg := cmd()
	activated, ok := msg.(RowActivatedMsg)
	if !ok {
		t.Fatalf("msg = %#v, want RowActivatedMsg", msg)
	}
	if activated.Row.Key != rows[0].Key {
		t.Fatalf("activated row = %+v, want %+v", activated.Row, rows[0])
	}
}

func TestUpdatePlusEmitsAddToSidebar(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	_, cmd := m.Update(tea.KeyPressMsg{Text: "+", Code: '+'})
	if cmd == nil {
		t.Fatal("+ did not return a command")
	}
	msg := cmd()
	added, ok := msg.(tui.AddToSidebarMsg)
	if !ok {
		t.Fatalf("msg = %#v, want tui.AddToSidebarMsg", msg)
	}
	if !added.Ref.Same(*rows[0].Ref) {
		t.Fatalf("added ref = %+v, want %+v", added.Ref, rows[0].Ref)
	}
}

func TestUpdateViewSwitch(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	m.Update(tea.KeyPressMsg{Text: "2", Code: '2'})
	if m.view != ViewCard {
		t.Fatalf("view = %v, want ViewCard", m.view)
	}
	view := ansi.Strip(m.View(40, true))
	if !strings.Contains(view, "id") {
		t.Fatalf("card view missing field name: %q", view)
	}
	m.Update(tea.KeyPressMsg{Text: "3", Code: '3'})
	if m.view != ViewInspector {
		t.Fatalf("view = %v, want ViewInspector", m.view)
	}
}

func TestUpdateTabCyclesSortTarget(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	if m.sortTarget != 0 {
		t.Fatalf("initial sortTarget = %d, want 0", m.sortTarget)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.sortTarget != 1 {
		t.Fatalf("sortTarget after Tab = %d, want 1", m.sortTarget)
	}
}

func TestCurrentRowContentNoRows(t *testing.T) {
	m := New(nil, nil)
	got := currentRowContent(m.columns, m.rows, -1, 20, false)
	if got != "No current row." {
		t.Fatalf("got %q", got)
	}
}

func TestEmptyGridCurrentIndex(t *testing.T) {
	m := New(nil, nil)
	if m.CurrentIndex() != -1 {
		t.Fatalf("CurrentIndex() = %d, want -1", m.CurrentIndex())
	}
	if m.Current() != nil {
		t.Fatal("Current() non-nil on empty grid")
	}
}

func TestRowValuesAbsentVsNull(t *testing.T) {
	cols := []Column{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	rows := []Row{{Key: "0", Values: []any{"x", nil, Absent}}}
	m := New(cols, rows)
	if m.cells[0][0] != "x" {
		t.Fatalf("present value cell = %q", m.cells[0][0])
	}
	if m.cells[0][1] != "NULL" {
		t.Fatalf("nil value cell = %q, want NULL", m.cells[0][1])
	}
	if m.cells[0][2] != "" {
		t.Fatalf("absent value cell = %q, want empty", m.cells[0][2])
	}
	card := currentRowContent(m.columns, m.rows, 0, 40, false)
	if !strings.Contains(card, "NULL") {
		t.Fatalf("card view missing NULL: %q", card)
	}
	if !strings.Contains(card, "—") {
		t.Fatalf("card view missing absent marker: %q", card)
	}
}

// TestCurrentIndexHonoursFilter is the regression test for the B2 bug:
// bubble-table's cursor indexes the filtered (visible) rows, not the
// unfiltered Model.rows slice, so CurrentIndex/Current/CurrentRow must
// resolve through the highlighted row's hidden source-index metadata, not
// the raw cursor position. With "Gamma" filtered in, the only visible row
// sits at cursor index 0 but is source row 2; the old code returned
// m.rows[0] ("Alpha") instead of m.rows[2] ("Gamma").
func TestCurrentIndexHonoursFilter(t *testing.T) {
	cols := []Column{{Name: "name"}}
	rows := []Row{
		{Key: "0", Values: []any{"Alpha"}, Ref: &session.EntityRef{Type: "letter", Keys: map[string]string{"id": "0"}}},
		{Key: "1", Values: []any{"Beta"}, Ref: &session.EntityRef{Type: "letter", Keys: map[string]string{"id": "1"}}},
		{Key: "2", Values: []any{"Gamma"}, Ref: &session.EntityRef{Type: "letter", Keys: map[string]string{"id": "2"}}},
	}
	m := New(cols, rows)
	m.SetWidth(40)

	m.table = m.table.Focused(true)
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(tea.KeyPressMsg{Text: "/", Code: '/'})
	_ = cmd
	if !m.CapturesEsc() {
		t.Fatal("CapturesEsc() = false while filter input is focused")
	}
	for _, r := range "Gamma" {
		m.table, cmd = m.table.Update(tea.KeyPressMsg{Text: string(r), Code: r})
		_ = cmd
	}

	i := m.CurrentIndex()
	if i != 2 {
		t.Fatalf("CurrentIndex() under filter = %d, want 2 (source index of the only visible row, \"Gamma\")", i)
	}
	current, ok := m.CurrentRow()
	if !ok || current.Key != "2" {
		t.Fatalf("CurrentRow() under filter = %+v, want Key=2", current)
	}
	if ref := m.Current(); ref == nil || !ref.Same(*rows[2].Ref) {
		t.Fatalf("Current() under filter = %v, want %v", ref, rows[2].Ref)
	}
}

func TestMaxVisibleRowsCapsPageSize(t *testing.T) {
	cols := []Column{{Name: "n", Numeric: true}}
	rows := make([]Row, 50)
	for i := range rows {
		rows[i] = Row{Key: string(rune('a' + i%26)), Values: []any{i}}
	}
	m := New(cols, rows, WithMaxVisibleRows(5))
	m.SetWidth(20)
	view := ansi.Strip(m.View(20, true))
	// The 5-row page never reaches the tail of a 50-row result.
	if strings.Contains(view, "49") {
		t.Fatalf("view rendered rows beyond a 5-row page size cap: %q", view)
	}
}

func TestDefaultMaxVisibleRowsIsApplied(t *testing.T) {
	cols := []Column{{Name: "n", Numeric: true}}
	rows := make([]Row, DefaultMaxVisibleRows+20)
	for i := range rows {
		rows[i] = Row{Key: string(rune('a' + i%26)), Values: []any{i}}
	}
	m := New(cols, rows)
	m.SetWidth(20)
	view := ansi.Strip(m.View(20, true))
	last := strconv.Itoa(len(rows) - 1)
	if strings.Contains(view, last) {
		t.Fatalf("view rendered rows beyond the default page size cap: %q", view)
	}
}

func TestExtraViewsRegisterAndSwitch(t *testing.T) {
	cols, rows := sampleRows()
	rendered := false
	extra := ExtraView{
		Label: "Charts",
		Render: func(m *Model, width, height int) string {
			rendered = true
			return "chart body"
		},
	}
	m := New(cols, rows, WithExtraViews(extra))
	m.SetWidth(60)
	header := ansi.Strip(m.headerLine(60))
	if !strings.Contains(header, "4 Charts") {
		t.Fatalf("header missing extra view label: %q", header)
	}
	m.Update(tea.KeyPressMsg{Text: "4", Code: '4'})
	if m.view != View(firstExtraView) {
		t.Fatalf("view after pressing 4 = %v, want first extra view", m.view)
	}
	view := ansi.Strip(m.View(60, true))
	if !rendered || !strings.Contains(view, "chart body") {
		t.Fatalf("extra view was not rendered: %q", view)
	}
}

func TestExtraViewUpdateHandlesKeys(t *testing.T) {
	cols, rows := sampleRows()
	handled := false
	extra := ExtraView{
		Label:  "Charts",
		Render: func(m *Model, width, height int) string { return "" },
		Update: func(m *Model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
			if msg.String() == "down" {
				handled = true
				return nil, true
			}
			return nil, false
		},
	}
	m := New(cols, rows, WithExtraViews(extra))
	m.view = View(firstExtraView)
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if !handled {
		t.Fatal("ExtraView.Update did not see the key press")
	}
}

func TestSplitLayoutSideBySideWhenRoomy(t *testing.T) {
	cols, rows := sampleRows()
	var gotView View
	layoutCalls := 0
	m := New(cols, rows,
		WithExtraViews(ExtraView{
			Label: "Charts",
			Render: func(m *Model, width, height int) string {
				return strings.Repeat("x", width)
			},
		}),
		WithSplitLayout(func(totalWidth, naturalWidth int, view View) SplitLayout {
			layoutCalls++
			gotView = view
			if totalWidth < 80 {
				return SplitLayout{}
			}
			return SplitLayout{Split: true, PrimaryWidth: 40, SecondaryWidth: totalWidth - 40}
		}),
	)
	m.view = View(firstExtraView)
	body := m.body(100)
	if layoutCalls != 1 || gotView != View(firstExtraView) {
		t.Fatalf("layout func calls=%d view=%v", layoutCalls, gotView)
	}
	if !strings.Contains(body, strings.Repeat("x", 60)) {
		t.Fatalf("split body missing secondary content at expected width: %q", body)
	}

	// Below the layout's own threshold, it reports no split: full width.
	narrow := m.body(40)
	if strings.Contains(narrow, "x") && strings.Count(narrow, "x") != 40 {
		t.Fatalf("unsplit body width mismatch: %q", narrow)
	}
}

func TestNaturalWidth(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	if got := m.NaturalWidth(); got <= 0 {
		t.Fatalf("NaturalWidth() = %d, want > 0", got)
	}
}
