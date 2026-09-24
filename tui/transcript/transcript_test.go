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

type wheelConsumingBlock struct {
	fakeBlock
	consume bool
}

func (b *wheelConsumingBlock) ConsumesWheel(msg tea.MouseWheelMsg) bool { return b.consume }

func (b *wheelConsumingBlock) Update(msg tea.Msg) (Block, tea.Cmd) {
	b.updates++
	return b, nil
}

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

func TestDeliverWheelToFocusedBlock_NoFocus(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	m.Append(Entry{Block: &wheelConsumingBlock{consume: true}})
	consumed, cmd := m.DeliverWheelToFocusedBlock(tea.MouseWheelMsg{})
	if consumed {
		t.Fatal("consumed = true, want false (nothing focused)")
	}
	if cmd != nil {
		t.Fatal("cmd != nil, want nil")
	}
}

func TestDeliverWheelToFocusedBlock_FocusedButNoBlock(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	m.Append(Entry{Role: RoleUser, Text: "hi"}) // focusable (RoleUser), no Block
	m.Focus(0)
	consumed, _ := m.DeliverWheelToFocusedBlock(tea.MouseWheelMsg{})
	if consumed {
		t.Fatal("consumed = true, want false (focused entry has no Block)")
	}
}

func TestDeliverWheelToFocusedBlock_NonConsumerBlock(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	blk := &fakeBlock{label: "b"} // does not implement WheelConsumer
	m.Append(Entry{Block: blk})
	m.Focus(0)
	consumed, _ := m.DeliverWheelToFocusedBlock(tea.MouseWheelMsg{})
	if consumed {
		t.Fatal("consumed = true, want false (Block doesn't implement WheelConsumer)")
	}
	if blk.updates != 0 {
		t.Fatalf("blk.updates = %d, want 0 (never dispatched)", blk.updates)
	}
}

func TestDeliverWheelToFocusedBlock_DecliningBlock(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	blk := &wheelConsumingBlock{consume: false}
	m.Append(Entry{Block: blk})
	m.Focus(0)
	consumed, _ := m.DeliverWheelToFocusedBlock(tea.MouseWheelMsg{})
	if consumed {
		t.Fatal("consumed = true, want false (Block declined)")
	}
	if blk.updates != 0 {
		t.Fatalf("blk.updates = %d, want 0 (declined, never dispatched)", blk.updates)
	}
}

func TestDeliverWheelToFocusedBlock_ConsumingBlockDispatchesAndRebuilds(t *testing.T) {
	m := New()
	m.SetSize(40, 10)
	blk := &wheelConsumingBlock{consume: true}
	m.Append(Entry{Block: blk})
	m.Focus(0)
	consumed, cmd := m.DeliverWheelToFocusedBlock(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if !consumed {
		t.Fatal("consumed = false, want true")
	}
	if cmd != nil {
		t.Fatal("cmd != nil, want nil (fake block returns nil)")
	}
	if blk.updates != 1 {
		t.Fatalf("blk.updates = %d, want 1", blk.updates)
	}
	// Rebuild ran (and re-validated the cache) as part of the dispatch --
	// confirmed indirectly via View() not panicking/erroring on the fresh
	// state; the important behavior is consumed=true, updates=1 above.
	_ = m.View()
}

// --- r3 review, B1: SetSize must not re-Rebuild (and so not snap an
// unfocused, manually-scrolled-up viewport back to the bottom, and not
// re-force a focused entry back into view) when neither dimension
// actually changed -- a caller (chatshell's View(), which now calls its
// own resize() on every render) may call SetSize with the SAME width and
// height on every single frame. ---------------------------------------------

func TestSetSizeSameDimensionsIsNoopAfterWheelScroll(t *testing.T) {
	m := New()
	m.SetSize(20, 3)
	m.Append(Entry{Role: RoleAssistant, Text: strings.Repeat("line\n", 20)})
	if !m.viewport.AtBottom() {
		t.Fatal("expected to be at bottom after an unfocused Append")
	}
	m.ScrollUp(3) // e.g. a mouse wheel tick
	off := m.viewport.YOffset()
	if off == 0 {
		t.Fatal("ScrollUp did not move the viewport off the bottom")
	}

	// The next render calls SetSize with the SAME dimensions (chatshell's
	// View() does this every frame) -- it must not snap back to the bottom.
	m.SetSize(20, 3)
	if got := m.viewport.YOffset(); got != off {
		t.Fatalf("YOffset = %d after a same-size SetSize, want unchanged %d (wheel scroll was undone)", got, off)
	}

	// A second same-size call is equally a no-op.
	m.SetSize(20, 3)
	if got := m.viewport.YOffset(); got != off {
		t.Fatalf("YOffset = %d after a second same-size SetSize, want unchanged %d", got, off)
	}
}

func TestSetSizeSameDimensionsDoesNotReForceFocusedEntryIntoView(t *testing.T) {
	m := New()
	m.SetSize(20, 3)
	for i := 0; i < 5; i++ {
		m.Append(Entry{Block: &fakeBlock{label: "grid"}})
	}
	m.Focus(4) // focus the LAST stop -- ensureBlockVisible scrolls to it
	scrolledTo := m.viewport.YOffset()

	// Scroll away from the focused entry, as a user reading earlier
	// content might (e.g. PgUp/wheel while a block still holds focus).
	m.ScrollUp(100)
	off := m.viewport.YOffset()
	if off == scrolledTo {
		t.Fatal("ScrollUp did not move the viewport away from the focused entry")
	}

	// A same-size SetSize (chatshell's per-frame resize()) must not
	// re-run ensureBlockVisible and pull the view back to the focused
	// entry -- that's the "focused entry not re-forced into view every
	// frame" regression this fix closes.
	m.SetSize(20, 3)
	if got := m.viewport.YOffset(); got != off {
		t.Fatalf("YOffset = %d after a same-size SetSize, want unchanged %d (focused entry was re-forced into view)", got, off)
	}
}

func TestSetSizeHeightChangePreservesOffsetWhenNotAtBottom(t *testing.T) {
	m := New()
	m.SetSize(20, 3)
	m.Append(Entry{Role: RoleAssistant, Text: strings.Repeat("line\n", 40)})
	bottom := m.viewport.YOffset()
	m.ScrollUp(3) // partway up, not all the way to the top
	off := m.viewport.YOffset()
	if off == 0 || off == bottom {
		t.Fatalf("ScrollUp(3) did not move the viewport partway up: bottom=%d off=%d", bottom, off)
	}
	if m.viewport.AtBottom() {
		t.Fatal("expected NOT to be at bottom after scrolling up")
	}

	// A genuine height change (e.g. the terminal window resized, or
	// historyHeight() actually grew/shrank) must preserve the scroll
	// position rather than snapping to the bottom, since nothing NEW
	// arrived -- unlike Append's own auto-follow-when-unfocused rule.
	m.SetSize(20, 5)
	if got := m.viewport.YOffset(); got != off {
		t.Fatalf("YOffset = %d after a height change while scrolled up, want unchanged %d", got, off)
	}
}

func TestSetSizeHeightChangeKeepsFollowingWhenAtBottom(t *testing.T) {
	m := New()
	m.SetSize(20, 3)
	m.Append(Entry{Role: RoleAssistant, Text: strings.Repeat("line\n", 40)})
	if !m.viewport.AtBottom() {
		t.Fatal("expected to be at bottom after an unfocused Append")
	}

	m.SetSize(20, 5)
	if !m.viewport.AtBottom() {
		t.Fatal("expected to still be at bottom after a height change that started at the bottom")
	}
}
