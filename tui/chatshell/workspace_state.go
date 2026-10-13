package chatshell

import (
	tea "charm.land/bubbletea/v2"
	"github.com/tuigoff/tuigoff/pkg/focus"
)

// ComposerState is a product-owned draft snapshot. Cursor counts Unicode runes,
// including newlines; chips are copied when captured and restored.
type ComposerState struct {
	Text   string
	Cursor int
	Chips  []Chip
}

// CaptureComposerState does not disturb editing or focus.
func (m *Model) CaptureComposerState() ComposerState {
	return ComposerState{Text: m.ComposerText(), Cursor: m.ComposerCursor(), Chips: m.Chips()}
}

// RestoreComposerState restores a session's draft and exact bounded caret.
// It clears cross-session undo/action focus without issuing product mutations.
func (m *Model) RestoreComposerState(state ComposerState) {
	focused := m.input.Focused()
	m.input.SetValue(state.Text)
	m.input.Focus()
	m.input, _ = m.input.Update(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	for range min(max(0, state.Cursor), len([]rune(m.input.Value()))) {
		m.input, _ = m.input.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	}
	if !focused {
		m.input.Blur()
	}
	m.chips = append([]Chip(nil), state.Chips...)
	m.chipFocus = -1
	m.composerUndo = nil
	m.composerActionFocused = false
	m.resize()
}

// WorkspaceViewport retains the focused entry and transcript scroll position.
// A missing entry restores composer focus, never a different result.
type WorkspaceViewport struct {
	EntryID      string
	ScrollOffset int
}

func (m *Model) CaptureWorkspaceViewport() WorkspaceViewport {
	id := ""
	if !m.leftFocused && !m.centerFocused && m.Zone() == focus.ZoneTranscript {
		id = m.FocusedEntryID()
	}
	return WorkspaceViewport{EntryID: id, ScrollOffset: m.transcript.ScrollOffset()}
}

// RestoreWorkspaceViewport is called after the exact session history is loaded.
func (m *Model) RestoreWorkspaceViewport(view WorkspaceViewport) {
	if view.EntryID == "" || !m.FocusEntry(view.EntryID) {
		m.FocusComposer()
	}
	m.transcript.RestoreScrollOffset(max(0, view.ScrollOffset))
}

// HasOverlay reports membership by the same non-nil pointer identity used by
// CloseOverlay, without changing the stack or focus. It is safe for async
// product results whose originating overlay may have been closed or covered.
func (m *Model) HasOverlay(overlay Overlay) bool {
	target, ok := overlayIdentity(overlay)
	if !ok {
		return false
	}
	for _, existing := range m.overlays {
		if id, ok := overlayIdentity(existing); ok && id == target {
			return true
		}
	}
	return false
}

// InputEpoch counts key presses, paste boundaries/content, and mouse messages
// before routing, including input consumed by overlays or workspace controls.
// Key releases are deliberately neutral so release-reporting terminals can
// still recognize consecutive action-key sequences. Non-input messages and
// programmatic focus/overlay changes do not increment it.
func (m *Model) InputEpoch() uint64 { return m.inputEpoch }
