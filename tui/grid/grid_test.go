package grid

import (
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
		{Key: "1", Values: map[string]any{"id": 2, "name": "Prague"}, Ref: &session.EntityRef{Type: "city", Keys: map[string]string{"id": "2"}}},
		{Key: "0", Values: map[string]any{"id": 10, "name": "Vienna"}, Ref: &session.EntityRef{Type: "city", Keys: map[string]string{"id": "10"}}},
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
	if m.rows[0].Values["id"] != 2 {
		t.Fatalf("ascending first row = %+v", m.rows[0])
	}
	m.Sort(0) // descending
	if m.rows[0].Values["id"] != 10 {
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
