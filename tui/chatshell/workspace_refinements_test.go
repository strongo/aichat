package chatshell

import (
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/tuigoff/tuigoff/pkg/focus"
	"github.com/tuigoff/tuigoff/pkg/theme"
	"github.com/tuigoff/tuigoff/pkg/uitest"
	"github.com/tuigoff/tuigoff/pkg/widgets"
	"strings"
	"testing"
)

type refinedPanel struct{ fakeSidePanel }

func (*refinedPanel) WorkspaceHeader() (string, string) { return "Explorer", "[F6]" }
func refinementShell(t *testing.T) (*Model, *int, *int) {
	t.Helper()
	switched, created := 0, 0
	m := New(&fakeHandler{}, WithWorkspace(&refinedPanel{}), WithSidePanel(&fakeSidePanel{title: "Inspect"}), WithWorkspaceSpacing(), WithWorkspaceTopBarProvider(func(int) (string, string, []theme.MenuItem) {
		return "DataTug", "Project: Chinook [F3] ¦ Chat", []theme.MenuItem{{Label: "Server OFF [F8]"}, {Label: "Help [F1]"}}
	}), WithWorkspaceChatTabs(func(int) ([]widgets.Tab, int) {
		return []widgets.Tab{{ID: "a", Title: "First"}, {ID: "b", Title: "Second"}, {ID: "c", Title: "Current"}}, 1
	}, func(index int) tea.Cmd { switched = index; return nil }), WithWorkspaceChatTabActions(WorkspaceTabAction{Label: "+New", Action: func() tea.Cmd { created++; return nil }}, WorkspaceTabAction{Label: "…List"}, WorkspaceTabAction{Label: strings.Repeat("huge", 100)}))
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 42})
	return m, &switched, &created
}
func TestRefinedWorkspaceGeometryMouseAndExclusiveTabs(t *testing.T) {
	m, switched, created := refinementShell(t)
	for _, size := range [][2]int{{140, 42}, {100, 30}, {80, 24}, {24, 8}, {8, 4}, {1, 2}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := ansi.Strip(m.View().Content)
		lines := strings.Split(view, "\n")
		if len(lines) != size[1] {
			t.Fatalf("height %v got %d", size, len(lines))
		}
		for _, line := range lines {
			if ansi.StringWidth(line) != size[0] {
				t.Fatalf("width %v: %q", size, line)
			}
		}
		if m.historyHeight() < 1 || m.workspaceComposerHeight() < 1 {
			t.Fatal("nonpositive viewport")
		}
		if size[1] >= 24 {
			l, c, _ := m.workspaceWidths()
			row, _, ranges := m.workspaceChatTabRow(c)
			if !strings.Contains(ansi.Strip(row), "›Second") || !strings.Contains(row, "+New") {
				t.Fatalf("active/New missing: %q", row)
			}
			m.workspaceClick(tea.MouseClickMsg{Button: tea.MouseLeft, X: l + ranges[0].start, Y: m.workspaceBodyTop()})
			if *created < 1 {
				t.Fatal("New click missed reserved header")
			}
			m.workspaceClick(tea.MouseClickMsg{Button: tea.MouseLeft, X: l, Y: m.workspaceBodyTop()})
			m.SetChips([]Chip{{ID: "one", Label: "table"}})
			m.View()
			cells, _ := m.chipRow(c)
			m.workspaceClick(tea.MouseClickMsg{Button: tea.MouseLeft, X: l + cells[0].x, Y: m.workspaceHistoryTop() + m.historyHeight() + m.menuHeight()})
			if len(m.Chips()) != 0 {
				t.Fatal("chip mouse coordinate drift")
			}
		}
	}
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 42})
	m.FocusComposer()
	m.Update(uitest.Key("ctrl+tab"))
	if *switched != 2 {
		t.Fatal("forward session chord")
	}
	m.Update(uitest.Key("ctrl+shift+tab"))
	if *switched != 0 {
		t.Fatal("reverse session chord")
	}
	m.FocusLeftPanel()
	m.Update(uitest.Key("ctrl+tab"))
	if *switched != 0 {
		t.Fatal("stole explorer input")
	}
	m.FocusComposer()
	m.PushOverlay(&workspaceDialog{title: "modal"})
	m.Update(uitest.Key("ctrl+tab"))
	if *switched != 0 {
		t.Fatal("stole modal input")
	}
	m.PopOverlay()
	if _, handled := m.workspaceChatTabKey(uitest.Key("tab")); handled {
		t.Fatal("stole ordinary Tab")
	}
	m.SetCenterPanel(&fakeSidePanel{title: "API"}, true)
	if m.workspaceChatHeaderHeight() != 0 {
		t.Fatal("sessions displayed over API")
	}
	m.ShowTranscript()
	m.FocusRightPanel()
	m.Update(uitest.Key("ctrl+tab"))
	if *switched != 0 {
		t.Fatal("stole sidebar input")
	}
}
func TestComposerStateRetainsUnicodeMultilineCaretAndSessionChips(t *testing.T) {
	m, _, _ := refinementShell(t)
	m.RestoreComposerState(ComposerState{Text: "first α\nsecond 😀", Cursor: 9, Chips: []Chip{{ID: "a", Label: "A"}}})
	state := m.CaptureComposerState()
	if state.Text != "first α\nsecond 😀" || state.Cursor != 9 || len(state.Chips) != 1 {
		t.Fatalf("draft restore: %#v", state)
	}
	state.Chips[0].Label = "changed"
	if m.Chips()[0].Label != "A" {
		t.Fatal("capture aliases chips")
	}
	m.FocusLeftPanel()
	m.RestoreComposerState(ComposerState{Text: "draft", Cursor: 999})
	if m.input.Focused() || m.ComposerCursor() != 5 {
		t.Fatal("blur/clamped caret lost")
	}
	m.RestoreComposerState(ComposerState{Text: "draft", Cursor: -1})
	if m.ComposerCursor() != 0 {
		t.Fatal("negative caret")
	}
	m.AppendBlockWithID("retained", &fakeBlock{})
	m.FocusEntry("retained")
	viewport := m.CaptureWorkspaceViewport()
	if viewport.EntryID != "retained" {
		t.Fatal("entry capture")
	}
	m.FocusComposer()
	m.RestoreWorkspaceViewport(viewport)
	if m.FocusedEntryID() != "retained" {
		t.Fatal("entry restore")
	}
	m.RestoreWorkspaceViewport(WorkspaceViewport{EntryID: "deleted", ScrollOffset: -1})
	if m.Zone() != focus.ZoneInput {
		t.Fatal("missing entry silently focused another result")
	}
	if m.CaptureWorkspaceViewport().EntryID != "" {
		t.Fatal("composer captured result focus")
	}
	m.RestoreWorkspaceViewport(WorkspaceViewport{})
}
func TestRefinementOptionsStayOptInAndDegenerateSessionRowsSafe(t *testing.T) {
	m := New(&fakeHandler{}, WithWorkspaceSpacing(), WithWorkspaceTopBarProvider(func(int) (string, string, []theme.MenuItem) { return "SHOULD NOT SHOW", "", nil }))
	if strings.Contains(m.View().Content, "SHOULD NOT SHOW") {
		t.Fatal("legacy chrome changed")
	}
	m, _, _ = refinementShell(t)
	m.workspaceChatTabs = func(int) ([]widgets.Tab, int) { return nil, -1 }
	if _, handled := m.workspaceChatTabKey(uitest.Key("ctrl+tab")); !handled {
		t.Fatal("empty strip chord")
	}
	m.workspaceChatActivate = nil
	m.workspaceChatTabClick(0, 20)
	m.workspaceChatTabActions = []WorkspaceTabAction{{Label: "noop"}}
	_, _, ranges := m.workspaceChatTabRow(20)
	m.workspaceChatTabClick(ranges[0].start, 20)
	m.workspaceChatTabActions = nil
	m.workspaceChatTabClick(999, 20)
	m.workspaceChatActivate = func(int) tea.Cmd { return nil }
	m.workspaceChatTabs = func(int) ([]widgets.Tab, int) { return []widgets.Tab{{Title: "a"}, {Title: "b"}}, 0 }
	m.workspaceChatTabClick(4, 20)
	m.workspaceSpacing = false
	if m.workspaceTopGap() != 0 || m.workspaceComposerSeparation() != 0 {
		t.Fatal("spacing cannot disable")
	}
	restore := theme.SetColorProfileDetector(func() colorprofile.Profile { return colorprofile.TrueColor })
	defer restore()
	theme.SetHalfBlockEdges(true)
	defer theme.SetHalfBlockEdges(false)
	m.workspaceSpacing = true
	m.View()
}

func TestWorkspaceSessionCloseMarkUsesProductChooserOnly(t *testing.T) {
	m, _, _ := refinementShell(t)
	closed := -1
	WithWorkspaceChatTabClose(func(index int) tea.Cmd { closed = index; return nil })(m)
	m.workspaceChatTabActions = nil
	m.workspaceChatTabs = func(int) ([]widgets.Tab, int) {
		return []widgets.Tab{{ID: "a", Title: "First", Closable: true}, {ID: "b", Title: "Second", Closable: true}}, 0
	}
	// First tab: space/marker + First + space + close mark + trailing space.
	m.workspaceChatTabClick(7, 40)
	if closed != 0 {
		t.Fatalf("close target %d", closed)
	}
	m.workspaceChatClose = nil
	m.workspaceChatTabClick(7, 40)
}

func TestWorkspaceChatChromePreservesAttachmentActionFocusRing(t *testing.T) {
	m, _, _ := refinementShell(t)
	observer := &chipHandler{}
	m.handler = observer
	WithWorkspaceComposerAction("Select text", func() tea.Cmd { return nil })(m)
	m.SetChips([]Chip{{ID: "context", Label: "Customer"}})
	m.SetComposerText("Top 5 rows")
	m.FocusComposer()
	routingKey(m, "tab")
	if m.chipFocus != 0 || m.ComposerActionFocused() {
		t.Fatal("chat header stole attachment focus")
	}
	routingKey(m, "backspace")
	if len(m.Chips()) != 0 || len(observer.changes) != 1 || m.ComposerText() != "Top 5 rows" {
		t.Fatal("focused chip removal lost observer/draft")
	}
	routingKey(m, "shift+esc")
	if len(m.Chips()) != 1 || len(observer.changes) != 2 || m.ComposerText() != "Top 5 rows" {
		t.Fatal("chip undo lost context/draft")
	}
	m.FocusComposer()
	routingKey(m, "tab")
	routingKey(m, "tab")
	if !m.ComposerActionFocused() {
		t.Fatal("Select text not reachable after attachment")
	}
	routingKey(m, "shift+tab")
	if m.chipFocus != 0 || m.ComposerActionFocused() {
		t.Fatal("reverse Tab lost attachment focus")
	}
}

func TestOverlayMembershipUsesPointerIdentityWithoutFocusMutation(t *testing.T) {
	m, _, _ := refinementShell(t)
	origin, cover := &workspaceDialog{title: "origin"}, &workspaceDialog{title: "cover"}
	var nilDialog *workspaceDialog
	if m.HasOverlay(nil) || m.HasOverlay(nilDialog) || m.HasOverlay(nonPointerOverlay{}) {
		t.Fatal("invalid overlay matched")
	}
	m.PushOverlay(origin)
	m.PushOverlay(cover)
	if !m.HasOverlay(origin) || !m.HasOverlay(cover) || m.HasOverlay(&workspaceDialog{title: "origin"}) {
		t.Fatal("overlay membership lost pointer identity")
	}
	if len(m.overlays) != 2 || m.input.Focused() {
		t.Fatal("read-only membership mutated stack/focus")
	}
	m.CloseOverlay(origin)
	if m.HasOverlay(origin) || !m.HasOverlay(cover) {
		t.Fatal("closed originating overlay still active")
	}
}

func TestInputEpochTracksConsumedActionsBeforeRoutingButNotReleases(t *testing.T) {
	m, _, created := refinementShell(t)
	if m.InputEpoch() != 0 {
		t.Fatal("resize advanced input epoch")
	}
	m.workspaceChatTabActions = []WorkspaceTabAction{{Label: "New", Action: func() tea.Cmd {
		if m.InputEpoch() != 1 {
			t.Fatal("epoch not incremented before tab callback")
		}
		*created++
		return nil
	}}}
	_, c, _ := m.workspaceWidths()
	l, _, _ := m.workspaceWidths()
	_, _, actions := m.workspaceChatTabRow(c)
	m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: l + actions[0].start, Y: m.workspaceBodyTop()})
	if *created != 1 || m.InputEpoch() != 1 {
		t.Fatal("provider-consumed mouse not tracked")
	}
	dialog := &workspaceDialog{title: "modal"}
	m.PushOverlay(dialog)
	events := []tea.Msg{uitest.Key("a"), tea.PasteStartMsg{}, tea.PasteMsg{Content: "pasted"}, tea.PasteEndMsg{}, tea.MouseReleaseMsg{}, tea.MouseWheelMsg{}, tea.MouseMotionMsg{}}
	for _, event := range events {
		before := m.InputEpoch()
		m.Update(event)
		if m.InputEpoch() != before+1 {
			t.Fatalf("input epoch missing %T", event)
		}
	}
	before := m.InputEpoch()
	m.Update(tea.KeyReleaseMsg{Code: tea.KeyF12})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.Update(struct{}{})
	if m.InputEpoch() != before {
		t.Fatal("release/non-input advanced epoch")
	}
	if len(dialog.keys) != 1 || dialog.keys[0] != "a" {
		t.Fatal("modal key routing changed")
	}
}
