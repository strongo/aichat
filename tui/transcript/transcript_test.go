package transcript

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/strongo/aichat/ai/session"
)

type fakeBlock struct {
	ref     *session.EntityRef
	updates int
	label   string
}

func (b *fakeBlock) View(width int, focused bool) string {
	if focused {
		return "[" + b.label + "]"
	}
	return b.label
}

func (b *fakeBlock) Update(msg tea.Msg) (Block, tea.Cmd) {
	b.updates++
	return b, nil
}

func (b *fakeBlock) Focusable() bool { return true }

func (b *fakeBlock) Current() *session.EntityRef { return b.ref }

type escCapturingBlock struct {
	fakeBlock
	captures bool
}

func (b *escCapturingBlock) CapturesEsc() bool { return b.captures }

func (b *escCapturingBlock) Update(msg tea.Msg) (Block, tea.Cmd) {
	b.updates++
	return b, nil
}

type targetedMsg struct{ id string }

func (m targetedMsg) TargetEntryID() string { return m.id }

func TestAppendAndView(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	m.Append(Entry{Role: RoleUser, Text: "hello"})
	m.Append(Entry{Role: RoleAssistant, Text: "hi there"})
	view := m.View()
	if !strings.Contains(view, "hello") || !strings.Contains(view, "hi there") {
		t.Fatalf("view missing entries: %q", view)
	}
}

func TestAppendDeltaCreatesAndAppends(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	m.AppendDelta("turn-1", "Hel")
	m.AppendDelta("turn-1", "lo")
	if len(m.Entries()) != 1 {
		t.Fatalf("entries = %d, want 1", len(m.Entries()))
	}
	if got := m.Entries()[0].Text; got != "Hello" {
		t.Fatalf("text = %q, want Hello", got)
	}
	if m.Entries()[0].Role != RoleAssistant {
		t.Fatalf("role = %v, want assistant", m.Entries()[0].Role)
	}
}

func TestStopsCountsOnlyFocusable(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	m.Append(Entry{Role: RoleUser, Text: "u1"})
	m.Append(Entry{Role: RoleAssistant, Text: "a1"})
	m.Append(Entry{Role: RoleUser, Text: "u2"})
	if got := m.Stops(); got != 2 {
		t.Fatalf("Stops() = %d, want 2", got)
	}
}

func TestFocusAndCurrentDelegatesToEntityBlock(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	ref := &session.EntityRef{Type: "row", Keys: map[string]string{"id": "1"}}
	m.Append(Entry{Block: &fakeBlock{ref: ref, label: "grid"}})
	m.Focus(0)
	if got := m.Current(); got == nil || !got.Same(*ref) {
		t.Fatalf("Current() = %v, want %v", got, ref)
	}
	view := m.View()
	if !strings.Contains(view, "[grid]") {
		t.Fatalf("focused block not rendered focused: %q", view)
	}
}

func TestBlurClearsFocus(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	m.Append(Entry{Block: &fakeBlock{label: "grid"}})
	m.Focus(0)
	m.Blur()
	if m.Current() != nil {
		t.Fatal("Current() non-nil after Blur")
	}
}

func TestUpdateRoutesToFocusedBlock(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	blk := &fakeBlock{label: "grid"}
	m.Append(Entry{Block: blk})
	m.Focus(0)
	m.Update(tea.KeyPressMsg{Code: 'j'})
	if blk.updates != 1 {
		t.Fatalf("Block.Update calls = %d, want 1", blk.updates)
	}
}

func TestUpdateWithoutFocusIsNoop(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	blk := &fakeBlock{label: "grid"}
	m.Append(Entry{Block: blk})
	m.Update(tea.KeyPressMsg{Code: 'j'})
	if blk.updates != 0 {
		t.Fatalf("Block.Update calls = %d, want 0", blk.updates)
	}
}

func TestEnsureBlockVisibleScrollsFocusedBlockIntoView(t *testing.T) {
	m := New()
	m.SetSize(20, 3)
	m.Append(Entry{Role: RoleAssistant, Text: strings.Repeat("line\n", 10)})
	m.Append(Entry{Block: &fakeBlock{label: "grid"}})
	m.Focus(0)
	// The focused block is the last entry; the viewport should have scrolled
	// down from the top so it is visible.
	if off := m.viewport.YOffset(); off == 0 {
		t.Fatalf("YOffset = %d, want > 0 (scrolled to focused block)", off)
	}
}

func TestAppendDeltaOnlyInvalidatesStreamingEntry(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	m.Append(Entry{Role: RoleUser, Text: "static message"})
	m.AppendDelta("turn-1", "Hel")
	// Force both entries' caches to be populated.
	m.View()
	firstRenderedBefore := m.entries[0].renderOut
	if !m.entries[0].renderValid || !m.entries[1].renderValid {
		t.Fatalf("expected both entries cached: %+v", m.entries)
	}
	m.AppendDelta("turn-1", "lo")
	// The static first entry's cache must survive untouched...
	if !m.entries[0].renderValid {
		t.Fatal("unrelated entry's cache was invalidated by AppendDelta")
	}
	if m.entries[0].renderOut != firstRenderedBefore {
		t.Fatalf("unrelated entry re-rendered: %q vs %q", m.entries[0].renderOut, firstRenderedBefore)
	}
	// ...while the streaming entry's cache was invalidated and refreshed.
	if !strings.Contains(m.entries[1].renderOut, "Hello") {
		t.Fatalf("streaming entry not re-rendered: %q", m.entries[1].renderOut)
	}
}

func TestAppendDoesNotAutoFollowWhenFocusedOnEarlierStopAndScrolledUp(t *testing.T) {
	m := New()
	m.SetSize(20, 3)
	for i := 0; i < 5; i++ {
		m.Append(Entry{Block: &fakeBlock{label: "grid"}})
	}
	m.Focus(0) // focus the first (earliest) stop
	m.ScrollUp(100)
	off := m.viewport.YOffset()
	m.Append(Entry{Block: &fakeBlock{label: "grid"}})
	if m.viewport.YOffset() != off {
		t.Fatalf("YOffset changed from %d to %d: Append force-scrolled while an earlier stop was focused", off, m.viewport.YOffset())
	}
}

func TestAppendAutoFollowsWhenUnfocused(t *testing.T) {
	m := New()
	m.SetSize(20, 3)
	m.Append(Entry{Role: RoleAssistant, Text: strings.Repeat("line\n", 20)})
	if !m.viewport.AtBottom() {
		t.Fatal("expected to be at bottom after an unfocused Append")
	}
}

func TestCapturesEscDelegatesToFocusedBlock(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	m.Append(Entry{Block: &escCapturingBlock{fakeBlock: fakeBlock{label: "grid"}, captures: true}})
	if m.CapturesEsc() {
		t.Fatal("CapturesEsc should be false before focusing")
	}
	m.Focus(0)
	if !m.CapturesEsc() {
		t.Fatal("CapturesEsc should delegate to the focused block")
	}
}

func TestCapturesEscFalseWhenBlockDoesNotImplementIt(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	m.Append(Entry{Block: &fakeBlock{label: "grid"}})
	m.Focus(0)
	if m.CapturesEsc() {
		t.Fatal("plain block should not capture Esc")
	}
}

func TestUpdateBroadcastsNonKeyMessagesToAllBlocks(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	b1 := &fakeBlock{label: "one"}
	b2 := &fakeBlock{label: "two"}
	m.Append(Entry{Block: b1})
	m.Append(Entry{Block: b2})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if b1.updates != 1 || b2.updates != 1 {
		t.Fatalf("updates = %d/%d, want 1/1", b1.updates, b2.updates)
	}
}

func TestUpdateTargetedMessageOnlyReachesMatchingEntry(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	b1 := &fakeBlock{label: "one"}
	b2 := &fakeBlock{label: "two"}
	m.Append(Entry{ID: "a", Block: b1})
	m.Append(Entry{ID: "b", Block: b2})
	m.Update(targetedMsg{id: "b"})
	if b1.updates != 0 {
		t.Fatalf("non-targeted entry received the message: updates=%d", b1.updates)
	}
	if b2.updates != 1 {
		t.Fatalf("targeted entry did not receive the message: updates=%d", b2.updates)
	}
}

func TestScrollUpDown(t *testing.T) {
	m := New()
	m.SetSize(20, 3)
	m.Append(Entry{Role: RoleAssistant, Text: strings.Repeat("line\n", 20)})
	m.ScrollDown(5)
	off := m.viewport.YOffset()
	if off == 0 {
		t.Fatal("ScrollDown did not move viewport")
	}
	m.ScrollUp(2)
	if m.viewport.YOffset() != off-2 {
		t.Fatalf("YOffset after ScrollUp = %d, want %d", m.viewport.YOffset(), off-2)
	}
}

type nonFocusableBlock struct{ fakeBlock }

func (b *nonFocusableBlock) Focusable() bool { return false }

func TestReplaceBlockSwapsInPlace(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	m.Append(Entry{ID: "a", Block: &fakeBlock{label: "old"}})

	m.ReplaceBlock("a", &fakeBlock{label: "new"})

	if m.entries[0].Block.(*fakeBlock).label != "new" {
		t.Fatalf("Block not replaced: %+v", m.entries[0])
	}
	if m.entries[0].ID != "a" {
		t.Fatalf("ID changed: %+v", m.entries[0])
	}
}

func TestReplaceBlockUnknownIDIsNoop(t *testing.T) {
	m := New()
	m.Append(Entry{ID: "a", Block: &fakeBlock{label: "old"}})
	m.ReplaceBlock("nope", &fakeBlock{label: "new"})
	if m.entries[0].Block.(*fakeBlock).label != "old" {
		t.Fatalf("entry mutated for an unknown id: %+v", m.entries[0])
	}
}

func TestReplaceBlockKeepsFocusOnSameEntryAcrossFocusabilityChange(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	m.Append(Entry{ID: "a", Block: &fakeBlock{label: "a"}})
	m.Append(Entry{ID: "b", Block: &fakeBlock{label: "b"}})
	m.Focus(1) // stop 1 == entry "b"
	if got := m.FocusedEntry(); got == nil || got.ID != "b" {
		t.Fatalf("FocusedEntry() = %+v, want b", got)
	}

	// Swap "a" (NOT focused) to non-focusable: this removes a stop BEFORE
	// "b"'s, so "b" now occupies stop 0 -- ReplaceBlock must follow it.
	m.ReplaceBlock("a", &nonFocusableBlock{fakeBlock{label: "a2"}})

	got := m.FocusedEntry()
	if got == nil || got.ID != "b" {
		t.Fatalf("FocusedEntry() = %+v, want still b after an earlier entry's focusability changed", got)
	}
	if m.focusIndex != 0 {
		t.Errorf("focusIndex = %d, want 0 (b is now the only/first stop)", m.focusIndex)
	}
}

func TestReplaceBlockOnFocusedEntryToNonFocusableClearsThatStop(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	m.Append(Entry{ID: "a", Block: &fakeBlock{label: "a"}})
	m.Focus(0)

	m.ReplaceBlock("a", &nonFocusableBlock{fakeBlock{label: "a2"}})

	if m.focusIndex != -1 {
		t.Errorf("focusIndex = %d, want -1 (the focused entry is no longer focusable)", m.focusIndex)
	}
}

func TestClearRemovesEntriesAndFocus(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	m.Append(Entry{ID: "a", Block: &fakeBlock{label: "a"}})
	m.Focus(0)

	m.Clear()

	if len(m.Entries()) != 0 {
		t.Fatalf("Entries() = %+v, want empty", m.Entries())
	}
	if m.FocusedEntry() != nil {
		t.Fatalf("FocusedEntry() = %+v, want nil after Clear", m.FocusedEntry())
	}
}

func TestStopForID(t *testing.T) {
	m := New()
	m.Append(Entry{Role: RoleAssistant, Text: "not focusable"})
	m.Append(Entry{ID: "grid-1", Block: &fakeBlock{label: "g1"}})
	m.Append(Entry{ID: "grid-2", Block: &fakeBlock{label: "g2"}})

	if stop := m.StopForID("grid-2"); stop != 1 {
		t.Errorf("StopForID(grid-2) = %d, want 1", stop)
	}
	if stop := m.StopForID("nope"); stop != -1 {
		t.Errorf("StopForID(nope) = %d, want -1", stop)
	}
	if stop := m.StopForID(""); stop != -1 {
		t.Errorf("StopForID(\"\") = %d, want -1", stop)
	}
}

func TestStopForIDIgnoresNonFocusableEntry(t *testing.T) {
	m := New()
	m.Append(Entry{ID: "sys", Role: RoleSystem, Text: "not focusable but has an ID"})
	if stop := m.StopForID("sys"); stop != -1 {
		t.Errorf("StopForID(sys) = %d, want -1 (system entries aren't focusable)", stop)
	}
}

func TestSetMarkdownRendererAppliesAfterConstruction(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	var gotWidth int
	m.SetMarkdownRenderer(func(text string, width int) string {
		gotWidth = width
		return "RENDERED:" + text
	})
	m.Append(Entry{Role: RoleAssistant, Text: "hi", Markdown: true})

	if !strings.Contains(m.View(), "RENDERED:hi") {
		t.Fatalf("View() = %q, want the configured renderer applied", m.View())
	}
	if gotWidth <= 0 {
		t.Errorf("renderer width = %d, want > 0", gotWidth)
	}
}
