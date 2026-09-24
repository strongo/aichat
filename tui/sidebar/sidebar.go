package sidebar

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/strongo/aichat/ai/session"
)

// Renderer formats one entity ref for the sidebar list at the given width.
type Renderer func(ref session.EntityRef, width int) string

// RemoveMsg is emitted when the user removes ref from the sidebar (x/Delete).
type RemoveMsg struct{ Ref session.EntityRef }

// OpenMsg is emitted when the user activates ref (Enter).
type OpenMsg struct{ Ref session.EntityRef }

// Default split-width bounds, matching DataTug's Ctrl+←/→ chatPanePercent
// clamp (ui.go: max(40, min(75, ...))). Percent is the CHAT pane's share; the
// sidebar gets the remainder.
const (
	MinChatPercent = 40
	MaxChatPercent = 75
)

// Model is the sidebar panel.
type Model struct {
	refs    []session.EntityRef
	render  Renderer
	cursor  int
	visible bool
	width   int
	// chatPercent is the chat pane's width share when split; the sidebar's
	// own width is derived by the caller (chatshell) from the total width.
	chatPercent int
}

// New returns a visible, empty sidebar using render to format entries.
func New(render Renderer) *Model {
	if render == nil {
		render = func(ref session.EntityRef, width int) string { return ref.Title }
	}
	return &Model{render: render, visible: true, chatPercent: 65}
}

// Refs returns the current ordered sidebar entries.
func (m *Model) Refs() []session.EntityRef { return m.refs }

// Add pins ref unless already present. Reports whether it changed.
func (m *Model) Add(ref session.EntityRef) bool {
	for _, r := range m.refs {
		if r.Same(ref) {
			return false
		}
	}
	m.refs = append(m.refs, ref)
	return true
}

// Remove unpins ref. Reports whether it was present.
func (m *Model) Remove(ref session.EntityRef) bool {
	for i, r := range m.refs {
		if r.Same(ref) {
			m.refs = append(m.refs[:i], m.refs[i+1:]...)
			if m.cursor >= len(m.refs) {
				m.cursor = max(0, len(m.refs)-1)
			}
			return true
		}
	}
	return false
}

// Cursor returns the index of the highlighted entry, or -1 when empty.
func (m *Model) Cursor() int {
	if len(m.refs) == 0 {
		return -1
	}
	return m.cursor
}

// Toggle flips visibility (F6).
func (m *Model) Toggle()           { m.visible = !m.visible }
func (m *Model) Visible() bool     { return m.visible }
func (m *Model) SetVisible(v bool) { m.visible = v }

// SetWidth sets the panel's rendering width.
func (m *Model) SetWidth(width int) { m.width = max(1, width) }

// ChatPercent returns the chat pane's current width share (see
// MinChatPercent/MaxChatPercent).
func (m *Model) ChatPercent() int { return m.chatPercent }

// GrowChat/ShrinkChat adjust the split by delta percentage points, clamped
// to [MinChatPercent, MaxChatPercent] (Ctrl+→/Ctrl+← in DataTug).
func (m *Model) GrowChat(delta int) {
	m.chatPercent = max(MinChatPercent, min(MaxChatPercent, m.chatPercent+delta))
}
func (m *Model) ShrinkChat(delta int) { m.GrowChat(-delta) }

// Update handles cursor movement, remove and open key presses. focused gates
// whether the sidebar consumes the key (callers route key events here only
// while the focus ring is on the sidebar).
func (m *Model) Update(msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch keyMsg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor+1 < len(m.refs) {
			m.cursor++
		}
	case "x", "delete", "backspace":
		if m.cursor >= 0 && m.cursor < len(m.refs) {
			ref := m.refs[m.cursor]
			m.Remove(ref)
			return func() tea.Msg { return RemoveMsg{Ref: ref} }
		}
	case "enter":
		if m.cursor >= 0 && m.cursor < len(m.refs) {
			ref := m.refs[m.cursor]
			return func() tea.Msg { return OpenMsg{Ref: ref} }
		}
	}
	return nil
}

// View renders the sidebar list.
func (m *Model) View(width int, focused bool) string {
	width = max(1, width)
	if width != m.width {
		m.SetWidth(width)
	}
	title := lipgloss.NewStyle().Bold(true).Render("Sidebar")
	if len(m.refs) == 0 {
		return title + "\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render("(empty)")
	}
	lines := make([]string, 0, len(m.refs)+1)
	lines = append(lines, title)
	for i, ref := range m.refs {
		line := m.render(ref, max(1, width-2))
		if focused && i == m.cursor {
			line = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("51")).Render("› " + line)
		} else {
			line = "  " + line
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
