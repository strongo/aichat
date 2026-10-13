package chatshell

import (
	tea "charm.land/bubbletea/v2"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"github.com/tuigoff/tuigoff/pkg/grid"
	"github.com/tuigoff/tuigoff/pkg/gridblock"
	"github.com/tuigoff/tuigoff/pkg/theme"
	"github.com/tuigoff/tuigoff/pkg/transcript"
	"github.com/tuigoff/tuigoff/pkg/uitest"
	"github.com/tuigoff/tuigoff/pkg/widgets"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func framedShell(t *testing.T) (*Model, *int) {
	t.Helper()
	m, switched, _ := refinementShell(t)
	WithWorkspaceChatFrame()(m)
	WithWorkspacePanelControls()(m)
	WithWorkspaceChromeProvider(func(int) theme.WorkspaceChrome {
		return theme.WorkspaceChrome{Title: "DataTug", Context: []theme.WorkspaceContext{{Label: "Project", Value: "Chinook", Key: "F3"}}, Actions: []theme.WorkspaceChromeAction{{Key: "F8", Label: "Server", Status: "LIVE"}, {Key: "F1", Label: "Help"}}}
	})(m)
	WithWorkspaceComposerMetadata(func() theme.WorkspaceContext { return theme.WorkspaceContext{Label: "Model", Value: "test-model"} })(m)
	return m, switched
}
func TestPanelPolishFrameGeometryAndMouse(t *testing.T) {
	for _, size := range [][2]int{{140, 42}, {100, 30}, {80, 24}} {
		m, switched := framedShell(t)
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.RestoreComposerState(ComposerState{Text: "draft stays editable"})
		view := ansi.Strip(m.View().Content)
		lines := strings.Split(view, "\n")
		if len(lines) != size[1] {
			t.Fatalf("%v height %d", size, len(lines))
		}
		for _, line := range lines {
			if ansi.StringWidth(line) != size[0] {
				t.Fatalf("%v width %q", size, line)
			}
		}
		l, c, _ := m.workspaceWidths()
		if !strings.Contains(lines[0], "F1 Help") || strings.Contains(lines[0], "test-model") {
			t.Fatal("header metadata/help")
		}
		top := m.workspaceBodyTop()
		seam := []rune(lines[top+1])[l : l+c]
		if seam[0] != '╭' || seam[len(seam)-1] != '╮' {
			t.Fatalf("seam: %q", string(seam))
		}
		bottom := top + m.workspaceBodyHeight() - 1
		if []rune(lines[bottom])[l] != '╰' {
			t.Fatal("missing chat bottom")
		}
		cap := bottom - m.workspaceComposerHeight() - 1
		if []rune(lines[cap])[l] != '▎' || []rune(lines[cap+1])[l] != '▎' {
			t.Fatal("quarter marker excludes composer cap")
		}
		if !strings.Contains(view, "Model: test-model") || !strings.Contains(view, "draft stays editable") {
			t.Fatal("composer content cropped")
		}
		*switched = -1
		// Same content inset is used by rendering and all tab/action hitboxes.
		row, _, actions := m.workspaceChatTabRow(c)
		first := strings.Split(ansi.Strip(row), "\n")[0]
		x := strings.Index(first, "First")
		if x < 0 {
			t.Fatal("First missing")
		}
		m.workspaceClick(tea.MouseClickMsg{Button: tea.MouseLeft, X: l + x, Y: top})
		if *switched != 0 {
			t.Fatalf("tab hitbox missed: %d", *switched)
		}
		*switched = -1
		m.workspaceClick(tea.MouseClickMsg{Button: tea.MouseLeft, X: l + 2, Y: top + 1})
		if *switched != -1 {
			t.Fatal("seam activated a tab")
		}
		if len(actions) == 0 || actions[0].start < 2 {
			t.Fatal("action inset")
		}
	}
}
func TestPanelPolishTabsHaveExclusiveOwnerAndRestoreCursor(t *testing.T) {
	m, switched := framedShell(t)
	m.FocusLeftPanel()
	m.Update(uitest.Key("f11"))
	if m.WorkspaceFocus() != "session-tabs" || m.input.Focused() || m.transcript.FocusedBlock() != nil || m.leftFocused {
		t.Fatal("tabs did not acquire exclusive owner")
	}
	*switched = -1
	m.Update(uitest.Key("right"))
	if *switched != -1 || m.workspaceTabSelection != 2 {
		t.Fatal("arrow activated instead of selecting")
	}
	m.Update(uitest.Key("esc"))
	if m.WorkspaceFocus() != "explorer" {
		t.Fatal("Esc lost previous owner")
	}
	m.Update(uitest.Key("f11"))
	m.Update(uitest.Key("left"))
	m.Update(uitest.Key("enter"))
	if *switched != 0 || m.WorkspaceFocus() != "explorer" {
		t.Fatal("Enter activation/restore")
	}
	m.FocusComposer()
	m.Update(uitest.Key("f11"))
	m.Update(uitest.Key("ctrl+tab"))
	if *switched != 2 {
		t.Fatal("CtrlTab changed meaning")
	}
	m.Update(uitest.Key("f11"))
	m.FocusRightPanel()
	if m.workspaceTabFocused || m.WorkspaceFocus() != "sidebar" {
		t.Fatal("explicit focus left tab ghost")
	}
	m.FocusComposer()
	m.Update(uitest.Key("f11"))
	m.Update(uitest.Key("shift+left"))
	if m.WorkspaceFocus() != "explorer" || m.workspaceTabFocused {
		t.Fatal("Shift navigation lost")
	}
}
func TestPanelPolishIndependentWidthsAndFocusedFooter(t *testing.T) {
	m, _ := framedShell(t)
	l, _, r := m.workspaceWidths()
	m.FocusLeftPanel()
	m.Update(uitest.Key("ctrl+shift+right"))
	ll, _, rr := m.workspaceWidths()
	if ll != l+2 || rr != r {
		t.Fatal("Explorer resize changed other side")
	}
	view := ansi.Strip(m.View().Content)
	if strings.Count(view, "resize") != 1 {
		t.Fatal("focused footer missing or duplicated")
	}
	leftPreference := m.workspaceLeftWidth
	m.FocusRightPanel()
	m.Update(uitest.Key("ctrl+shift+left"))
	if m.workspaceLeftWidth != leftPreference || m.workspaceRightWidth != r-2 {
		t.Fatal("Inspect resize changed Explorer preference")
	}
	m.FocusComposer()
	if strings.Contains(ansi.Strip(m.View().Content), "resize") {
		t.Fatal("blurred resize hints remain")
	}
	m.Update(uitest.Key("f6"))
	m.Update(uitest.Key("f6"))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 42})
	ll, _, rr = m.workspaceWidths()
	if ll != l+2 || rr != r-2 {
		t.Fatal("temporary geometry overwrote preferred widths")
	}
	m.AppendBlockWithID("retained", &fakeBlock{})
	m.FocusEntry("retained")
	if !m.ExpandSelected() {
		t.Fatal("could not expand selected result")
	}
	m.Update(uitest.Key("shift+left"))
	ll, _, rr = m.workspaceWidths()
	if ll != l+2 || rr != r-2 || m.WorkspaceFocus() != "explorer" {
		t.Fatal("expanded reveal lost side preferences/focus")
	}

}
func TestPanelPolishNoSessionFrameAndLegacyOptIn(t *testing.T) {
	m := New(&fakeHandler{}, WithWorkspace(nil), WithWorkspaceChatFrame())
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "╭") || !strings.Contains(view, "╰") {
		t.Fatal("empty session has no frame")
	}
	legacy := New(&fakeHandler{}, WithWorkspace(nil), WithWorkspaceChatTabs(func(int) ([]widgets.Tab, int) { return []widgets.Tab{{Title: "one"}}, 0 }, nil))
	legacy.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if legacy.workspaceChatHeaderHeight() != 1 || legacy.workspaceFrameBottom() != 0 {
		t.Fatal("legacy opted into frame")
	}
}

func TestPanelPolishTabFocusPreservesHelpAndGlobalKeys(t *testing.T) {
	m, _ := framedShell(t)
	WithWorkspaceHelp(func() Overlay { return &workspaceDialog{title: "HELP"} })(m)
	calls := 0
	m.globalKeys = func(msg tea.KeyPressMsg) (tea.Cmd, bool) {
		if msg.String() == "f8" {
			calls++
			return nil, true
		}
		return nil, false
	}
	m.Update(uitest.Key("f11"))
	m.Update(uitest.Key("f8"))
	if calls != 1 || m.WorkspaceFocus() != "session-tabs" {
		t.Fatal("global shortcut lost from tab owner")
	}
	m.Update(uitest.Key("f1"))
	if len(m.overlays) != 1 || m.input.Focused() {
		t.Fatal("help unreachable from session strip")
	}
	m.PopOverlay()
	m.Update(uitest.Key("esc"))
	if m.WorkspaceFocus() != "input" || !m.input.Focused() {
		t.Fatal("help lost tab return owner")
	}
	m.FocusLeftPanel()
	m.Update(uitest.Key("f11"))
	m.Update(uitest.Key("ctrl+tab"))
	if m.workspaceTabFocused {
		t.Fatal("CtrlTab leaves stale strip focus")
	}
}

func TestPanelPolishTabsHandleEmptyAndChangedSessionLists(t *testing.T) {
	m, switched := framedShell(t)
	*switched = -1
	m.Update(uitest.Key("f11"))
	if !strings.Contains(ansi.Strip(m.View().Content), "Second") {
		t.Fatal("focused selection missing")
	}
	m.Update(uitest.Key("x"))
	if m.ComposerText() != "" || *switched != -1 {
		t.Fatal("typing leaked from tab focus")
	}
	m.Update(uitest.Key("f11"))
	if m.WorkspaceFocus() != "input" {
		t.Fatal("F11 toggle lost owner")
	}
	m.workspaceChatTabs = func(int) ([]widgets.Tab, int) { return nil, -1 }
	m.Update(uitest.Key("f11"))
	if m.workspaceTabFocused {
		t.Fatal("empty tabs acquired focus")
	}
	m.workspaceChatTabs = func(int) ([]widgets.Tab, int) { return []widgets.Tab{{Title: "Only"}}, 0 }
	m.Update(uitest.Key("f11"))
	m.workspaceChatTabs = func(int) ([]widgets.Tab, int) { return nil, -1 }
	m.Update(uitest.Key("enter"))
	if *switched != -1 || m.workspaceTabFocused {
		t.Fatal("stale tab index activated")
	}
	m.workspaceChatTabs = func(int) ([]widgets.Tab, int) { return []widgets.Tab{{Title: "Only"}}, 0 }
	m.Update(uitest.Key("f11"))
	m.Update(uitest.Key("ctrl+c"))
	if !m.quit {
		t.Fatal("quit shortcut swallowed by tabs")
	}
	if cmd := m.workspaceChatTabClick(0, m.chatWidth()); cmd != nil {
		t.Fatal("left frame cell accepted click")
	}
}
func TestPanelPolishWidthsDoNotCaptureCenterOrLoseNarrowHint(t *testing.T) {
	m, _ := framedShell(t)
	if _, handled := m.workspaceKey(uitest.Key("ctrl+shift+right")); handled {
		t.Fatal("side width shortcut captured composer")
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.workspaceLeftWidth = 16
	m.FocusLeftPanel()
	if !strings.Contains(ansi.Strip(m.View().Content), "Ctrl⇧←/→ width") {
		t.Fatal("narrow width hint cropped")
	}
}

func TestPanelPolishSessionMouseAndExplicitFocusClearTabOwner(t *testing.T) {
	m, _ := framedShell(t)
	m.AppendBlockWithID("result", &fakeBlock{})
	m.FocusEntry("result")
	m.workspaceChatActivate = func(int) tea.Cmd {
		if m.WorkspaceFocus() != "transcript" || m.FocusedEntryID() != "result" {
			t.Fatal("mouse switch lost old session viewport")
		}
		return nil
	}
	m.Update(uitest.Key("f11"))
	m.workspaceChatTabClick(3, m.chatWidth())
	if m.workspaceTabFocused {
		t.Fatal("mouse session switch retained stale owner")
	}
	m.Update(uitest.Key("f11"))
	m.FocusEntry("result")
	if m.WorkspaceFocus() != "transcript" {
		t.Fatal("explicit result focus left ghost tab owner")
	}
	m.Update(uitest.Key("f11"))
	m.ClearTranscript()
	if m.WorkspaceFocus() != "input" || !m.input.Focused() {
		t.Fatal("session clear left ghost tab owner")
	}
	m.Update(uitest.Key("f11"))
	_, _, actions := m.workspaceChatTabRow(m.chatWidth())
	m.workspaceChatTabClick(actions[0].start, m.chatWidth())
	if m.workspaceTabFocused {
		t.Fatal("tab action left ghost owner")
	}
}

func TestPanelPolishModalDoesNotDuplicateVisibleHeaderHelp(t *testing.T) {
	m, _ := framedShell(t)
	m.PushOverlay(&workspaceDialog{title: "Dialog"})
	for _, returnHint := range []bool{false, true} {
		text := ansi.Strip(m.workspaceDialogHint(returnHint))
		if strings.Contains(text, "F1") || !strings.Contains(text, "Esc") {
			t.Fatal("duplicate help or missing close hint")
		}
	}
	lines := strings.Split(ansi.Strip(m.View().Content), "\n")
	if strings.Contains(lines[len(lines)-1], "F1") {
		t.Fatal("modal hardcoded duplicate F1")
	}
	m.workspaceModalHint = true
	lines = strings.Split(ansi.Strip(m.View().Content), "\n")
	if strings.Contains(lines[len(lines)-1], "F1") || !strings.Contains(lines[len(lines)-1], "return to workspace") {
		t.Fatal("modal hint override duplicated F1")
	}
	m.workspaceChromeProvider = nil
	if !strings.Contains(m.workspaceDialogHint(false), "F1") || !strings.Contains(m.workspaceDialogHint(true), "F1") {
		t.Fatal("legacy modal help lost")
	}
	m.workspaceChromeProvider = func(int) theme.WorkspaceChrome { return theme.WorkspaceChrome{Title: "Header without help"} }
	if !strings.Contains(m.workspaceDialogHint(false), "F1") {
		t.Fatal("missing help not restored")
	}
}

type panelPolishEdgePanel struct {
	width int
	click tea.MouseClickMsg
	calls int
}

func (*panelPolishEdgePanel) Title() string { return "Query editor" }
func (p *panelPolishEdgePanel) View(width, height int, focused bool) string {
	p.width = width
	return strings.Repeat(".", max(0, width-1)) + "Z"
}
func (p *panelPolishEdgePanel) Update(msg tea.Msg) (SidePanel, tea.Cmd) {
	if click, ok := msg.(tea.MouseClickMsg); ok {
		p.click = click
		p.calls++
	}
	return p, nil
}

type panelPolishEdgeBlock struct {
	width int
	click tea.MouseClickMsg
	calls int
}

func (b *panelPolishEdgeBlock) View(width int, focused bool) string {
	b.width = width
	return strings.Repeat(".", max(0, width-1)) + "Z"
}
func (*panelPolishEdgeBlock) Focusable() bool { return true }
func (b *panelPolishEdgeBlock) Update(msg tea.Msg) (transcript.Block, tea.Cmd) {
	if click, ok := msg.(tea.MouseClickMsg); ok {
		b.click = click
		b.calls++
	}
	return b, nil
}

func TestPanelPolishGlobalHeaderMouseRoutesBeforeFrameAtEverySize(t *testing.T) {
	for _, size := range [][2]int{{140, 42}, {100, 30}, {80, 24}} {
		m, _ := framedShell(t)
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		handler := m.handler.(*fakeHandler)
		header := ansi.Strip(m.topBarView())
		handler.msgsSeen = nil
		for _, label := range []string{"F3", "F8 Server", "F1 Help"} {
			x := strings.Index(header, label)
			if x < 0 {
				t.Fatalf("missing header action %s at%v", label, size)
			}
			click := tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: 0}
			m.Update(click)
			if len(handler.msgsSeen) == 0 {
				t.Fatalf("header action %s swallowed at%v", label, size)
			}
			if got, ok := handler.msgsSeen[len(handler.msgsSeen)-1].(tea.MouseClickMsg); !ok || got != click {
				t.Fatalf("header action %s swallowed at%v", label, size)
			}
		}
		if len(handler.msgsSeen) != 3 {
			t.Fatal("header action dispatched more than once")
		}
		l, c, _ := m.workspaceWidths()
		for _, click := range []tea.MouseClickMsg{{Button: tea.MouseLeft, X: l + 2, Y: m.workspaceHistoryTop() - 1}, {Button: tea.MouseLeft, X: l, Y: m.workspaceHistoryTop()}, {Button: tea.MouseLeft, X: l + c - 1, Y: m.workspaceHistoryTop()}} {
			m.Update(click)
		}
		if len(handler.msgsSeen) != 3 {
			t.Fatal("frame seam/border escaped to product")
		}
	}
}
func TestPanelPolishLastCenterCellAndMouseSurviveFrame(t *testing.T) {
	for _, size := range [][2]int{{140, 42}, {100, 30}, {80, 24}} {
		m, _ := framedShell(t)
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		panel := &panelPolishEdgePanel{}
		m.SetCenterPanel(panel, true)
		view := strings.Split(ansi.Strip(m.View().Content), "\n")
		l, c, _ := m.workspaceWidths()
		last := l + workspaceContentInset + panel.width - 1
		if panel.width != c-3 || last != l+c-2 || []rune(view[m.workspaceHistoryTop()])[last] != 'Z' || []rune(view[m.workspaceHistoryTop()])[last+1] != '│' {
			t.Fatalf("center last content cell lost at%v width=%d row=%q", size, panel.width, view[m.workspaceHistoryTop()])
		}
		m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: last, Y: m.workspaceHistoryTop()})
		if panel.calls != 1 || panel.click.X != panel.width-1 || panel.click.Y != 0 {
			t.Fatal("center last-cell mouse target unreachable")
		}
	}
}
func TestPanelPolishLastExpandedCellRoutesOnlyRetainedBlock(t *testing.T) {
	for _, size := range [][2]int{{140, 42}, {100, 30}, {80, 24}} {
		m, _ := framedShell(t)
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		other, block := &panelPolishEdgeBlock{}, &panelPolishEdgeBlock{}
		m.AppendBlockWithID("other", other)
		m.AppendBlockWithID("edge", block)
		m.FocusEntry("edge")
		if !m.ExpandSelected() {
			t.Fatal("could not expand")
		}
		lines := strings.Split(ansi.Strip(m.View().Content), "\n")
		l, c, _ := m.workspaceWidths()
		last := l + workspaceContentInset + block.width - 1
		if block.width != c-3 || last != l+c-2 || []rune(lines[m.workspaceHistoryTop()])[last] != 'Z' {
			t.Fatalf("expanded rightmost content lost at%v width=%d", size, block.width)
		}
		handler := m.handler.(*fakeHandler)
		handler.msgsSeen = nil
		renderedWidth := block.width
		click := tea.MouseClickMsg{Button: tea.MouseLeft, X: last, Y: m.workspaceHistoryTop()}
		m.Update(click)
		if block.calls != 1 || other.calls != 0 || block.click.X != renderedWidth-1 || block.click.Y != 0 {
			t.Fatalf("expanded click was not exclusive/local: block=%d other=%d x=%d want=%d y=%d", block.calls, other.calls, block.click.X, block.width-1, block.click.Y)
		}
		if len(handler.msgsSeen) != 1 || handler.msgsSeen[0] != click {
			t.Fatal("product did not receive original click exactly once")
		}
		if m.transcript.BlockAt(m.expandedStop) != block || m.FocusedEntryID() != "edge" || m.WorkspaceFocus() != "transcript" {
			t.Fatal("mouse replaced retained result/focus")
		}
		m.handler = bareHandler{}
		m.Update(click)
		if block.calls != 2 || other.calls != 0 {
			t.Fatal("expanded click requires optional product mouse handler")
		}

	}
}

func TestPanelPolishExpandedRealGridKeepsEveryVisibleCellAndScroll(t *testing.T) {
	for _, size := range [][2]int{{140, 42}, {100, 30}, {80, 24}} {
		m, _ := framedShell(t)
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		columns := make([]grid.Column, 12)
		values := make([]any, 12)
		for i := range columns {
			columns[i] = grid.Column{Name: fmt.Sprintf("Column%d", i)}
			values[i] = fmt.Sprintf("value-%d", i)
		}
		values[11] = "LASTCELL"
		g := grid.New(columns, []grid.Row{{Key: "row", Values: values}})
		g.SetWorkspaceStyle(true)
		m.AppendBlockWithID("grid", gridblock.Wrap(g))
		m.FocusEntry("grid")
		m.ExpandSelected()
		g.SelectColumn(11)
		screen := strings.Split(ansi.Strip(m.View().Content), "\n")
		width := m.workspaceContentWidth()
		if g.Width() != width {
			t.Fatalf("grid got width%d, want%d", g.Width(), width)
		}
		expected := strings.Split(ansi.Strip(g.View(width, true)), "\n")
		rendered := ""
		for y, line := range expected {
			if y >= m.historyHeight() {
				break
			}
			want := ansi.Truncate(line, width, "")
			want += strings.Repeat(" ", max(0, width-ansi.StringWidth(want)))
			actual := ansi.Cut(screen[m.workspaceHistoryTop()+y], workspaceContentInset, workspaceContentInset+width)
			if actual != want {
				t.Fatalf("expanded grid cell lost size%v row%d actual%q want%q", size, y, actual, want)
			}
			rendered += actual
		}
		if !strings.Contains(rendered, "LASTCELL") {
			t.Fatal("last horizontal column is unreachable")
		}
	}
}

// panelPolishCellBackgrounds decodes the SGR background actually emitted for
// each one-cell glyph in these fixtures, including border and padding cells.
func panelPolishCellBackgrounds(view string) [][]string { return panelPolishCellColors(view, false) }
func panelPolishCellColors(view string, foreground bool) [][]string {
	cells := [][]string{{}}
	bg := "default"
	for len(view) > 0 {
		if strings.HasPrefix(view, "\x1b[") {
			end := strings.IndexByte(view, 'm')
			if end < 0 {
				panic("unterminated SGR in render")
			}
			params := strings.Split(view[2:end], ";")
			for i := 0; i < len(params); i++ {
				value, _ := strconv.Atoi(params[i])
				switch value {
				case 0:
					bg = "default"
				case 49:
					if !foreground {
						bg = "default"
					}
				case 39:
					if foreground {
						bg = "default"
					}
				case 38, 48:
					if i+1 < len(params) && params[i+1] == "2" && i+4 < len(params) {
						if (foreground && value == 38) || (!foreground && value == 48) {
							bg = strings.Join(params[i+2:i+5], ",")
						}
						i += 4
					} else if i+1 < len(params) && params[i+1] == "5" && i+2 < len(params) {
						if (foreground && value == 38) || (!foreground && value == 48) {
							bg = "index" + params[i+2]
						}
						i += 2
					}
				}
			}
			view = view[end+1:]
			continue
		}
		r, n := utf8.DecodeRuneInString(view)
		view = view[n:]
		if r == '\n' {
			cells = append(cells, []string{})
			continue
		}
		cells[len(cells)-1] = append(cells[len(cells)-1], bg)
	}
	return cells
}

type panelPolishCardDialog struct {
	boundedMouseDialog
	width, height int
	card          bool
}

func (o *panelPolishCardDialog) WorkspaceBorderlessCard() bool { return o.card }
func (o *panelPolishCardDialog) View(width, height int) string {
	o.width, o.height = width, height
	return strings.Repeat(".", max(0, width-1)) + "Z\nActions\n" + theme.WorkspaceKeyHint("F1") + " Help\n" + strings.Repeat(" ", max(0, width-1)) + "┃"
}
func TestPanelPolishBorderlessCardGeometryBackgroundAndMouse(t *testing.T) {
	previous := theme.Dark
	theme.SetDark(true)
	t.Cleanup(func() { theme.SetDark(previous) })
	for _, size := range [][2]int{{140, 42}, {100, 30}, {80, 24}} {
		m, _ := framedShell(t)
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		o := &panelPolishCardDialog{boundedMouseDialog: boundedMouseDialog{local: true, bounded: true}, card: true}
		m.PushOverlay(o)
		view := m.View().Content
		plain := strings.Split(ansi.Strip(view), "\n")
		if strings.ContainsAny(ansi.Strip(view), "│─┌┐└┘╭╮╰╯▌▎") {
			t.Fatal("borderless help retains outline or active marker")
		}
		x, y := boundedMouseTextPosition(t, view, "Actions")
		if x != m.overlayMouseBounds.Min.X || y != m.overlayMouseBounds.Min.Y+1 || m.overlayMouseBounds.Dx() != o.width || o.height != size[1]-8 || o.width != min(102, size[0]-8) {
			t.Fatal("card content geometry/padding mismatch")
		}
		backgrounds := panelPolishCellBackgrounds(view)
		foregrounds := panelPolishCellColors(view, true)
		left, top := m.overlayMouseBounds.Min.X-2, m.overlayMouseBounds.Min.Y-1
		right, bottom := m.overlayMouseBounds.Max.X-1, m.overlayMouseBounds.Max.Y
		if left < 3 || size[0]-right-1 < 3 || top-1 < 2 || size[1]-2-bottom < 2 {
			t.Fatal("requested outer margin lost")
		}
		for yy := top; yy <= bottom; yy++ {
			for xx := left; xx <= right; xx++ {
				if backgrounds[yy][xx] != "48,52,59" {
					t.Fatalf("size%v cardcell(%d,%d) background%s", size, xx, yy, backgrounds[yy][xx])
				}
			}
		}
		if backgrounds[top][left-1] != "default" {
			t.Fatal("card background escaped rectangle")
		}
		// Content hint styles survive painting the complete padded rectangle.
		if foregrounds[m.overlayMouseBounds.Min.Y+2][x] != "112,125,145" {
			t.Fatal("card fill replaced dim key style")
		}
		m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x + 2, Y: y})
		click := o.seen[len(o.seen)-1].(tea.MouseClickMsg)
		if click.X != 2 || click.Y != 1 {
			t.Fatal("Actions click does not match new inset")
		}
		last := m.overlayMouseBounds.Max.X - 1
		if []rune(plain[m.overlayMouseBounds.Min.Y])[last] != 'Z' {
			t.Fatal("card loses final usable content cell")
		}
		barX, barY := boundedMouseTextPosition(t, view, "┃")
		if barX != right || barX != last || backgrounds[barY][barX+1] != "default" {
			t.Fatal("scrollbar does not touch outer card edge")
		}
		m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: barX, Y: barY})
		barClick := o.seen[len(o.seen)-1].(tea.MouseClickMsg)
		if barClick.X != o.width-1 || barClick.Y != 3 {
			t.Fatal("edge scrollbar mouse column changed")
		}

		m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: last, Y: m.overlayMouseBounds.Min.Y})
		click = o.seen[len(o.seen)-1].(tea.MouseClickMsg)
		if click.X != o.width-1 || click.Y != 0 {
			t.Fatal("last content cell is not reachable")
		}
		before := len(o.seen)
		for _, position := range [][2]int{{x - 1, y}, {x, top}, {right + 1, y}, {x, bottom}} {
			m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: position[0], Y: position[1]})
		}
		if len(o.seen) != before {
			t.Fatal("card padding accepted content click")
		}
		m.Update(uitest.Key("down"))
		m.Update(uitest.Key("f1"))
		if len(o.seen) != before+2 {
			t.Fatal("card appearance swallowed scroll/help keys")
		}
		m.PopOverlay()
		o.card = false
		m.PushOverlay(o)
		legacy := ansi.Strip(m.View().Content)
		if !strings.Contains(legacy, "╭") || !strings.Contains(legacy, "▌") {
			t.Fatal("other modal appearance changed")
		}
	}
}

func TestPanelPolishBorderlessCardTinyViewportClipsMouseBounds(t *testing.T) {
	for _, size := range [][2]int{{20, 8}, {8, 4}, {1, 2}, {0, 0}, {-1, -1}} {
		m, _ := framedShell(t)
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		o := &panelPolishCardDialog{boundedMouseDialog: boundedMouseDialog{local: true, bounded: true}, card: true}
		m.PushOverlay(o)
		m.View()
		bounds := m.overlayMouseBounds
		if !bounds.Empty() && (bounds.Min.X < 0 || bounds.Min.Y < 0 || bounds.Max.X > max(0, size[0]) || bounds.Max.Y > max(0, size[1])) {
			t.Fatal("tiny card content bounds escaped visible screen")
		}
		before := len(o.seen)
		m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: max(0, size[0]) + 1, Y: max(0, size[1]) + 1})
		if len(o.seen) != before {
			t.Fatal("clipped content accepted outside mouse")
		}
	}
}
