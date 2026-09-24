package grid

import (
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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
	column, desc := m.SortState()
	if column != 0 || !desc {
		t.Fatalf("SortState() = %d,%v want 0,true", column, desc)
	}
}

func TestSKeySortsBySelectedColumn(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	m.SelectColumn(0)
	m.Update(tea.KeyPressMsg{Text: "s", Code: 's'})
	if m.rows[0].Values[0] != 2 {
		t.Fatalf("ascending first row after s = %+v", m.rows[0])
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

func TestColumnNavigationSelectsAndAutoScrolls(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	if m.SelectedColumn() != 0 {
		t.Fatalf("initial SelectedColumn() = %d, want 0", m.SelectedColumn())
	}
	m.Update(tea.KeyPressMsg{Text: "l", Code: 'l'})
	if m.SelectedColumn() != 1 {
		t.Fatalf("SelectedColumn() after l = %d, want 1", m.SelectedColumn())
	}
	m.Update(tea.KeyPressMsg{Text: "h", Code: 'h'})
	if m.SelectedColumn() != 0 {
		t.Fatalf("SelectedColumn() after h = %d, want 0", m.SelectedColumn())
	}
	// h at column 0 does not go negative.
	m.Update(tea.KeyPressMsg{Text: "h", Code: 'h'})
	if m.SelectedColumn() != 0 {
		t.Fatalf("SelectedColumn() clamps at 0, got %d", m.SelectedColumn())
	}
}

func TestSelectColumnClampsAndRebuilds(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	m.SelectColumn(99)
	if m.SelectedColumn() != len(cols)-1 {
		t.Fatalf("SelectColumn(99) = %d, want %d", m.SelectedColumn(), len(cols)-1)
	}
}

func TestCardAndInspectorViewsViaExtraViews(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows, WithExtraViews(CardView(""), InspectorView("")))
	m.Update(tea.KeyPressMsg{Text: "2", Code: '2'})
	if m.CurrentView() != View(firstExtraView) {
		t.Fatalf("view after 2 = %v, want first extra view", m.CurrentView())
	}
	view := ansi.Strip(m.View(40, true))
	if !strings.Contains(view, "id") {
		t.Fatalf("card view missing field name: %q", view)
	}
	m.Update(tea.KeyPressMsg{Text: "3", Code: '3'})
	if m.CurrentView() != View(firstExtraView+1) {
		t.Fatalf("view after 3 = %v, want second extra view", m.CurrentView())
	}
	raw := ansi.Strip(m.View(40, true))
	if !strings.Contains(raw, "2") {
		t.Fatalf("inspector view missing raw value: %q", raw)
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
	m := New(cols, rows, WithExtraViews(CardView("")))
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
	header := ansi.Strip(m.headerLine(60))
	if !strings.Contains(header, "2 Charts") {
		t.Fatalf("header missing extra view label: %q", header)
	}
	m.Update(tea.KeyPressMsg{Text: "2", Code: '2'})
	if m.CurrentView() != View(firstExtraView) {
		t.Fatalf("view after pressing 2 = %v, want first extra view", m.CurrentView())
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
	m.SetView(View(firstExtraView))
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if !handled {
		t.Fatal("ExtraView.Update did not see the key press")
	}
}

func TestKeyHandlerClaimsKeyFirst(t *testing.T) {
	cols, rows := sampleRows()
	var seen []string
	m := New(cols, rows, WithKeyHandler(func(m *Model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
		if msg.String() == "b" {
			seen = append(seen, "b")
			return nil, true
		}
		return nil, false
	}))
	m.Update(tea.KeyPressMsg{Text: "b", Code: 'b'})
	if len(seen) != 1 {
		t.Fatalf("KeyHandler did not see 'b': %v", seen)
	}
	// A key it declines still reaches the grid's own default handling.
	m.Update(tea.KeyPressMsg{Text: "s", Code: 's'})
	if col, _ := m.SortState(); col != 0 {
		t.Fatalf("declined key did not fall through to default sort: SortState col=%d", col)
	}
}

func TestFooterHookAppendsToBuiltinFooter(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows, WithFooterHook(func(m *Model, builtin string) string {
		return builtin + " • badge"
	}))
	view := ansi.Strip(m.View(60, true))
	if !strings.Contains(view, "badge") {
		t.Fatalf("footer hook text missing: %q", view)
	}
}

func TestStylePresetsChangeBorderColor(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows, WithStyle(StyleMinimal))
	if m.Style().Name != "Minimal" {
		t.Fatalf("Style().Name = %q, want Minimal", m.Style().Name)
	}
	m.SetStyle(StyleSoft)
	if m.Style().Name != "Soft" {
		t.Fatalf("Style().Name after SetStyle = %q, want Soft", m.Style().Name)
	}
}

func TestParseStyleFallsBackToLines(t *testing.T) {
	if got := ParseStyle("Soft"); got.Name != "Soft" {
		t.Fatalf("ParseStyle(Soft) = %q", got.Name)
	}
	if got := ParseStyle("unknown"); got.Name != "Lines" {
		t.Fatalf("ParseStyle(unknown) = %q, want Lines", got.Name)
	}
}

func TestIndexForKeySurvivesSort(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	if i := m.IndexForKey("0"); i != 1 {
		t.Fatalf("IndexForKey(0) before sort = %d, want 1", i)
	}
	m.Sort(0) // ascending by id: Key "1" (id 2) first, then Key "0" (id 10)
	if i := m.IndexForKey("0"); i != 1 {
		t.Fatalf("IndexForKey(0) after sort = %d, want 1", i)
	}
	if i := m.IndexForKey("missing"); i != -1 {
		t.Fatalf("IndexForKey(missing) = %d, want -1", i)
	}
}

func TestSelectRowClamps(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	m.SelectRow(99)
	if m.CurrentIndex() != len(rows)-1 {
		t.Fatalf("SelectRow(99) CurrentIndex() = %d, want %d", m.CurrentIndex(), len(rows)-1)
	}
	m.SelectRow(-5)
	if m.CurrentIndex() != 0 {
		t.Fatalf("SelectRow(-5) CurrentIndex() = %d, want 0", m.CurrentIndex())
	}
}

func TestCellReturnsFormattedText(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	if got := m.Cell(0, 1); got != "Prague" {
		t.Fatalf("Cell(0,1) = %q, want Prague", got)
	}
	if got := m.Cell(-1, 0); got != "" {
		t.Fatalf("Cell out of bounds = %q, want empty", got)
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
	m.SetView(View(firstExtraView)) // itself consults the layout once, to decide initial secondary focus
	layoutCalls = 0
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

func TestToggleSecondaryFocusIfSplit(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows,
		WithExtraViews(ExtraView{Label: "Charts", Render: func(m *Model, w, h int) string { return "" }}),
		WithSplitLayout(func(totalWidth, naturalWidth int, view View) SplitLayout {
			return SplitLayout{Split: true, PrimaryWidth: totalWidth / 2, SecondaryWidth: totalWidth / 2}
		}),
	)
	m.SetWidth(100)
	m.SetView(View(firstExtraView))
	if m.SecondaryFocus() {
		t.Fatal("SecondaryFocus() true right after SetView with a split layout")
	}
	if !m.ToggleSecondaryFocusIfSplit() {
		t.Fatal("ToggleSecondaryFocusIfSplit() = false, want true (view is split)")
	}
	if !m.SecondaryFocus() {
		t.Fatal("SecondaryFocus() false after toggling on")
	}
	// On the table view, toggling is always a no-op.
	m.SetView(ViewTable)
	if m.ToggleSecondaryFocusIfSplit() {
		t.Fatal("ToggleSecondaryFocusIfSplit() = true on the table view")
	}
}

func TestSetExtraViewsResetsViewIfOutOfRange(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows, WithExtraViews(CardView(""), InspectorView("")))
	m.SetView(View(firstExtraView + 1))
	m.SetExtraViews(CardView(""))
	if m.CurrentView() != ViewTable {
		t.Fatalf("CurrentView() after shrinking ExtraViews = %v, want ViewTable", m.CurrentView())
	}
}

func TestNaturalWidth(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	if got := m.NaturalWidth(); got <= 0 {
		t.Fatalf("NaturalWidth() = %d, want > 0", got)
	}
}

func TestSetFocusedTogglesRebuild(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	if m.Focused() {
		t.Fatal("Focused() true before SetFocused")
	}
	m.SetFocused(true)
	if !m.Focused() {
		t.Fatal("Focused() false after SetFocused(true)")
	}
}

func TestSimpleAccessors(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows, WithTitle("Cities"))
	m.SetWidth(50)
	if m.Width() != 50 {
		t.Fatalf("Width() = %d, want 50", m.Width())
	}
	if len(m.Columns()) != len(cols) {
		t.Fatalf("Columns() len = %d, want %d", len(m.Columns()), len(cols))
	}
	if len(m.Rows()) != len(rows) {
		t.Fatalf("Rows() len = %d, want %d", len(m.Rows()), len(rows))
	}
}

func TestExtraViewsGetterAndSetters(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows, WithExtraViews(CardView("")))
	if len(m.ExtraViews()) != 1 {
		t.Fatalf("ExtraViews() len = %d, want 1", len(m.ExtraViews()))
	}
	claimed := false
	m.SetKeyHandler(func(m *Model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
		if msg.String() == "z" {
			claimed = true
			return nil, true
		}
		return nil, false
	})
	m.Update(tea.KeyPressMsg{Text: "z", Code: 'z'})
	if !claimed {
		t.Fatal("SetKeyHandler's hook did not see 'z'")
	}
	appended := false
	m.SetFooterHook(func(m *Model, builtin string) string {
		appended = true
		return builtin
	})
	m.View(60, true)
	if !appended {
		t.Fatal("SetFooterHook's hook was not called")
	}
}

func TestCardViewScrollsWithManyColumns(t *testing.T) {
	cols := make([]Column, 20)
	values := make([]any, 20)
	for i := range cols {
		cols[i] = Column{Name: "col" + strconv.Itoa(i)}
		values[i] = "v" + strconv.Itoa(i)
	}
	rows := []Row{{Key: "0", Values: values}}
	m := New(cols, rows, WithExtraViews(CardView("")), WithMaxVisibleRows(4))
	m.SetView(View(firstExtraView))
	first := ansi.Strip(m.View(40, true))
	if strings.Contains(first, "col19") {
		t.Fatalf("first page should not show the last column yet: %q", first)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	scrolled := ansi.Strip(m.View(40, true))
	if scrolled == first {
		t.Fatal("down did not scroll the card view")
	}
	// Scrolling back up returns toward the top.
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	backAtTop := ansi.Strip(m.View(40, true))
	if backAtTop != first {
		t.Fatalf("scrolling back to the top did not match the original page:\nfirst=%q\nback=%q", first, backAtTop)
	}
}

func TestSetTitleAndTitle(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows, WithTitle("Cities"))
	if m.Title() != "Cities" {
		t.Fatalf("Title() = %q, want Cities", m.Title())
	}
	m.SetTitle("changed · Cities")
	if m.Title() != "changed · Cities" {
		t.Fatalf("Title() after SetTitle = %q", m.Title())
	}
	if view := ansi.Strip(m.View(60, true)); !strings.Contains(view, "changed") {
		t.Fatalf("view missing updated title: %q", view)
	}
}

func TestColumnAlignment(t *testing.T) {
	textColumn := Column{Name: "Customer"}
	numberColumn := Column{Name: "Total", Numeric: true}
	for _, selected := range []bool{false, true} {
		if got := columnStyle(textColumn, selected).GetAlignHorizontal(); got != lipgloss.Left {
			t.Fatalf("text alignment selected=%v = %v, want left", selected, got)
		}
		if got := columnStyle(numberColumn, selected).GetAlignHorizontal(); got != lipgloss.Right {
			t.Fatalf("numeric alignment selected=%v = %v, want right", selected, got)
		}
	}
}

func TestCardViewResetsScrollWhenHighlightedRowChanges(t *testing.T) {
	cols := make([]Column, 10)
	rows := make([]Row, 2)
	for r := range rows {
		values := make([]any, 10)
		for c := range cols {
			cols[c] = Column{Name: "col" + strconv.Itoa(c)}
			values[c] = "row" + strconv.Itoa(r) + "-v" + strconv.Itoa(c)
		}
		rows[r] = Row{Key: strconv.Itoa(r), Values: values}
	}
	m := New(cols, rows, WithExtraViews(CardView("")), WithMaxVisibleRows(4))
	m.SetView(View(firstExtraView))
	first := ansi.Strip(m.View(40, true))
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // scroll the card
	scrolled := ansi.Strip(m.View(40, true))
	if scrolled == first {
		t.Fatal("card did not scroll")
	}
	m.SelectRow(1) // change the highlighted row
	afterRowChange := ansi.Strip(m.View(40, true))
	if !strings.Contains(afterRowChange, "col0") {
		t.Fatalf("card did not reset to the top after the highlighted row changed: %q", afterRowChange)
	}
}
