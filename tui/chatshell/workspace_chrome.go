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
	if (m.workspaceSpacing || m.workspaceChatFrame) && m.workspaceBodyHeight() >= 8 {
		return 2
	}
	return 0
}

// WithWorkspaceChromeProvider installs a structured, independently styled header.
func WithWorkspaceChromeProvider(provider func(int) theme.WorkspaceChrome) Option {
	return func(m *Model) { m.workspaceChromeProvider = provider }
}

// WithWorkspaceComposerMetadata places descriptive model context in the composer.
func WithWorkspaceComposerMetadata(provider func() theme.WorkspaceContext) Option {
	return func(m *Model) { m.workspaceComposerMetadata = provider }
}

// WithWorkspaceChatFrame joins session tabs, history and composer, reusing their marker column.
func WithWorkspaceChatFrame() Option { return func(m *Model) { m.workspaceChatFrame = true } }

// WithWorkspacePanelControls enables independent side widths and focused resize footers.
func WithWorkspacePanelControls() Option { return func(m *Model) { m.workspacePanelControls = true } }

func (m *Model) workspaceFrameBottom() int {
	if m.workspaceChatFrame {
		return 1
	}
	return 0
}
func (m *Model) workspaceChatHeaderHeight() int {
	if m.workspaceChatFrame && m.centerPanel == nil {
		if m.workspaceChatTabs != nil && m.workspaceBodyHeight() >= 6 {
			return 2
		}
		return 1
	}
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
	inset := 0
	if m.workspaceChatFrame {
		inset = 2
		width = max(1, width-3)
	}
	tabs, active := m.workspaceChatTabs(width)
	if m.workspaceTabFocused {
		active = max(0, min(m.workspaceTabSelection, len(tabs)-1))
	}
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
	strip := widgets.NewTabs("workspace-chat", widgets.TabsStyle{Workspace: true, ChatFrame: m.workspaceChatFrame}, tabs...)
	strip.SetActive(active)
	strip.SetSize(available, 1)
	if len(m.overlays) == 0 && ((!m.workspaceChatFrame && !m.leftFocused && !m.centerFocused && m.Zone() != focus.ZoneSidebar) || m.workspaceTabFocused) {
		strip.Focus()
	}
	for i := range actions {
		actions[i].start += inset
		actions[i].end += inset
	}
	row := strip.View()
	if m.workspaceChatFrame {
		rows := strings.Split(row, "\n")
		row = "  " + rows[0] + suffix.String()
		row += "\n" + theme.WorkspaceFrameRule("╭─") + rows[1] + theme.WorkspaceFrameRule(strings.Repeat("─", ansi.StringWidth(suffix.String()))+"╮")
	} else {
		row += suffix.String()
	}
	return row, strip, actions
}
func (m *Model) workspaceChatTabClick(x, width int) tea.Cmd {
	_, strip, actions := m.workspaceChatTabRow(width)
	for _, r := range actions {
		if x >= r.start && x < r.end {
			if m.workspaceTabFocused {
				m.restoreWorkspaceTabFocus()
			}
			if action := m.workspaceChatTabActions[r.index].Action; action != nil {
				return action()
			}
			return nil
		}
	}
	if m.workspaceChatFrame {
		x -= 2
	}
	if x < 0 {
		return nil
	}
	_, cmd := strip.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: 0})
	if cmd != nil {
		if m.workspaceTabFocused {
			m.restoreWorkspaceTabFocus()
		}
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
	key := msg.String()
	wasTabFocused := m.workspaceTabFocused
	if key == "f11" && m.workspaceChatFrame && m.workspaceChatTabs != nil && m.workspaceChatActivate != nil && m.centerPanel == nil {
		if m.workspaceTabFocused {
			m.restoreWorkspaceTabFocus()
			return nil, true
		}
		m.workspaceTabReturn = m.workspaceCursor()
		tabs, active := m.workspaceChatTabs(m.chatWidth())
		if len(tabs) == 0 {
			return nil, true
		}
		m.workspaceTabSelection = max(0, active)
		m.workspaceTabFocused = true
		m.leftFocused, m.centerFocused = false, false
		m.composerActionFocused = false
		m.syncFocus()
		return nil, true
	}
	if m.workspaceTabFocused {
		tabs, _ := m.workspaceChatTabs(m.chatWidth())
		switch key {
		case "left":
			m.workspaceTabSelection = max(0, m.workspaceTabSelection-1)
			return nil, true
		case "right":
			m.workspaceTabSelection = min(max(0, len(tabs)-1), m.workspaceTabSelection+1)
			return nil, true
		case "enter":
			index := m.workspaceTabSelection
			m.restoreWorkspaceTabFocus()
			if index >= 0 && index < len(tabs) {
				return m.workspaceChatActivate(index), true
			}
			return nil, true
		case "esc":
			m.restoreWorkspaceTabFocus()
			return nil, true
		case "ctrl+tab", "ctrl+shift+tab", "shift+ctrl+tab":
			m.restoreWorkspaceTabFocus()
		case "shift+left", "shift+right", "shift+up", "shift+down", "f4", "f6", "f7":
			m.restoreWorkspaceTabFocus()
			return nil, false
		case "ctrl+c":
			return nil, false
		default:
			return nil, true
		}
	}
	if m.workspaceChatHeaderHeight() == 0 || m.workspaceChatTabs == nil || m.workspaceChatActivate == nil || (!wasTabFocused && (m.leftFocused || m.centerFocused || m.Zone() == focus.ZoneSidebar || len(m.commandMenuMatches()) > 0)) {
		return nil, false
	}
	delta := 0
	switch key {
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
func (m *Model) restoreWorkspaceTabFocus() {
	m.workspaceTabFocused = false
	m.restoreWorkspaceCursor(m.workspaceTabReturn)
}

// WithWorkspaceChatTabClose routes a tab close mark to a product chooser.
// The shell never deletes sessions or removes tabs by itself.
func WithWorkspaceChatTabClose(closeTab func(index int) tea.Cmd) Option {
	return func(m *Model) { m.workspaceChatClose = closeTab }
}

// workspaceDialogHint keeps help discoverable unless the typed header already shows it.
func (m *Model) workspaceDialogHint(returnHint bool) string {
	if m.workspaceChromeProvider != nil {
		chrome := m.workspaceChromeProvider(m.width)
		for _, action := range chrome.Actions {
			if action.Key == "F1" && strings.Contains(ansi.Strip(m.topBarView()), action.Key+" "+action.Label) {
				label := "close dialog"
				if returnHint {
					label = "close dialog to return to workspace"
				}
				return theme.WorkspaceKeyHint("Esc") + " " + label
			}
		}
	}
	if returnHint {
		return "F1 Hotkeys · Close dialog to return to workspace"
	}
	return "F1 Hotkeys · Esc close dialog"
}
