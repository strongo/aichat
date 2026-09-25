package chatshell

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/strongo/aichat/ai/session"
	"github.com/strongo/aichat/tui/focus"
	"github.com/strongo/aichat/tui/theme"
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
// so a chip change against it exercises notifyChipsChange's "no
// ChipObserver" branch.
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
	m.removeChipAt(0) // snapshots composerUndo
	if m.composerUndo == nil {
		t.Fatal("expected a pending undo snapshot")
	}
	m.SetChips(threeChips()) // product-driven change, unrelated to the removal
	if m.composerUndo == nil {
		t.Fatal("SetChips must not clear a pending Shift+Esc/Ctrl+Y undo")
	}
}

// --- Tab/Left/Right focus -------------------------------------------------

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

// --- Backspace/Delete/Ctrl+D removal ---------------------------------------

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

func TestCtrlDRemovesLastChipRegardlessOfFocus(t *testing.T) {
	h := &chipHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	// No chip focused (input holds keyboard focus): Ctrl+D still works.
	m.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	got := m.Chips()
	if len(got) != 2 || got[0].Label != "Customer" || got[1].Label != "Invoice" {
		t.Fatalf("Chips() = %+v, want the last chip removed", got)
	}
	if len(h.changes) != 1 {
		t.Fatalf("OnChipsChange calls = %d, want 1", len(h.changes))
	}
	if m.composerUndo == nil {
		t.Fatal("Ctrl+D should snapshot like any other removal")
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

func TestRemoveChipAtOutOfRangeIsNoop(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	if cmd := m.removeChipAt(-1); cmd != nil {
		t.Fatal("removeChipAt(-1) should return nil")
	}
	if cmd := m.removeChipAt(99); cmd != nil {
		t.Fatal("removeChipAt(99) should return nil")
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

// --- exported RemoveChip/ClearChips ----------------------------------------

func TestRemoveChipByID(t *testing.T) {
	h := &chipHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	if _, ok := m.RemoveChip("b"); !ok {
		t.Fatal("RemoveChip(\"b\") should report found=true")
	}
	got := m.Chips()
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("Chips() = %+v", got)
	}
	if m.composerUndo == nil {
		t.Fatal("RemoveChip should snapshot like any other removal")
	}
	// An unknown ID is a documented no-op.
	if cmd, ok := m.RemoveChip("nope"); cmd != nil || ok {
		t.Fatal("RemoveChip(unknown) should return (nil, false)")
	}
	if len(m.Chips()) != 2 {
		t.Fatalf("Chips() = %+v, want unchanged for an unknown ID", m.Chips())
	}
}

func TestClearChipsDetachesAllAndSnapshots(t *testing.T) {
	h := &chipHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.ClearChips()
	if len(h.changes) != 1 || len(h.changes[0]) != 0 {
		t.Fatalf("OnChipsChange calls = %+v, want one call with an empty list", h.changes)
	}
	if len(m.Chips()) != 0 {
		t.Fatalf("Chips() = %+v, want empty", m.Chips())
	}
	if m.composerUndo == nil {
		t.Fatal("ClearChips should snapshot the pre-clear chips")
	}
	if !m.input.Focused() {
		t.Fatal("input should hold focus once every chip is cleared")
	}
}

func TestClearChipsNoopWithoutChips(t *testing.T) {
	h := &chipHandler{}
	m := newTestShell(h)
	if cmd := m.ClearChips(); cmd != nil {
		t.Fatal("ClearChips with no chips should return nil")
	}
	if len(h.changes) != 0 {
		t.Fatal("no notification expected for a no-op ClearChips")
	}
}

// --- Esc's two-step clear (b1) ---------------------------------------------

func TestEscFirstClearsTextSecondClearsChipsEvenWhileAChipIsFocused(t *testing.T) {
	h := &chipHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.input.SetValue("draft")
	m.chipFocus = 1 // a chip is focused; the first Esc must still clear TEXT

	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.input.Value() != "" {
		t.Fatalf("input = %q, want cleared by the first Esc", m.input.Value())
	}
	if len(m.Chips()) != 3 {
		t.Fatalf("Chips() = %+v, want untouched by the first Esc", m.Chips())
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if len(m.Chips()) != 0 {
		t.Fatalf("Chips() = %+v, want cleared by the second Esc", m.Chips())
	}
}

func TestEscWithNoTextGoesStraightToClearingChips(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if len(m.Chips()) != 0 {
		t.Fatalf("Chips() = %+v, want cleared by a single Esc when there's no text", m.Chips())
	}
}

func TestEscIsANoopAndFallsThroughWhenNothingToClear(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	// No text, no chips: Esc has nothing to clear, so it falls through to
	// the default focus-ring Esc (already Input -> Input, a harmless no-op).
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.focusRing.Zone() != focus.ZoneInput {
		t.Fatalf("Zone() = %v, want ZoneInput", m.focusRing.Zone())
	}
}

// --- Shift+Esc / Ctrl+Y restore (b1) ---------------------------------------

func TestShiftEscRestoresTextAndChipsClearedByEscTwoStep(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.input.SetValue("draft")

	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape}) // clears text, snapshots "draft"+3 chips
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape}) // clears chips; snapshot untouched (already set)
	if m.input.Value() != "" || len(m.Chips()) != 0 {
		t.Fatalf("input=%q chips=%+v, want both cleared", m.input.Value(), m.Chips())
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape, Mod: tea.ModShift})
	if m.input.Value() != "draft" || len(m.Chips()) != 3 {
		t.Fatalf("input=%q chips=%+v, want the draft and chips restored", m.input.Value(), m.Chips())
	}
}

func TestCtrlYRestoresSameAsShiftEsc(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.chipFocus = 0
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if len(m.Chips()) != 2 {
		t.Fatalf("Chips() = %+v", m.Chips())
	}
	m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	if len(m.Chips()) != 3 {
		t.Fatalf("Chips() = %+v, want restored via Ctrl+Y", m.Chips())
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
	if m.composerUndo != nil {
		t.Fatal("composerUndo should be cleared after a successful restore")
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

// --- m2: restore merges chips added since the snapshot ---------------------

func TestShiftEscRestoreKeepsChipsAddedSinceTheSnapshot(t *testing.T) {
	h := &chipHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.chipFocus = 1
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace}) // removes Invoice; snapshots [Customer,Invoice,Order]
	if got := m.Chips(); len(got) != 2 {
		t.Fatalf("Chips() = %+v", got)
	}
	// The product attaches something new (by ID, not in the snapshot) while
	// a restore is still pending.
	d := Chip{ID: "d", Label: "Shipment"}
	m.SetChips(append(m.Chips(), d))

	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape, Mod: tea.ModShift})
	got := m.Chips()
	if len(got) != 4 {
		t.Fatalf("Chips() = %+v, want the 3 restored plus the 1 added since", got)
	}
	byID := map[string]bool{}
	for _, c := range got {
		byID[c.ID] = true
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		if !byID[id] {
			t.Fatalf("Chips() = %+v, missing %q", got, id)
		}
	}
}

func TestMergeRestoredChipsDedupsIDlessChipsByLabel(t *testing.T) {
	snapshot := []Chip{{Label: "Customer"}, {ID: "b", Label: "Invoice"}}
	current := []Chip{{Label: "Customer"}} // same (ID-less) chip, still present -- not a duplicate
	merged := mergeRestoredChips(snapshot, current)
	if len(merged) != 2 {
		t.Fatalf("mergeRestoredChips = %+v, want no duplicate for the still-present ID-less chip", merged)
	}

	current2 := []Chip{{Label: "Different"}} // a genuinely different ID-less chip: kept
	merged2 := mergeRestoredChips(snapshot, current2)
	if len(merged2) != 3 {
		t.Fatalf("mergeRestoredChips = %+v, want the distinct ID-less chip kept", merged2)
	}
}

// --- m1: Enter with a chip focused ------------------------------------------

func TestEnterWithChipFocusedSubmitsAndResetsChipFocus(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.chipFocus = 1
	m.input.Blur()
	m.input.SetValue("go")

	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(h.submitted) != 1 || h.submitted[0] != "go" {
		t.Fatalf("submitted = %v", h.submitted)
	}
	if m.chipFocus != -1 {
		t.Fatalf("chipFocus = %d, want -1 after submit", m.chipFocus)
	}
	if !m.input.Focused() {
		t.Fatal("input should regain focus after submit")
	}
}

func TestSubmitClearsPendingComposerUndo(t *testing.T) {
	h := &chipHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.chipFocus = 0
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.composerUndo == nil {
		t.Fatal("expected a pending undo snapshot before submit")
	}
	m.input.SetValue("go")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.composerUndo != nil {
		t.Fatal("submitting the message should clear the pending undo")
	}
	// Shift+Esc after submit is now a no-op.
	before := len(m.Chips())
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape, Mod: tea.ModShift})
	if len(m.Chips()) != before {
		t.Fatalf("Chips() changed after post-submit Shift+Esc")
	}
}

func TestComposerTextEditDropsPendingUndo(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.chipFocus = 0
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.composerUndo == nil {
		t.Fatal("expected a pending undo snapshot")
	}
	m.Update(tea.KeyPressMsg{Text: "x"})
	if m.composerUndo != nil {
		t.Fatal("editing the composer text should drop the pending undo")
	}
}

// --- M1: ClearTranscript -----------------------------------------------------

func TestClearTranscriptDropsComposerUndoAndChipFocus(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetChips(threeChips())
	m.chipFocus = 1
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.composerUndo == nil {
		t.Fatal("expected a pending undo snapshot")
	}
	m.ClearTranscript()
	if m.composerUndo != nil {
		t.Fatal("ClearTranscript should drop the pending composer-undo snapshot")
	}
	if m.chipFocus != -1 {
		t.Fatalf("chipFocus = %d, want -1 after ClearTranscript", m.chipFocus)
	}
	// ClearTranscript does not itself touch the chip list -- that's the
	// product's call, via SetChips.
	if len(m.Chips()) != 2 {
		t.Fatalf("Chips() = %+v, want left as-is by ClearTranscript", m.Chips())
	}
}

// --- rendering / layout ------------------------------------------------------

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

// TestComposerShrinksAsWrappedAttachmentsAreRemoved (m2, r2 review) is an
// exact port of DataTug's own test of the same name
// (origin/main:pkg/chat/workspace_test.go): width 62, three long chip
// labels wrapping across two rows, the third (second-row) chip removed
// grows historyHeight by exactly 1, and clearing every remaining chip grows
// it by exactly 2 (from the original 2-row baseline).
func TestComposerShrinksAsWrappedAttachmentsAreRemoved(t *testing.T) {
	h := &fakeHandler{}
	m := New(h)
	m.Update(tea.WindowSizeMsg{Width: 62, Height: 30})
	chips := []Chip{
		{ID: "1", Label: "First customer table"},
		{ID: "2", Label: "Second customer table"},
		{ID: "3", Label: "Third customer table"},
	}
	m.SetChips(chips)
	width := m.chatWidth()
	if got := len(m.chipRows(width)); got != 2 {
		t.Fatalf("chip rows = %d, want 2", got)
	}
	initialHeight := m.historyHeight()
	if !strings.Contains(m.chipsView(width), "Third customer table") {
		t.Fatal("wrapped chip is not visible")
	}
	lastChip := m.chipRows(width)[1][0]
	if lastChip.index != 2 {
		t.Fatalf("second-row chip index = %d, want 2 (Third customer table)", lastChip.index)
	}

	m.SetChips(chips[:2])
	if got := len(m.chipRows(width)); got != 1 {
		t.Fatalf("chip rows after removing the third chip = %d, want 1", got)
	}
	if got := m.historyHeight(); got != initialHeight+1 {
		t.Fatalf("history height after removing the second-row chip = %d, want %d", got, initialHeight+1)
	}

	m.SetChips(nil)
	if got := len(m.chipRows(width)); got != 0 {
		t.Fatalf("chip rows after clearing chips = %d, want 0", got)
	}
	if got := m.historyHeight(); got != initialHeight+2 {
		t.Fatalf("history height after clearing chips = %d, want %d", got, initialHeight+2)
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

// --- cheap fix: historyHeight() accounts for the menu and a multi-line
// top bar so View()'s total rendered height matches m.height exactly ------

func renderedLineCount(s string) int { return len(strings.Split(s, "\n")) }

func TestViewHeightMatchesTerminalHeightBaseline(t *testing.T) {
	h := &fakeHandler{}
	m := New(h)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24}) // narrow: no sidebar split
	if got := renderedLineCount(m.View().Content); got != m.height {
		t.Fatalf("rendered %d lines, want exactly m.height = %d", got, m.height)
	}
}

func TestViewHeightMatchesTerminalHeightWithMultiLineTopBar(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithTopBar(func(int) string { return "line1\nline2\nline3" }))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if got := renderedLineCount(m.View().Content); got != m.height {
		t.Fatalf("rendered %d lines, want exactly m.height = %d", got, m.height)
	}
}

func TestViewHeightMatchesTerminalHeightWithOpenCommandMenu(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithCommands([]Command{{Name: "/a", Help: "a"}, {Name: "/ab", Help: "ab"}}))
	m.input.SetValue("/a")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if m.commandMenuView() == "" {
		t.Fatal("expected the command menu to be open")
	}
	if got := renderedLineCount(m.View().Content); got != m.height {
		t.Fatalf("rendered %d lines, want exactly m.height = %d", got, m.height)
	}
}

func TestViewHeightMatchesTerminalHeightWithStatus(t *testing.T) {
	h := &fakeHandler{}
	m := New(h)
	m.SetStatus("model: gpt\nusage: 12")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if got := renderedLineCount(m.View().Content); got != m.height {
		t.Fatalf("rendered %d lines, want exactly m.height = %d", got, m.height)
	}
	if m.statusSegmentHeight() != 2 {
		t.Fatalf("statusSegmentHeight() = %d, want 2", m.statusSegmentHeight())
	}
}

func TestViewHeightMatchesTerminalHeightWithChipsAndMenu(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithCommands([]Command{{Name: "/a", Help: "a"}}), WithChips(threeChips()))
	m.input.SetValue("/a")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if got := renderedLineCount(m.View().Content); got != m.height {
		t.Fatalf("rendered %d lines, want exactly m.height = %d", got, m.height)
	}
}

// --- B1 (r2 review): historyHeight must stay in sync with chrome that
// changes WITHOUT going through resize()'s old call sites (WindowSizeMsg,
// F6, Ctrl+Left/Right, a chip-list change) -- opening the slash-command
// menu by typing, SetBusy(true)/StartStream, and SetStatus all change how
// much chrome View() draws around the transcript, and previously left the
// transcript's ACTUAL viewport size stale until the next one of those
// events. View() now re-applies resize() on every call, and historyHeight
// reserves the busy spinner's own trailing line. -----------------------------

// TestWheelScrollSurvivesTheNextViewCall (r3 review, B1, closing #6's
// regression) confirms the fix at the integration level chatshell's own
// View() operates at: a wheel-scrolled-up transcript must stay exactly
// where the user left it across a SUBSEQUENT View() call (the per-frame
// resize() this REQ introduced, calling transcript.SetSize with the SAME
// dimensions every render, must not silently snap it back to the bottom).
func TestWheelScrollSurvivesTheNextViewCall(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithMouse(MouseCellMotion))
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 8})
	for i := 0; i < 100; i++ {
		m.AppendAssistant(fmt.Sprintf("line %d", i))
	}
	m.View() // an initial render, matching a real Bubble Tea loop

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	afterWheel := m.transcript.View()

	// The next render, with nothing else having changed, must not undo
	// the wheel scroll.
	m.View()
	afterNextView := m.transcript.View()
	if afterNextView != afterWheel {
		t.Fatalf("the transcript view changed across a same-size View() call after a wheel scroll:\nafterWheel=%q\nafterNextView=%q", afterWheel, afterNextView)
	}
}

func TestViewHeightMatchesAfterOpeningMenuByKeystroke(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithCommands([]Command{{Name: "/a", Help: "a"}}))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	renderedLineCount(m.View().Content) // render once at the pre-menu size
	for _, r := range "/a" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if m.commandMenuView() == "" {
		t.Fatal("expected the command menu to be open")
	}
	if got := renderedLineCount(m.View().Content); got != m.height {
		t.Fatalf("rendered %d lines, want exactly m.height = %d once the menu opens by keystroke", got, m.height)
	}
}

func TestViewHeightMatchesAfterSetBusy(t *testing.T) {
	h := &fakeHandler{}
	m := New(h)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	renderedLineCount(m.View().Content) // render once while idle
	m.SetBusy(true)
	if got := renderedLineCount(m.View().Content); got != m.height {
		t.Fatalf("rendered %d lines, want exactly m.height = %d while busy (spinner line reserved)", got, m.height)
	}
}

func TestViewHeightMatchesAfterSetStatusPostRender(t *testing.T) {
	h := &fakeHandler{}
	m := New(h)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	renderedLineCount(m.View().Content) // render once with no status
	m.SetStatus("model: gpt\nusage: 12")
	if got := renderedLineCount(m.View().Content); got != m.height {
		t.Fatalf("rendered %d lines, want exactly m.height = %d after a post-render SetStatus", got, m.height)
	}
}

func TestMouseClickFindsCloseGlyphWithMenuOpenAndNeverHitsAMenuRow(t *testing.T) {
	h := &chipHandler{}
	m := New(h, WithMouse(MouseCellMotion), WithCommands([]Command{{Name: "/a", Help: "a"}}))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.SetMouseEnabled(true)
	m.SetChips(threeChips())
	renderedLineCount(m.View().Content) // render once before the menu opens

	for _, r := range "/a" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if m.commandMenuView() == "" {
		t.Fatal("expected the command menu to be open")
	}

	// The rendered × is still findable and clickable with the menu open --
	// resize() having run for this render keeps chipsTopY (computed at
	// click time) in sync with what's actually drawn.
	x, y := findCloseGlyph(t, m)
	before := len(m.Chips())
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if len(m.Chips()) != before-1 {
		t.Fatalf("Chips() = %+v, want one chip removed by the click on ×", m.Chips())
	}

	// A click on one of the MENU's own rows (well above the chip row) must
	// never be misread as landing on a chip's × -- it falls through
	// untouched, same as any other non-chip click.
	menuRow := m.topBarHeight() + m.historyHeight() // the menu's first line
	beforeMenuClick := len(m.Chips())
	m.Update(tea.MouseClickMsg{X: 2, Y: menuRow, Button: tea.MouseLeft})
	if len(m.Chips()) != beforeMenuClick {
		t.Fatalf("Chips() = %+v, a click on a menu row must never remove a chip", m.Chips())
	}
}

// --- mouse click removal ----------------------------------------------------

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

// TestMouseClickFindsCloseGlyphInRenderedView (m3, r1 review) locates the ×
// by scanning View()'s ACTUAL rendered output -- not via the internal
// chipsTopY/chipRows helpers under test elsewhere -- so this test would
// catch a real drift between where chips are drawn and where a click is
// interpreted, not just an internal-helper self-consistency bug.
func TestMouseClickFindsCloseGlyphInRenderedView(t *testing.T) {
	h, m := chipMouseSetup(t)
	x, y := findCloseGlyph(t, m)
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if len(m.Chips()) != 2 {
		t.Fatalf("Chips() = %+v, want the first rendered chip removed", m.Chips())
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
	wantWithoutMenu := 2 /* two top-bar lines */ + 2*theme.ContentMargins(m.height) /* top-bar/content margin row + last-card/composer margin row, both now above the chip row */ + m.historyHeight()
	if withoutMenu != wantWithoutMenu {
		t.Fatalf("chipsTopY() = %d, want %d", withoutMenu, wantWithoutMenu)
	}

	// Opening the menu grows menuHeight() but shrinks historyHeight() by
	// exactly as much (historyHeight() now accounts for the menu, per the
	// "cheap fix" REQ), so the chip row's own Y position is UNCHANGED --
	// that's the point: View()'s total height stays == m.height either way,
	// with the menu and the chip row always landing in the same place
	// relative to the top bar and transcript combined.
	m.input.SetValue("/help")
	if m.commandMenuView() == "" {
		t.Fatal("expected the command menu to be open")
	}
	withMenu := m.chipsTopY()
	if withMenu != withoutMenu {
		t.Fatalf("chipsTopY() = %d, want unchanged at %d once the command menu shows (historyHeight absorbs it)", withMenu, withoutMenu)
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

	// Esc from the chip row -- with no text and a fresh chip list -- clears
	// the chips (Esc's second step, since there's no text to clear first)
	// and drops chip focus.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.chipFocus != -1 || !m.input.Focused() {
		t.Fatalf("chipFocus=%d inputFocused=%v, want -1/true after Esc", m.chipFocus, m.input.Focused())
	}

	// Moving into the transcript also drops chip focus.
	m.SetChips(threeChips())
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

// --- b1: replay of DataTug's own
// TestComposerAttachmentChipsCanBeFocusedClearedAndRestored
// (origin/main:pkg/chat/workspace_test.go) against chatshell's
// product-neutral composer, step for step. ---------------------------------

// findCloseGlyph renders m's current View() and returns the (X, Y)
// coordinates of the FIRST "×" glyph it finds, ansi-stripped so the
// coordinates are display columns/rows exactly as a mouse click would
// report them (m3, r1/r2 review: every × lookup in this replay locates the
// glyph by scanning the ACTUAL rendered output, never via the internal
// chipsTopY/chipRows helpers under test elsewhere).
func findCloseGlyph(t *testing.T, m *Model) (x, y int) {
	t.Helper()
	view := m.View().Content
	for row, line := range strings.Split(view, "\n") {
		if col := strings.Index(ansi.Strip(line), chipCloseGlyph); col >= 0 {
			return col, row
		}
	}
	t.Fatalf("rendered view has no %q glyph:\n%s", chipCloseGlyph, view)
	return 0, 0
}

func TestReplayDataTugComposerAttachmentChipsCanBeFocusedClearedAndRestored(t *testing.T) {
	h := &chipHandler{}
	m := newTestShell(h) // width 120, height 40
	m.SetMouseEnabled(true)
	customer := Chip{ID: "customer", Label: "Customer"}
	m.SetChips([]Chip{customer})
	m.input.SetValue("Top 5 rows")

	if !strings.Contains(m.chipsView(m.chatWidth()), "Customer") {
		t.Fatal("attached Customer chip is not inside the chip row")
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.chipFocus != 0 {
		t.Fatalf("Tab did not focus the attachment chip: %d", m.chipFocus)
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.input.Value() != "" || len(m.Chips()) != 1 {
		t.Fatalf("first Esc should clear text only: %q, %+v", m.input.Value(), m.Chips())
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if len(m.Chips()) != 0 {
		t.Fatalf("second Esc should clear attachments: %+v", m.Chips())
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape, Mod: tea.ModShift})
	if m.input.Value() != "Top 5 rows" || len(m.Chips()) != 1 {
		t.Fatalf("Shift+Esc did not restore draft and attachments: %q, %+v", m.input.Value(), m.Chips())
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if len(m.Chips()) != 0 {
		t.Fatal("Backspace did not remove the focused chip")
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape, Mod: tea.ModShift})
	if len(m.Chips()) != 1 || m.input.Value() != "Top 5 rows" {
		t.Fatal("Shift+Esc did not restore the chip removed with Backspace")
	}

	// Clicking the visible × (found in the rendered view, as m3 requires)
	// removes the chip.
	x, y := findCloseGlyph(t, m)
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if len(m.Chips()) != 0 {
		t.Fatal("clicking the visible × did not remove the chip")
	}

	// r2 review, m1: the replay continues past the mouse removal --
	// Shift+Esc restores the chip the click removed, Ctrl+D removes it
	// again (DataTug's own built-in shortcut, independent of chip focus),
	// and a further Shift+Esc restores it once more.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape, Mod: tea.ModShift})
	if len(m.Chips()) != 1 || m.Chips()[0].ID != "customer" {
		t.Fatalf("Shift+Esc did not restore the chip removed by the mouse click: %+v", m.Chips())
	}

	m.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	if len(m.Chips()) != 0 {
		t.Fatalf("Ctrl+D did not remove the last chip: %+v", m.Chips())
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape, Mod: tea.ModShift})
	if len(m.Chips()) != 1 || m.Chips()[0].ID != "customer" {
		t.Fatalf("Shift+Esc did not restore the chip removed by Ctrl+D: %+v", m.Chips())
	}
}

// withTrueColorEnv sets TERM/COLORTERM so theme.HalfBlockEdgesActive()
// reports true for the duration of fn -- the seam every merged-edge test
// below uses instead of reaching into theme's own unexported detection
// var.
func withTrueColorEnv(t *testing.T, fn func()) {
	t.Helper()
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "truecolor")
	if !theme.HalfBlockEdgesActive() {
		t.Fatal("withTrueColorEnv: theme.HalfBlockEdgesActive() still false")
	}
	fn()
}

// TestChipsRenderAsHalfBlockEdgeWithSingleCellSeparator covers the
// founder's r9 chips-in-edge idea directly: with chips present and
// half-block edges active, chipsView's row uses EXACTLY one filler cell
// between adjacent pills (founder, verbatim: "Have a 1 char half height
// separator between attachments" -- checked for two AND three chips), and
// the composer renders via ComposerFrameNoTopEdge (no separate top "▄"
// row of its own -- the chip row performs that role).
func TestChipsRenderAsHalfBlockEdgeWithSingleCellSeparator(t *testing.T) {
	withTrueColorEnv(t, func() {
		for _, n := range []int{2, 3} {
			h := &fakeHandler{}
			m := newTestShell(h)
			m.SetChips(threeChips()[:n])

			view := m.chipsView(m.chatWidth())
			plain := ansi.Strip(view)
			// n pills joined by n-1 single "▄" separators, plus a
			// trailing fill run -- never a plain space anywhere between
			// or after a pill in this mode.
			if strings.Contains(plain, "  ") {
				t.Fatalf("n=%d: expected no double space / plain-space gaps in half-block mode: %q", n, plain)
			}
			gotSeparators := strings.Count(view, "▄")
			if gotSeparators == 0 {
				t.Fatalf("n=%d: expected half-block filler glyphs in the chips row: %q", n, view)
			}

			rendered := m.View().Content
			if !m.composerUsesChipsAsTopEdge() {
				t.Fatalf("n=%d: expected composerUsesChipsAsTopEdge() true", n)
			}
			if !strings.Contains(rendered, "▄") {
				t.Fatalf("n=%d: expected the rendered view to contain half-block glyphs", n)
			}
		}
	})
}

// TestChipsMergedEdgeKeepsTotalRenderedHeightExact covers the composerHeight
// adjustment: with the composer's own top edge omitted (absorbed into the
// chip row), View()'s TOTAL rendered line count must still equal m.height
// exactly -- the same invariant historyHeight's own doc requires
// unconditionally.
func TestChipsMergedEdgeKeepsTotalRenderedHeightExact(t *testing.T) {
	withTrueColorEnv(t, func() {
		h := &fakeHandler{}
		m := newTestShell(h)
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		m.SetChips(threeChips())
		content := m.View().Content
		lines := strings.Count(content, "\n") + 1
		if lines != m.height {
			t.Fatalf("rendered %d lines, want exactly m.height=%d:\n%s", lines, m.height, content)
		}
	})
}

// TestChipCloseClickStillHitsGlyphInMergedEdgeMode is the critical
// regression check for the chips-in-edge redesign: chipRows' x/y layout
// math is UNCHANGED by the visual redesign (same cell widths, same 1-col
// gap accounting -- see chipRows' own doc), so a click on the rendered ×
// must still remove the right chip even when that row is now doubling as
// the composer's own top edge.
func TestChipCloseClickStillHitsGlyphInMergedEdgeMode(t *testing.T) {
	withTrueColorEnv(t, func() {
		h := &chipHandler{}
		m := New(h, WithMouse(MouseCellMotion))
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		m.SetMouseEnabled(true)
		m.SetChips(threeChips())

		cell := chipCellFor(t, m, 1)
		y := m.chipsTopY()
		m.Update(tea.MouseClickMsg{X: cell.x, Y: y, Button: tea.MouseLeft})
		if got := m.Chips(); len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
			t.Fatalf("expected chip %q removed by its close-glyph click in merged-edge mode, got %+v", "b", got)
		}
	})
}

// TestComposerFallsBackToOwnTopEdgeWithoutChips covers the "no chips"
// case in half-block mode: composerUsesChipsAsTopEdge() must be false and
// the composer must render its OWN top edge as usual.
func TestComposerFallsBackToOwnTopEdgeWithoutChips(t *testing.T) {
	withTrueColorEnv(t, func() {
		h := &fakeHandler{}
		m := newTestShell(h)
		if m.composerUsesChipsAsTopEdge() {
			t.Fatal("expected composerUsesChipsAsTopEdge() false with no chips")
		}
	})
}
