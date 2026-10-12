package chatshell

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/strongo/aichat/ai/session"
	"github.com/strongo/aichat/tui/stream"
	"github.com/tuigoff/tuigoff/pkg/focus"
	"github.com/tuigoff/tuigoff/pkg/transcript"
)

type workspaceJourneyPanel struct {
	name        string
	updates     []tea.Msg
	capturesEsc bool
}

func (p *workspaceJourneyPanel) Title() string              { return p.name }
func (p *workspaceJourneyPanel) View(int, int, bool) string { return p.name }
func (p *workspaceJourneyPanel) Update(msg tea.Msg) (SidePanel, tea.Cmd) {
	p.updates = append(p.updates, msg)
	return p, nil
}
func (p *workspaceJourneyPanel) CapturesEsc() bool { return p.capturesEsc }

type workspaceJourneyWheelBlock struct {
	fakeBlock
	consumes bool
}

func (b *workspaceJourneyWheelBlock) ConsumesWheel(tea.MouseWheelMsg) bool { return b.consumes }

type workspaceJourneyInitHandler struct {
	fakeHandler
	initCalls int
}

func (h *workspaceJourneyInitHandler) Init() tea.Cmd {
	h.initCalls++
	return func() tea.Msg { return stream.DoneMsg{} }
}

type workspaceJourneyAsyncHandler struct {
	fakeHandler
	model  *Model
	target Overlay
}

func (h *workspaceJourneyAsyncHandler) OnMsg(msg tea.Msg) tea.Cmd {
	if result, ok := msg.(asyncResultMsg); ok && result.ok {
		h.model.CloseOverlay(h.target)
	}
	return nil
}

func newWorkspaceJourneyShell(h Handler, left, right *workspaceJourneyPanel) *Model {
	m := New(h,
		WithWorkspace(left), WithSidePanel(right), WithMouse(MouseCellMotion),
		WithTopBar(func(int) string { return "DataTug" }),
	)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 42})
	return m
}

func TestWorkspaceMouseJourneyRoutesPanelsComposerChipsAndHeaderOnce(t *testing.T) {
	h := &fakeHandler{}
	left := &workspaceJourneyPanel{name: "Explorer"}
	right := &workspaceJourneyPanel{name: "Inspect"}
	center := &workspaceJourneyPanel{name: "API"}
	m := newWorkspaceJourneyShell(h, left, right)
	block := &fakeBlock{}
	m.AppendBlockWithID("result", block)
	m.FocusEntry("result")
	m.SetCenterPanel(center, false)
	m.SetMouseEnabled(true)
	left.updates, right.updates = nil, nil
	h.msgsSeen = nil

	l, c, _ := m.workspaceWidths()
	top := m.topBarHeight()
	click := func(x, y int) { m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}) }

	click(5, top+2)
	if len(left.updates) != 1 || m.WorkspaceFocus() != "explorer" || m.input.Focused() || m.centerFocused || m.Zone() == focus.ZoneSidebar {
		t.Fatalf("Explorer click did not take exclusive focus: updates=%d focus=%s", len(left.updates), m.WorkspaceFocus())
	}
	leftClick := left.updates[0].(tea.MouseClickMsg)
	if leftClick.X != 3 || leftClick.Y != 1 {
		t.Fatalf("Explorer got translated click (%d,%d), want (3,1)", leftClick.X, leftClick.Y)
	}

	click(l+c+4, top+3)
	if len(right.updates) != 1 || m.WorkspaceFocus() != focus.ZoneSidebar.String() || m.input.Focused() {
		t.Fatalf("Inspector click did not take exclusive focus: updates=%d focus=%s", len(right.updates), m.WorkspaceFocus())
	}
	rightClick := right.updates[0].(tea.MouseClickMsg)
	if rightClick.X != 2 || rightClick.Y != 2 {
		t.Fatalf("Inspector got translated click (%d,%d), want (2,2)", rightClick.X, rightClick.Y)
	}

	click(l+6, top+4)
	if len(center.updates) != 1 || m.WorkspaceFocus() != "center" || m.input.Focused() {
		t.Fatalf("center click did not take exclusive focus: updates=%d focus=%s", len(center.updates), m.WorkspaceFocus())
	}
	centerClick := center.updates[0].(tea.MouseClickMsg)
	if centerClick.X != 4 || centerClick.Y != 4 {
		t.Fatalf("center got translated click (%d,%d), want (4,4)", centerClick.X, centerClick.Y)
	}
	if block.updates != 0 || len(h.msgsSeen) != 0 {
		t.Fatalf("routed panel click leaked underlay: block updates=%d handler messages=%d", block.updates, len(h.msgsSeen))
	}

	// Clicking the attachment row removes its chip without being delivered to a panel.
	m.SetChips([]Chip{{ID: "one", Label: "invoice.csv"}, {ID: "two", Label: "notes.txt"}})
	cells, _ := m.chipRow(c)
	chipBefore := len(m.Chips())
	click(l+cells[0].x, top+m.historyHeight()+m.menuHeight())
	if got := len(m.Chips()); got != chipBefore-1 {
		t.Fatalf("chip row click left %d chips, want %d", got, chipBefore-1)
	}
	if len(left.updates) != 1 || len(center.updates) != 1 || len(right.updates) != 1 || block.updates != 0 {
		t.Fatalf("chip click reached another surface: left=%d center=%d right=%d block=%d", len(left.updates), len(center.updates), len(right.updates), block.updates)
	}

	// A composer click clears panel focus and does not bubble to result blocks.
	composerY := top + m.historyHeight() + m.menuHeight() + m.chipsHeight(c)
	click(l+2, composerY)
	if m.WorkspaceFocus() != focus.ZoneInput.String() || !m.input.Focused() || m.centerFocused || m.leftFocused {
		t.Fatalf("composer click did not take exclusive focus: focus=%s", m.WorkspaceFocus())
	}
	if block.updates != 0 {
		t.Fatalf("composer click reached transcript block %d times", block.updates)
	}

	// A click on the top bar uses the normal unhandled path once, preserving the
	// product hook without also routing through the workspace panels.
	leftBefore, centerBefore, rightBefore, handlerBefore := len(left.updates), len(center.updates), len(right.updates), len(h.msgsSeen)
	click(1, 0)
	if len(h.msgsSeen) != handlerBefore+1 || len(left.updates) != leftBefore || len(center.updates) != centerBefore || len(right.updates) != rightBefore+1 || block.updates != 1 {
		t.Fatalf("header click dispatched more than once: handler %d->%d panels %d/%d/%d -> %d/%d/%d", handlerBefore, len(h.msgsSeen), leftBefore, centerBefore, rightBefore, len(left.updates), len(center.updates), len(right.updates))
	}
}

func TestWorkspaceWheelJourneyRoutesOnlyToColumnOwner(t *testing.T) {
	h := &fakeHandler{}
	left := &workspaceJourneyPanel{name: "Explorer"}
	right := &workspaceJourneyPanel{name: "Inspect"}
	center := &workspaceJourneyPanel{name: "API"}
	m := newWorkspaceJourneyShell(h, left, right)
	block := &workspaceJourneyWheelBlock{consumes: true}
	m.AppendBlockWithID("wheel", block)
	m.FocusEntry("wheel")
	m.SetCenterPanel(center, false)
	left.updates, center.updates, right.updates = nil, nil, nil
	leftWidth, centerWidth, _ := m.workspaceWidths()

	m.Update(tea.MouseWheelMsg{X: leftWidth - 1, Button: tea.MouseWheelDown})
	m.Update(tea.MouseWheelMsg{X: leftWidth + 2, Button: tea.MouseWheelDown})
	m.Update(tea.MouseWheelMsg{X: leftWidth + centerWidth + 1, Button: tea.MouseWheelDown})
	if len(left.updates) != 1 || len(center.updates) != 1 || len(right.updates) != 1 {
		t.Fatalf("wheel owners got calls left/center/right=%d/%d/%d", len(left.updates), len(center.updates), len(right.updates))
	}
	if block.updates != 0 {
		t.Fatalf("column-owned wheel reached transcript block %d times", block.updates)
	}
	if _, ok := left.updates[0].(tea.MouseWheelMsg); !ok {
		t.Fatalf("Explorer received %T, want wheel", left.updates[0])
	}

	// With no center panel, the focused result gets first refusal and the
	// transcript viewport must not also scroll for the same wheel event.
	m.SetCenterPanel(nil, false)
	m.FocusEntry("wheel")
	before := m.transcript.ScrollOffset()
	m.Update(tea.MouseWheelMsg{X: leftWidth + 2, Button: tea.MouseWheelDown})
	if block.updates != 1 || m.transcript.ScrollOffset() != before {
		t.Fatalf("focused result wheel was duplicated: block updates=%d scroll %d->%d", block.updates, before, m.transcript.ScrollOffset())
	}
}

func TestWorkspaceFocusJourneyF4EscInwardShiftAndCapturingPanels(t *testing.T) {
	for _, start := range []string{"explorer", "center", "inspector"} {
		t.Run(start, func(t *testing.T) {
			left := &workspaceJourneyPanel{name: "Explorer"}
			right := &workspaceJourneyPanel{name: "Inspect"}
			m := newWorkspaceJourneyShell(&fakeHandler{}, left, right)
			center := &workspaceJourneyPanel{name: "API"}
			m.SetCenterPanel(center, false)
			switch start {
			case "explorer":
				m.FocusLeftPanel()
			case "center":
				m.centerFocused = true
				m.syncFocus()
			case "inspector":
				m.FocusRightPanel()
			}
			routingKey(m, "f4")
			if m.WorkspaceFocus() != focus.ZoneInput.String() || !m.input.Focused() || m.leftFocused || m.centerFocused {
				t.Fatalf("F4 did not transfer exclusive focus from %s to composer", start)
			}
			routingKey(m, "esc")
			want := map[string]string{"explorer": "explorer", "center": "center", "inspector": "sidebar"}[start]
			if m.WorkspaceFocus() != want || m.input.Focused() {
				t.Fatalf("Esc returned focus to %s, want %s", m.WorkspaceFocus(), want)
			}
		})
	}

	left := &workspaceJourneyPanel{name: "Explorer"}
	right := &workspaceJourneyPanel{name: "Inspect"}
	m := newWorkspaceJourneyShell(&fakeHandler{}, left, right)
	center := &workspaceJourneyPanel{name: "API"}
	m.SetCenterPanel(center, false)
	m.FocusLeftPanel()
	routingKey(m, "shift+right")
	if !m.centerFocused || m.WorkspaceFocus() != "center" {
		t.Fatal("Shift+Right from Explorer did not move inward to the center panel")
	}
	m.FocusRightPanel()
	routingKey(m, "shift+left")
	if !m.centerFocused || m.WorkspaceFocus() != "center" {
		t.Fatal("Shift+Left from Inspector did not move inward to the center panel")
	}

	left.capturesEsc = true
	m.FocusLeftPanel()
	routingKey(m, "esc")
	if len(left.updates) != 1 || m.WorkspaceFocus() != "explorer" || m.input.Focused() {
		t.Fatal("Explorer did not retain an Esc it captures")
	}
	center.capturesEsc = true
	m.SetCenterPanel(center, true)
	center.updates = nil
	routingKey(m, "esc")
	if len(center.updates) != 1 || m.WorkspaceFocus() != "center" {
		t.Fatal("center panel did not retain an Esc it captures")
	}
	right.capturesEsc = true
	m.FocusRightPanel()
	right.updates = nil
	routingKey(m, "esc")
	if len(right.updates) != 1 || m.WorkspaceFocus() != focus.ZoneSidebar.String() {
		t.Fatal("Inspector did not retain an Esc it captures")
	}
}

func TestWorkspaceTemporaryPanelRestoresEveryCursorAndRejectsStaleResult(t *testing.T) {
	for _, start := range []string{"explorer", "center", "inspector", "input"} {
		t.Run(start, func(t *testing.T) {
			left := &workspaceJourneyPanel{name: "Explorer"}
			right := &workspaceJourneyPanel{name: "Inspect"}
			m := newWorkspaceJourneyShell(&fakeHandler{}, left, right)
			center := &workspaceJourneyPanel{name: "API"}
			m.SetCenterPanel(center, false)
			switch start {
			case "explorer":
				m.FocusLeftPanel()
			case "center":
				m.centerFocused = true
				m.syncFocus()
			case "inspector":
				m.FocusRightPanel()
			case "input":
				m.FocusComposer()
			}
			before := m.workspaceCursor()
			restore := m.ShowTemporaryCenterPanel(&workspaceTextPanel{text: "Temporary API"})
			if !m.centerFocused || m.centerPanel == nil || m.centerPanel.Title() != "Temporary API" {
				t.Fatal("temporary panel did not become the center owner")
			}
			restore()
			restore() // restoration is idempotent
			if got := m.workspaceCursor(); got != before {
				t.Fatalf("temporary return changed cursor from %+v to %+v", before, got)
			}
		})
	}

	m, _ := routingWorkspace(t)
	m.ExpandSelected()
	routingKey(m, "f4")
	if m.WorkspaceFocus() != focus.ZoneInput.String() {
		t.Fatal("expanded result did not transfer to the composer")
	}
	routingKey(m, "esc")
	if !m.Expanded() || m.WorkspaceFocus() != "transcript" {
		t.Fatal("Esc did not restore the captured expanded transcript cursor")
	}

	m, _ = routingWorkspace(t)
	staleReturn := m.ShowTemporaryCenterPanel(&workspaceTextPanel{text: "Stale API"})
	m.ClearTranscript()
	staleReturn()
	if m.centerPanel != nil || m.WorkspaceFocus() != focus.ZoneInput.String() {
		t.Fatal("stale API return restored a panel after transcript replacement")
	}
}

func TestWorkspaceChangingExpandedResultMovesFocusAndRightPanelRestoresLayout(t *testing.T) {
	m, grid := routingWorkspace(t)
	if !m.ExpandSelected() {
		t.Fatal("result did not expand")
	}
	if !m.FocusEntry("first") {
		t.Fatal("could not focus the preceding transcript result")
	}
	if m.Expanded() || m.transcript.FocusedBlock() == nil || m.transcript.FocusedBlock().(*fakeBlock) == nil || grid.Focused() {
		t.Fatal("switching result left the old expanded grid active")
	}
	if m.focusRing.Stop() != 0 {
		t.Fatalf("new transcript stop = %d, want 0", m.focusRing.Stop())
	}
	m.FocusRightPanel()
	if m.WorkspaceFocus() != focus.ZoneSidebar.String() || !m.sidePanelVisible || m.Expanded() {
		t.Fatal("Inspector focus did not restore the regular workspace layout")
	}
	m.SetLeftPanel(nil)
	if m.leftPanel != nil || m.leftVisible || m.WorkspaceFocus() != focus.ZoneSidebar.String() {
		t.Fatalf("removing the Explorer disturbed the remaining focus owner: %s", m.WorkspaceFocus())
	}
}

func TestWorkspaceComposerAndTemporaryReturnFollowEntryWhenEarlierStopChanges(t *testing.T) {
	for _, temporary := range []bool{false, true} {
		name := "composer"
		if temporary {
			name = "temporary panel"
		}
		t.Run(name, func(t *testing.T) {
			m := newWorkspaceJourneyShell(&fakeHandler{}, &workspaceJourneyPanel{name: "Explorer"}, &workspaceJourneyPanel{name: "Inspect"})
			m.AppendBlockWithID("a", &variableFocusBlock{focusable: true})
			m.AppendBlockWithID("b", &variableFocusBlock{focusable: true})
			m.AppendBlockWithID("c", &variableFocusBlock{focusable: true})
			if !m.FocusEntry("b") {
				t.Fatal("could not focus entry b")
			}
			var restore func()
			if temporary {
				restore = m.ShowTemporaryCenterPanel(&workspaceTextPanel{text: "Temporary API"})
			} else {
				routingKey(m, "f4")
			}
			m.ReplaceBlock("a", &variableFocusBlock{focusable: false})
			if temporary {
				restore()
			} else {
				routingKey(m, "esc")
			}
			if got := m.FocusedEntryID(); got != "b" {
				t.Fatalf("return focused entry %q after an earlier stop disappeared, want b", got)
			}
		})
	}
}

func TestWorkspaceRemovingFocusedExplorerClearsItsFocus(t *testing.T) {
	m := newWorkspaceJourneyShell(&fakeHandler{}, &workspaceJourneyPanel{name: "Explorer"}, &workspaceJourneyPanel{name: "Inspect"})
	m.FocusLeftPanel()
	if !m.leftFocused {
		t.Fatal("setup did not focus Explorer")
	}
	m.SetLeftPanel(nil)
	if m.leftFocused || m.WorkspaceFocus() == "explorer" {
		t.Fatalf("removing focused Explorer left ghost focus: leftFocused=%v zone=%s", m.leftFocused, m.WorkspaceFocus())
	}
}

func TestWorkspaceRemovingFocusedExplorerFallsBackToVisibleCenter(t *testing.T) {
	left := &workspaceJourneyPanel{name: "Explorer"}
	right := &workspaceJourneyPanel{name: "Inspect"}
	center := &workspaceJourneyPanel{name: "Explore"}
	m := newWorkspaceJourneyShell(&fakeHandler{}, left, right)
	m.SetCenterPanel(center, true)
	m.FocusLeftPanel()
	m.SetLeftPanel(nil)
	if m.leftFocused || !m.centerFocused || m.centerPanel != center || m.WorkspaceFocus() != "center" {
		t.Fatalf("removing focused Explorer did not transfer focus to the visible center: left=%v center=%v focus=%s", m.leftFocused, m.centerFocused, m.WorkspaceFocus())
	}
}

func TestWorkspaceTemporaryCenterReturnPreservesBothPanelVisibilityAndFocus(t *testing.T) {
	for _, inspectorVisible := range []bool{true, false} {
		name := "both panels visible"
		if !inspectorVisible {
			name = "hidden inspector remains hidden"
		}
		t.Run(name, func(t *testing.T) {
			left := &workspaceJourneyPanel{name: "Explorer"}
			right := &workspaceJourneyPanel{name: "Inspect"}
			center := &workspaceJourneyPanel{name: "Explore"}
			m := newWorkspaceJourneyShell(&fakeHandler{}, left, right)
			m.SetCenterPanel(center, true)
			m.sidePanelVisible = inspectorVisible
			m.leftVisible = true
			m.centerFocused = true
			m.syncFocus()
			restore := m.ShowTemporaryCenterPanel(&workspaceTextPanel{text: "Temporary API"})
			routingKey(m, "f4")
			routingKey(m, "esc")
			restore()
			if m.leftPanel != left || !m.leftVisible || m.sidePanel != right || m.sidePanelVisible != inspectorVisible || m.centerPanel != center {
				t.Fatalf("temporary return lost panel state: left=%v right=%v center=%v", m.leftVisible, m.sidePanelVisible, m.centerPanel == center)
			}
			if !m.centerFocused || m.leftFocused || m.WorkspaceFocus() != "center" {
				t.Fatalf("temporary return restored focus %s (left=%v center=%v), want center", m.WorkspaceFocus(), m.leftFocused, m.centerFocused)
			}
		})
	}
}

func TestWorkspaceFullWidthTemporaryAPIRestoresVisibleAndHiddenPanelsAfterEsc(t *testing.T) {
	for _, fullWidth := range []bool{false, true} {
		name := "shared columns"
		if fullWidth {
			name = "full width"
		}
		t.Run(name, func(t *testing.T) {
			left := &workspaceJourneyPanel{name: "Explorer"}
			right := &workspaceJourneyPanel{name: "Inspect"}
			center := &workspaceJourneyPanel{name: "Explore"}
			m := newWorkspaceJourneyShell(&fakeHandler{}, left, right)
			m.SetCenterPanel(center, true)
			m.sidePanelVisible = false // return must preserve an intentionally hidden panel
			m.leftVisible = true
			m.centerFocused = true
			m.syncFocus()
			restore := m.ShowTemporaryCenterPanel(&workspaceTextPanel{text: "API overview"}, fullWidth)
			if fullWidth {
				if m.leftVisible || m.sidePanelVisible {
					t.Fatal("full-width API left a side column visible")
				}
			} else if !m.leftVisible || m.sidePanelVisible {
				t.Fatal("column API changed the existing left/hidden-right layout")
			}
			routingKey(m, "f4")
			routingKey(m, "esc")
			if !m.centerFocused || m.centerPanel == nil || m.centerPanel.Title() != "API overview" {
				t.Fatal("Esc did not return from the composer to the active API panel")
			}
			restore()
			if m.centerPanel != center || !m.leftVisible || m.sidePanelVisible || !m.centerFocused || m.leftFocused {
				t.Fatalf("API return lost prior panel layout/focus: left=%v right=%v center=%v focus=%s", m.leftVisible, m.sidePanelVisible, m.centerPanel == center, m.WorkspaceFocus())
			}
		})
	}
}

func TestWorkspaceCursorReturnUsesRawIndexForIDlessEntry(t *testing.T) {
	m := newWorkspaceJourneyShell(&fakeHandler{}, &workspaceJourneyPanel{name: "Explorer"}, &workspaceJourneyPanel{name: "Inspect"})
	m.AppendBlockWithID("earlier", &variableFocusBlock{focusable: true})
	want := &variableFocusBlock{focusable: true}
	m.AppendBlock(want) // legacy non-streaming entries may not have an ID
	m.focusRing.FocusStop(1)
	m.syncFocus()
	m.FocusComposer()
	m.ReplaceBlock("earlier", &variableFocusBlock{focusable: false})
	routingKey(m, "esc")
	if got := m.transcript.FocusedBlock(); got != want {
		t.Fatalf("ID-less cursor restored block %p, want the original block %p", got, want)
	}
	if m.focusRing.Stop() != 0 {
		t.Fatalf("ID-less cursor restored stop %d after earlier stop disappeared, want 0", m.focusRing.Stop())
	}
}

func TestWorkspaceCursorReturnFallsBackWhenNamedEntryStopsBeingFocusable(t *testing.T) {
	m := newWorkspaceJourneyShell(&fakeHandler{}, &workspaceJourneyPanel{name: "Explorer"}, &workspaceJourneyPanel{name: "Inspect"})
	m.AppendBlockWithID("a", &variableFocusBlock{focusable: true})
	m.AppendBlockWithID("b", &variableFocusBlock{focusable: true})
	m.AppendBlockWithID("c", &variableFocusBlock{focusable: true})
	if !m.FocusEntry("b") {
		t.Fatal("could not focus entry b")
	}
	restore := m.ShowTemporaryCenterPanel(&workspaceTextPanel{text: "Temporary API"})
	m.ReplaceBlock("b", &variableFocusBlock{focusable: false})
	restore()
	if m.WorkspaceFocus() != focus.ZoneInput.String() || m.FocusedEntryID() != "" {
		t.Fatalf("removed named stop restored stale focus to %q in zone %s", m.FocusedEntryID(), m.WorkspaceFocus())
	}
}

func TestWorkspaceStaleStopReturnUsesLastSelectedResult(t *testing.T) {
	m := newWorkspaceJourneyShell(&fakeHandler{}, &workspaceJourneyPanel{name: "Explorer"}, &workspaceJourneyPanel{name: "Inspect"})
	m.AppendBlockWithID("selected", &fakeBlock{})
	if !m.FocusEntry("selected") {
		t.Fatal("could not focus selected result")
	}
	// A selection can outlive a stale cursor stop while async transcript
	// updates settle; restoring it must use the stable selected result.
	m.focusRing.FocusStop(99)
	m.selectedStop = 0 // retained selection is independent of the live focus cursor
	m.FocusComposer()
	routingKey(m, "esc")
	if got := m.FocusedEntryID(); got != "selected" || m.WorkspaceFocus() != "transcript" {
		t.Fatalf("stale stop restore focused %q in %s, want selected transcript entry", got, m.WorkspaceFocus())
	}
}

func TestWorkspaceViewPadsShortContentAndMissingHelpFactoryIsSafe(t *testing.T) {
	padded := fitWorkspace("one", 8, 3)
	if got := strings.Count(padded, "\n") + 1; got != 3 {
		t.Fatalf("short workspace view has %d lines, want 3", got)
	}
	m := New(&fakeHandler{}, WithWorkspace(&workspaceJourneyPanel{name: "Explorer"}), WithWorkspaceHelp(func() Overlay { return nil }))
	if cmd := m.toggleWorkspaceHelp(); cmd != nil || len(m.overlays) != 0 {
		t.Fatalf("missing help factory created a command/overlay: %v/%d", cmd, len(m.overlays))
	}
}

func TestWorkspaceKeyboardJourneysTogglePanelsAndReturnThroughTranscript(t *testing.T) {
	left := &workspaceJourneyPanel{name: "Explorer"}
	right := &workspaceJourneyPanel{name: "Inspect"}
	m := newWorkspaceJourneyShell(&fakeHandler{}, left, right)
	m.AppendBlockWithID("a", &variableFocusBlock{focusable: true})
	m.AppendBlockWithID("b", &variableFocusBlock{focusable: true})
	if !m.FocusEntry("a") {
		t.Fatal("could not focus initial result")
	}
	m.FocusLeftPanel()
	routingKey(m, "f6")
	if m.leftVisible || m.leftFocused || m.WorkspaceFocus() != focus.ZoneInput.String() {
		t.Fatal("F6 did not hide focused Explorer and transfer focus to the composer")
	}
	routingKey(m, "f6")
	if !m.leftVisible {
		t.Fatal("F6 did not reveal Explorer again")
	}
	m.FocusRightPanel()
	routingKey(m, "f7")
	if m.sidePanelVisible || m.WorkspaceFocus() != focus.ZoneInput.String() {
		t.Fatal("F7 did not hide focused Inspector and transfer focus to the composer")
	}
	routingKey(m, "f7")
	if !m.sidePanelVisible {
		t.Fatal("F7 did not reveal Inspector again")
	}
	m.SetCenterPanel(nil, false)
	m.FocusRightPanel()
	routingKey(m, "shift+left")
	if m.WorkspaceFocus() != "transcript" || m.FocusedEntryID() != "a" {
		t.Fatalf("Shift+Left without a center panel did not return to selected transcript entry: focus=%s entry=%q", m.WorkspaceFocus(), m.FocusedEntryID())
	}
	if !m.ExpandSelected() {
		t.Fatal("could not expand selected transcript result")
	}
	routingKey(m, "shift+right")
	if m.Expanded() || m.WorkspaceFocus() != focus.ZoneSidebar.String() || !m.sidePanelVisible {
		t.Fatal("Shift+Right from expanded result did not restore and focus Inspector")
	}
	m.FocusLeftPanel()
	routingKey(m, "shift+down")
	if m.centerPanel != nil || m.FocusedEntryID() != "b" || m.WorkspaceFocus() != "transcript" {
		t.Fatalf("Shift+Down from Explorer did not hand focus to next result: panel=%v focus=%s entry=%q", m.centerPanel, m.WorkspaceFocus(), m.FocusedEntryID())
	}
	m.FocusLeftPanel()
	routingKey(m, "shift+right")
	if m.WorkspaceFocus() != "transcript" || m.FocusedEntryID() != "b" {
		t.Fatalf("Shift+Right without a center panel did not return to selected result: focus=%s entry=%q", m.WorkspaceFocus(), m.FocusedEntryID())
	}
	if _, handled := m.workspaceKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); handled {
		t.Fatal("workspace swallowed Ctrl+C instead of allowing the product quit handler")
	}
}

func TestWorkspacePanelKeyboardOwnershipAndEmptyFocusFallbacks(t *testing.T) {
	left := &workspaceJourneyPanel{name: "Explorer"}
	right := &workspaceJourneyPanel{name: "Inspect"}
	center := &workspaceJourneyPanel{name: "Explore"}
	m := newWorkspaceJourneyShell(&fakeHandler{}, left, right)
	if m.ExpandSelected() {
		t.Fatal("empty transcript expanded without a focused result")
	}
	m.FocusLeftPanel()
	routingKey(m, "x")
	if len(left.updates) != 1 || m.WorkspaceFocus() != "explorer" {
		t.Fatal("Explorer did not retain an ordinary key while focused")
	}
	routingKey(m, "esc")
	if m.WorkspaceFocus() != focus.ZoneInput.String() {
		t.Fatal("uncaptured Explorer Esc did not return focus to the composer")
	}
	m.SetCenterPanel(center, true)
	routingKey(m, "x")
	if len(center.updates) != 1 || m.WorkspaceFocus() != "center" {
		t.Fatal("center panel did not retain an ordinary key while focused")
	}
	routingKey(m, "esc")
	if m.WorkspaceFocus() != focus.ZoneInput.String() {
		t.Fatal("uncaptured center-panel Esc did not return focus to the composer")
	}
	// With no selected transcript stop, inward Shift navigation from the
	// Explorer still exits the panel cleanly and leaves the composer focused.
	m.FocusLeftPanel()
	routingKey(m, "shift+down")
	if m.centerPanel != nil || m.WorkspaceFocus() != focus.ZoneInput.String() {
		t.Fatalf("Shift+Down without a transcript selection left focus at %s", m.WorkspaceFocus())
	}
}

func TestWorkspaceSelectedRefSurvivesMovingFocusToComposer(t *testing.T) {
	m := newWorkspaceJourneyShell(&fakeHandler{}, &workspaceJourneyPanel{name: "Explorer"}, &workspaceJourneyPanel{name: "Inspect"})
	ref := session.EntityRef{Type: "row", Keys: map[string]string{"id": "42"}, Title: "Selected row"}
	block := &entityBlock{ref: ref}
	m.AppendBlockWithID("selected", block)
	if !m.FocusEntry("selected") {
		t.Fatal("could not focus selected entity")
	}
	m.FocusComposer()
	if got := m.SelectedRef(); got == nil || !got.Same(ref) {
		t.Fatalf("SelectedRef after focus moved to composer = %v, want %v", got, ref)
	}
}

func TestWorkspaceMouseIgnoresNonLeftAndUnclaimedChipClicksAndScrollsHistory(t *testing.T) {
	left := &workspaceJourneyPanel{name: "Explorer"}
	right := &workspaceJourneyPanel{name: "Inspect"}
	m := newWorkspaceJourneyShell(&fakeHandler{}, left, right)
	m.SetMouseEnabled(true)
	for i := 0; i < 45; i++ {
		m.AppendUser(strings.Repeat("history ", 8))
	}
	m.SetChips([]Chip{{ID: "one", Label: "invoice.csv"}})
	l, c, _ := m.workspaceWidths()
	top := m.topBarHeight()
	chipY := top + m.historyHeight() + m.menuHeight()
	cells, _ := m.chipRow(c)
	if len(cells) != 1 {
		t.Fatalf("setup produced %d chip hit areas, want one", len(cells))
	}
	chipX := l + cells[0].x
	m.Update(tea.MouseClickMsg{X: chipX, Y: chipY, Button: tea.MouseRight})
	if got := len(m.Chips()); got != 1 {
		t.Fatalf("right-click removed a chip; remaining=%d", got)
	}
	m.SetBusy(true)
	m.Update(tea.MouseClickMsg{X: chipX, Y: chipY, Button: tea.MouseLeft})
	m.SetBusy(false)
	if got := len(m.Chips()); got != 1 {
		t.Fatalf("busy chip click removed an attachment; remaining=%d", got)
	}
	// A click elsewhere in the attachment row must continue through ordinary
	// center routing without accidentally activating the chip.
	m.SetCenterPanel(&workspaceJourneyPanel{name: "API"}, false)
	m.Update(tea.MouseClickMsg{X: l + c - 1, Y: chipY, Button: tea.MouseLeft})
	if got := len(m.Chips()); got != 1 {
		t.Fatalf("unclaimed click removed a chip; remaining=%d", got)
	}
	m.SetCenterPanel(nil, false)
	before := m.transcript.ScrollOffset()
	m.Update(tea.MouseWheelMsg{X: l + c/2, Button: tea.MouseWheelUp})
	afterUp := m.transcript.ScrollOffset()
	if afterUp >= before {
		t.Fatalf("unclaimed workspace wheel did not scroll history up: %d -> %d", before, afterUp)
	}
	m.Update(tea.MouseWheelMsg{X: l + c/2, Button: tea.MouseWheelDown})
	if got := m.transcript.ScrollOffset(); got <= afterUp {
		t.Fatalf("workspace wheel down did not move history toward the bottom: after up=%d current=%d", afterUp, got)
	}
}

func TestWorkspaceAsyncOverlayCloseKeepsOnlyCurrentOwnerFocused(t *testing.T) {
	m, grid := routingWorkspace(t)
	underlay := &asyncFakeOverlay{}
	newOwner := &workspaceDialog{title: "New owner"}
	h := &workspaceJourneyAsyncHandler{model: m, target: underlay}
	m.handler = h
	m.PushOverlay(underlay)
	m.PushOverlay(newOwner)
	if grid.Focused() || m.input.Focused() {
		t.Fatal("opening nested overlays left the workspace underlay focused")
	}
	m.Update(asyncResultMsg{ok: true})
	if len(m.overlays) != 1 || m.overlays[0] != newOwner || grid.Focused() || m.input.Focused() {
		t.Fatal("late async completion closed the new owner or focused the underlay")
	}
	m.CloseOverlay(newOwner)
	if len(m.overlays) != 0 || !grid.Focused() || m.input.Focused() {
		t.Fatal("closing the current overlay did not restore the selected result as sole focus owner")
	}
}

func TestWorkspaceInitBusyMenuChipsMouseAndRenderStayCoherent(t *testing.T) {
	h := &workspaceJourneyInitHandler{}
	left := &workspaceJourneyPanel{name: "Explorer"}
	right := &workspaceJourneyPanel{name: "Inspect"}
	m := New(h,
		WithWorkspace(left), WithSidePanel(right), WithMouse(MouseCellMotion),
		WithCommands([]Command{{Name: "/api", Help: "Open API"}}),
		WithTopBar(func(int) string { return "DataTug" }),
	)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if cmd := m.Init(); cmd == nil || h.initCalls != 1 {
		t.Fatalf("startup Init hook = calls %d cmd %v", h.initCalls, cmd)
	}
	m.SetChips([]Chip{{ID: "file", Label: "query.sql"}})
	for _, r := range "/api" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if m.commandMenuView() == "" {
		t.Fatal("typed command did not open the menu")
	}
	m.SetBusy(true)
	m.SetMouseEnabled(false)
	view := m.View()
	if view.MouseMode != tea.MouseModeNone {
		t.Fatalf("disabled mouse mode = %v, want none", view.MouseMode)
	}
	if lines := strings.Split(view.Content, "\n"); len(lines) != m.height {
		t.Fatalf("busy/menu/chip workspace rendered %d lines, want %d", len(lines), m.height)
	}
	if !strings.Contains(view.Content, "query.sql") || !strings.Contains(view.Content, "/api") {
		t.Fatal("render lost its attachment chip or open command menu")
	}
	m.SetMouseEnabled(true)
	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("reenabled workspace mouse mode = %v", got)
	}
	m.SetBusy(false)
	if lines := strings.Split(m.View().Content, "\n"); len(lines) != m.height {
		t.Fatalf("idle workspace rendered %d lines, want %d", len(lines), m.height)
	}
}

var _ transcript.WheelConsumer = (*workspaceJourneyWheelBlock)(nil)
