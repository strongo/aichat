package chatshell

import (
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/tuigoff/tuigoff/pkg/theme"
	"strings"
	"testing"
)

func TestCompactWorkspacePrioritizesReadableCenterAndExplicitInspector(t *testing.T) {
	m := newWorkspaceJourneyShell(&fakeHandler{}, &workspaceJourneyPanel{name: "Explorer"}, &workspaceJourneyPanel{name: "Inspect"})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.FocusComposer()
	m.input.SetValue("retained compact draft")
	left, center, right := m.workspaceWidths()
	if left < 24 || center < 50 || right != 0 {
		t.Fatalf("compact default wastes content width: %d/%d/%d", left, center, right)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF7})
	_, _, right = m.workspaceWidths()
	if right == 0 || m.input.Value() != "retained compact draft" {
		t.Fatal("explicit F7 did not reveal Inspect or retained draft")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF7})
	_, _, right = m.workspaceWidths()
	if right != 0 {
		t.Fatal("F7 did not hide explicitly opened Inspect")
	}
	m.FocusRightPanel()
	left, _, right = m.workspaceWidths()
	if right < 34 || left != 0 || m.WorkspaceFocus() != "sidebar" {
		t.Fatal("focused Inspect was hidden on compact screens")
	}
	WithWorkspace(nil)(m)
	if left, _, _ = m.workspaceWidths(); left != 0 {
		t.Fatal("compact layout invented an Explorer")
	}
}

func TestWorkspaceExpandedEntryBecomingUnfocusableRestoresValidOwner(t *testing.T) {
	m := newWorkspaceJourneyShell(&fakeHandler{}, &workspaceJourneyPanel{name: "Explorer"}, &workspaceJourneyPanel{name: "Inspect"})
	m.AppendBlockWithID("a", &variableFocusBlock{focusable: true})
	m.AppendBlockWithID("b", &variableFocusBlock{focusable: true})
	m.FocusEntry("b")
	m.ExpandSelected()
	m.ReplaceBlock("b", &variableFocusBlock{focusable: false})
	if m.Expanded() || m.FocusedEntryID() != "a" || m.WorkspaceFocus() != "transcript" {
		t.Fatalf("lost fallback owner: expanded=%v entry=%q owner=%s", m.Expanded(), m.FocusedEntryID(), m.WorkspaceFocus())
	}
}
func TestWorkspaceModalRetainsTitleHelpAndOneActiveSurface(t *testing.T) {
	m := newWorkspaceJourneyShell(&fakeHandler{}, &workspaceJourneyPanel{name: "Explorer"}, &workspaceJourneyPanel{name: "Inspect"})
	m.PushOverlay(&fakeOverlay{name: "Group loaded"})
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "F1 Hotkeys") || !strings.Contains(view, "Esc close dialog") {
		t.Fatalf("modal lost essential controls: %s", view)
	}
	if strings.Count(view, "▌") != 1 {
		t.Fatalf("modal should have one active surface edge: %s", view)
	}
}

type contextualWorkspaceDialog struct{ fakeOverlay }

func (*contextualWorkspaceDialog) HelpHints() []string {
	return []string{"Group columns", "Enter loaded group", "Esc retain source"}
}
func TestWorkspaceHelpContextTracksModalOwnerAndVisibleCenter(t *testing.T) {
	m := newWorkspaceJourneyShell(&fakeHandler{}, &workspaceJourneyPanel{name: "Explorer"}, &workspaceJourneyPanel{name: "Inspect"})
	if m.ActiveOverlayHints() != nil || m.CenterPanel() != nil {
		t.Fatal("empty workspace has a modal context")
	}
	center := &workspaceJourneyPanel{name: "Temporary group"}
	m.SetCenterPanel(center, true)
	if m.CenterPanel() != center {
		t.Fatal("center owner not reported")
	}
	m.PushOverlay(&fakeOverlay{name: "Unknown dialog"})
	if hints := m.ActiveOverlayHints(); len(hints) != 2 || hints[0] != "Dialog" {
		t.Fatalf("unknown modal hints: %v", hints)
	}
	m.PushOverlay(&contextualWorkspaceDialog{})
	hints := m.ActiveOverlayHints()
	if len(hints) != 3 || hints[0] != "Group columns" {
		t.Fatalf("modal hints: %v", hints)
	}
	hints[0] = "mutated"
	if m.ActiveOverlayHints()[0] != "Group columns" {
		t.Fatal("mutable hints leaked into modal")
	}
}

func TestWorkspaceOptionalToolbarRecoversContentAndRetainsFeedback(t *testing.T) {
	visible := true
	m := newWorkspaceJourneyShell(&fakeHandler{}, &workspaceJourneyPanel{name: "Explorer"}, &workspaceJourneyPanel{name: "Inspect"})
	WithHintsProvider(func(int) ([]theme.Hint, []string) { return []theme.Hint{{Key: "F1", Label: "Hotkeys"}}, nil })(m)
	WithStatusBarVisibility(func() bool { return visible })(m)
	shown := m.workspaceBodyHeight()
	if m.statusSegmentHeight() == 0 {
		t.Fatal("toolbar not shown")
	}
	visible = false
	if m.statusSegmentHeight() != 0 || m.workspaceBodyHeight() <= shown || strings.Contains(ansi.Strip(m.View().Content), "Hotkeys") {
		t.Fatal("hidden toolbar retained reserved content rows")
	}
	visible = true
	if m.statusSegmentHeight() == 0 {
		t.Fatal("feedback cannot restore status space")
	}
}

type localMouseWorkspaceDialog struct{ fakeOverlay }

func (*localMouseWorkspaceDialog) LocalMouseCoordinates() bool { return true }
func TestWorkspaceModalMouseUsesVisibleContentCoordinates(t *testing.T) {
	m := newWorkspaceJourneyShell(&fakeHandler{}, &workspaceJourneyPanel{name: "Explorer"}, &workspaceJourneyPanel{name: "Inspect"})
	dialog := &localMouseWorkspaceDialog{fakeOverlay: fakeOverlay{name: "click target"}}
	m.PushOverlay(dialog)
	for y, line := range strings.Split(ansi.Strip(m.View().Content), "\n") {
		if at := strings.Index(line, "overlay:click target"); at >= 0 {
			m.Update(tea.MouseClickMsg{X: ansi.StringWidth(line[:at]) + 3, Y: y, Button: tea.MouseLeft})
			click, ok := dialog.seen[len(dialog.seen)-1].(tea.MouseClickMsg)
			if !ok || click.X != 3 || click.Y != 0 {
				t.Fatalf("local mouse missed rendered target: %#v", click)
			}
			return
		}
	}
	t.Fatal("no visible modal content")
}
