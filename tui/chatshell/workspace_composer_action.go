package chatshell

import (
	tea "charm.land/bubbletea/v2"
	"github.com/tuigoff/tuigoff/pkg/focus"
	"github.com/tuigoff/tuigoff/pkg/theme"
	"strings"
)

// WithWorkspaceComposerAction adds one visible Tab/Enter and mouse action to
// the opt-in workspace composer. The callback can open a product-owned modal;
// the textarea remains intact, including its caret and undo history.
func WithWorkspaceComposerAction(label string, action func() tea.Cmd) Option {
	return func(m *Model) { m.composerActionLabel, m.composerAction = label, action }
}

// ComposerActionFocused reports whether the Tab-reachable action owns input.
func (m *Model) ComposerActionFocused() bool { return m.composerActionFocused }

// ComposerText returns the current draft without changing editing state.
func (m *Model) ComposerText() string { return m.input.Value() }

// ComposerCursor returns the caret's rune offset in ComposerText.
func (m *Model) ComposerCursor() int {
	lines := strings.Split(m.input.Value(), "\n")
	offset := 0
	for i := 0; i < m.input.Line(); i++ {
		offset += len([]rune(lines[i])) + 1
	}
	return offset + m.input.Column()
}

func (m *Model) workspaceComposerActionKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if m.composerAction == nil || m.busy || m.WorkspaceFocus() != focus.ZoneInput.String() {
		return nil, false
	}
	if m.composerActionFocused {
		switch msg.String() {
		case "enter":
			m.composerActionFocused = false
			return m.composerAction(), true
		case "tab", "shift+tab", "esc":
			m.composerActionFocused = false
			m.syncFocus()
			if msg.String() == "shift+tab" {
				m.cycleChipFocus(false)
			}
			return nil, true
		default:
			// Leaving the action with a workspace shortcut retains the original caret.
			m.composerActionFocused = false
			m.syncFocus()
			return nil, false
		}
	}
	if (msg.String() == "tab" && (len(m.chips) == 0 || m.chipFocus == len(m.chips)-1) && len(m.commandMenuMatches()) == 0) || (msg.String() == "shift+tab" && m.chipFocus < 0) {
		m.composerActionFocused = true
		m.chipFocus = -1
		m.input.Blur()
		return nil, true
	}
	return nil, false
}

func (m *Model) workspaceComposerContent(width, height int) string {
	rows := 0
	metadata := ""
	if m.workspaceComposerMetadata != nil {
		metadata = theme.WorkspaceMetadata(m.workspaceComposerMetadata())
		rows++
	}
	action := ""
	if m.composerAction != nil {
		prefix := "  "
		if m.composerActionFocused {
			prefix = "› "
		}
		action = prefix + m.composerActionLabel + " " + theme.WorkspaceKeyHint("Tab, Enter")
		rows++
	}
	if rows == 0 {
		return m.input.View()
	}
	text := fitWorkspace(m.input.View(), m.workspaceContentWidth(), max(1, height-rows))
	if metadata != "" {
		text = metadata + "\n" + text
	}
	if action != "" {
		text += "\n" + action
	}
	return text
}
