package chatshell

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/strongo/aichat/ai/session"
	"github.com/tuigoff/tuigoff/pkg/focus"
	"github.com/tuigoff/tuigoff/pkg/theme"
	"github.com/tuigoff/tuigoff/pkg/transcript"
	"strings"
)

// WithWorkspace enables an opt-in three-column workspace. left is the explorer;
// WithSidePanel supplies the right inspector. Existing shell defaults are unchanged.
func WithWorkspace(left SidePanel) Option {
	return func(m *Model) {
		m.workspaceLayout = true
		m.leftPanel = left
		m.leftVisible = left != nil
		m.selectedStop = -1
		m.expandedStop = -1
		m.transcript.SetNeutralWorkspace(true)
	}
}

// FocusComposer transfers exclusive keyboard focus to the persistent composer.
func (m *Model) FocusComposer() {
	m.composerActionFocused = false
	if m.workspaceLayout && (m.leftFocused || m.centerFocused || m.Zone() != focus.ZoneInput) {
		m.composerReturn = m.workspaceCursor()
	}
	m.leftFocused = false
	m.centerFocused = false
	m.focusRing.FocusInput()
	m.syncFocus()
}

// WorkspaceFocus names the active control, including the two optional panels.
func (m *Model) WorkspaceFocus() string {
	if m.leftFocused {
		return "explorer"
	}
	if m.centerFocused {
		return "center"
	}
	return m.Zone().String()
}

// SelectedRef retains the last selected transcript entity after focus leaves it.
func (m *Model) SelectedRef() *session.EntityRef { return m.transcript.SelectedRef(m.selectedStop) }

// Expanded reports whether the selected result owns the center viewport.
func (m *Model) Expanded() bool { return m.expanded }

// ExpandSelected preserves and renders the same Block instance, retaining its state.
func (m *Model) ExpandSelected() bool {
	if m.Zone() != focus.ZoneTranscript || m.transcript.FocusedBlock() == nil {
		return false
	}
	if !m.expanded {
		m.restoreLeft, m.restoreRight = m.leftVisible, m.sidePanelVisible
		m.expandedHistoryOffset = m.transcript.ScrollOffset()
	}
	m.expanded = true
	m.expandedStop = m.focusRing.Stop()
	m.leftVisible = false
	m.sidePanelVisible = false
	m.resize()
	return true
}

// RestoreWorkspace restores panel visibility while retaining selected result and focus.
func (m *Model) RestoreWorkspace() {
	if !m.expanded {
		return
	}
	m.expanded = false
	m.leftVisible, m.sidePanelVisible = m.restoreLeft, m.restoreRight
	m.resize()
	m.transcript.RestoreScrollOffset(m.expandedHistoryOffset)
}

// SetCenterPanel shows non-chat Explore/API content above the same composer.
func (m *Model) SetCenterPanel(p SidePanel, focused bool) {
	m.RestoreWorkspace()
	m.centerPanel = p
	m.leftFocused = false
	m.centerFocused = focused && p != nil
	m.focusRing.FocusInput()
	m.syncFocus()
	m.resize()
}

// ShowTranscript reveals the chat history without creating a second program.
func (m *Model) ShowTranscript() { m.centerPanel = nil; m.FocusComposer() }
func (m *Model) workspaceBodyHeight() int {
	return max(1, m.height-m.topBarHeight()-m.statusSegmentHeight())
}
func (m *Model) workspaceComposerHeight() int { return min(5, max(3, m.workspaceBodyHeight()/4)) }
func (m *Model) workspaceWidths() (left, center, right int) {
	if m.leftVisible && m.leftPanel != nil {
		left = min(28, max(16, m.width/5))
	}
	if m.sidePanelVisible && m.sidePanel != nil {
		right = min(34, max(18, m.width/4))
	}
	if m.width < 100 {
		if !m.compactRightPinned && m.Zone() != focus.ZoneSidebar {
			right = 0
		}
		if left > 0 {
			left = min(24, max(16, m.width/3))
		}
		if right > 0 && m.Zone() == focus.ZoneSidebar {
			right = min(34, max(18, m.width/2))
		}
	}
	if m.width-left-right < 30 {
		if !m.leftFocused {
			left = 0
		}
		if m.width-left-right < 24 && m.Zone() != focus.ZoneSidebar {
			right = 0
		}
	}
	left = min(left, max(0, m.width-1))
	right = min(right, max(0, m.width-left-1))
	center = max(1, m.width-left-right)
	return
}
func (m *Model) resizeWorkspace() {
	m.transcript.SetSize(m.chatWidth(), m.historyHeight())
	m.input.SetWidth(max(1, m.chatWidth()-3))
	m.input.SetHeight(max(1, m.workspaceComposerHeight()-2))
}
func (m *Model) workspaceKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if cmd, handled := m.workspaceComposerActionKey(msg); handled {
		return cmd, true
	}
	switch msg.String() {
	case "ctrl+c":
		// Preserve the shell's cancel/quit handler from every workspace panel.
		return nil, false
	case "f4":
		m.FocusComposer()
		return nil, true
	case "f6":
		m.leftVisible = !m.leftVisible
		if !m.leftVisible && m.leftFocused {
			m.FocusComposer()
		}
		m.resize()
		return nil, true
	case "f7":
		if m.width < 100 && !m.compactRightPinned {
			m.compactRightPinned = true
			m.sidePanelVisible = true
		} else {
			m.sidePanelVisible = !m.sidePanelVisible
		}
		if !m.sidePanelVisible && m.Zone() == focus.ZoneSidebar {
			m.FocusComposer()
		}
		m.resize()
		return nil, true
	case "shift+left":
		if m.expanded {
			m.RestoreWorkspace()
			m.FocusLeftPanel()
			return nil, true
		}
		if m.Zone() == focus.ZoneSidebar {
			m.leftFocused = false
			m.centerFocused = m.centerPanel != nil
			if !m.centerFocused {
				m.focusRing.FocusStop(m.selectedStop)
			}
			m.syncFocus()
			return nil, true
		}
		m.leftVisible = m.leftPanel != nil
		m.leftFocused = m.leftPanel != nil
		m.centerFocused = false
		m.syncFocus()
		m.resize()
		return nil, true
	case "shift+right":
		if m.expanded {
			m.RestoreWorkspace()
			m.sidePanelVisible = m.sidePanel != nil
			m.leftFocused = false
			m.centerFocused = false
			m.focusRing.FocusSidebar()
			m.syncFocus()
			m.resize()
			return nil, true
		}
		if m.leftFocused {
			m.leftFocused = false
			m.centerFocused = m.centerPanel != nil
			if !m.centerFocused {
				m.focusRing.FocusStop(m.selectedStop)
			}
			m.syncFocus()
			return nil, true
		}
		if m.sidePanel != nil {
			m.sidePanelVisible = true
			m.leftFocused = false
			m.centerFocused = false
			m.focusRing.FocusSidebar()
			m.syncFocus()
			m.resize()
		}
		return nil, true
	case "shift+up", "shift+down":
		if m.leftFocused || m.centerFocused || m.Zone() == focus.ZoneSidebar {
			m.leftFocused, m.centerFocused = false, false
			m.centerPanel = nil
			if m.selectedStop >= 0 && m.selectedStop < m.transcript.Stops() {
				m.focusRing.FocusStop(m.selectedStop)
			} else {
				m.focusRing.FocusInput()
			}
		}
		if msg.String() == "shift+up" {
			m.focusRing.ShiftUp(m.transcript.Stops())
		} else {
			m.focusRing.ShiftDown(m.transcript.Stops())
		}
		if m.expanded && (m.Zone() != focus.ZoneTranscript || m.focusRing.Stop() != m.expandedStop) {
			m.RestoreWorkspace()
		}
		m.syncFocus()
		return nil, true
	case "esc":
		if !m.leftFocused && !m.centerFocused && m.Zone() == focus.ZoneTranscript && m.transcript.CapturesEsc() {
			return m.transcript.Update(msg), true
		}
		if !m.leftFocused && !m.centerFocused && m.Zone() == focus.ZoneInput {
			m.restoreWorkspaceCursor(m.composerReturn)
			return nil, true
		}
		if m.leftFocused && m.leftPanel != nil {
			if p, ok := m.leftPanel.(interface{ CapturesEsc() bool }); ok && p.CapturesEsc() {
				panel, cmd := m.leftPanel.Update(msg)
				m.leftPanel = panel
				return cmd, true
			}
		}
		if m.centerFocused && m.centerPanel != nil {
			if p, ok := m.centerPanel.(interface{ CapturesEsc() bool }); ok && p.CapturesEsc() {
				panel, cmd := m.centerPanel.Update(msg)
				m.centerPanel = panel
				return cmd, true
			}
		}
		if !m.leftFocused && m.Zone() == focus.ZoneSidebar && m.sidePanel != nil {
			if p, ok := m.sidePanel.(interface{ CapturesEsc() bool }); ok && p.CapturesEsc() {
				panel, cmd := m.sidePanel.Update(msg)
				m.sidePanel = panel
				return cmd, true
			}
		}
		if m.expanded {
			m.RestoreWorkspace()
			return nil, true
		}
		if m.leftFocused || m.centerFocused || m.Zone() == focus.ZoneSidebar {
			m.FocusComposer()
			return nil, true
		}
	}
	if m.leftFocused && m.leftPanel != nil {
		p, cmd := m.leftPanel.Update(msg)
		m.leftPanel = p
		return cmd, true
	}
	if m.centerFocused && m.centerPanel != nil {
		p, cmd := m.centerPanel.Update(msg)
		m.centerPanel = p
		return cmd, true
	}
	return nil, false
}
func (m *Model) workspaceWheel(msg tea.MouseWheelMsg) tea.Cmd {
	l, c, _ := m.workspaceWidths()
	mouse := msg.Mouse()
	if mouse.X < l && m.leftPanel != nil {
		p, cmd := m.leftPanel.Update(msg)
		m.leftPanel = p
		return cmd
	}
	if mouse.X >= l+c && m.sidePanel != nil {
		p, cmd := m.sidePanel.Update(msg)
		m.sidePanel = p
		return cmd
	}
	if m.centerPanel != nil {
		p, cmd := m.centerPanel.Update(msg)
		m.centerPanel = p
		return cmd
	}
	if consumed, cmd := m.transcript.DeliverWheelToFocusedBlock(msg); consumed {
		return cmd
	}
	if mouse.Button == tea.MouseWheelUp {
		m.transcript.ScrollUp(3)
	} else {
		m.transcript.ScrollDown(3)
	}
	return nil
}
func (m *Model) workspaceView() tea.View {
	m.resizeWorkspace()
	l, c, r := m.workspaceWidths()
	h := m.workspaceBodyHeight()
	interactive := len(m.overlays) == 0
	inputFocus := interactive && !m.leftFocused && !m.centerFocused && m.Zone() == focus.ZoneInput
	m.input.SetStyles(composerTextAreaStyles(false))
	body := m.transcript.View()
	if m.centerPanel != nil {
		body = theme.WorkspaceSurface(m.centerPanel.View(max(1, c-2), m.historyHeight(), interactive && m.centerFocused), c, m.historyHeight(), interactive && m.centerFocused)
	}
	if m.expanded {
		if block := m.transcript.BlockAt(m.expandedStop); block != nil {
			body = theme.WorkspaceSurface(block.View(max(1, c-2), interactive && m.Zone() == focus.ZoneTranscript && !m.leftFocused && !m.centerFocused), c, m.historyHeight(), interactive && m.Zone() == focus.ZoneTranscript && !m.leftFocused && !m.centerFocused)
		}
	}
	if m.busy {
		body = fitWorkspace(body+"\n"+m.spinner.View()+" thinking…", c, m.historyHeight())
	}
	parts := []string{body}
	if menu := m.commandMenuView(); menu != "" {
		parts = append(parts, menu)
	}
	if chips := m.chipsView(c, false); chips != "" {
		parts = append(parts, chips)
	}
	// Constrain the upper content before appending the composer. Large chip or
	// command menus may scroll/crop, but must never crop the active input away.
	upper := fitWorkspace(strings.Join(parts, "\n"), c, max(1, h-m.workspaceComposerHeight()))
	center := fitWorkspace(upper+"\n"+theme.WorkspaceSurface(m.workspaceComposerContent(c, m.workspaceComposerHeight()), c, m.workspaceComposerHeight(), inputFocus), c, h)
	columns := []string{}
	if l > 0 {
		columns = append(columns, theme.WorkspacePane(m.leftPanel.Title(), m.leftPanel.View(max(1, l-2), max(1, h-1), interactive && m.leftFocused), l, h, interactive && m.leftFocused, true))
	}
	columns = append(columns, center)
	if r > 0 {
		active := interactive && !m.leftFocused && !m.centerFocused && m.Zone() == focus.ZoneSidebar
		columns = append(columns, theme.WorkspacePane(m.sidePanel.Title(), m.sidePanel.View(max(1, r-2), max(1, h-1), active), r, h, active, false))
	}
	content := m.topBarView() + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, columns...)
	if rows := m.statusSegmentHeight(); rows > 0 {
		content += "\n" + fitWorkspace(m.statusBarView(), m.width, rows)
	}
	content = fitWorkspace(content, m.width, m.height)
	if n := len(m.overlays); n > 0 {
		content = m.renderOverlay(content, m.overlays[n-1])
		if m.workspaceModalHint {
			lines := strings.Split(content, "\n")
			lines[len(lines)-1] = fitWorkspace(theme.StatusLine(m.width, "F1 Hotkeys · Close dialog to return to workspace"), m.width, 1)
			content = strings.Join(lines, "\n")
		}
	}
	v := tea.NewView(content)
	v.AltScreen = true
	if m.mouseEnabled {
		v.MouseMode = m.mouseMode.mouseTeaMode()
	}
	return v
}
func fitWorkspace(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	for len(lines) < h {
		lines = append(lines, "")
	}
	lines = lines[:min(len(lines), max(1, h))]
	for i, line := range lines {
		line = ansi.Truncate(line, max(1, w), "")
		lines[i] = line + strings.Repeat(" ", max(0, w-lipgloss.Width(line)))
	}
	return strings.Join(lines, "\n")
}

// SetLeftPanel installs a product explorer without changing right inspection.
func (m *Model) SetLeftPanel(p SidePanel) {
	m.leftPanel, m.leftVisible = p, p != nil
	if p == nil && m.leftFocused {
		m.leftFocused = false
		m.restoreWorkspaceCursor(workspaceCursor{})
	}
	m.syncFocus()
	m.resize()
}

// FocusLeftPanel reveals the explorer and gives it exclusive keyboard focus.
func (m *Model) FocusLeftPanel() {
	m.leftVisible = m.leftPanel != nil
	m.leftFocused = m.leftPanel != nil
	m.centerFocused = false
	m.syncFocus()
	m.resize()
}

func (m *Model) workspaceClick(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button != tea.MouseLeft {
		return nil
	}
	l, c, _ := m.workspaceWidths()
	top := m.topBarHeight()
	if !m.busy && len(m.chips) > 0 && msg.X >= l && msg.X < l+c && msg.Y == top+m.historyHeight()+m.menuHeight() {
		cells, _ := m.chipRow(c)
		for _, cell := range cells {
			if msg.X-l == cell.x {
				return m.removeChipAt(cell.index)
			}
		}
	}
	if m.composerAction != nil && !m.busy && msg.X >= l+2 && msg.X < l+c && msg.Y == top+m.workspaceBodyHeight()-1 {
		m.FocusComposer()
		m.composerActionFocused = false
		return m.composerAction()
	}
	if msg.Y < top {
		return m.dispatchUnhandled(msg)
	}
	if msg.X < l && m.leftPanel != nil {
		m.FocusLeftPanel()
		msg.X = max(0, msg.X-2)
		msg.Y = max(0, msg.Y-top-1)
		p, cmd := m.leftPanel.Update(msg)
		m.leftPanel = p
		return cmd
	}
	if msg.X >= l+c && m.sidePanel != nil {
		m.leftFocused = false
		m.centerFocused = false
		m.focusRing.FocusSidebar()
		m.syncFocus()
		msg.X = max(0, msg.X-l-c-2)
		msg.Y = max(0, msg.Y-top-1)
		p, cmd := m.sidePanel.Update(msg)
		m.sidePanel = p
		return cmd
	}
	if msg.Y >= top+m.historyHeight()+m.menuHeight()+m.chipsHeight(c) {
		m.FocusComposer()
		return nil
	}
	if m.centerPanel != nil {
		m.leftFocused = false
		m.centerFocused = true
		m.syncFocus()
		msg.X = max(0, msg.X-l-2)
		msg.Y = max(0, msg.Y-top)
		p, cmd := m.centerPanel.Update(msg)
		m.centerPanel = p
		return cmd
	}
	return m.dispatchUnhandled(msg)
}

// FocusRightPanel reveals Inspect and gives it exclusive focus.
func (m *Model) FocusRightPanel() {
	m.RestoreWorkspace()
	m.sidePanelVisible = m.sidePanel != nil
	m.leftFocused = false
	m.centerFocused = false
	m.focusRing.FocusSidebar()
	m.syncFocus()
	m.resize()
}

// WithWorkspaceHelp supplies contextual help, including above a modal. It is
// used only by opt-in workspaces; all other input stays with the top modal.
func WithWorkspaceHelp(factory func() Overlay) Option {
	return func(m *Model) { m.workspaceHelp = factory }
}

// ActiveOverlayHints lets product help describe the modal that owns input.
// Call before pushing the help overlay so its own controls do not hide those
// of the dialog underneath it. Unknown dialogs still get truthful close help.
func (m *Model) ActiveOverlayHints() []string {
	if len(m.overlays) == 0 {
		return nil
	}
	if provider, ok := m.overlays[len(m.overlays)-1].(interface{ HelpHints() []string }); ok {
		return append([]string(nil), provider.HelpHints()...)
	}
	return []string{"Dialog", "Esc close dialog"}
}

// CenterPanel reports the visible center owner, including during composer
// focus. It is useful for contextual hints about retained temporary views.
func (m *Model) CenterPanel() SidePanel { return m.centerPanel }

func (m *Model) toggleWorkspaceHelp() tea.Cmd {
	m.workspaceModalHint = false
	if len(m.overlays) > 0 {
		top, topOK := overlayIdentity(m.overlays[len(m.overlays)-1])
		help, helpOK := overlayIdentity(m.workspaceHelpOverlay)
		if topOK && helpOK && top == help {
			return m.PopOverlay()
		}
	}
	m.workspaceHelpOverlay = m.workspaceHelp()
	if m.workspaceHelpOverlay == nil {
		return nil
	}
	return m.PushOverlay(m.workspaceHelpOverlay)
}

type workspaceCursor struct {
	left, center bool
	zone         focus.Zone
	stop         int
	valid        bool
	entryID      string
	rawIndex     int
}

func (m *Model) workspaceCursor() workspaceCursor {
	c := workspaceCursor{left: m.leftFocused, center: m.centerFocused, zone: m.Zone(), stop: m.focusRing.Stop(), valid: true, rawIndex: -1}
	if c.zone == focus.ZoneTranscript {
		c.rawIndex = rawIndexForStop(m.transcript.Entries(), c.stop)
		if c.rawIndex >= 0 {
			c.entryID = m.transcript.Entries()[c.rawIndex].ID
		}
	}
	return c
}
func rawIndexForStop(entries []transcript.Entry, target int) int {
	stop := 0
	for i, e := range entries {
		if (e.Block != nil && e.Block.Focusable()) || (e.Block == nil && e.Role == transcript.RoleUser) {
			if stop == target {
				return i
			}
			stop++
		}
	}
	return -1
}
func (m *Model) cursorStop(c workspaceCursor) int {
	if c.entryID != "" {
		return m.transcript.StopForID(c.entryID)
	}
	if c.rawIndex >= 0 {
		return stopForRawIndex(m.transcript.Entries(), c.rawIndex)
	}
	return -1
}
func (m *Model) restoreWorkspaceCursor(c workspaceCursor) {
	m.leftFocused, m.centerFocused = false, false
	switch {
	case c.valid && c.left && m.leftPanel != nil:
		m.leftVisible, m.leftFocused = true, true
	case c.valid && c.center && m.centerPanel != nil:
		m.centerFocused = true
	case c.valid && c.zone == focus.ZoneSidebar && m.sidePanel != nil:
		m.sidePanelVisible = true
		m.focusRing.FocusSidebar()
	case c.valid && c.zone == focus.ZoneTranscript && m.cursorStop(c) >= 0:
		m.centerPanel = nil
		m.focusRing.FocusStop(m.cursorStop(c))
	case c.valid && c.zone == focus.ZoneInput:
		m.focusRing.FocusInput()
	case m.centerPanel != nil:
		m.centerFocused = true
	case m.selectedStop >= 0 && m.selectedStop < m.transcript.Stops():
		m.focusRing.FocusStop(m.selectedStop)
	default:
		m.focusRing.FocusInput()
	}
	m.syncFocus()
	m.resize()
}

// ShowTemporaryCenterPanel shows API/other temporary content and returns an
// idempotent restore function for the exact previous layout and focus. A changed
// transcript invalidates old result/expansion state rather than resurrecting it.
func (m *Model) ShowTemporaryCenterPanel(p SidePanel, fullWidth ...bool) func() {
	center, cursor := m.centerPanel, m.workspaceCursor()
	composerReturn := m.composerReturn
	expanded, stop := m.expanded, m.expandedStop
	expandedRaw := rawIndexForStop(m.transcript.Entries(), stop)
	left, right, restoreLeft, restoreRight := m.leftVisible, m.sidePanelVisible, m.restoreLeft, m.restoreRight
	generation := m.workspaceGeneration
	m.SetCenterPanel(p, true)
	if len(fullWidth) > 0 && fullWidth[0] {
		m.leftVisible, m.sidePanelVisible = false, false
		m.resize()
	}
	restored := false
	return func() {
		if restored {
			return
		}
		restored = true
		if generation != m.workspaceGeneration {
			m.ShowTranscript()
			return
		}
		if expanded {
			stop = stopForRawIndex(m.transcript.Entries(), expandedRaw)
			expanded = stop >= 0
		}
		m.centerPanel, m.expanded, m.expandedStop = center, expanded, stop
		m.composerReturn = composerReturn
		m.leftVisible, m.sidePanelVisible, m.restoreLeft, m.restoreRight = left, right, restoreLeft, restoreRight
		m.restoreWorkspaceCursor(cursor)
	}
}
