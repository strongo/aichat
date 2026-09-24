package grid

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

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
	// The secondary pane now gets its own bordered card (M4): a 1-cell gap
	// plus its own 2-cell left/right border eat into the 60 cells
	// SplitLayout allotted it, leaving 57 for the ExtraView's own content.
	plain := ansi.Strip(body)
	if !strings.Contains(plain, strings.Repeat("x", 57)) {
		t.Fatalf("split body missing secondary content at expected width: %q", plain)
	}
	if !strings.Contains(plain, "Charts") {
		t.Fatalf("secondary card missing its title: %q", plain)
	}
	if !strings.Contains(plain, "╭") || !strings.Contains(plain, "╯") {
		t.Fatalf("secondary card missing its own border: %q", plain)
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

func TestRowNavigationDoesNotWrap(t *testing.T) {
	cols := []Column{{Name: "n", Numeric: true}}
	rows := make([]Row, 5)
	for i := range rows {
		rows[i] = Row{Key: strconv.Itoa(i), Values: []any{i}}
	}
	m := New(cols, rows)
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.CurrentIndex() != 0 {
		t.Fatalf("up at first row wrapped to %d", m.CurrentIndex())
	}
	for range rows {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if m.CurrentIndex() != len(rows)-1 {
		t.Fatalf("down repeated past the last row = %d, want %d", m.CurrentIndex(), len(rows)-1)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.CurrentIndex() != len(rows)-1 {
		t.Fatalf("down at last row wrapped to %d", m.CurrentIndex())
	}
}

// TestWithInitialSortSeedsToggleDirection is the regression test for
// DataTug's docked-view sort toggle: a product that re-fetches externally
// pre-sorted rows (rather than calling Sort itself) still needs the grid to
// know which column/direction that is, so the NEXT Sort(column) call (e.g.
// from a subsequent "s" keypress translated into another external re-fetch)
// computes the opposite direction instead of always defaulting to ascending.
func TestWithInitialSortSeedsToggleDirection(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows, WithInitialSort(0, false))
	column, desc := m.SortState()
	if column != 0 || desc {
		t.Fatalf("SortState() = %d,%v want 0,false", column, desc)
	}
	if !strings.Contains(ansi.Strip(m.View(60, true)), "sort id ↑") {
		t.Fatalf("footer missing seeded ascending sort indicator: %q", ansi.Strip(m.View(60, true)))
	}
}

// typeFilter opens the built-in filter (if not already open) and types text
// into it, mirroring how a user reaches a filtered state interactively.
func typeFilter(t *testing.T, m *Model, text string) {
	t.Helper()
	m.table = m.table.Focused(true)
	if !m.table.GetIsFilterInputFocused() {
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(tea.KeyPressMsg{Text: "/", Code: '/'})
		_ = cmd
	}
	for _, r := range text {
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(tea.KeyPressMsg{Text: string(r), Code: r})
		_ = cmd
	}
	// Blur the filter input (Escape) so Down/Up reach the grid's own
	// row-navigation handling instead of being consumed as filter text.
	m.table, _ = m.table.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
}

// TestRowNavigationUnderFilterMovesByVisiblePosition is the regression test
// for the S1 bug (introduced by 65dfdfd, the up/down non-wrap fix): Update's
// up/k/down cases computed "CurrentIndex() ± 1" — a SOURCE row index — and
// passed it to SelectRow, which (before this fix) forwarded it straight to
// bubble-table's WithHighlightedRow, a POSITION within the filtered/visible
// row set. With rows 0/2/4 visible (1/3 filtered out), adjacent visible
// positions are NOT adjacent source indices, so navigation skipped rows,
// got stuck, or landed on a filtered-out row. Down must move by one visible
// position (clamped, no wrap) and so must Up.
func TestRowNavigationUnderFilterMovesByVisiblePosition(t *testing.T) {
	cols := []Column{{Name: "name"}}
	rows := []Row{
		{Key: "0", Values: []any{"Alpha0"}},
		{Key: "1", Values: []any{"Beta1"}},
		{Key: "2", Values: []any{"Alpha2"}},
		{Key: "3", Values: []any{"Beta3"}},
		{Key: "4", Values: []any{"Alpha4"}},
	}
	m := New(cols, rows)
	m.SetWidth(40)
	typeFilter(t, m, "Alpha")
	if n := len(m.table.GetVisibleRows()); n != 3 {
		t.Fatalf("visible rows after filter = %d, want 3", n)
	}

	var got []int
	got = append(got, m.CurrentIndex())
	for range 3 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		got = append(got, m.CurrentIndex())
	}
	for range 3 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		got = append(got, m.CurrentIndex())
	}
	want := []int{0, 2, 4, 4, 2, 0, 0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CurrentIndex() sequence = %v, want %v", got, want)
	}
}

// TestRebuildTablePreservesFilterAndHighlightedSource is the regression test
// for the other half of the S1 bug: rebuildTable (called by SelectColumn/
// SetWidth/SetFocused/Sort and anything else that reconstructs the inner
// bubble-table) built a brand new table.Model with no filter text at all —
// silently clearing an active filter — and then reused the OLD table's raw
// cursor POSITION as if it were valid in the new (now unfiltered, larger)
// visible set, landing the highlight on an arbitrary, usually wrong, row.
func TestRebuildTablePreservesFilterAndHighlightedSource(t *testing.T) {
	cols := []Column{{Name: "name"}}
	rows := []Row{
		{Key: "0", Values: []any{"Alpha0"}},
		{Key: "1", Values: []any{"Beta1"}},
		{Key: "2", Values: []any{"Alpha2"}},
		{Key: "3", Values: []any{"Beta3"}},
		{Key: "4", Values: []any{"Alpha4"}},
	}
	m := New(cols, rows)
	m.SetWidth(40)
	typeFilter(t, m, "Alpha")
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // source 0 -> source 2

	for _, step := range []struct {
		name string
		do   func()
	}{
		{"SetWidth", func() { m.SetWidth(50) }},
		{"SelectColumn (h/l)", func() { m.SelectColumn(0) }},
		{"SetFocused", func() { m.SetFocused(!m.Focused()) }},
		{"Sort", func() { m.Sort(0); m.Sort(0) }}, // sort then re-sort back to original order
	} {
		if n := len(m.table.GetVisibleRows()); n != 3 {
			t.Fatalf("%s: visible rows before = %d, want 3", step.name, n)
		}
		step.do()
		if got := m.table.GetCurrentFilter(); got != "Alpha" {
			t.Fatalf("%s: filter text = %q, want %q (rebuild dropped it)", step.name, got, "Alpha")
		}
		if n := len(m.table.GetVisibleRows()); n != 3 {
			t.Fatalf("%s: visible rows after = %d, want 3 (filter should still apply)", step.name, n)
		}
		if i := m.CurrentIndex(); i != 2 {
			t.Fatalf("%s: CurrentIndex() after rebuild = %d, want 2 (highlighted source row must survive)", step.name, i)
		}
	}
}

// TestJKeyDefaultsToRowDownUnlessKeyHandlerClaimsIt is the regression test
// for n8: "j" is now a default RowDown binding (see gridKeyMap), but a
// product's WithKeyHandler is checked first and can still claim it for its
// own purpose (e.g. DataTug's join-candidate navigation) by reporting
// handled=true.
func TestJKeyDefaultsToRowDownUnlessKeyHandlerClaimsIt(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	m.Update(tea.KeyPressMsg{Text: "j", Code: 'j'})
	if i := m.CurrentIndex(); i != 1 {
		t.Fatalf("CurrentIndex() after j with no KeyHandler = %d, want 1 (row moved down)", i)
	}

	m2 := New(cols, rows, WithKeyHandler(func(*Model, tea.KeyPressMsg) (tea.Cmd, bool) {
		return nil, true // claims every key, including "j"
	}))
	m2.Update(tea.KeyPressMsg{Text: "j", Code: 'j'})
	if i := m2.CurrentIndex(); i != 0 {
		t.Fatalf("CurrentIndex() after j with a claiming KeyHandler = %d, want 0 (KeyHandler owned it)", i)
	}
}

// TestSplitLayoutFilterFocusSurvivesRender is the regression test for M1:
// bubble-table v0.23.0's WithFilterInputValue always blurs the filter
// input, and tableViewAt used to rebuild the whole table (mutating m.table)
// on every single View() call whenever a split layout was active — since
// layout.PrimaryWidth is essentially never equal to m.width. That blurred
// the real filter on the very next render after "/" opened it, so the next
// keystroke fell through to the grid's own key handling (e.g. "s" sorts,
// "j" moves) instead of extending the filter text. tableViewAt must render
// a split's primary pane without touching the real table/filter at all.
func TestSplitLayoutFilterFocusSurvivesRender(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows,
		WithExtraViews(ExtraView{Label: "Charts", Render: func(m *Model, w, h int) string { return "chart" }}),
		WithSplitLayout(func(totalWidth, naturalWidth int, view View) SplitLayout {
			return SplitLayout{Split: true, PrimaryWidth: 20, SecondaryWidth: totalWidth - 20}
		}),
	)
	m.SetView(View(firstExtraView))
	m.SetFocused(true) // before the filter opens, so this rebuild (a legitimate one) doesn't need to restore anything
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(tea.KeyPressMsg{Text: "/", Code: '/'})
	_ = cmd
	if !m.table.GetIsFilterInputFocused() {
		t.Fatal("filter did not focus on /")
	}

	// Render at the SAME width/focused View was last called with (via
	// SetFocused/New, both m.width==80): the top-level width/focused check
	// in View() sees no change and skips its own rebuild, isolating what's
	// under test — the split layout's tableViewAt path, which must not
	// touch the real table/filter as a side effect of rendering the
	// primary pane at a different (narrower) width.
	_ = m.View(m.Width(), m.Focused())
	if !m.table.GetIsFilterInputFocused() {
		t.Fatal("filter lost focus after View() with a split layout active")
	}

	m.table, cmd = m.table.Update(tea.KeyPressMsg{Text: "A", Code: 'A'})
	_ = cmd
	if got := m.table.GetCurrentFilter(); got != "A" {
		t.Fatalf("filter text after typing A = %q, want %q (keystroke reached the filter, not grid key handling)", got, "A")
	}
}

// TestFilterFocusSurvivesResizeWhileTyping is the other regression test for
// M1: a resize (SetWidth/View with a new width) legitimately calls
// rebuildTable, which builds a brand-new bubble-table with no filter focus
// of its own (WithFilterInputValue always blurs). rebuildTable must
// explicitly restore focus when the filter was focused going in.
func TestFilterFocusSurvivesResizeWhileTyping(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows)
	m.SetWidth(60)
	m.SetFocused(true) // the Model's own focus, not just m.table's — rebuildTable rebuilds the table from THIS, and an unfocused table drops all Update()s, filter included
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(tea.KeyPressMsg{Text: "/", Code: '/'})
	_ = cmd
	for _, r := range "Pr" {
		m.table, cmd = m.table.Update(tea.KeyPressMsg{Text: string(r), Code: r})
		_ = cmd
	}
	if !m.table.GetIsFilterInputFocused() {
		t.Fatal("filter did not focus/accept text before resize")
	}

	m.SetWidth(45) // triggers rebuildTable directly (width changed)

	if got := m.table.GetCurrentFilter(); got != "Pr" {
		t.Fatalf("filter text after resize = %q, want %q", got, "Pr")
	}
	if !m.table.GetIsFilterInputFocused() {
		t.Fatal("filter lost focus after a resize while typing")
	}

	m.table, cmd = m.table.Update(tea.KeyPressMsg{Text: "a", Code: 'a'})
	_ = cmd
	if got := m.table.GetCurrentFilter(); got != "Pra" {
		t.Fatalf("filter text after typing post-resize = %q, want %q", got, "Pra")
	}
}

// TestSplitLayoutTableShowsHighlightedRowAtPageBoundary is the regression
// test for B1: a split layout's combined primary(table)+secondary content
// used to be clamped to paneHeight() total lines regardless of the table's
// own naturally-taller rendering (a header line plus paneHeight() data
// rows, once its header/data separator is stripped) — silently clipping
// the table's own bottommost row(s), including the highlighted one, off
// the page. With WithMaxVisibleRows(10) and the cursor moved down to row
// index 9 (the last row of the first 10-row page), switching to a split
// secondary view must still show row 9 in the table pane.
func TestSplitLayoutTableShowsHighlightedRowAtPageBoundary(t *testing.T) {
	cols := []Column{{Name: "n"}}
	rows := make([]Row, 20)
	for i := range rows {
		rows[i] = Row{Key: strconv.Itoa(i), Values: []any{"Row" + strconv.Itoa(i)}}
	}
	m := New(cols, rows,
		WithMaxVisibleRows(10),
		WithExtraViews(ExtraView{Label: "Charts", Render: func(m *Model, w, h int) string { return "chart" }}),
		WithSplitLayout(func(totalWidth, naturalWidth int, view View) SplitLayout {
			return SplitLayout{Split: true, PrimaryWidth: 60, SecondaryWidth: totalWidth - 60}
		}),
	)
	for range 9 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if i := m.CurrentIndex(); i != 9 {
		t.Fatalf("CurrentIndex() after 9x down = %d, want 9", i)
	}
	m.Update(tea.KeyPressMsg{Text: "2", Code: '2'})
	if m.CurrentView() != View(firstExtraView) {
		t.Fatalf("CurrentView() after 2 = %v, want first extra view", m.CurrentView())
	}
	view := ansi.Strip(m.View(160, true))
	if !strings.Contains(view, "Row9") {
		t.Fatalf("highlighted row 9 clipped from split table pane:\n%s", view)
	}
}

// TestWithFilterDisabledBlocksSlash is the regression test for M3: a grid
// built with WithFilterDisabled() must not open bubble-table's built-in
// filter on "/" at all — restoring DataTug's pre-adoption main behaviour,
// where "/" is deliberately not a filter shortcut for grids like bookmark/
// dock/parameter-lookup views that already show a narrow, purpose-built
// row set.
func TestWithFilterDisabledBlocksSlash(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows, WithFilterDisabled())
	m.table = m.table.Focused(true)
	m.table, _ = m.table.Update(tea.KeyPressMsg{Text: "/", Code: '/'})
	if m.table.GetIsFilterInputFocused() {
		t.Fatal("/ opened the filter despite WithFilterDisabled()")
	}
	if m.CapturesEsc() {
		t.Fatal("CapturesEsc() true despite the filter never having opened")
	}
}

// TestSecondaryCardShowsFocusBullet is the regression test for M4: a split
// layout's secondary pane gets its own bordered card with a ●/○ focus
// bullet reflecting m.secondaryFocus specifically — distinct from the
// primary (table) pane's own focus cue, the outer card m.View wraps
// everything in — so a user can tell which of the two panes Tab currently
// routes keys to.
func TestSecondaryCardShowsFocusBullet(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows,
		WithExtraViews(ExtraView{Label: "Charts", Render: func(m *Model, w, h int) string { return "" }}),
		WithSplitLayout(func(totalWidth, naturalWidth int, view View) SplitLayout {
			return SplitLayout{Split: true, PrimaryWidth: totalWidth / 2, SecondaryWidth: totalWidth / 2}
		}),
	)
	m.SetFocused(true)
	m.SetView(View(firstExtraView))

	unfocused := ansi.Strip(m.View(100, true))
	if !strings.Contains(unfocused, "○ Charts") {
		t.Fatalf("secondary card should show the inactive bullet while table has focus: %q", unfocused)
	}
	if strings.Contains(unfocused, "● Charts") {
		t.Fatalf("secondary card should not show the active bullet while table has focus: %q", unfocused)
	}

	m.ToggleSecondaryFocusIfSplit()
	focused := ansi.Strip(m.View(100, true))
	if !strings.Contains(focused, "● Charts") {
		t.Fatalf("secondary card should show the active bullet once it has focus: %q", focused)
	}
}

// TestSortLargeIntegersExactly, TestNumericSortKeepsEqualValuesStable and
// TestSortHandlesPartialRows are M5: 3 of the 4 sort tests ported from
// DataTug's pre-adoption GridModel.Sort test suite (pkg/chat/grid_test.go)
// when that logic moved into Model.Sort — the 4th
// (TestGridModelSortTogglesAndHandlesEmpty) is already covered here by
// TestSortTogglesAscDesc.

// TestSortLargeIntegersExactly guards against float64 precision loss:
// Sort's numeric comparator must use big.Rat on the exact display string,
// not a float64 conversion, or values above 2^53 (where float64 can no
// longer represent every integer exactly) can compare equal or invert.
func TestSortLargeIntegersExactly(t *testing.T) {
	cols := []Column{{Name: "id", Numeric: true}}
	rows := []Row{
		{Key: "a", Values: []any{int64(9007199254740993)}}, // 2^53 + 1
		{Key: "b", Values: []any{int64(9007199254740992)}}, // 2^53
	}
	m := New(cols, rows)
	m.Sort(0) // ascending
	if m.rows[0].Values[0] != int64(9007199254740992) {
		t.Fatalf("ascending rows = %+v", m.rows)
	}
	m.Sort(0) // descending
	if m.rows[0].Values[0] != int64(9007199254740993) {
		t.Fatalf("descending rows = %+v", m.rows)
	}
}

// TestNumericSortKeepsEqualValuesStable: "2" and "2.0" are the same numeric
// value under big.Rat comparison but different display text; sort.SliceStable
// must keep their original relative order rather than treating a big.Rat
// tie as license to reorder them.
func TestNumericSortKeepsEqualValuesStable(t *testing.T) {
	cols := []Column{{Name: "n", Numeric: true}}
	rows := []Row{
		{Key: "a", Values: []any{"2"}},
		{Key: "b", Values: []any{"2.0"}},
		{Key: "c", Values: []any{"10"}},
	}
	m := New(cols, rows)
	m.Sort(0) // ascending
	m.Sort(0) // descending
	if m.rows[0].Values[0] != "10" || m.rows[1].Values[0] != "2" || m.rows[2].Values[0] != "2.0" {
		t.Fatalf("descending stable rows = %+v", m.rows)
	}
}

// TestSortHandlesPartialRows is the generalised equivalent of DataTug's
// RawRows-desync guard: a row with fewer Values than columns (Row.value
// returns Absent for the missing ones, sanitized to an empty display cell)
// must sort without panicking, and — the actual regression the original
// test caught — reordering must never desync a row's Key from its own
// Values (each row carries both together, unlike the pre-adoption
// GridModel's parallel Rows/RawRows arrays that could drift apart).
func TestSortHandlesPartialRows(t *testing.T) {
	cols := []Column{{Name: "n", Numeric: true}}
	rows := []Row{
		{Key: "a", Values: []any{2}},
		{Key: "b", Values: []any{}}, // partial/sparse: no value for column 0
		{Key: "c", Values: []any{1}},
	}
	m := New(cols, rows)
	m.Sort(0) // ascending: "" (absent) sorts before any numeric text
	if m.rows[0].Key != "b" || m.rows[1].Key != "c" || m.rows[2].Key != "a" {
		t.Fatalf("partial-row sort order = %+v", m.rows)
	}
	for _, row := range m.rows {
		switch row.Key {
		case "a":
			if row.value(0) != 2 {
				t.Fatalf("row a lost its value after sort: %+v", row)
			}
		case "c":
			if row.value(0) != 1 {
				t.Fatalf("row c lost its value after sort: %+v", row)
			}
		case "b":
			if row.value(0) != Absent {
				t.Fatalf("row b should stay Absent after sort: %+v", row)
			}
		}
	}
}

// TestHeaderTitleTrimmedBeforeBorder is the regression test for m7: a long
// title truncated by headerLine (padded to width-2 for the focus bullet)
// used to carry its own trailing padding into card()'s second truncation
// pass (which re-fits the bulleted title against the border, 2 cells
// narrower still), leaving a stray "… " immediately before the top-right
// corner instead of a clean "…╮".
func TestHeaderTitleTrimmedBeforeBorder(t *testing.T) {
	cols, rows := sampleRows()
	m := New(cols, rows, WithTitle(strings.Repeat("very long generated title ", 8)))
	top := strings.Split(ansi.Strip(m.View(22, true)), "\n")[0]
	if !strings.HasSuffix(top, "╮") {
		t.Fatalf("top border should end at the corner: %q", top)
	}
	if strings.HasSuffix(strings.TrimSuffix(top, "╮"), " ") {
		t.Fatalf("stray padding between the truncated title and the border corner: %q", top)
	}
}

// TestCardViewPagingAndHomeEndKeys is part of m8: the current-row card
// view must support pgup/pgdown/home/end scrolling for a row with many
// fields, not just up/down by 2 lines at a time.
func TestCardViewPagingAndHomeEndKeys(t *testing.T) {
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
	m.Update(tea.KeyPressMsg{Text: "end", Code: tea.KeyEnd})
	end := ansi.Strip(m.View(40, true))
	if end == first {
		t.Fatal("end did not scroll the card view")
	}
	if !strings.Contains(end, "col19") {
		t.Fatalf("end did not reach the last field: %q", end)
	}
	m.Update(tea.KeyPressMsg{Text: "home", Code: tea.KeyHome})
	backAtTop := ansi.Strip(m.View(40, true))
	if backAtTop != first {
		t.Fatalf("home did not return to the top:\nfirst=%q\nback=%q", first, backAtTop)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	pagedDown := ansi.Strip(m.View(40, true))
	if pagedDown == first {
		t.Fatal("pgdown did not scroll the card view")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	pagedUp := ansi.Strip(m.View(40, true))
	if pagedUp != first {
		t.Fatalf("pgup did not return to the top:\nfirst=%q\nback=%q", first, pagedUp)
	}
}

// TestInspectorViewShowsFullDateNotGoDump is part of m8: the raw/Inspector
// view's "raw" formatting (%#v) is meant for arbitrary Go values, but for a
// time.Time it dumps unexported internal fields instead of a readable date.
// It should show FormatValue's full RFC3339 rendering for dates specifically.
func TestInspectorViewShowsFullDateNotGoDump(t *testing.T) {
	when := time.Date(2024, time.March, 5, 13, 30, 0, 0, time.UTC)
	cols := []Column{{Name: "created"}}
	rows := []Row{{Key: "0", Values: []any{when}}}
	content := currentRowContent(cols, rows, 0, 60, true)
	if !strings.Contains(content, when.Format(time.RFC3339)) {
		t.Fatalf("raw date content = %q, want it to contain %q", content, when.Format(time.RFC3339))
	}
	if strings.Contains(content, "wall:") || strings.Contains(content, "time.Time{") {
		t.Fatalf("raw date content leaked Go's internal struct dump: %q", content)
	}
}
