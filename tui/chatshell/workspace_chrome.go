package chatshell

import (
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/tuigoff/tuigoff/pkg/focus"
	"github.com/tuigoff/tuigoff/pkg/theme"
	"github.com/tuigoff/tuigoff/pkg/widgets"
	"strings"
)

// WithWorkspaceTopBarProvider adds distinct opt-in chrome and right navigation.
func WithWorkspaceTopBarProvider(provider TopBarProvider) Option {
	return func(m *Model) { m.workspaceTopBarProvider = provider }
}

// WithWorkspaceSpacing reserves top edge and composer gap/cap rows once.
func WithWorkspaceSpacing() Option { return func(m *Model) { m.workspaceSpacing = true } }

func workspacePanelTitle(panel SidePanel) string {
	if p, ok := panel.(interface{ WorkspaceHeader() (string, string) }); ok {
		title, hint := p.WorkspaceHeader()
		return theme.WorkspacePanelHeader(title, hint)
	}
	return theme.WorkspacePanelHeader(panel.Title(), "")
}
func (m *Model) workspaceTopGap() int {
	if m.workspaceSpacing && m.height-m.topBarHeight()-m.statusSegmentHeight() >= 8 {
		return 1
	}
	return 0
}
func (m *Model) workspaceBodyTop() int { return m.topBarHeight() + m.workspaceTopGap() }
func (m *Model) workspaceComposerSeparation() int {
	if m.workspaceSpacing && m.workspaceBodyHeight() >= 8 {
		return 2
	}
	return 0
}
func (m *Model) workspaceChatHeaderHeight() int {
	if m.workspaceChatTabs != nil && m.centerPanel == nil && m.workspaceBodyHeight() >= 5 {
		return 1
	}
	return 0
}
func (m *Model) workspaceHistoryTop() int {
	return m.workspaceBodyTop() + m.workspaceChatHeaderHeight()
}

// WorkspaceChatTabsProvider supplies session labels and the selected index.
// The product owns session identity, persistence and all switch fences.
type WorkspaceChatTabsProvider func(width int) (tabs []widgets.Tab, active int)

// WorkspaceTabAction is an independent pinned action, such as New or a picker.
type WorkspaceTabAction struct {
	Label  string
	Action func() tea.Cmd
}

// WithWorkspaceChatTabs installs one reserved row above chat, plus click and
// Ctrl+Tab/Ctrl+Shift+Tab switching. Ordinary Tab remains with the focused widget.
func WithWorkspaceChatTabs(provider WorkspaceChatTabsProvider, activate func(index int) tea.Cmd) Option {
	return func(m *Model) { m.workspaceChatTabs, m.workspaceChatActivate = provider, activate }
}

// WithWorkspaceChatTabActions pins product actions to the right of chat tabs.
// Earlier actions have priority when space is limited.
func WithWorkspaceChatTabActions(actions ...WorkspaceTabAction) Option {
	return func(m *Model) { m.workspaceChatTabActions = append([]WorkspaceTabAction(nil), actions...) }
}

type workspaceActionRange struct{ start, end, index int }

func (m *Model) workspaceChatTabRow(width int) (string, widgets.Tabs, []workspaceActionRange) {
	tabs, active := m.workspaceChatTabs(width)
	available := max(1, width)
	var actions []workspaceActionRange
	var suffix strings.Builder
	// Keep at least half the row for the active session; omit later actions.
	for i, action := range m.workspaceChatTabActions {
		label := " " + action.Label + " "
		actionWidth := ansi.StringWidth(label)
		if actionWidth > available-max(1, width/2) {
			break
		}
		actions = append(actions, workspaceActionRange{start: 0, end: actionWidth, index: i})
		available -= actionWidth
		suffix.WriteString(theme.WorkspaceTabStyle(false, false).Render(label))
	}
	offset := available
	for i := range actions {
		actions[i].start = offset
		offset += actions[i].end
		actions[i].end = offset
	}
	strip := widgets.NewTabs("workspace-chat", widgets.TabsStyle{Workspace: true}, tabs...)
	strip.SetActive(active)
	strip.SetSize(available, 1)
	if len(m.overlays) == 0 && !m.leftFocused && !m.centerFocused && m.Zone() != focus.ZoneSidebar {
		strip.Focus()
	}
	return strip.View() + suffix.String(), strip, actions
}
func (m *Model) workspaceChatTabClick(x, width int) tea.Cmd {
	_, strip, actions := m.workspaceChatTabRow(width)
	for _, r := range actions {
		if x >= r.start && x < r.end {
			if action := m.workspaceChatTabActions[r.index].Action; action != nil {
				return action()
			}
			return nil
		}
	}
	_, cmd := strip.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: 0})
	if cmd != nil {
		switch event := cmd().(type) {
		case widgets.TabChangedMsg:
			if m.workspaceChatActivate != nil {
				return m.workspaceChatActivate(event.Index)
			}
		case widgets.TabCloseMsg:
			if m.workspaceChatClose != nil {
				return m.workspaceChatClose(event.Index)
			}
		}
	}
	return nil
}
func (m *Model) workspaceChatTabKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if m.workspaceChatHeaderHeight() == 0 || m.workspaceChatActivate == nil || m.leftFocused || m.centerFocused || m.Zone() == focus.ZoneSidebar || len(m.commandMenuMatches()) > 0 {
		return nil, false
	}
	delta := 0
	switch msg.String() {
	case "ctrl+tab":
		delta = 1
	case "ctrl+shift+tab", "shift+ctrl+tab":
		delta = -1
	default:
		return nil, false
	}
	tabs, active := m.workspaceChatTabs(m.chatWidth())
	if len(tabs) < 2 {
		return nil, true
	}
	next := (max(0, active) + delta + len(tabs)) % len(tabs)
	return m.workspaceChatActivate(next), true
}

// WithWorkspaceChatTabClose routes a tab close mark to a product chooser.
// The shell never deletes sessions or removes tabs by itself.
func WithWorkspaceChatTabClose(closeTab func(index int) tea.Cmd) Option {
	return func(m *Model) { m.workspaceChatClose = closeTab }
}
