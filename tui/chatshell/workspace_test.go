package chatshell

import (
	tea "charm.land/bubbletea/v2"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"github.com/tuigoff/tuigoff/pkg/grid"
	"github.com/tuigoff/tuigoff/pkg/gridblock"
	"github.com/tuigoff/tuigoff/pkg/theme"
	"github.com/tuigoff/tuigoff/pkg/uitest"
	"strings"
	"testing"
)

type workspaceDialog struct {
	keys  []string
	title string
}

func (o *workspaceDialog) View(int, int) string { return o.title }
func (o *workspaceDialog) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return o, nil, false
	}
	o.keys = append(o.keys, k.String())
	return o, nil, k.String() == "esc"
}
func routingWorkspace(t *testing.T) (*Model, *grid.Model) {
	t.Helper()
	m := New(&fakeHandler{}, WithWorkspace(&fakeSidePanel{title: "Explorer"}), WithSidePanel(&fakeSidePanel{title: "Inspect"}), WithWorkspaceHelp(func() Overlay { return &workspaceDialog{title: "HELP"} }))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	g := grid.New([]grid.Column{{Name: "Name"}}, []grid.Row{{Key: "one", Values: []any{"alpha"}}, {Key: "two", Values: []any{"beta"}}})
	g.SetWorkspaceStyle(true)
	m.AppendBlockWithID("first", &fakeBlock{})
	m.AppendBlockWithID("grid", gridblock.Wrap(g))
	m.FocusEntry("grid")
	m.View()
	return m, g
}
func routingKey(m *Model, s string) { m.Update(uitest.Key(s)); m.View() }

type replacingWorkspaceDialog struct {
	model *Model
	next  Overlay
}

func (o *replacingWorkspaceDialog) View(int, int) string { return "ORIGIN MENU" }
func (o *replacingWorkspaceDialog) Update(tea.Msg) (Overlay, tea.Cmd, bool) {
	o.model.PushOverlay(o.next)
	return o, nil, true
}

func TestWorkspaceOverlayReplacementPreservesNewDialogAndUnderlay(t *testing.T) {
	m, g := routingWorkspace(t)
	underlay := &workspaceDialog{title: "UNDERLAY DIALOG"}
	replacement := &workspaceDialog{title: "REPLACEMENT DIALOG"}
	m.PushOverlay(underlay)
	m.PushOverlay(&replacingWorkspaceDialog{model: m, next: replacement})
	routingKey(m, "enter")
	if len(m.overlays) != 2 || m.overlays[0] != underlay || m.overlays[1] != replacement || !strings.Contains(m.View().Content, replacement.title) || g.Focused() || m.input.Focused() {
		t.Fatal("closing originating overlay lost its replacement or activated the underlay")
	}
	routingKey(m, "esc")
	if len(m.overlays) != 1 || m.overlays[0] != underlay || g.Focused() {
		t.Fatal("replacement cancellation did not return to underlying dialog")
	}
	routingKey(m, "esc")
	if len(m.overlays) != 0 || !g.Focused() || m.input.Focused() {
		t.Fatal("last dialog cancellation did not restore exclusive grid focus")
	}
}

func TestWorkspaceModalSearchExpansionEscapeLayers(t *testing.T) {
	m, g := routingWorkspace(t)
	m.ExpandSelected()
	routingKey(m, "a")
	dialog := &workspaceDialog{title: "DIALOG"}
	m.PushOverlay(dialog)
	m.View()
	if g.Focused() || m.input.Focused() {
		t.Fatal("modal left an underlay control focused")
	}
	routingKey(m, "f4")
	routingKey(m, "f8")
	if len(m.overlays) != 1 || m.WorkspaceFocus() != "transcript" || !strings.Contains(m.View().Content, "Close dialog to return to workspace") || len(dialog.keys) != 0 {
		t.Fatal("workspace globals escaped modal")
	}
	routingKey(m, "f1")
	if len(m.overlays) != 2 || !strings.Contains(m.View().Content, "HELP") {
		t.Fatal("F1 did not layer help above dialog")
	}
	routingKey(m, "f1")
	if len(m.overlays) != 1 || m.overlays[0] != dialog {
		t.Fatal("help lost originating dialog")
	}
	routingKey(m, "esc")
	if len(m.overlays) != 0 || !m.Expanded() || !g.Editing() {
		t.Fatal("first Esc changed underlying search/expansion")
	}
	routingKey(m, "esc")
	if !m.Expanded() || !g.Editing() {
		t.Fatal("search clear also exited search or expansion")
	}
	routingKey(m, "esc")
	if !m.Expanded() || g.Editing() {
		t.Fatal("search exit also restored expansion")
	}
	routingKey(m, "esc")
	if m.Expanded() || m.transcript.BlockAt(1).(*gridblock.Block).Model != g {
		t.Fatal("restore lost original block")
	}
}

func TestWorkspaceVisibilityAndNarrowFocus(t *testing.T) {
	for _, width := range []int{24, 40, 80, 120} {
		for _, hidden := range [][]string{nil, {"f6"}, {"f7"}, {"f6", "f7"}} {
			m, g := routingWorkspace(t)
			for _, key := range hidden {
				routingKey(m, key)
			}
			left, right := m.leftVisible, m.sidePanelVisible
			m.ExpandSelected()
			m.RestoreWorkspace()
			if m.leftVisible != left || m.sidePanelVisible != right {
				t.Fatal("expansion changed saved visibility")
			}
			m.Update(tea.WindowSizeMsg{Width: width, Height: 12})
			m.hintsProvider = func(int) ([]theme.Hint, []string) { return nil, []string{strings.Repeat("long hint ", 80)} }
			for _, key := range []string{"shift+left", "f6", "shift+right", "f7", "f4"} {
				routingKey(m, key)
				l, c, r := m.workspaceWidths()
				if l+c+r != width || (m.leftFocused && l == 0) || (m.Zone().String() == "sidebar" && r == 0) {
					t.Fatalf("width %d: invisible focus owner", width)
				}
				view := m.View().Content
				if len(strings.Split(view, "\n")) != 12 {
					t.Fatal("layout exceeds height")
				}
				for _, line := range strings.Split(view, "\n") {
					if ansi.StringWidth(line) != width {
						t.Fatal("layout exceeds width")
					}
				}
			}
			m.SetComposerText("draft")
			if !strings.Contains(m.View().Content, "draft") || !m.input.Focused() || g.Focused() {
				t.Fatal("narrow footer hid composer or retained grid focus")
			}
		}
	}
}

func TestWorkspaceCtrlCFromPanelsRetainsShellQuit(t *testing.T) {
	for _, key := range []string{"shift+left", "shift+right"} {
		m, _ := routingWorkspace(t)
		routingKey(m, key)
		_, cmd := m.Update(uitest.Key("ctrl+c"))
		if cmd == nil {
			t.Fatal("panel swallowed quit")
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatal("panel did not invoke shell quit")
		}
	}
}

func TestWorkspaceComposerAndHistoryPreserveDraftAndSearch(t *testing.T) {
	m, g := routingWorkspace(t)
	routingKey(m, "a")
	m.SetComposerText("keep this draft")
	routingKey(m, "f4")
	if g.Focused() || !m.input.Focused() {
		t.Fatal("composer and grid focus overlap")
	}
	routingKey(m, "esc")
	if m.WorkspaceFocus() != "transcript" || m.input.Value() != "keep this draft" || !g.Editing() {
		t.Fatal("Esc lost draft or prior search")
	}
	m.ExpandSelected()
	routingKey(m, "shift+up")
	if m.Expanded() || m.focusRing.Stop() != 0 {
		t.Fatal("history moved while old result remained expanded")
	}
	routingKey(m, "f4")
	routingKey(m, "shift+left")
	if m.WorkspaceFocus() != "explorer" || m.input.Value() != "keep this draft" {
		t.Fatal("editor Shift navigation lost draft")
	}
	routingKey(m, "shift+down")
	if m.WorkspaceFocus() != "transcript" || m.focusRing.Stop() != 1 {
		t.Fatal("panel Shift history did not navigate")
	}
}

func TestWorkspaceExpandedArrowsRevealHiddenSides(t *testing.T) {
	for _, key := range []string{"shift+left", "shift+right"} {
		t.Run(key, func(t *testing.T) {
			m, g := routingWorkspace(t)
			routingKey(m, "f6")
			routingKey(m, "f7")
			m.ExpandSelected()
			routingKey(m, key)
			if m.Expanded() {
				t.Fatal("still expanded")
			}
			if key == "shift+left" && (!m.leftVisible || m.WorkspaceFocus() != "explorer") {
				t.Fatal("left not revealed/focused")
			}
			if key == "shift+right" && (!m.sidePanelVisible || m.WorkspaceFocus() != "sidebar") {
				t.Fatal("right not revealed/focused")
			}
			if g.Focused() {
				t.Fatal("grid retained input focus")
			}
		})
	}
}

func TestWorkspaceTemporaryCenterRestoresExactState(t *testing.T) {
	m, g := routingWorkspace(t)
	m.ExpandSelected()
	restore := m.ShowTemporaryCenterPanel(&workspaceTextPanel{text: "API OVERVIEW"})
	if m.Expanded() || !strings.Contains(m.View().Content, "API OVERVIEW") {
		t.Fatal("expanded result obscured API")
	}
	restore()
	restore()
	m.View()
	if !m.Expanded() || m.WorkspaceFocus() != "transcript" || !g.Focused() {
		t.Fatal("API return lost expanded grid/focus")
	}
	restore = m.ShowTemporaryCenterPanel(&workspaceTextPanel{text: "API"})
	m.ClearTranscript()
	restore()
	if m.Expanded() || m.WorkspaceFocus() != "input" {
		t.Fatal("restored stale result after session change")
	}
}

func TestWorkspaceExpansionRetainsGridAndHistoryView(t *testing.T) {
	m, g := routingWorkspace(t)
	columns := make([]grid.Column, 10)
	for i := range columns {
		columns[i] = grid.Column{Name: fmt.Sprintf("Column %d", i)}
	}
	rows := make([]grid.Row, 30)
	for i := range rows {
		values := make([]any, len(columns))
		for c := range values {
			values[c] = fmt.Sprintf("value-%02d-%02d", i, c)
		}
		rows[i] = grid.Row{Key: fmt.Sprint(i), Values: values}
	}
	g.SetData(columns, rows)
	g.Sort(2)
	g.SelectRow(20)
	g.SelectColumn(8)
	m.transcript.InvalidateAndRebuild("grid")
	for i := 0; i < 20; i++ {
		m.AppendAssistant(fmt.Sprintf("Later message %d", i))
	}
	m.FocusEntry("grid")
	m.View()
	m.transcript.ScrollDown(2)
	row, _ := g.CurrentRow()
	column, desc := g.SortState()
	offset := g.ColumnOffset()
	before := m.transcript.View()
	m.ExpandSelected()
	m.View()
	m.RestoreWorkspace()
	m.View()
	returned, _ := g.CurrentRow()
	returnedColumn, returnedDesc := g.SortState()
	if returned.Key != row.Key || g.SelectedColumn() != 8 || returnedColumn != column || returnedDesc != desc || g.ColumnOffset() != offset {
		t.Fatal("expansion changed row/column/sort/scroll state")
	}
	if m.transcript.View() != before {
		t.Fatal("expansion changed history viewport")
	}
}

type workspaceTextPanel struct{ text string }

func (p *workspaceTextPanel) Title() string                       { return p.text }
func (p *workspaceTextPanel) View(int, int, bool) string          { return p.text }
func (p *workspaceTextPanel) Update(tea.Msg) (SidePanel, tea.Cmd) { return p, nil }

func TestWorkspaceOptInLeavesLegacyEscAndModalBehavior(t *testing.T) {
	m := newTestShell(&fakeHandler{})
	m.SetComposerText("legacy draft")
	routingKey(m, "esc")
	if m.input.Value() != "" {
		t.Fatal("legacy Esc behavior changed")
	}
	d := &workspaceDialog{title: "legacy"}
	m.PushOverlay(d)
	routingKey(m, "f1")
	if len(m.overlays) != 1 || len(d.keys) != 1 || d.keys[0] != "f1" {
		t.Fatal("legacy overlay routing changed")
	}
}
