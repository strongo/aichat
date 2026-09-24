package chatshell

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/strongo/aichat/ai/session"
	"github.com/strongo/aichat/tui/focus"
)

// chipHandler is a Handler + ChipObserver fake for exercising the
// OnChipsChange notification path independently of fakeHandler's many other
// optional capabilities.
type chipHandler struct {
	fakeHandler
	changes [][]Chip
}

func (h *chipHandler) OnChipsChange(chips []Chip) tea.Cmd {
	h.changes = append(h.changes, append([]Chip(nil), chips...))
	return nil
}

// bareHandler implements only Handler -- no optional capabilities at all --
// so chip removal against it exercises notifyChipsChange's "no ChipObserver"
// branch.
type bareHandler struct{}

func (bareHandler) Submit(string) tea.Cmd { return nil }

func threeChips() []Chip {
	return []Chip{
		{ID: "a", Label: "Customer"},
		{ID: "b", Label: "Invoice", Ref: &session.EntityRef{Type: "table", Keys: map[string]string{"id": "invoice"}}},
		{ID: "c", Label: "Order"},
	}
}

func TestWithChipsAndChipsRoundTrip(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithChips(threeChips()))
	got := m.Chips()
	if len(got) != 3 || got[0].Label != "Customer" || got[1].Ref == nil || got[1].Ref.Keys["id"] != "invoice" {
		t.Fatalf("Chips() = %+v", got)
	}
	// Chips() is a defensive copy: mutating it must not affect the Model.
	got[0].Label = "mutated"
	if m.Chips()[0].Label != "Customer" {
		t.Fatalf("Chips() leaked internal slice")
	}
}

func TestSetChipsReplacesListAndResetsOutOfRangeFocus(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.chipFocus = 2
	m.SetChips(threeChips()[:1]) // shrinks to 1 chip; focus index 2 is now out of range
	if m.chipFocus != -1 {
		t.Fatalf("chipFocus = %d, want -1 after shrink", m.chipFocus)
	}
	if !m.input.Focused() {
		t.Fatal("input should regain focus once chip focus is cleared")
	}

	// A focus index that still fits is preserved.
	m.SetChips(threeChips())
	m.chipFocus = 1
	m.SetChips(threeChips())
	if m.chipFocus != 1 {
		t.Fatalf("chipFocus = %d, want preserved 1", m.chipFocus)
	}
}

func TestSetChipsDoesNotClearPendingUndo(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.removeChip(0) // snapshots chipUndo
	if m.chipUndo == nil {
		t.Fatal("expected a pending undo snapshot")
	}
	m.SetChips(threeChips()) // product-driven change, unrelated to the removal
	if m.chipUndo == nil {
		t.Fatal("SetChips must not clear a pending Shift+Esc undo")
	}
}

func TestCycleChipFocusForwardAndBackward(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	if m.chipFocus != -1 {
		t.Fatalf("initial chipFocus = %d, want -1", m.chipFocus)
	}

	// Tab walks 0, 1, 2, then back to -1 (input).
	for i, want := range []int{0, 1, 2, -1} {
		m.cycleChipFocus(true)
		if m.chipFocus != want {
			t.Fatalf("tab step %d: chipFocus = %d, want %d", i, m.chipFocus, want)
		}
	}
	if !m.input.Focused() {
		t.Fatal("input should be focused once the cycle returns to -1")
	}

	// Shift+Tab is the same cycle in reverse: -1 -> 2, 1, 0, -1.
	for i, want := range []int{2, 1, 0, -1} {
		m.cycleChipFocus(false)
		if m.chipFocus != want {
			t.Fatalf("shift+tab step %d: chipFocus = %d, want %d", i, m.chipFocus, want)
		}
	}
}

func TestCycleChipFocusNoopWithoutChips(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.cycleChipFocus(true)
	if m.chipFocus != -1 {
		t.Fatalf("chipFocus = %d, want unchanged -1", m.chipFocus)
	}
}

func TestTabKeyEntersAndLeavesChipRow(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.chipFocus != 0 {
		t.Fatalf("chipFocus = %d, want 0 after Tab", m.chipFocus)
	}
	if m.input.Focused() {
		t.Fatal("input must blur once a chip is focused")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m.chipFocus != -1 {
		t.Fatalf("chipFocus = %d, want -1 after Shift+Tab back to input", m.chipFocus)
	}
}

func TestLeftRightMoveBetweenFocusedChips(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.chipFocus = 0
	m.Update(tea.KeyPressMsg{Code: tea.KeyLeft}) // already at first chip: clamped
	if m.chipFocus != 0 {
		t.Fatalf("chipFocus = %d, want clamped at 0", m.chipFocus)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if m.chipFocus != 1 {
		t.Fatalf("chipFocus = %d, want 1", m.chipFocus)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // already at last chip: clamped
	if m.chipFocus != 2 {
		t.Fatalf("chipFocus = %d, want clamped at 2", m.chipFocus)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.chipFocus != 1 {
		t.Fatalf("chipFocus = %d, want 1", m.chipFocus)
	}
}

func TestLeftRightIgnoredWithoutChipFocus(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.input.SetValue("ab")
	m.input.CursorEnd()
	// chipFocus is -1 (composer typing): Left/Right must reach the textarea,
	// not be intercepted as chip navigation.
	m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.chipFocus != -1 {
		t.Fatalf("chipFocus = %d, want untouched -1", m.chipFocus)
	}
}

func TestBackspaceAndDeleteRemoveFocusedChip(t *testing.T) {
	h := &chipHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.chipFocus = 1
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got := m.Chips(); len(got) != 2 || got[0].Label != "Customer" || got[1].Label != "Order" {
		t.Fatalf("Chips() = %+v", got)
	}
	if len(h.changes) != 1 || len(h.changes[0]) != 2 {
		t.Fatalf("OnChipsChange calls = %+v", h.changes)
	}

	m.chipFocus = 0
	m.Update(tea.KeyPressMsg{Code: tea.KeyDelete})
	if got := m.Chips(); len(got) != 1 || got[0].Label != "Order" {
		t.Fatalf("Chips() = %+v", got)
	}
}

func TestRemovingLastChipReturnsFocusToInput(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips([]Chip{{ID: "a", Label: "Only"}})
	m.chipFocus = 0
	m.input.Blur()
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if len(m.Chips()) != 0 {
		t.Fatalf("Chips() = %+v, want empty", m.Chips())
	}
	if m.chipFocus != -1 {
		t.Fatalf("chipFocus = %d, want -1", m.chipFocus)
	}
	if !m.input.Focused() {
		t.Fatal("input should regain focus once the last chip is removed")
	}
}

func TestBackspaceWithoutChipFocusReachesTextarea(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.input.SetValue("ab")
	m.input.CursorEnd()
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.input.Value() != "a" {
		t.Fatalf("input = %q, want backspace to edit text", m.input.Value())
	}
	if len(m.Chips()) != 3 {
		t.Fatalf("Chips() = %+v, want unchanged", m.Chips())
	}
}

func TestRemoveChipOutOfRangeIsNoop(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	if cmd := m.removeChip(-1); cmd != nil {
		t.Fatal("removeChip(-1) should return nil")
	}
	if cmd := m.removeChip(99); cmd != nil {
		t.Fatal("removeChip(99) should return nil")
	}
	if len(m.Chips()) != 3 {
		t.Fatalf("Chips() = %+v, want unchanged", m.Chips())
	}
}

func TestRemoveChipWithoutChipObserverIsSafe(t *testing.T) {
	m := newTestShell(bareHandler{})
	m.SetChips(threeChips())
	m.chipFocus = 0
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if len(m.Chips()) != 2 {
		t.Fatalf("Chips() = %+v", m.Chips())
	}
}

func TestShiftEscRestoresChipsRemovedSinceLastSnapshot(t *testing.T) {
	h := &chipHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.chipFocus = 0
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace}) // removes Customer, snapshots [Customer,Invoice,Order]
	m.chipFocus = 0
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace}) // removes Invoice; snapshot untouched (already set)
	if len(m.Chips()) != 1 {
		t.Fatalf("Chips() = %+v, want 1 remaining", m.Chips())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape, Mod: tea.ModShift})
	got := m.Chips()
	if len(got) != 3 || got[0].Label != "Customer" || got[1].Label != "Invoice" || got[2].Label != "Order" {
		t.Fatalf("Chips() after restore = %+v", got)
	}
	if m.chipUndo != nil {
		t.Fatal("chipUndo should be cleared after a successful restore")
	}
	last := h.changes[len(h.changes)-1]
	if len(last) != 3 {
		t.Fatalf("final OnChipsChange = %+v, want the restored 3", last)
	}
}

func TestShiftEscNoopWithoutPendingUndo(t *testing.T) {
	h := &chipHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	before := len(h.changes)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape, Mod: tea.ModShift})
	if len(m.Chips()) != 3 {
		t.Fatalf("Chips() = %+v, want unchanged", m.Chips())
	}
	if len(h.changes) != before {
		t.Fatal("OnChipsChange must not fire when there's nothing to restore")
	}
}

func TestShiftEscIgnoredWhileBusy(t *testing.T) {
	h := &chipHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.chipFocus = 0
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	m.busy = true
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape, Mod: tea.ModShift})
	if len(m.Chips()) != 2 {
		t.Fatalf("Chips() = %+v, restore must not run while busy", m.Chips())
	}
}

func TestSubmitClearsPendingChipUndo(t *testing.T) {
	h := &chipHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.chipFocus = 0
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.chipUndo == nil {
		t.Fatal("expected a pending undo snapshot before submit")
	}
	m.input.SetValue("go")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.chipUndo != nil {
		t.Fatal("submitting the message should clear the pending chip undo")
	}
	// Shift+Esc after submit is now a no-op.
	before := len(m.Chips())
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape, Mod: tea.ModShift})
	if len(m.Chips()) != before {
		t.Fatalf("Chips() changed after post-submit Shift+Esc")
	}
}

func TestChipRowsEmptyWithoutChips(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	if rows := m.chipRows(80); rows != nil {
		t.Fatalf("chipRows = %+v, want nil", rows)
	}
	if v := m.chipsView(80); v != "" {
		t.Fatalf("chipsView = %q, want empty", v)
	}
	if h := m.chipsHeight(80); h != 0 {
		t.Fatalf("chipsHeight = %d, want 0", h)
	}
}

func TestChipRowsWrapAcrossMultipleRowsAtNarrowWidth(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	rows := m.chipRows(12) // narrow enough that each chip needs its own row
	if len(rows) < 2 {
		t.Fatalf("chipRows(12) = %+v, want at least 2 rows", rows)
	}
	total := 0
	for _, row := range rows {
		total += len(row)
	}
	if total != 3 {
		t.Fatalf("total chips across rows = %d, want 3", total)
	}
	if h := m.chipsHeight(12); h != len(rows) {
		t.Fatalf("chipsHeight(12) = %d, want %d", h, len(rows))
	}
}

func TestChipRowsTruncatesOverlongLabel(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips([]Chip{{ID: "a", Label: strings.Repeat("x", 200)}})
	rows := m.chipRows(20)
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("chipRows = %+v", rows)
	}
	if !strings.Contains(rows[0][0].text, chipCloseGlyph) {
		t.Fatalf("truncated chip lost its close glyph: %q", rows[0][0].text)
	}
}

func TestChipsViewHighlightsFocusedChip(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.chipFocus = 1
	view := m.chipsView(80)
	if !strings.Contains(view, "Customer") || !strings.Contains(view, "Invoice") || !strings.Contains(view, "Order") {
		t.Fatalf("chipsView = %q, missing a label", view)
	}
}

func TestHistoryHeightShrinksAsChipRowsAppearAndGrowsBackAsTheyClear(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	before := m.historyHeight()
	m.SetChips(threeChips())
	during := m.historyHeight()
	if during >= before {
		t.Fatalf("historyHeight() = %d, want less than %d once chips render", during, before)
	}
	m.SetChips(nil)
	after := m.historyHeight()
	if after != before {
		t.Fatalf("historyHeight() = %d, want it to recover to %d once chips clear", after, before)
	}
}

func TestViewRendersChipRowAboveComposer(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	view := m.View()
	if !strings.Contains(view.Content, "Customer") {
		t.Fatalf("rendered view missing chip label:\n%s", view.Content)
	}
}

// --- mouse click removal -------------------------------------------------

func chipMouseSetup(t *testing.T) (*chipHandler, *Model) {
	t.Helper()
	h := &chipHandler{}
	m := New(h, WithMouse(MouseCellMotion))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SetMouseEnabled(true)
	m.SetChips(threeChips())
	return h, m
}

// chipCellFor finds the rendered cell for chip index in row 0 (all three
// short labels fit on one row at chatWidth() for a 120-wide shell).
func chipCellFor(t *testing.T, m *Model, index int) chipCell {
	t.Helper()
	rows := m.chipRows(m.chatWidth())
	for _, row := range rows {
		for _, cell := range row {
			if cell.index == index {
				return cell
			}
		}
	}
	t.Fatalf("no rendered cell for chip %d", index)
	return chipCell{}
}

func TestMouseClickOnCloseGlyphRemovesChip(t *testing.T) {
	h, m := chipMouseSetup(t)
	cell := chipCellFor(t, m, 1)
	y := m.chipsTopY()
	m.Update(tea.MouseClickMsg{X: cell.x, Y: y, Button: tea.MouseLeft})
	got := m.Chips()
	if len(got) != 2 || got[0].Label != "Customer" || got[1].Label != "Order" {
		t.Fatalf("Chips() = %+v", got)
	}
	if len(h.changes) != 1 {
		t.Fatalf("OnChipsChange calls = %d, want 1", len(h.changes))
	}
}

func TestMouseClickMissingCloseGlyphFallsThrough(t *testing.T) {
	h, m := chipMouseSetup(t)
	cell := chipCellFor(t, m, 0)
	y := m.chipsTopY()
	// One column left of the close glyph: inside the pill, not on ×.
	m.Update(tea.MouseClickMsg{X: cell.x - 1, Y: y, Button: tea.MouseLeft})
	if len(m.Chips()) != 3 {
		t.Fatalf("Chips() = %+v, want unchanged", m.Chips())
	}
	if len(h.msgsSeen) == 0 {
		t.Fatal("a non-chip click should still reach MsgHandler.OnMsg via dispatchUnhandled")
	}
}

func TestMouseClickOutsideChipRowFallsThrough(t *testing.T) {
	h, m := chipMouseSetup(t)
	m.Update(tea.MouseClickMsg{X: 0, Y: m.chipsTopY() + 50, Button: tea.MouseLeft})
	if len(m.Chips()) != 3 {
		t.Fatalf("Chips() = %+v, want unchanged", m.Chips())
	}
	if len(h.msgsSeen) == 0 {
		t.Fatal("expected the click to reach dispatchUnhandled")
	}

	before := len(h.msgsSeen)
	m.Update(tea.MouseClickMsg{X: 0, Y: m.chipsTopY() - 1, Button: tea.MouseLeft})
	if len(h.msgsSeen) != before+1 {
		t.Fatal("a click above the chip row should also fall through")
	}
}

func TestMouseClickIgnoredWhenMouseDisabled(t *testing.T) {
	h := &chipHandler{}
	m := newTestShell(h) // mouse off by default
	m.SetChips(threeChips())
	cell := chipCellFor(t, m, 0)
	m.Update(tea.MouseClickMsg{X: cell.x, Y: m.chipsTopY(), Button: tea.MouseLeft})
	if len(m.Chips()) != 3 {
		t.Fatalf("Chips() = %+v, want unchanged with mouse reporting off", m.Chips())
	}
}

func TestMouseClickIgnoredWhileBusy(t *testing.T) {
	_, m := chipMouseSetup(t)
	cell := chipCellFor(t, m, 0)
	m.busy = true
	m.Update(tea.MouseClickMsg{X: cell.x, Y: m.chipsTopY(), Button: tea.MouseLeft})
	if len(m.Chips()) != 3 {
		t.Fatalf("Chips() = %+v, want unchanged while busy", m.Chips())
	}
}

func TestMouseClickIgnoredWithoutChips(t *testing.T) {
	h := &chipHandler{}
	m := New(h, WithMouse(MouseCellMotion))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SetMouseEnabled(true)
	m.Update(tea.MouseClickMsg{X: 5, Y: 5, Button: tea.MouseLeft})
	if len(h.changes) != 0 {
		t.Fatal("no chips means nothing to remove")
	}
}

func TestMouseClickIgnoredForNonLeftButton(t *testing.T) {
	_, m := chipMouseSetup(t)
	cell := chipCellFor(t, m, 0)
	m.Update(tea.MouseClickMsg{X: cell.x, Y: m.chipsTopY(), Button: tea.MouseRight})
	if len(m.Chips()) != 3 {
		t.Fatalf("Chips() = %+v, want unchanged for a right click", m.Chips())
	}
}

func TestChipsTopYAccountsForMultiLineTopBarAndMenu(t *testing.T) {
	h := &fakeHandler{}
	m := New(h,
		WithTopBar(func(int) string { return "line1\nline2" }),
		WithCommands([]Command{{Name: "/help", Help: "help"}}),
	)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SetChips(threeChips())

	withoutMenu := m.chipsTopY()
	wantWithoutMenu := 2 /* two top-bar lines */ + m.historyHeight()
	if withoutMenu != wantWithoutMenu {
		t.Fatalf("chipsTopY() = %d, want %d", withoutMenu, wantWithoutMenu)
	}

	m.input.SetValue("/help")
	withMenu := m.chipsTopY()
	if withMenu <= withoutMenu {
		t.Fatalf("chipsTopY() = %d, want greater than %d once the command menu shows", withMenu, withoutMenu)
	}
	if view := m.View(); !strings.Contains(view.Content, "help") {
		t.Fatalf("expected the open command menu in the rendered view:\n%s", view.Content)
	}
}

func TestSyncFocusClearsChipFocusOnZoneChange(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.chipFocus = 1
	m.input.Blur()

	// Esc from the chip row returns focus to the composer and drops chip
	// focus (default branch of syncFocus).
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.chipFocus != -1 || !m.input.Focused() {
		t.Fatalf("chipFocus=%d inputFocused=%v, want -1/true after Esc", m.chipFocus, m.input.Focused())
	}

	// Moving into the transcript also drops chip focus.
	m.AppendBlock(&fakeBlock{})
	m.chipFocus = 0
	m.input.Blur()
	if !m.focusRing.ShiftUp(m.transcript.Stops()) {
		t.Fatal("expected ShiftUp to move focus into the transcript")
	}
	m.syncFocus()
	if m.chipFocus != -1 {
		t.Fatalf("chipFocus = %d, want -1 once the transcript holds focus", m.chipFocus)
	}
	if m.focusRing.Zone() != focus.ZoneTranscript {
		t.Fatalf("Zone() = %v, want ZoneTranscript", m.focusRing.Zone())
	}
}
