package chatshell

import (
	tea "charm.land/bubbletea/v2"
	"github.com/tuigoff/tuigoff/pkg/uitest"
	"strings"
	"testing"
)

func TestWorkspaceComposerActionKeyboardMouseAndCaret(t *testing.T) {
	handler := &fakeHandler{}
	var m *Model
	calls := 0
	m = New(handler, WithWorkspace(nil), WithWorkspaceComposerAction("Select text", func() tea.Cmd { calls++; return m.PushOverlay(&workspaceDialog{title: "SELECT"}) }))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.SetComposerText("first\n世界 last")
	m.Update(uitest.Key("left"))
	draft, caret := m.ComposerText(), m.ComposerCursor()
	if caret != 12 {
		t.Fatalf("multiline rune caret=%d", caret)
	}
	if !strings.Contains(m.View().Content, "Select text") {
		t.Fatal("action not visible")
	}
	routingKey(m, "tab")
	m.syncFocus()
	if m.input.Focused() || !m.ComposerActionFocused() {
		t.Fatal("Tab did not focus action")
	}
	routingKey(m, "enter")
	if calls != 1 || len(m.overlays) != 1 {
		t.Fatal("Enter did not open action")
	}
	routingKey(m, "esc")
	if !m.input.Focused() || m.ComposerText() != draft || m.ComposerCursor() != caret {
		t.Fatal("return lost draft/caret")
	}
	routingKey(m, "shift+tab")
	routingKey(m, "esc")
	if m.composerActionFocused || !m.input.Focused() {
		t.Fatal("Esc did not return from action to editor")
	}
	routingKey(m, "tab")
	routingKey(m, "shift+tab")
	if m.composerActionFocused {
		t.Fatal("reverse Tab stuck on action")
	}
	routingKey(m, "tab")
	routingKey(m, "left")
	if m.composerActionFocused || m.ComposerCursor() != caret-1 {
		t.Fatal("ordinary edit key did not return to editor")
	}
	l, _, _ := m.workspaceWidths()
	m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: l + 3, Y: m.topBarHeight() + m.workspaceBodyHeight() - 1})
	if calls != 2 || len(m.overlays) != 1 {
		t.Fatal("visible mouse action unavailable")
	}
	routingKey(m, "esc")
	if len(handler.submitted) != 0 || m.ComposerText() != draft {
		t.Fatal("action sent/changed draft")
	}
	m.SetBusy(true)
	routingKey(m, "tab")
	if m.composerActionFocused {
		t.Fatal("busy composer activated action")
	}
}

func TestWorkspaceComposerActionRetainsChipsAndCommandPriority(t *testing.T) {
	m := New(&fakeHandler{}, WithWorkspace(nil), WithWorkspaceComposerAction("Select text", func() tea.Cmd { return nil }), WithChips([]Chip{{ID: "one", Label: "Context"}}))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	routingKey(m, "tab")
	if m.chipFocus != 0 || m.composerActionFocused {
		t.Fatal("action stole attachment Tab")
	}
	routingKey(m, "tab")
	if !m.composerActionFocused {
		t.Fatal("action not reachable after attachments")
	}
	routingKey(m, "tab")
	if !m.input.Focused() {
		t.Fatal("Tab did not wrap to draft")
	}
	m.FocusLeftPanel()
	routingKey(m, "tab")
	if m.composerActionFocused {
		t.Fatal("other owner activated composer action")
	}
	legacy := New(&fakeHandler{})
	legacy.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	legacy.SetComposerText("legacy")
	if text := legacy.workspaceComposerContent(20, 3); text != legacy.input.View() {
		t.Fatal("opt-out renderer changed")
	}
}

func TestWorkspaceComposerActionTwoChipBidirectionalFocusRing(t *testing.T) {
	handler := &fakeHandler{}
	m := New(handler, WithWorkspace(nil),
		WithWorkspaceComposerAction("Select text", func() tea.Cmd { return nil }),
		WithChips([]Chip{{ID: "one", Label: "First context"}, {ID: "two", Label: "Second context"}}))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.SetComposerText("draft 世界 tail")
	routingKey(m, "left")
	draft, caret := m.ComposerText(), m.ComposerCursor()
	assertFocus := func(chip int, action bool) {
		t.Helper()
		if m.chipFocus != chip || m.ComposerActionFocused() != action || m.input.Focused() != (chip == -1 && !action) {
			t.Fatalf("focus chip=%d action=%v text=%v; want chip=%d action=%v", m.chipFocus, m.ComposerActionFocused(), m.input.Focused(), chip, action)
		}
		if m.ComposerText() != draft || m.ComposerCursor() != caret || len(handler.submitted) != 0 {
			t.Fatal("focus traversal changed draft/caret or submitted text")
		}
	}
	for _, step := range []struct {
		key    string
		chip   int
		action bool
	}{
		{"tab", 0, false}, {"tab", 1, false},
		{"shift+tab", 0, false}, {"tab", 1, false},
		{"tab", -1, true}, {"tab", -1, false},
		{"shift+tab", -1, true}, {"shift+tab", 1, false},
		{"shift+tab", 0, false}, {"shift+tab", -1, false},
		{"shift+tab", -1, true}, {"esc", -1, false},
	} {
		routingKey(m, step.key)
		assertFocus(step.chip, step.action)
	}
	m.commands = []Command{{Name: "/help", Help: "Show help"}}
	m.SetComposerText("/he")
	routingKey(m, "tab")
	if m.ComposerText() != "/help " || m.ComposerActionFocused() || m.chipFocus != -1 {
		t.Fatal("action intercepted command-menu completion")
	}
}

func TestWorkspaceComposerActionDoesNotChangeNonWorkspaceChipRing(t *testing.T) {
	m := New(&fakeHandler{}, WithWorkspaceComposerAction("Select text", func() tea.Cmd { return nil }),
		WithChips([]Chip{{ID: "one", Label: "First"}, {ID: "two", Label: "Second"}}))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.SetComposerText("legacy draft")
	for _, step := range []struct {
		key  string
		chip int
	}{
		{"tab", 0}, {"tab", 1}, {"tab", -1},
		{"shift+tab", 1}, {"shift+tab", 0}, {"shift+tab", -1},
	} {
		routingKey(m, step.key)
		if m.chipFocus != step.chip || m.ComposerActionFocused() || m.ComposerText() != "legacy draft" {
			t.Fatal("nonworkspace chip ring changed")
		}
	}
}
