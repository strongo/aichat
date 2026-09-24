package chatshell

import (
	"context"
	"iter"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/session"
	"github.com/strongo/aichat/tui"
	"github.com/strongo/aichat/tui/focus"
	"github.com/strongo/aichat/tui/sidebar"
	"github.com/strongo/aichat/tui/stream"
	"github.com/strongo/aichat/tui/transcript"
)

type fakeHandler struct {
	submitted    []string
	sidebarSeen  [][]session.EntityRef
	submitResult tea.Cmd
	streamEvents []ai.Event
	msgsSeen     []tea.Msg
	streamDone   []streamDoneCall
}

type streamDoneCall struct {
	id  string
	err error
}

func (h *fakeHandler) Submit(text string) tea.Cmd {
	h.submitted = append(h.submitted, text)
	return h.submitResult
}

func (h *fakeHandler) OnSidebarChange(refs []session.EntityRef) {
	h.sidebarSeen = append(h.sidebarSeen, append([]session.EntityRef(nil), refs...))
}

func (h *fakeHandler) OnStreamEvent(id string, ev ai.Event) tea.Cmd {
	h.streamEvents = append(h.streamEvents, ev)
	return nil
}

func (h *fakeHandler) OnStreamDone(id string, err error) tea.Cmd {
	h.streamDone = append(h.streamDone, streamDoneCall{id: id, err: err})
	return nil
}

func (h *fakeHandler) OnMsg(msg tea.Msg) tea.Cmd {
	h.msgsSeen = append(h.msgsSeen, msg)
	return nil
}

// fakeBlock is a minimal transcript.Block used to test focus/Esc/update
// routing without depending on tui/grid (owned by another concurrent lane).
type fakeBlock struct {
	updates   int
	captures  bool
	lastEvent tea.Msg
}

func (b *fakeBlock) View(width int, focused bool) string { return "block" }

func (b *fakeBlock) Update(msg tea.Msg) (transcript.Block, tea.Cmd) {
	b.updates++
	b.lastEvent = msg
	return b, nil
}

func (b *fakeBlock) Focusable() bool { return true }

func (b *fakeBlock) CapturesEsc() bool { return b.captures }

func newTestShell(handler Handler) *Model {
	m := New(handler)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return m
}

func TestEnterSubmitsAndAppendsUserMessage(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.input.SetValue("hello there")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(h.submitted) != 1 || h.submitted[0] != "hello there" {
		t.Fatalf("submitted = %v", h.submitted)
	}
	if m.input.Value() != "" {
		t.Fatalf("input not cleared: %q", m.input.Value())
	}
	entries := m.transcript.Entries()
	if len(entries) != 1 || entries[0].Text != "hello there" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestEnterOnEmptyInputDoesNothing(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(h.submitted) != 0 {
		t.Fatalf("submitted = %v, want none", h.submitted)
	}
}

func TestShiftEnterInsertsNewlineWithoutSubmitting(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.input.SetValue("line1")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	if len(h.submitted) != 0 {
		t.Fatalf("submitted = %v, want none", h.submitted)
	}
	if !strings.Contains(m.input.Value(), "\n") {
		t.Fatalf("input = %q, want a newline", m.input.Value())
	}
}

func TestBusyBlocksSubmit(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.busy = true
	m.input.SetValue("hello")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(h.submitted) != 0 {
		t.Fatalf("submitted while busy = %v", h.submitted)
	}
}

func seqOf(events ...ai.Event) iter.Seq2[ai.Event, error] {
	return func(yield func(ai.Event, error) bool) {
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// openSeq adapts a fixed event list to StartStream's open func shape. Most
// tests don't care about the ctx StartStream hands them.
func openSeq(events ...ai.Event) func(context.Context) iter.Seq2[ai.Event, error] {
	return func(context.Context) iter.Seq2[ai.Event, error] { return seqOf(events...) }
}

// drainCmd runs cmd and every tea.Cmd it (transitively) produces through
// m.Update, unpacking tea.BatchMsg the way the real Bubble Tea runtime does,
// until no command remains or max steps have run.
func drainCmd(t *testing.T, m *Model, cmd tea.Cmd, max int) {
	t.Helper()
	pending := []tea.Cmd{cmd}
	for step := 0; len(pending) > 0 && step < max; step++ {
		next := pending[0]
		pending = pending[1:]
		if next == nil {
			continue
		}
		msg := next()
		if batch, ok := msg.(tea.BatchMsg); ok {
			pending = append(pending, batch...)
			continue
		}
		var out tea.Cmd
		_, out = m.Update(msg)
		if out != nil {
			pending = append(pending, out)
		}
	}
}

func TestStartStreamRendersDeltasProgressively(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.ctx = context.Background()
	events := []ai.Event{
		{Type: ai.EventStarted},
		{Type: ai.EventTextDelta, Text: "Hel"},
		{Type: ai.EventTextDelta, Text: "lo"},
		{Type: ai.EventCompleted},
	}
	cmd := m.StartStream("turn-1", openSeq(events...))
	if !m.Busy() {
		t.Fatal("StartStream did not set busy")
	}
	drainCmd(t, m, cmd, 20)
	if m.Busy() {
		t.Fatal("still busy after stream completed")
	}
	entries := m.transcript.Entries()
	if len(entries) != 1 || entries[0].Text != "Hello" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestStreamErrorAppendsSystemMessage(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	events := []ai.Event{
		{Type: ai.EventError, Error: &ai.Error{Code: ai.ErrCodeUpstream, Message: "boom"}},
	}
	cmd := m.StartStream("turn-1", openSeq(events...))
	drainCmd(t, m, cmd, 20)
	found := false
	for _, e := range m.transcript.Entries() {
		if strings.Contains(e.Text, "boom") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no error message in transcript: %+v", m.transcript.Entries())
	}
}

func TestAddToSidebarMsgPinsAndNotifiesHandler(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	ref := session.EntityRef{Type: "row", Keys: map[string]string{"id": "1"}}
	m.Update(tui.AddToSidebarMsg{Ref: ref})
	if len(m.SidebarRefs()) != 1 {
		t.Fatalf("sidebar refs = %v", m.SidebarRefs())
	}
	if len(h.sidebarSeen) != 1 {
		t.Fatalf("handler not notified: %v", h.sidebarSeen)
	}
}

func TestSidebarRemoveMsgNotifiesHandler(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	ref := session.EntityRef{Type: "row", Keys: map[string]string{"id": "1"}}
	m.PinToSidebar(ref)
	h.sidebarSeen = nil
	m.Update(sidebar.RemoveMsg{Ref: ref})
	if len(h.sidebarSeen) != 1 {
		t.Fatalf("handler not notified on RemoveMsg: %v", h.sidebarSeen)
	}
}

func TestShiftRightFocusesSidebarWhenSplit(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h) // width 120 >= splitMinWidth
	m.Update(tea.KeyPressMsg{Code: 'j', Text: "j", Mod: 0})
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift})
	if m.focusRing.Zone() != focus.ZoneSidebar {
		t.Fatalf("zone = %v, want sidebar", m.focusRing.Zone())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift})
	if m.focusRing.Zone() != focus.ZoneInput {
		t.Fatalf("zone = %v, want input", m.focusRing.Zone())
	}
}

func TestShiftRightNoopWhenNotSplit(t *testing.T) {
	h := &fakeHandler{}
	m := New(h)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 24}) // narrow: no split
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift})
	if m.focusRing.Zone() != focus.ZoneInput {
		t.Fatalf("zone = %v, want input (split disabled)", m.focusRing.Zone())
	}
}

func TestF6TogglesSidebarVisibility(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	if !m.sidebar.Visible() {
		t.Fatal("sidebar should start visible")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	if m.sidebar.Visible() {
		t.Fatal("sidebar should be hidden after F6")
	}
}

func TestCtrlLeftRightResizeSplit(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	before := m.sidebar.ChatPercent()
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModCtrl})
	if m.sidebar.ChatPercent() <= before {
		t.Fatalf("ChatPercent did not grow: %d -> %d", before, m.sidebar.ChatPercent())
	}
}

func TestFocusedRefReflectsTranscriptFocus(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	if m.FocusedRef() != nil {
		t.Fatal("FocusedRef should be nil with no transcript focus")
	}
}

func TestOptionsConfigureModel(t *testing.T) {
	h := &fakeHandler{}
	ctx := context.Background()
	m := New(h,
		WithCommands([]Command{{Name: "/help"}}),
		WithSidebarRenderer(func(ref session.EntityRef, width int) string { return "R:" + ref.Title }),
		WithContext(ctx),
	)
	if len(m.commands) != 1 {
		t.Fatalf("commands = %v", m.commands)
	}
	if m.ctx != ctx {
		t.Fatal("WithContext did not set ctx")
	}
	ref := session.EntityRef{Type: "t", Keys: map[string]string{"id": "1"}, Title: "X"}
	m.PinToSidebar(ref)
	view := m.sidebar.View(30, false)
	if !strings.Contains(view, "R:X") {
		t.Fatalf("sidebar renderer not applied: %q", view)
	}
}

func TestAppendHelpers(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.AppendAssistant("assist")
	m.AppendBlock(nil)
	m.SetStatus("provider/model")
	if m.status != "provider/model" {
		t.Fatalf("status = %q", m.status)
	}
	entries := m.transcript.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestUnpinFromSidebar(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	ref := session.EntityRef{Type: "t", Keys: map[string]string{"id": "1"}}
	m.PinToSidebar(ref)
	h.sidebarSeen = nil
	m.UnpinFromSidebar(ref)
	if len(m.SidebarRefs()) != 0 {
		t.Fatalf("refs = %v", m.SidebarRefs())
	}
	if len(h.sidebarSeen) != 1 {
		t.Fatal("handler not notified on unpin")
	}
}

func TestInitReturnsBlinkCmd(t *testing.T) {
	h := &fakeHandler{}
	m := New(h)
	if m.Init() == nil {
		t.Fatal("Init() returned nil cmd")
	}
}

func TestViewRendersSplitAndNonSplit(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h) // width 120: split
	m.AppendUser("hi")
	view := m.View()
	if !view.AltScreen {
		t.Fatal("AltScreen should be set")
	}

	narrow := New(h)
	narrow.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	narrow.View()
}

func TestViewShowsSpinnerWhileBusy(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.busy = true
	view := m.View()
	if !strings.Contains(view.Content, "thinking") {
		t.Fatalf("view content missing spinner text: %q", view.Content)
	}
}

func TestOpenMsgIsAccepted(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	ref := session.EntityRef{Type: "t", Keys: map[string]string{"id": "1"}}
	if _, cmd := m.Update(sidebar.OpenMsg{Ref: ref}); cmd != nil {
		t.Fatal("OpenMsg should not return a command")
	}
}

func TestUnknownMsgIsIgnored(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	if _, cmd := m.Update(struct{}{}); cmd != nil {
		t.Fatal("unknown message should not return a command")
	}
}

func TestSlashCommandMenuInsertsOnEnter(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.commands = []Command{{Name: "/help", Help: "Show help"}}
	for _, r := range "/he" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if len(m.commandMenuMatches()) != 1 {
		t.Fatalf("matches = %v", m.commandMenuMatches())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.input.Value() != "/help " {
		t.Fatalf("input = %q, want '/help '", m.input.Value())
	}
	if len(h.submitted) != 0 {
		t.Fatal("Enter on menu should not submit")
	}
}

// --- M5/M9 fix-round tests -------------------------------------------------

func TestEscWhileBusyCancelsStreamAndRendersStopped(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	var openedCtx context.Context
	block := make(chan struct{})
	open := func(ctx context.Context) iter.Seq2[ai.Event, error] {
		openedCtx = ctx // S2: StartStream must hand its per-stream ctx to open
		return func(yield func(ai.Event, error) bool) {
			if !yield(ai.Event{Type: ai.EventStarted}, nil) {
				return
			}
			<-block // blocks until the derived ctx is cancelled by Esc
		}
	}
	cmd := m.StartStream("turn-1", open)
	if !m.Busy() {
		t.Fatal("StartStream did not set busy")
	}
	if openedCtx == nil {
		t.Fatal("StartStream did not pass a ctx to open")
	}
	// Drain only the first message (EventStarted); the pump then blocks on
	// the second yield until cancelled.
	msg := firstFromBatch(t, cmd)
	_, next := m.Update(msg)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	// S2: cancellation must reach the actual ctx the provider call was
	// opened with, not just chatshell's local bookkeeping.
	if openedCtx.Err() == nil {
		t.Fatal("Esc while busy did not cancel the ctx handed to open (the provider's own request)")
	}
	drainCmd(t, m, next, 20)
	close(block)
	if m.Busy() {
		t.Fatal("still busy after cancellation")
	}
	found := false
	for _, e := range m.transcript.Entries() {
		if strings.Contains(e.Text, "(stopped)") {
			found = true
		}
		if strings.Contains(e.Text, "context canceled") {
			t.Fatalf("cancellation rendered as an error, not (stopped): %+v", e)
		}
	}
	if !found {
		t.Fatalf("no (stopped) entry in transcript: %+v", m.transcript.Entries())
	}
	if len(h.streamDone) != 1 || h.streamDone[0].id != "turn-1" {
		t.Fatalf("OnStreamDone not called for the cancelled stream: %+v", h.streamDone)
	}
}

func TestCtrlCWhileBusyCancelsStreamInsteadOfQuitting(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.busy = true
	m.streamID = "turn-1"
	cancelled := false
	m.streamCancel = func() { cancelled = true }
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !cancelled {
		t.Fatal("Ctrl+C while busy did not cancel the stream")
	}
	if cmd != nil {
		t.Fatal("Ctrl+C while busy should not quit")
	}
	if m.quit {
		t.Fatal("quit flag set while busy")
	}
}

func TestCtrlCWhenNotBusyStillQuits(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil || !m.quit {
		t.Fatal("Ctrl+C when idle should quit")
	}
}

func TestStreamObserverReceivesEveryEvent(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	events := []ai.Event{
		{Type: ai.EventStarted},
		{Type: ai.EventTextDelta, Text: "hi"},
		{Type: ai.EventUsage},
		{Type: ai.EventCompleted},
	}
	cmd := m.StartStream("turn-1", openSeq(events...))
	drainCmd(t, m, cmd, 20)
	if len(h.streamEvents) != len(events) {
		t.Fatalf("observer saw %d events, want %d: %+v", len(h.streamEvents), len(events), h.streamEvents)
	}
	for i, ev := range events {
		if h.streamEvents[i].Type != ev.Type {
			t.Errorf("event %d type = %v, want %v", i, h.streamEvents[i].Type, ev.Type)
		}
	}
	if len(h.streamDone) != 1 || h.streamDone[0].id != "turn-1" || h.streamDone[0].err != nil {
		t.Fatalf("OnStreamDone = %+v, want one success call for turn-1", h.streamDone)
	}
}

func TestStreamObserverOnStreamDoneFiresForFatalError(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	boom := &ai.Error{Code: ai.ErrCodeUpstream, Message: "boom"}
	seq := func(yield func(ai.Event, error) bool) {
		yield(ai.Event{Type: ai.EventError, Error: boom}, boom)
	}
	cmd := m.StartStream("turn-1", func(context.Context) iter.Seq2[ai.Event, error] { return seq })
	drainCmd(t, m, cmd, 20)
	if len(h.streamDone) != 1 || h.streamDone[0].err == nil {
		t.Fatalf("OnStreamDone = %+v, want one call with a non-nil error", h.streamDone)
	}
}

func TestStreamObserverOnStreamDoneFiresEvenForASupersededStream(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	block := make(chan struct{})
	first := func(context.Context) iter.Seq2[ai.Event, error] {
		return func(yield func(ai.Event, error) bool) { <-block }
	}
	firstCmd := m.StartStream("turn-1", first)
	// Superseding with a new StartStream cancels "turn-1" via cancelStream.
	secondCmd := m.StartStream("turn-2", openSeq(ai.Event{Type: ai.EventCompleted}))
	close(block)
	drainCmd(t, m, firstCmd, 20)
	drainCmd(t, m, secondCmd, 20)
	sawFirst, sawSecond := false, false
	for _, d := range h.streamDone {
		if d.id == "turn-1" {
			sawFirst = true
		}
		if d.id == "turn-2" {
			sawSecond = true
		}
	}
	if !sawFirst {
		t.Fatal("OnStreamDone not called for the superseded stream turn-1")
	}
	if !sawSecond {
		t.Fatal("OnStreamDone not called for the current stream turn-2")
	}
	// The superseded stream's belated Done must not touch the CURRENT
	// stream's busy state.
	if m.Busy() {
		t.Fatal("still busy after the current stream (turn-2) completed")
	}
}

func TestSetBusyGatesInputAndStartsSpinner(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	cmd := m.SetBusy(true)
	if cmd == nil {
		t.Fatal("SetBusy(true) should return a spinner-tick command")
	}
	if !m.Busy() {
		t.Fatal("Busy() should be true")
	}
	m.input.SetValue("")
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if m.input.Value() != "" {
		t.Fatalf("input accepted a keystroke while busy: %q", m.input.Value())
	}
	if cmd := m.SetBusy(false); cmd != nil {
		t.Fatal("SetBusy(false) should not return a command")
	}
	if m.Busy() {
		t.Fatal("Busy() should be false")
	}
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if m.input.Value() != "x" {
		t.Fatalf("input did not accept a keystroke once idle: %q", m.input.Value())
	}
}

func TestF6HidingFocusedSidebarReturnsFocus(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift}) // focus sidebar
	if m.focusRing.Zone() != focus.ZoneSidebar {
		t.Fatalf("zone = %v, want sidebar", m.focusRing.Zone())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	if m.sidebar.Visible() {
		t.Fatal("sidebar should be hidden after F6")
	}
	if m.focusRing.Zone() == focus.ZoneSidebar {
		t.Fatal("focus should have moved off the hidden sidebar")
	}
}

func TestEscReachesCapturingBlockBeforeGlobalEsc(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	blk := &fakeBlock{captures: true}
	m.AppendBlock(blk)
	m.focusRing.FocusStop(0)
	m.syncFocus()
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if blk.updates != 1 {
		t.Fatalf("capturing block did not receive Esc: updates=%d", blk.updates)
	}
	if m.focusRing.Zone() != focus.ZoneTranscript {
		t.Fatalf("zone = %v, want transcript (Esc should not have returned to input)", m.focusRing.Zone())
	}
}

func TestEscReturnsToInputWhenBlockDoesNotCaptureIt(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	blk := &fakeBlock{captures: false}
	m.AppendBlock(blk)
	m.focusRing.FocusStop(0)
	m.syncFocus()
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if blk.updates != 0 {
		t.Fatal("non-capturing block should not receive Esc")
	}
	if m.focusRing.Zone() != focus.ZoneInput {
		t.Fatalf("zone = %v, want input", m.focusRing.Zone())
	}
}

func TestShiftLeftInTranscriptFallsThroughInsteadOfSwallowed(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	blk := &fakeBlock{}
	m.AppendBlock(blk)
	m.focusRing.FocusStop(0)
	m.syncFocus()
	m.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift})
	if blk.updates != 1 {
		t.Fatalf("Shift+Left was swallowed instead of reaching the focused block: updates=%d", blk.updates)
	}
	if m.focusRing.Zone() != focus.ZoneTranscript {
		t.Fatalf("zone changed to %v; Shift+Left with nothing to return to should not move focus", m.focusRing.Zone())
	}
}

func TestSelectionRefsIsTranscriptNotSidebar(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.PinToSidebar(session.EntityRef{Type: "t", Keys: map[string]string{"id": "sidebar-1"}})
	if refs := m.SelectionRefs(); len(refs) != 0 {
		t.Fatalf("SelectionRefs should ignore sidebar pins: %v", refs)
	}
	if refs := m.SidebarRefs(); len(refs) != 1 {
		t.Fatalf("SidebarRefs = %v, want 1 pin", refs)
	}
}

func TestFocusedRefFromSidebarCursor(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	ref := session.EntityRef{Type: "t", Keys: map[string]string{"id": "1"}, Title: "One"}
	m.PinToSidebar(ref)
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift}) // -> sidebar
	if m.focusRing.Zone() != focus.ZoneSidebar {
		t.Fatalf("zone = %v, want sidebar", m.focusRing.Zone())
	}
	got := m.FocusedRef()
	if got == nil || !got.Same(ref) {
		t.Fatalf("FocusedRef() = %v, want %v", got, ref)
	}
}

func TestWithTitleSetsTopBar(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithTitle("datatug chat"))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if !strings.Contains(m.View().Content, "datatug chat") {
		t.Fatal("View() does not contain the configured title")
	}
}

// --- S2/S3/n6 fix-round-2 tests ---------------------------------------------

func TestSetBusyCancelIsCalledByEscAndRendersStoppedSynchronously(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	cancelled := false
	if cmd := m.SetBusy(true); cmd == nil {
		t.Fatal("SetBusy(true) should return a spinner-tick command")
	}
	m.SetBusyCancel(func() { cancelled = true })
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !cancelled {
		t.Fatal("Esc while a SetBusy(true) phase is active did not call SetBusyCancel")
	}
	if m.Busy() {
		t.Fatal("still busy after Esc cancelled the busy phase")
	}
	found := false
	for _, e := range m.transcript.Entries() {
		if strings.Contains(e.Text, "(stopped)") {
			found = true
		}
	}
	if !found {
		t.Fatal("Esc did not render (stopped) synchronously for a bare SetBusy phase")
	}
}

func TestCtrlCCancelsSetBusyPhaseTooWithoutAnActiveStream(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	cancelled := false
	m.SetBusy(true)
	m.SetBusyCancel(func() { cancelled = true })
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !cancelled || cmd != nil || m.quit {
		t.Fatalf("first Ctrl+C should cancel, not quit: cancelled=%v cmd=%v quit=%v", cancelled, cmd, m.quit)
	}
}

func TestSecondConsecutiveCtrlCAlwaysQuits(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.busy = true
	m.streamID = "turn-1"
	m.streamCancel = func() {}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd != nil {
		t.Fatal("first Ctrl+C should not quit")
	}
	// Still busy (the stream's cancellation is asynchronous); a second,
	// immediately-following Ctrl+C must quit regardless.
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil || !m.quit {
		t.Fatal("second consecutive Ctrl+C should always quit")
	}
}

func TestCtrlCArmingResetsOnAnyOtherKey(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.busy = true
	m.streamID = "turn-1"
	m.streamCancel = func() {}
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}) // arms (busy stays true: cancellation is async)
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})        // any other key disarms
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd != nil {
		t.Fatal("Ctrl+C after an intervening key should cancel again, not quit")
	}
}

func TestEscClosesSlashCommandMenuFirst(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.commands = []Command{{Name: "/help", Help: "Show help"}}
	m.input.SetValue("/he")
	if len(m.commandMenuMatches()) != 1 {
		t.Fatalf("matches = %v", m.commandMenuMatches())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if len(m.commandMenuMatches()) != 0 {
		t.Fatal("Esc did not close the slash-command menu")
	}
	// Esc must not have done anything else (e.g. moved focus): still input,
	// and the typed text is untouched.
	if m.focusRing.Zone() != focus.ZoneInput {
		t.Fatalf("zone = %v, want input", m.focusRing.Zone())
	}
	if m.input.Value() != "/he" {
		t.Fatalf("input = %q, want unchanged /he", m.input.Value())
	}
	// Editing the input re-shows the menu for the new value.
	m.input.SetValue("/hel")
	if len(m.commandMenuMatches()) != 1 {
		t.Fatal("menu should reappear once the input value changes")
	}
}

func TestEscOnEmptyInputStillReturnsFocusWhenNoMenu(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	blk := &fakeBlock{}
	m.AppendBlock(blk)
	m.focusRing.FocusStop(0)
	m.syncFocus()
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.focusRing.Zone() != focus.ZoneInput {
		t.Fatalf("zone = %v, want input (no menu open, no CapturesEsc block)", m.focusRing.Zone())
	}
}

// firstFromBatch runs cmd and, if it produces a tea.BatchMsg, returns the
// first EventMsg/DoneMsg found in it (chatshell's spinner tick and the
// stream's own first message race inside one Batch).
func firstFromBatch(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return msg
	}
	for _, sub := range batch {
		if sub == nil {
			continue
		}
		m := sub()
		switch m.(type) {
		case stream.EventMsg, stream.DoneMsg:
			return m
		}
	}
	t.Fatal("no stream message found in batch")
	return nil
}

// fakeSidePanel is a minimal SidePanel used to test that it fully replaces
// the default sidebar in the focus ring, split layout and key routing.
type fakeSidePanel struct {
	title     string
	updates   int
	lastFocus bool
	lastMsg   tea.Msg
}

func (p *fakeSidePanel) Title() string { return p.title }

func (p *fakeSidePanel) View(width, height int, focused bool) string {
	p.lastFocus = focused
	return "sidepanel"
}

func (p *fakeSidePanel) Update(msg tea.Msg) (SidePanel, tea.Cmd) {
	p.updates++
	p.lastMsg = msg
	return p, nil
}

func TestSidePanelReplacesSidebarInFocusRingAndSplit(t *testing.T) {
	h := &fakeHandler{}
	panel := &fakeSidePanel{title: "workspace"}
	m := New(h, WithSidePanel(panel))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	if !m.splitEnabled() {
		t.Fatal("expected split enabled with a SidePanel set and starting visible")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift})
	if m.focusRing.Zone() != focus.ZoneSidebar {
		t.Fatalf("Zone() = %v, want ZoneSidebar after Shift+Right", m.focusRing.Zone())
	}
	m.Update(tea.KeyPressMsg{Text: "x"})
	if panel.updates == 0 {
		t.Error("expected the SidePanel to receive key updates while focused")
	}

	view := m.View()
	_ = view // View() must not panic when a SidePanel is active.

	// F6 hides it; splitEnabled must follow the SidePanel's own visibility,
	// not the (unused, but still constructed) default sidebar's.
	m.focusRing.FocusInput()
	m.syncFocus()
	m.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	if m.splitEnabled() {
		t.Error("expected splitEnabled() false after F6 hides the SidePanel")
	}
}

func TestSidePanelCtrlLeftRightResizesSplit(t *testing.T) {
	h := &fakeHandler{}
	panel := &fakeSidePanel{}
	m := New(h, WithSidePanel(panel))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	before := m.panelChatPercent()
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModCtrl})
	if m.panelChatPercent() <= before {
		t.Errorf("panelChatPercent() = %d, want > %d after Ctrl+Right", m.panelChatPercent(), before)
	}
}

// fakeOverlay is a minimal Overlay that closes itself the first time it sees
// a KeyPressMsg with text "q", and otherwise just records what it saw.
type fakeOverlay struct {
	name     string
	seen     []tea.Msg
	closeKey string
}

func (o *fakeOverlay) View(width, height int) string { return "overlay:" + o.name }

func (o *fakeOverlay) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	o.seen = append(o.seen, msg)
	if k, ok := msg.(tea.KeyPressMsg); ok && k.Text == o.closeKey {
		return o, nil, true
	}
	return o, nil, false
}

func TestOverlayCapturesKeysUntilDone(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	ov := &fakeOverlay{name: "dialog", closeKey: "q"}
	m.PushOverlay(ov)

	// While the overlay is up, a key that would normally submit the composer
	// must be captured by the overlay instead.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(h.submitted) != 0 {
		t.Fatalf("submitted = %v, want none (overlay should have captured Enter)", h.submitted)
	}
	if len(ov.seen) != 1 {
		t.Fatalf("overlay saw %d messages, want 1", len(ov.seen))
	}

	m.Update(tea.KeyPressMsg{Text: "q"})
	if len(m.overlays) != 0 {
		t.Fatalf("overlays = %v, want empty after done", m.overlays)
	}

	// Now Enter reaches the composer again.
	m.input.SetValue("hi")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(h.submitted) != 1 {
		t.Fatalf("submitted = %v, want 1 after overlay closed", h.submitted)
	}
}

func TestOverlayStacking(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	first := &fakeOverlay{name: "first", closeKey: "1"}
	second := &fakeOverlay{name: "second", closeKey: "2"}
	m.PushOverlay(first)
	m.PushOverlay(second)

	// The TOP overlay (second) gets keys first.
	m.Update(tea.KeyPressMsg{Text: "x"})
	if len(second.seen) != 1 || len(first.seen) != 0 {
		t.Fatalf("second.seen=%d first.seen=%d, want top overlay only", len(second.seen), len(first.seen))
	}

	m.Update(tea.KeyPressMsg{Text: "2"})
	if len(m.overlays) != 1 {
		t.Fatalf("overlays = %d, want 1 after popping the top", len(m.overlays))
	}
	m.Update(tea.KeyPressMsg{Text: "1"})
	if len(m.overlays) != 0 {
		t.Fatalf("overlays = %d, want 0 after popping the last", len(m.overlays))
	}
}

func TestGlobalKeysCheckedBeforeShellDefaults(t *testing.T) {
	h := &fakeHandler{}
	var seen []string
	m := New(h, WithGlobalKeys(func(msg tea.KeyPressMsg) (tea.Cmd, bool) {
		if msg.Text == "f3" || msg.String() == "f3" {
			seen = append(seen, "f3")
			return nil, true
		}
		return nil, false
	}))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.Update(tea.KeyPressMsg{Code: tea.KeyF3})
	if len(seen) != 1 {
		t.Fatalf("global key hook fired %d times, want 1", len(seen))
	}

	// A key the hook doesn't consume still reaches the normal composer path.
	m.input.SetValue("hi")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(h.submitted) != 1 {
		t.Fatalf("submitted = %v, want the Enter that the hook did not consume to reach the composer", h.submitted)
	}
}

func TestReplaceBlockUpdatesInPlace(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	first := &fakeBlock{}
	m.AppendBlock(transcript.Entry{ID: "grid-1", Block: first}.Block)
	m.transcript.Append(transcript.Entry{ID: "grid-1", Block: first})
	second := &fakeBlock{}
	m.ReplaceBlock("grid-1", second)
	entries := m.transcript.Entries()
	found := false
	for _, e := range entries {
		if e.ID == "grid-1" {
			found = true
			if e.Block != transcript.Block(second) {
				t.Errorf("entry Block not replaced")
			}
		}
	}
	if !found {
		t.Fatal("entry grid-1 not found")
	}
}

func TestSetComposerTextSetsInputValue(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetComposerText("edit this")
	if m.input.Value() != "edit this" {
		t.Errorf("input.Value() = %q, want %q", m.input.Value(), "edit this")
	}
}

func TestClearTranscriptEmptiesAndFocusesInput(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.AppendUser("hi")
	m.AppendAssistant("hello")
	if len(m.transcript.Entries()) == 0 {
		t.Fatal("expected entries before Clear")
	}
	m.ClearTranscript()
	if len(m.transcript.Entries()) != 0 {
		t.Errorf("Entries() = %v, want empty after ClearTranscript", m.transcript.Entries())
	}
	if m.focusRing.Zone() != focus.ZoneInput {
		t.Errorf("Zone() = %v, want ZoneInput after ClearTranscript", m.focusRing.Zone())
	}
}

func TestWithTopBarAndStatusBarOverrideDefaults(t *testing.T) {
	h := &fakeHandler{}
	m := New(h,
		WithTopBar(func(width int) string { return "TOP" }),
		WithStatusBar(func(width int) string { return "STATUS" }),
	)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	view := m.View()
	content := view.Content
	if !strings.Contains(content, "TOP") {
		t.Error("expected product-rendered top bar in the view")
	}
	if !strings.Contains(content, "STATUS") {
		t.Error("expected product-rendered status bar in the view")
	}
}
