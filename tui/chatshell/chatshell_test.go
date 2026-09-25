package chatshell

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/session"
	"github.com/strongo/aichat/tui"
	"github.com/strongo/aichat/tui/focus"
	"github.com/strongo/aichat/tui/sidebar"
	"github.com/strongo/aichat/tui/stream"
	"github.com/strongo/aichat/tui/theme"
	"github.com/strongo/aichat/tui/transcript"
)

type fakeHandler struct {
	submitted    []string
	sidebarSeen  [][]session.EntityRef
	submitResult tea.Cmd
	streamEvents []ai.Event
	msgsSeen     []tea.Msg
	streamDone   []streamDoneCall
	// onMsgCmd/onStreamEventCmd, when set, are returned by OnMsg/OnStreamEvent
	// respectively — used to exercise dispatchUnhandled/handleStreamEvent's
	// "propagate the handler's own non-nil cmd" branches.
	onMsgCmd         tea.Cmd
	onStreamEventCmd tea.Cmd
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
	return h.onStreamEventCmd
}

func (h *fakeHandler) OnStreamDone(id string, err error) tea.Cmd {
	h.streamDone = append(h.streamDone, streamDoneCall{id: id, err: err})
	return nil
}

func (h *fakeHandler) OnMsg(msg tea.Msg) tea.Cmd {
	h.msgsSeen = append(h.msgsSeen, msg)
	return h.onMsgCmd
}

// fakeBlock is a minimal transcript.Block used to test focus/Esc/update
// routing without depending on tui/grid (owned by another concurrent lane).
type fakeBlock struct {
	updates   int
	captures  bool
	lastEvent tea.Msg
	// cmdToReturn, when set, is returned by Update on every call — used to
	// exercise dispatchUnhandled's "transcript.Update returned a non-nil
	// cmd" branch via transcript.broadcast.
	cmdToReturn tea.Cmd
}

func (b *fakeBlock) View(width int, focused bool) string { return "block" }

func (b *fakeBlock) Update(msg tea.Msg) (transcript.Block, tea.Cmd) {
	b.updates++
	b.lastEvent = msg
	return b, b.cmdToReturn
}

func (b *fakeBlock) Focusable() bool { return true }

func (b *fakeBlock) CapturesEsc() bool { return b.captures }

func newTestShell(handler Handler) *Model {
	m := New(handler)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return m
}

// TestInitRequestsBackgroundColorAndUpdateAppliesIt covers the r10
// founder ruling: "derive surface tints from the ACTUAL terminal
// background ... Chatshell applies the message; products do nothing." A
// product never sends tea.BackgroundColorMsg itself, but Init's own
// tea.RequestBackgroundColor should always be part of the batch it
// returns, and Update must apply whatever answer arrives via
// theme.SetTerminalBackground.
func TestInitRequestsBackgroundColorAndUpdateAppliesIt(t *testing.T) {
	h := &fakeHandler{}
	m := New(h)
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init() returned a nil Cmd")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init()'s Cmd produced %T, want a tea.BatchMsg", msg)
	}
	// tea.RequestBackgroundColor's own Msg type (backgroundColorMsg) is
	// unexported -- tea's runtime is what turns it into the real terminal
	// query and, later, the real tea.BackgroundColorMsg answer this test
	// applies below. The observable contract from here is just that
	// Init's batch carries a SECOND command alongside textarea.Blink (the
	// background request), which tea.RequestBackgroundColor itself IS
	// (see Init's own doc).
	if len(batch) < 2 {
		t.Fatalf("Init()'s batch has %d commands, want >= 2 (textarea.Blink + tea.RequestBackgroundColor)", len(batch))
	}

	prev := theme.Dark
	t.Cleanup(func() { theme.SetTerminalBackground(nil); theme.SetDark(prev) })
	m.Update(tea.BackgroundColorMsg{Color: lipgloss.Color("#1e222b")})
	if got := theme.TerminalBackground(); got != lipgloss.Color("#1e222b") {
		t.Fatalf("theme.TerminalBackground() = %#v after BackgroundColorMsg, want the reported colour", got)
	}
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

// TestWithSidebarTitleSetsHeaderRegardlessOfOptionOrder covers
// WithSidebarTitle directly, in BOTH option orders -- WithSidebarRenderer
// replaces m.sidebar wholesale, so it must preserve a title
// WithSidebarTitle already set (see WithSidebarRenderer's own doc).
func TestWithSidebarTitleSetsHeaderRegardlessOfOptionOrder(t *testing.T) {
	renderer := func(ref session.EntityRef, width int) string { return ref.Title }
	titleFirst := New(&fakeHandler{}, WithSidebarTitle("My Panel"), WithSidebarRenderer(renderer))
	if got := titleFirst.sidebar.Title(); got != "My Panel" {
		t.Fatalf("title-then-renderer: sidebar title = %q, want %q", got, "My Panel")
	}
	rendererFirst := New(&fakeHandler{}, WithSidebarRenderer(renderer), WithSidebarTitle("My Panel"))
	if got := rendererFirst.sidebar.Title(); got != "My Panel" {
		t.Fatalf("renderer-then-title: sidebar title = %q, want %q", got, "My Panel")
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

// cmdSidePanel is a minimal SidePanel whose Update returns a caller-set cmd,
// used to exercise dispatchUnhandled's SidePanel-cmd-propagation branch.
type cmdSidePanel struct {
	cmdToReturn tea.Cmd
}

func (p *cmdSidePanel) Title() string                               { return "cmd" }
func (p *cmdSidePanel) View(width, height int, focused bool) string { return "cmd" }
func (p *cmdSidePanel) Update(msg tea.Msg) (SidePanel, tea.Cmd)     { return p, p.cmdToReturn }

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

// TestClearTranscriptCallsSetBusyCancel covers r2's m6: a product's own
// SetBusy(true) phase (no stream, e.g. a decision chain) has no DoneMsg to
// cancel it asynchronously, so ClearTranscript must invoke the registered
// SetBusyCancel callback directly, same as Esc/Ctrl+C's cancelBusy does.
func TestClearTranscriptCallsSetBusyCancel(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	var canceled bool
	m.SetBusy(true)
	m.SetBusyCancel(func() { canceled = true })

	m.ClearTranscript()

	if !canceled {
		t.Error("ClearTranscript did not call the registered SetBusyCancel callback")
	}
	if m.Busy() {
		t.Error("still busy after ClearTranscript")
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

func TestWithTopBarProviderAndHintsProviderOverrideDefaults(t *testing.T) {
	h := &fakeHandler{}
	m := New(h,
		WithTopBarProvider(func(width int) (string, string, []theme.MenuItem) {
			return "TitleFromProvider", "CtxFromProvider", []theme.MenuItem{{Label: "Sessions", Active: true}}
		}),
		WithHintsProvider(func(width int) ([]theme.Hint, []string) {
			return []theme.Hint{{Key: "Enter", Label: "send"}}, []string{"seg-one"}
		}),
	)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	content := ansi.Strip(m.View().Content)
	for _, want := range []string{"TitleFromProvider", "CtxFromProvider", "Sessions", "Enter", "send", "seg-one"} {
		if !strings.Contains(content, want) {
			t.Errorf("expected view to contain %q, got:\n%s", want, content)
		}
	}
}

func TestTopBarProviderTakesPriorityOverLegacyWithTopBar(t *testing.T) {
	h := &fakeHandler{}
	m := New(h,
		WithTopBar(func(width int) string { return "LEGACY-TOP" }),
		WithTopBarProvider(func(width int) (string, string, []theme.MenuItem) {
			return "PROVIDER-TOP", "", nil
		}),
	)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	content := m.View().Content
	if strings.Contains(content, "LEGACY-TOP") {
		t.Error("legacy WithTopBar should be shadowed by WithTopBarProvider")
	}
	if !strings.Contains(content, "PROVIDER-TOP") {
		t.Error("expected WithTopBarProvider content in the view")
	}
}

func TestHintsProviderTakesPriorityOverLegacyWithStatusBar(t *testing.T) {
	h := &fakeHandler{}
	m := New(h,
		WithStatusBar(func(width int) string { return "LEGACY-STATUS" }),
		WithHintsProvider(func(width int) ([]theme.Hint, []string) {
			return []theme.Hint{{Key: "K", Label: "provider-hint"}}, nil
		}),
	)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	content := ansi.Strip(m.View().Content)
	if strings.Contains(content, "LEGACY-STATUS") {
		t.Error("legacy WithStatusBar should be shadowed by WithHintsProvider")
	}
	if !strings.Contains(content, "provider-hint") {
		t.Error("expected WithHintsProvider content in the view")
	}
}

func TestDefaultTopBarAndStatusBarRenderThroughTheme(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithTitle("PlainTitle"))
	m.SetStatus("plain status")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	content := ansi.Strip(m.View().Content)
	if !strings.Contains(content, "PlainTitle") {
		t.Error("expected default title in the view")
	}
	if !strings.Contains(content, "plain status") {
		t.Error("expected default status text in the view")
	}
}

// --- r1 review fixes ------------------------------------------------------

func TestOverlayOnlyCapturesInputStreamStillCompletesAndClearsBusy(t *testing.T) {
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

	ov := &fakeOverlay{name: "dialog", closeKey: "q"}
	m.PushOverlay(ov)

	drainCmd(t, m, cmd, 20)

	if m.Busy() {
		t.Fatal("stream must still complete (and clear busy) while an overlay is open — only key/paste/mouse route to the overlay")
	}
	entries := m.transcript.Entries()
	if len(entries) != 1 || entries[0].Text != "Hello" {
		t.Fatalf("entries = %+v, want the stream's text to have rendered despite the open overlay", entries)
	}
	if len(ov.seen) != 0 {
		t.Fatalf("overlay saw %d non-input messages, want 0", len(ov.seen))
	}
}

func TestSidePanelReceivesWindowSizeAndUnhandledMsgs(t *testing.T) {
	h := &fakeHandler{}
	panel := &fakeSidePanel{}
	m := New(h, WithSidePanel(panel))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if panel.updates == 0 {
		t.Fatal("expected the SidePanel to receive the WindowSizeMsg")
	}

	type productMsg struct{}
	before := panel.updates
	m.Update(productMsg{})
	if panel.updates <= before {
		t.Fatal("expected the SidePanel to receive an otherwise-unhandled message")
	}
}

func TestClearTranscriptCancelsStreamAndIgnoresStaleEvents(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.ctx = context.Background()
	m.AppendUser("hi")

	ch := make(chan ai.Event)
	seq := func(yield func(ai.Event, error) bool) {
		for ev := range ch {
			if !yield(ev, nil) {
				return
			}
		}
	}
	cmd := m.StartStream("turn-1", func(ctx context.Context) iter.Seq2[ai.Event, error] { return seq })
	_ = cmd
	if !m.Busy() {
		t.Fatal("expected StartStream to set busy")
	}

	m.ClearTranscript()
	if m.Busy() {
		t.Fatal("ClearTranscript must cancel the active stream and clear busy")
	}
	if len(m.transcript.Entries()) != 0 {
		t.Fatalf("transcript not cleared: %+v", m.transcript.Entries())
	}

	// A stale EventMsg for the cancelled stream's ID must not resurrect a
	// transcript entry.
	m.Update(stream.EventMsg{ID: "turn-1", Event: ai.Event{Type: ai.EventTextDelta, Text: "stale"}, Next: func() tea.Msg { return nil }})
	if len(m.transcript.Entries()) != 0 {
		t.Fatalf("stale event resurrected a transcript entry: %+v", m.transcript.Entries())
	}
	close(ch)
}

func TestReplaceBlockKeepsFocusOnSameEntryWhenFocusabilityChanges(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.AppendUser("first")                                                // stop 0
	m.transcript.Append(transcript.Entry{ID: "b1", Block: &fakeBlock{}}) // stop 1

	// Focus the block entry (last stop).
	m.focusRing.FocusStop(m.transcript.Stops() - 1)
	m.transcript.Focus(m.focusRing.Stop())
	before := m.transcript.FocusedEntry()
	if before == nil || before.ID != "b1" {
		t.Fatalf("before = %+v, want focused on b1", before)
	}

	m.ReplaceBlock("b1", &fakeBlock{})

	after := m.transcript.FocusedEntry()
	if after == nil || after.ID != "b1" {
		t.Fatalf("after = %+v, want still focused on b1 (same entry) after ReplaceBlock", after)
	}
}

// variableFocusBlock is a fakeBlock whose Focusable() answer can flip, used
// to reproduce the "an EARLIER entry's ReplaceBlock shifts a LATER entry's
// stop index" scenario.
type variableFocusBlock struct {
	fakeBlock
	focusable bool
}

func (b *variableFocusBlock) Focusable() bool { return b.focusable }

// TestReplaceBlockMovesFocusToNearestStopWhenFocusedEntryBecomesNonFocusable
// covers r3's m2: when a ReplaceBlock makes the CURRENTLY FOCUSED entry
// itself non-focusable, focus must move to the nearest remaining focusable
// stop, not just fall silently unfocused inside the transcript zone.
func TestReplaceBlockMovesFocusToNearestStopWhenFocusedEntryBecomesNonFocusable(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.transcript.Append(transcript.Entry{ID: "a", Block: &fakeBlock{}})                         // stop 0
	m.transcript.Append(transcript.Entry{ID: "b", Block: &variableFocusBlock{focusable: true}}) // stop 1
	m.transcript.Append(transcript.Entry{ID: "c", Block: &fakeBlock{}})                         // stop 2

	m.focusRing.FocusStop(1)
	m.syncFocus()
	if fe := m.transcript.FocusedEntry(); fe == nil || fe.ID != "b" {
		t.Fatalf("setup: focused entry = %+v, want b", fe)
	}

	m.ReplaceBlock("b", &variableFocusBlock{focusable: false})

	if m.focusRing.Zone() != focus.ZoneTranscript {
		t.Fatalf("Zone() = %v, want still ZoneTranscript (a focusable entry remains)", m.focusRing.Zone())
	}
	fe := m.transcript.FocusedEntry()
	if fe == nil {
		t.Fatal("no focused entry after ReplaceBlock made \"b\" non-focusable, want the nearest remaining stop")
	}
	if fe.ID != "a" && fe.ID != "c" {
		t.Errorf("focused entry = %+v, want the nearest remaining focusable entry (a or c)", fe)
	}
}

// TestReplaceBlockFallsBackToComposerWhenNoFocusableEntryRemains covers r3's
// m2's other branch: when the focused entry becomes non-focusable and NO
// other focusable entry remains, focus hands off to the composer.
func TestReplaceBlockFallsBackToComposerWhenNoFocusableEntryRemains(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.transcript.Append(transcript.Entry{ID: "only", Block: &variableFocusBlock{focusable: true}})

	m.focusRing.FocusStop(0)
	m.syncFocus()
	if fe := m.transcript.FocusedEntry(); fe == nil || fe.ID != "only" {
		t.Fatalf("setup: focused entry = %+v, want only", fe)
	}

	m.ReplaceBlock("only", &variableFocusBlock{focusable: false})

	if m.focusRing.Zone() != focus.ZoneInput {
		t.Errorf("Zone() = %v, want ZoneInput (composer) since no focusable entry remains", m.focusRing.Zone())
	}
	if fe := m.transcript.FocusedEntry(); fe != nil {
		t.Errorf("FocusedEntry() = %+v, want nil", fe)
	}
}

// TestReplaceBlockSyncsFocusRingStopNotJustTranscript covers r2's m5:
// ReplaceBlock must resync m.focusRing's own stop, not just
// transcript.Model's internal focusIndex — otherwise the next syncFocus
// call (e.g. a WindowSizeMsg) reapplies the OLD, stale focusRing.Stop()
// value into the transcript, silently undoing the fix
// TestReplaceBlockKeepsFocusOnSameEntryWhenFocusabilityChanges checks for.
func TestReplaceBlockSyncsFocusRingStopNotJustTranscript(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	// "a" starts non-focusable, so "b1" is the only stop (index 0).
	m.transcript.Append(transcript.Entry{ID: "a", Block: &variableFocusBlock{focusable: false}})
	m.transcript.Append(transcript.Entry{ID: "b1", Block: &fakeBlock{}})

	m.focusRing.FocusStop(0)
	m.syncFocus()
	before := m.transcript.FocusedEntry()
	if before == nil || before.ID != "b1" {
		t.Fatalf("before = %+v, want focused on b1", before)
	}

	// Replacing "a" with a NOW-focusable block shifts "b1" from stop 0 to
	// stop 1 — the transcript-level fix follows the focused entry by id, but
	// chatshell's own focusRing.Stop() must be updated too.
	m.ReplaceBlock("a", &variableFocusBlock{focusable: true})

	// Simulate an unrelated later event that re-applies focusRing.Stop()
	// into the transcript, the way syncFocus normally runs on a zone change
	// or resize.
	m.syncFocus()

	after := m.transcript.FocusedEntry()
	if after == nil || after.ID != "b1" {
		t.Fatalf("after re-sync = %+v, want still focused on b1 — focusRing.Stop() must have been updated by ReplaceBlock, not left stale", after)
	}
}

func TestSidePanelPinnerRoutesPinUnpinAndSidebarRefs(t *testing.T) {
	h := &fakeHandler{}
	panel := &pinnerSidePanel{}
	m := New(h, WithSidePanel(panel))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	ref := session.EntityRef{Type: "row", Keys: map[string]string{"id": "1"}}
	m.PinToSidebar(ref)
	if len(m.SidebarRefs()) != 1 {
		t.Fatalf("SidebarRefs() = %v, want 1 ref routed through the SidePanelPinner", m.SidebarRefs())
	}
	if len(h.sidebarSeen) != 1 {
		t.Fatalf("handler not notified: %v", h.sidebarSeen)
	}
	m.UnpinFromSidebar(ref)
	if len(m.SidebarRefs()) != 0 {
		t.Fatalf("SidebarRefs() = %v, want empty after unpin", m.SidebarRefs())
	}
}

func TestSidePanelWithoutPinnerIsANoOp(t *testing.T) {
	h := &fakeHandler{}
	panel := &fakeSidePanel{} // does not implement SidePanelPinner
	m := New(h, WithSidePanel(panel))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	ref := session.EntityRef{Type: "row", Keys: map[string]string{"id": "1"}}
	m.PinToSidebar(ref)
	if got := m.SidebarRefs(); got != nil {
		t.Fatalf("SidebarRefs() = %v, want nil (documented no-op without SidePanelPinner)", got)
	}
	if len(h.sidebarSeen) != 0 {
		t.Fatalf("handler should not be notified for a no-op pin: %v", h.sidebarSeen)
	}
}

func TestOverlayClampedToGivenSize(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h) // width 120, height 40
	huge := &oversizedOverlay{}
	m.PushOverlay(huge)
	view := m.View()
	for _, line := range strings.Split(view.Content, "\n") {
		if w := lipgloss.Width(line); w > m.width {
			t.Fatalf("rendered line width %d exceeds screen width %d: %q", w, m.width, line)
		}
	}
	if lines := strings.Split(view.Content, "\n"); len(lines) > m.height+2 {
		t.Fatalf("rendered %d lines, want roughly bounded by screen height %d", len(lines), m.height)
	}
}

// pinnerSidePanel is a fakeSidePanel that also implements SidePanelPinner.
type pinnerSidePanel struct {
	fakeSidePanel
	refs []session.EntityRef
}

// Update overrides the promoted fakeSidePanel.Update, which would otherwise
// return the embedded *fakeSidePanel itself (losing the Pinner capability
// on the very first Update, since chatshell replaces m.sidePanel with
// whatever Update returns).
func (p *pinnerSidePanel) Update(msg tea.Msg) (SidePanel, tea.Cmd) {
	_, cmd := p.fakeSidePanel.Update(msg)
	return p, cmd
}

func (p *pinnerSidePanel) PinRef(ref session.EntityRef) bool {
	for _, r := range p.refs {
		if r.Same(ref) {
			return false
		}
	}
	p.refs = append(p.refs, ref)
	return true
}

func (p *pinnerSidePanel) UnpinRef(ref session.EntityRef) bool {
	for i, r := range p.refs {
		if r.Same(ref) {
			p.refs = append(p.refs[:i], p.refs[i+1:]...)
			return true
		}
	}
	return false
}

func (p *pinnerSidePanel) Refs() []session.EntityRef { return p.refs }

// oversizedOverlay renders far larger than any reasonable box, to exercise
// renderOverlay's clamp.
type oversizedOverlay struct{}

func (o *oversizedOverlay) View(width, height int) string {
	line := strings.Repeat("X", width+200)
	lines := make([]string, height+200)
	for i := range lines {
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

func (o *oversizedOverlay) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) { return o, nil, false }

func TestFocusEntryFocusesTranscriptStopByID(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.AppendUser("first")
	m.transcript.Append(transcript.Entry{ID: "grid-1", Block: &fakeBlock{}})

	if !m.FocusEntry("grid-1") {
		t.Fatal("FocusEntry(\"grid-1\") = false, want true")
	}
	if m.focusRing.Zone() != focus.ZoneTranscript {
		t.Fatalf("Zone() = %v, want ZoneTranscript", m.focusRing.Zone())
	}
	entry := m.transcript.FocusedEntry()
	if entry == nil || entry.ID != "grid-1" {
		t.Fatalf("FocusedEntry() = %+v, want grid-1", entry)
	}
}

func TestFocusEntryUnknownIDLeavesFocusUnchanged(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.AppendUser("first")
	before := m.focusRing.Zone()

	if m.FocusEntry("nope") {
		t.Fatal("FocusEntry(\"nope\") = true, want false for an unknown id")
	}
	if m.focusRing.Zone() != before {
		t.Fatalf("Zone() changed to %v after a failed FocusEntry", m.focusRing.Zone())
	}
}

func TestWithMarkdownRendererAndAppendAssistantMarkdown(t *testing.T) {
	h := &fakeHandler{}
	var gotText string
	var gotWidth int
	m := New(h, WithMarkdownRenderer(func(text string, width int) string {
		gotText, gotWidth = text, width
		return "RENDERED:" + text
	}))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.AppendAssistantMarkdown("# hi")

	entries := m.transcript.Entries()
	if len(entries) != 1 || !entries[0].Markdown || entries[0].Text != "# hi" {
		t.Fatalf("entries = %+v, want one Markdown entry with the raw text", entries)
	}
	view := m.transcript.View()
	if !strings.Contains(view, "RENDERED:# hi") {
		t.Fatalf("view = %q, want the configured renderer's output", view)
	}
	if gotText != "# hi" || gotWidth <= 0 {
		t.Errorf("renderer called with text=%q width=%d", gotText, gotWidth)
	}
}

func TestAppendBlockWithIDIsLaterFocusable(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.ctx = context.Background()
	if !m.AppendBlockWithID("grid-1", &fakeBlock{}) {
		t.Fatal("AppendBlockWithID(\"grid-1\", ...) = false, want true for a fresh id")
	}

	entries := m.transcript.Entries()
	if len(entries) != 1 || entries[0].ID != "grid-1" {
		t.Fatalf("entries = %+v, want one entry with ID %q", entries, "grid-1")
	}
	if !m.FocusEntry("grid-1") {
		t.Fatal("FocusEntry(\"grid-1\") = false, want true: AppendBlockWithID must make the block focusable by id")
	}
}

func TestAppendBlockWithIDRejectsDuplicateAndStreamIDClashes(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.ctx = context.Background()

	if !m.AppendBlockWithID("grid-1", &fakeBlock{}) {
		t.Fatal("first AppendBlockWithID(\"grid-1\", ...) = false, want true")
	}
	if m.AppendBlockWithID("grid-1", &fakeBlock{}) {
		t.Error("AppendBlockWithID(\"grid-1\", ...) = true on a duplicate id, want false (no second entry appended)")
	}
	if m.AppendBlockWithID("", &fakeBlock{}) {
		t.Error("AppendBlockWithID(\"\", ...) = true, want false (empty id rejected)")
	}

	events := []ai.Event{{Type: ai.EventStarted}}
	m.StartStream("turn-1", openSeq(events...))
	if m.AppendBlockWithID("turn-1", &fakeBlock{}) {
		t.Error("AppendBlockWithID(\"turn-1\", ...) = true while a stream owns that id, want false")
	}

	entries := m.transcript.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want exactly 2 (grid-1, and the streaming entry) after the rejected calls", entries)
	}
}

func TestStartStreamMarkdownRendersAccumulatedTextThroughRenderer(t *testing.T) {
	h := &fakeHandler{}
	var renderCalls int
	var lastText string
	m := New(h, WithMarkdownRenderer(func(text string, width int) string {
		renderCalls++
		lastText = text
		return "RENDERED:" + text
	}))
	m.ctx = context.Background()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	events := []ai.Event{
		{Type: ai.EventStarted},
		{Type: ai.EventTextDelta, Text: "# Hel"},
		{Type: ai.EventTextDelta, Text: "lo"},
		{Type: ai.EventCompleted},
	}
	cmd := m.StartStreamMarkdown("turn-1", openSeq(events...))
	drainCmd(t, m, cmd, 20)

	entries := m.transcript.Entries()
	if len(entries) != 1 || !entries[0].Markdown || entries[0].Text != "# Hello" {
		t.Fatalf("entries = %+v, want one Markdown entry with the accumulated text", entries)
	}
	view := m.transcript.View()
	if !strings.Contains(view, "RENDERED:# Hello") {
		t.Fatalf("view = %q, want the final accumulated text rendered through the configured renderer", view)
	}
	if renderCalls == 0 {
		t.Error("renderer was never called")
	}
	if lastText != "# Hello" {
		t.Errorf("last render call text = %q, want the fully accumulated text", lastText)
	}
}

// TestStartStreamMarkdownThrottlesReRenderAcrossRapidDeltas covers r2's m8:
// a burst of rapid deltas with no newline must not re-run the (potentially
// expensive) markdown renderer once per delta — only the throttle window's
// first hit, plus the guaranteed final render on completion. The
// accumulated Text is still complete even though intermediate renders were
// skipped.
func TestStartStreamMarkdownThrottlesReRenderAcrossRapidDeltas(t *testing.T) {
	h := &fakeHandler{}
	var renderCalls int
	m := New(h, WithMarkdownRenderer(func(text string, width int) string {
		renderCalls++
		return "RENDERED:" + text
	}))
	m.ctx = context.Background()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	// A fake clock that advances 1ms per call (far under
	// markdownRenderThrottle's 100ms window) makes the throttle
	// deterministic without depending on how fast the test itself runs —
	// real wall-clock deltas between rapid, back-to-back events are not
	// reliably sub-100ms in a loaded test environment.
	fakeNow := time.Now()
	m.nowFunc = func() time.Time {
		fakeNow = fakeNow.Add(time.Millisecond)
		return fakeNow
	}

	const n = 20
	events := make([]ai.Event, 0, n+2)
	events = append(events, ai.Event{Type: ai.EventStarted})
	for i := 0; i < n; i++ {
		events = append(events, ai.Event{Type: ai.EventTextDelta, Text: "x"})
	}
	events = append(events, ai.Event{Type: ai.EventCompleted})

	cmd := m.StartStreamMarkdown("turn-1", openSeq(events...))
	drainCmd(t, m, cmd, 4*n+20)

	entries := m.transcript.Entries()
	if len(entries) != 1 || entries[0].Text != strings.Repeat("x", n) {
		t.Fatalf("entries = %+v, want the fully accumulated %d-char text despite throttled rendering", entries, n)
	}
	// Rapid, newline-free deltas fired back-to-back land inside one 100ms
	// throttle window: at most the window's first hit plus the guaranteed
	// completion render, well under one render per delta.
	if renderCalls >= n {
		t.Errorf("renderCalls = %d for %d deltas, want throttled (well under one call per delta)", renderCalls, n)
	}
	view := m.transcript.View()
	if !strings.Contains(view, "RENDERED:"+strings.Repeat("x", n)) {
		t.Fatalf("view = %q, want the final render to reflect the complete accumulated text", view)
	}
}

// TestStartStreamMarkdownRendersOnNewlineBoundary covers r2's m8's other
// throttle trigger: a delta crossing a newline forces a re-render even
// inside the throttle window, not just the timer.
func TestStartStreamMarkdownRendersOnNewlineBoundary(t *testing.T) {
	h := &fakeHandler{}
	var renderTexts []string
	m := New(h, WithMarkdownRenderer(func(text string, width int) string {
		renderTexts = append(renderTexts, text)
		return "RENDERED:" + text
	}))
	m.ctx = context.Background()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	events := []ai.Event{
		{Type: ai.EventStarted},
		{Type: ai.EventTextDelta, Text: "line one\n"},
		{Type: ai.EventTextDelta, Text: "line two"},
		{Type: ai.EventCompleted},
	}
	cmd := m.StartStreamMarkdown("turn-1", openSeq(events...))
	drainCmd(t, m, cmd, 20)

	if len(renderTexts) < 2 {
		t.Fatalf("renderTexts = %v, want at least 2 renders: one triggered by the newline-crossing delta, one on completion", renderTexts)
	}
	var sawNewlineRender bool
	for _, txt := range renderTexts {
		if txt == "line one\n" {
			sawNewlineRender = true
		}
	}
	if !sawNewlineRender {
		t.Errorf("renderTexts = %v, want a render with exactly the text as of the newline-crossing delta", renderTexts)
	}
	if last := renderTexts[len(renderTexts)-1]; last != "line one\nline two" {
		t.Errorf("last render text = %q, want the full accumulated text on completion", last)
	}
}

// TestStartStreamMarkdownScheduledFollowUpTickRendersTrailingFragment is r3's
// m1 regression test: a delta that gets throttled out (no newline, inside
// the throttle window) must still render within ~markdownRenderThrottle even
// if NO further delta ever arrives (e.g. the stream stalls) -- via a
// scheduled tea.Tick follow-up, not only "wait for the next delta or
// completion". Drives the stream through a channel it controls (never
// closed, no completion) so the only way the trailing fragment renders is
// the scheduled tick firing.
func TestStartStreamMarkdownScheduledFollowUpTickRendersTrailingFragment(t *testing.T) {
	h := &fakeHandler{}
	var renderTexts []string
	m := New(h, WithMarkdownRenderer(func(text string, width int) string {
		renderTexts = append(renderTexts, text)
		return "RENDERED:" + text
	}))
	m.ctx = context.Background()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	fakeNow := time.Now()
	m.nowFunc = func() time.Time { return fakeNow }

	var tickDur time.Duration
	var tickFn func(time.Time) tea.Msg
	m.tickFunc = func(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
		tickDur, tickFn = d, fn
		return func() tea.Msg { return nil } // never auto-fires; the test invokes tickFn itself
	}

	// A manual, one-event-at-a-time driver (not drainCmd, which greedily
	// re-arms the pump and would block forever waiting on the next send
	// this test controls by hand): unwrap StartStreamMarkdown's initial
	// tea.Batch once, keep the stream.Start half, and thread stream.
	// EventMsg.Next by hand between sends.
	ch := make(chan ai.Event)
	seq := func(yield func(ai.Event, error) bool) {
		for ev := range ch {
			if !yield(ev, nil) {
				return
			}
		}
	}
	initial := m.StartStreamMarkdown("turn-1", func(ctx context.Context) iter.Seq2[ai.Event, error] { return seq })
	batch, ok := initial().(tea.BatchMsg)
	if !ok || len(batch) == 0 {
		t.Fatalf("initial cmd = %T, want a non-empty tea.BatchMsg", initial())
	}
	pump := batch[0] // stream.Start's re-armable half

	// First delta: lastMarkdownRender is zero-value, so time.Since is huge
	// -> renders immediately (establishing a recent lastMarkdownRender).
	ch <- ai.Event{Type: ai.EventTextDelta, Text: "Hel"}
	ev1, ok := pump().(stream.EventMsg)
	if !ok {
		t.Fatalf("pump() = %#v, want stream.EventMsg", ev1)
	}
	m.Update(ev1)
	if len(renderTexts) == 0 || renderTexts[len(renderTexts)-1] != "Hel" {
		t.Fatalf("renderTexts = %v, want the first delta rendered immediately", renderTexts)
	}
	rendersAfterFirst := len(renderTexts)

	// Second delta: no newline, and fakeNow hasn't advanced, so this one is
	// throttled out -- but it must schedule a follow-up tick.
	ch <- ai.Event{Type: ai.EventTextDelta, Text: "lo"}
	ev2, ok := ev1.Next().(stream.EventMsg)
	if !ok {
		t.Fatalf("ev1.Next() = %#v, want stream.EventMsg", ev2)
	}
	m.Update(ev2)
	if len(renderTexts) != rendersAfterFirst {
		t.Fatalf("renderTexts = %v, want no new render yet (this delta should have been throttled)", renderTexts)
	}
	if tickFn == nil {
		t.Fatal("no follow-up tick was scheduled for the throttled delta")
	}
	if tickDur != markdownRenderThrottle {
		t.Errorf("tick duration = %v, want markdownRenderThrottle (%v)", tickDur, markdownRenderThrottle)
	}

	// Fire the scheduled tick myself -- no further delta, no completion.
	fakeNow = fakeNow.Add(markdownRenderThrottle)
	msg := tickFn(fakeNow)
	m.Update(msg)

	if len(renderTexts) != rendersAfterFirst+1 {
		t.Fatalf("renderTexts = %v, want exactly one more render after the tick fired", renderTexts)
	}
	if last := renderTexts[len(renderTexts)-1]; last != "Hello" {
		t.Errorf("tick-triggered render text = %q, want the full accumulated \"Hello\"", last)
	}
	close(ch)
}

func TestStartStreamWithoutMarkdownRendererBehavesLikeStartStream(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.ctx = context.Background()
	events := []ai.Event{
		{Type: ai.EventStarted},
		{Type: ai.EventTextDelta, Text: "hi"},
		{Type: ai.EventCompleted},
	}
	cmd := m.StartStreamMarkdown("turn-1", openSeq(events...))
	drainCmd(t, m, cmd, 20)

	entries := m.transcript.Entries()
	if len(entries) != 1 || entries[0].Text != "hi" {
		t.Fatalf("entries = %+v, want the plain accumulated text", entries)
	}
}

// --- coverage-completion tests ---------------------------------------------

func TestFocusedRefFromTranscriptCurrent(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	blk := &fakeBlock{}
	m.AppendBlock(blk)
	m.focusRing.FocusStop(0)
	m.syncFocus()
	// fakeBlock does not implement transcript.Currenter (or whatever the
	// real accessor is), so Current() may be nil for a block-only entry;
	// what matters here is exercising the ZoneTranscript branch itself
	// (chatshell.go:474), whatever it returns.
	_ = m.FocusedRef()
	if m.focusRing.Zone() != focus.ZoneTranscript {
		t.Fatalf("zone = %v, want transcript", m.focusRing.Zone())
	}
}

func TestFocusedRefNilWhenSidePanelActive(t *testing.T) {
	h := &fakeHandler{}
	panel := &fakeSidePanel{}
	m := New(h, WithSidePanel(panel))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift}) // -> sidebar zone
	if m.focusRing.Zone() != focus.ZoneSidebar {
		t.Fatalf("zone = %v, want sidebar", m.focusRing.Zone())
	}
	if got := m.FocusedRef(); got != nil {
		t.Fatalf("FocusedRef() = %v, want nil while a SidePanel is active", got)
	}
}

func TestFocusedRefNilWhenSidebarCursorOutOfRange(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift}) // -> sidebar, no pins
	if m.focusRing.Zone() != focus.ZoneSidebar {
		t.Fatalf("zone = %v, want sidebar", m.focusRing.Zone())
	}
	if got := m.FocusedRef(); got != nil {
		t.Fatalf("FocusedRef() = %v, want nil for an empty sidebar (cursor out of range)", got)
	}
}

// entityBlock is a fakeBlock that also implements transcript.EntityBlock, so
// transcript.Current() (and thus FocusedRef/SelectionRefs) can return a real
// ref for it.
type entityBlock struct {
	fakeBlock
	ref session.EntityRef
}

func (b *entityBlock) Current() *session.EntityRef { return &b.ref }

func TestSelectionRefsReturnsCurrentTranscriptRef(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	ref := session.EntityRef{Type: "row", Keys: map[string]string{"id": "42"}, Title: "Row 42"}
	blk := &entityBlock{ref: ref}
	m.AppendBlock(blk)
	m.focusRing.FocusStop(0)
	m.syncFocus()

	refs := m.SelectionRefs()
	if len(refs) != 1 || !refs[0].Same(ref) {
		t.Fatalf("SelectionRefs() = %v, want [%v]", refs, ref)
	}

	// Also exercises FocusedRef's ZoneTranscript branch returning a real,
	// non-nil ref (chatshell.go:474).
	if got := m.FocusedRef(); got == nil || !got.Same(ref) {
		t.Fatalf("FocusedRef() = %v, want %v", got, ref)
	}
}

func TestUpdatePanelRoutesToDefaultSidebar(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	ref := session.EntityRef{Type: "t", Keys: map[string]string{"id": "1"}}
	m.PinToSidebar(ref)
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift}) // -> sidebar
	if m.focusRing.Zone() != focus.ZoneSidebar {
		t.Fatalf("zone = %v, want sidebar", m.focusRing.Zone())
	}
	// "x" on a sidebar entry removes it and returns a RemoveMsg cmd — proof
	// the key reached the default sidebar's own Update (updatePanel's
	// non-SidePanel branch), not just a no-op.
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if cmd == nil {
		t.Fatal("expected updatePanel to route the key to the default sidebar and return its cmd")
	}
	if len(m.sidebar.Refs()) != 0 {
		t.Fatalf("sidebar refs = %v, want empty after 'x' removed the only pin", m.sidebar.Refs())
	}
}

func TestIsOverlayInputMsgCoversEveryInputKind(t *testing.T) {
	inputs := []tea.Msg{
		tea.KeyPressMsg{Code: 'x'},
		tea.KeyReleaseMsg{},
		tea.PasteMsg{Content: "x"},
		tea.PasteStartMsg{},
		tea.PasteEndMsg{},
		tea.MouseClickMsg{},
		tea.MouseReleaseMsg{},
		tea.MouseWheelMsg{},
		tea.MouseMotionMsg{},
	}
	for _, in := range inputs {
		if !isOverlayInputMsg(in) {
			t.Errorf("isOverlayInputMsg(%T) = false, want true", in)
		}
	}
	if isOverlayInputMsg(struct{}{}) {
		t.Error("isOverlayInputMsg(struct{}{}) = true, want false")
	}
}

func TestOverlayCapturesMouseAndPasteMessages(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	ov := &fakeOverlay{name: "dialog", closeKey: "never"}
	m.PushOverlay(ov)
	for _, in := range []tea.Msg{
		tea.KeyReleaseMsg{},
		tea.PasteMsg{Content: "x"},
		tea.PasteStartMsg{},
		tea.PasteEndMsg{},
		tea.MouseClickMsg{},
		tea.MouseReleaseMsg{},
		tea.MouseWheelMsg{},
		tea.MouseMotionMsg{},
	} {
		m.Update(in)
	}
	if len(ov.seen) != 8 {
		t.Fatalf("overlay saw %d input messages, want 8: %+v", len(ov.seen), ov.seen)
	}
}

func TestDispatchUnhandledPropagatesTranscriptBlockCmd(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	ran := false
	blk := &fakeBlock{cmdToReturn: func() tea.Msg { ran = true; return nil }}
	m.AppendBlock(blk)

	type productMsg struct{}
	_, cmd := m.Update(productMsg{})
	if cmd == nil {
		t.Fatal("dispatchUnhandled should propagate a non-nil cmd from transcript.Update")
	}
	cmd()
	if !ran {
		t.Fatal("propagated cmd was not the block's own cmd")
	}
	if blk.updates != 1 {
		t.Fatalf("block updates = %d, want 1", blk.updates)
	}
}

func TestDispatchUnhandledPropagatesSidePanelCmd(t *testing.T) {
	h := &fakeHandler{}
	panel := &cmdSidePanel{}
	m := New(h, WithSidePanel(panel))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	type productMsg struct{}
	ran := false
	panel.cmdToReturn = func() tea.Msg { ran = true; return nil }
	_, cmd := m.Update(productMsg{})
	if cmd == nil {
		t.Fatal("dispatchUnhandled should propagate a non-nil cmd from the SidePanel's Update")
	}
	cmd()
	if !ran {
		t.Fatal("propagated cmd was not the SidePanel's own cmd")
	}
}

func TestDispatchUnhandledPropagatesMsgHandlerCmd(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	ran := false
	h.onMsgCmd = func() tea.Msg { ran = true; return nil }

	type productMsg struct{}
	_, cmd := m.Update(productMsg{})
	if cmd == nil {
		t.Fatal("dispatchUnhandled should propagate a non-nil cmd from MsgHandler.OnMsg")
	}
	cmd()
	if !ran {
		t.Fatal("propagated cmd was not the handler's own cmd")
	}
}

func TestHandleStreamEventPropagatesStreamObserverCmd(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	ran := false
	h.onStreamEventCmd = func() tea.Msg { ran = true; return nil }
	events := []ai.Event{
		{Type: ai.EventStarted},
		{Type: ai.EventCompleted},
	}
	cmd := m.StartStream("turn-1", openSeq(events...))
	drainCmd(t, m, cmd, 20)
	if !ran {
		t.Fatal("StreamObserver.OnStreamEvent's returned cmd was never run")
	}
}

func TestHandleMarkdownRenderTickIgnoresStaleTick(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.streamID = "current"
	m.streamMarkdown = true
	m.transcript.Append(transcript.Entry{ID: "current", Role: transcript.RoleAssistant, Text: "keep", Markdown: true})
	m.markdownTickPending = true
	sentinel := time.Now().Add(-time.Hour)
	m.lastMarkdownRender = sentinel

	// A tick for a DIFFERENT (superseded/stale) stream id must not trigger a
	// re-render (lastMarkdownRender stays untouched), even though
	// markdownTickPending is unconditionally cleared either way.
	m.handleMarkdownRenderTick(markdownRenderTickMsg{id: "stale"})
	if !m.lastMarkdownRender.Equal(sentinel) {
		t.Fatal("a stale tick must not re-render/update lastMarkdownRender for the current stream")
	}
	if m.markdownTickPending {
		t.Fatal("markdownTickPending should always be cleared once a tick fires")
	}

	// A tick that arrives after streamMarkdown flipped false (e.g. the
	// stream finished non-markdown in between) is also a no-op on the render.
	m.markdownTickPending = true
	m.streamMarkdown = false
	m.handleMarkdownRenderTick(markdownRenderTickMsg{id: "current"})
	if !m.lastMarkdownRender.Equal(sentinel) {
		t.Fatal("a tick for a non-markdown stream must not re-render/update lastMarkdownRender")
	}
	if m.markdownTickPending {
		t.Fatal("handleMarkdownRenderTick should still clear markdownTickPending even when it no-ops on the render")
	}
}

// TestStatusLinesAndStatusBarViewDefensiveEmptyBranches covers
// statusLines()'s and statusBarView()'s own empty-status fallbacks
// DIRECTLY: r12's statusBarVisible() gate means View() itself never
// reaches them any more (with nothing to show at all, View() skips
// calling statusBarView() entirely -- see statusBarVisible's own doc),
// but both methods stay defensively correct for any OTHER caller that
// might invoke them with an empty m.status.
func TestStatusLinesAndStatusBarViewDefensiveEmptyBranches(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	if lines := m.statusLines(); lines != nil {
		t.Fatalf("statusLines() with empty status = %v, want nil", lines)
	}
	if got := m.statusBarView(); got == "" {
		t.Fatal("statusBarView() with empty status returned empty string, want a padded blank line")
	}
}

func TestStatusLinesSplitsMultilineStatus(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.SetStatus("line one\nline two")
	lines := m.statusLines()
	if len(lines) != 2 || lines[0] != "line one" || lines[1] != "line two" {
		t.Fatalf("statusLines() = %v, want [\"line one\" \"line two\"]", lines)
	}
	view := m.View()
	if !strings.Contains(view.Content, "line one") || !strings.Contains(view.Content, "line two") {
		t.Fatalf("View() content missing status lines: %q", view.Content)
	}
}

func TestShiftUpFallsThroughToComposerWithNonEmptyInput(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.input.SetValue("hello")
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift})
	if m.focusRing.Zone() != focus.ZoneInput {
		t.Fatalf("zone = %v, want input (shift+up should fall through to the composer)", m.focusRing.Zone())
	}
}

func TestShiftUpMovesBetweenTranscriptStops(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.transcript.Append(transcript.Entry{ID: "a", Block: &fakeBlock{}})
	m.transcript.Append(transcript.Entry{ID: "b", Block: &fakeBlock{}})
	m.focusRing.FocusStop(1)
	m.syncFocus()
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift})
	if m.focusRing.Zone() != focus.ZoneTranscript || m.focusRing.Stop() != 0 {
		t.Fatalf("zone=%v stop=%d, want transcript stop 0 after shift+up", m.focusRing.Zone(), m.focusRing.Stop())
	}
}

func TestShiftDownMovesBetweenTranscriptStops(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.transcript.Append(transcript.Entry{ID: "a", Block: &fakeBlock{}})
	m.transcript.Append(transcript.Entry{ID: "b", Block: &fakeBlock{}})
	m.focusRing.FocusStop(0)
	m.syncFocus()
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModShift})
	if m.focusRing.Zone() != focus.ZoneTranscript || m.focusRing.Stop() != 1 {
		t.Fatalf("zone=%v stop=%d, want transcript stop 1 after shift+down", m.focusRing.Zone(), m.focusRing.Stop())
	}
}

func TestCommandMenuArrowNavigation(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.commands = []Command{
		{Name: "/help", Help: "Show help"},
		{Name: "/history", Help: "Show history"},
	}
	m.input.SetValue("/h")
	if len(m.commandMenuMatches()) != 2 {
		t.Fatalf("matches = %v, want 2", m.commandMenuMatches())
	}

	// "up" at index 0 stays at 0 (the guard branch).
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.commandMenuIndex != 0 {
		t.Fatalf("commandMenuIndex = %d, want 0 (up at top is a no-op)", m.commandMenuIndex)
	}

	// "down" moves forward.
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.commandMenuIndex != 1 {
		t.Fatalf("commandMenuIndex = %d, want 1 after down", m.commandMenuIndex)
	}

	// "down" at the last index stays put (the guard branch).
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.commandMenuIndex != 1 {
		t.Fatalf("commandMenuIndex = %d, want 1 (down at bottom is a no-op)", m.commandMenuIndex)
	}

	// "up" now moves back.
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.commandMenuIndex != 0 {
		t.Fatalf("commandMenuIndex = %d, want 0 after up", m.commandMenuIndex)
	}
}

func TestCommandMenuViewRendersSelectedAndUnselectedEntries(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.commands = []Command{
		{Name: "/help", Help: "Show help"},
		{Name: "/history", Help: "Show history"},
	}
	m.input.SetValue("/h")
	m.commandMenuIndex = 1

	view := m.commandMenuView()
	if !strings.Contains(view, "Commands · ↑↓ choose · Enter insert · Esc close") {
		t.Fatalf("commandMenuView() missing header: %q", view)
	}
	if !strings.Contains(view, "  /help  Show help") {
		t.Fatalf("commandMenuView() missing unselected entry prefix: %q", view)
	}
	if !strings.Contains(view, "› /history  Show history") {
		t.Fatalf("commandMenuView() missing selected entry prefix: %q", view)
	}

	// commandMenuView returns "" when there are no matches.
	m.input.SetValue("/nomatch")
	if got := m.commandMenuView(); got != "" {
		t.Fatalf("commandMenuView() = %q, want empty with no matches", got)
	}
}

func TestViewIncludesCommandMenuWhenOpen(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.commands = []Command{{Name: "/help", Help: "Show help"}}
	m.input.SetValue("/he")
	view := m.View()
	if !strings.Contains(view.Content, "Commands ·") {
		t.Fatalf("View() content missing the open command menu: %q", view.Content)
	}
}

func TestEnterWithNilHandlerAppendsUserOnlyAndDoesNotPanic(t *testing.T) {
	m := newTestShell(nil)
	m.input.SetValue("hi there")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	entries := m.transcript.Entries()
	if len(entries) != 1 || entries[0].Text != "hi there" {
		t.Fatalf("entries = %+v, want the user message appended even with a nil handler", entries)
	}
	if m.input.Value() != "" {
		t.Fatalf("input = %q, want cleared", m.input.Value())
	}
}

// TestReplaceBlockResyncsWhenNoPriorFocusButOneAppears covers the case
// where the transcript zone holds a stop index that resolves to nothing
// focusable BEFORE the swap (hadFocus false), but the swap itself makes that
// same stop resolve to a real entry — e.g. ReplaceBlock making the very
// entry focusRing.Stop() already pointed at newly focusable.
func TestReplaceBlockResyncsWhenNoPriorFocusButOneAppears(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.transcript.Append(transcript.Entry{ID: "a", Block: &variableFocusBlock{focusable: false}})
	m.focusRing.FocusStop(0) // zone=Transcript, stop=0, but nothing is focusable yet
	m.syncFocus()            // propagate stop=0 into transcript's own focusIndex
	if fe := m.transcript.FocusedEntry(); fe != nil {
		t.Fatalf("setup: FocusedEntry() = %+v, want nil (nothing focusable yet)", fe)
	}

	m.ReplaceBlock("a", &variableFocusBlock{focusable: true})

	if m.focusRing.Zone() != focus.ZoneTranscript {
		t.Fatalf("Zone() = %v, want ZoneTranscript", m.focusRing.Zone())
	}
	if stop := m.focusRing.Stop(); stop != 0 {
		t.Fatalf("focusRing.Stop() = %d, want 0", stop)
	}
	m.syncFocus()
	fe := m.transcript.FocusedEntry()
	if fe == nil || fe.ID != "a" {
		t.Fatalf("FocusedEntry() after resync = %+v, want entry a now that it's focusable", fe)
	}
}

func TestReplaceBlockNoFocusToLoseIsANoop(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.transcript.Append(transcript.Entry{ID: "a", Block: &variableFocusBlock{focusable: false}})
	// Focus the transcript zone directly even though nothing there is
	// focusable (FocusedEntry() is nil, so hadFocus stays false) — covers
	// the "!hadFocus" early return in ReplaceBlock (chatshell.go:554-556).
	m.focusRing.FocusStop(0)
	if fe := m.transcript.FocusedEntry(); fe != nil {
		t.Fatalf("setup: expected no focused entry, got %+v", fe)
	}

	m.ReplaceBlock("a", &variableFocusBlock{focusable: false})

	if fe := m.transcript.FocusedEntry(); fe != nil {
		t.Fatalf("FocusedEntry() = %+v, want still nil", fe)
	}
}

// TestReplaceBlockKeepsFocusOnNoIDEntryWhenEarlierEntryReplaced is a
// regression test for a real bug found while writing these tests:
// transcript.StopForID("") always returns -1 (transcript's own documented
// special case for an empty ID), and transcript.ReplaceBlock only
// re-resolves its own internal focus cursor by ID when the focused entry
// HAS one. So a focused entry with no ID (AppendUser/AppendAssistant/plain
// AppendBlock all leave ID empty — only AppendBlockWithID sets one) used to
// silently fall out of focus tracking whenever an EARLIER entry's
// ReplaceBlock shifted stop numbers, even though the focused entry's own
// Block never changed. Fixed in chatshell.go's ReplaceBlock by tracking the
// focused entry by its raw position in m.transcript.Entries() instead of by
// ID (see stopForRawIndex), and by pushing the recomputed stop straight into
// transcript via syncFocus() before returning — transcript's own focus
// cursor only re-resolves itself by ID, which is a no-op for a no-ID entry,
// so without that explicit resync the highlight would stay wherever
// transcript.ReplaceBlock last left it (lost, or on the wrong Block) instead
// of reflecting focusRing's corrected stop immediately.
func TestReplaceBlockKeepsFocusOnNoIDEntryWhenEarlierEntryReplaced(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	blk2 := &fakeBlock{}
	m.transcript.Append(transcript.Entry{ID: "a", Block: &variableFocusBlock{focusable: true}}) // stop 0
	m.transcript.Append(transcript.Entry{Block: blk2})                                          // no ID, stop 1

	m.focusRing.FocusStop(1)
	m.syncFocus()
	if fe := m.transcript.FocusedEntry(); fe == nil || fe.Block != transcript.Block(blk2) {
		t.Fatalf("setup: focused entry block = %+v, want blk2", fe)
	}

	// Replacing the EARLIER entry "a" (not the focused one) makes it
	// non-focusable, shifting blk2's own stop from 1 down to 0 — blk2 itself
	// is untouched and must keep focus.
	m.ReplaceBlock("a", &variableFocusBlock{focusable: false})

	if m.focusRing.Zone() != focus.ZoneTranscript {
		t.Fatalf("Zone() = %v, want still ZoneTranscript (blk2 is still focusable)", m.focusRing.Zone())
	}
	if stop := m.focusRing.Stop(); stop != 0 {
		t.Fatalf("focusRing.Stop() = %d, want 0 (blk2 is now the only focusable entry)", stop)
	}
	// transcript's own focus must already reflect the corrected stop right
	// after ReplaceBlock, with no further syncFocus() call needed.
	if fe := m.transcript.FocusedEntry(); fe == nil || fe.Block != transcript.Block(blk2) {
		t.Fatalf("FocusedEntry() immediately after ReplaceBlock = %+v, want still focused on blk2", fe)
	}
}

// TestReplaceBlockKeepsFocusOnNoIDEntryWithThreeEntries extends the above
// regression to a 3-entry transcript where the focused no-ID entry sits in
// the MIDDLE, and an EARLIER entry's swap shifts its stop down by one while
// a LATER entry stays untouched — exercising stopForRawIndex/syncFocus with
// entries on both sides of the focused one.
func TestReplaceBlockKeepsFocusOnNoIDEntryWithThreeEntries(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	blk2 := &fakeBlock{}
	m.transcript.Append(transcript.Entry{ID: "a", Block: &variableFocusBlock{focusable: true}}) // stop 0
	m.transcript.Append(transcript.Entry{Block: blk2})                                          // no ID, stop 1
	m.transcript.Append(transcript.Entry{ID: "c", Block: &fakeBlock{}})                         // stop 2

	m.focusRing.FocusStop(1)
	m.syncFocus()
	if fe := m.transcript.FocusedEntry(); fe == nil || fe.Block != transcript.Block(blk2) {
		t.Fatalf("setup: focused entry block = %+v, want blk2", fe)
	}

	// Replacing the EARLIER entry "a" shifts blk2's stop from 1 down to 0;
	// "c" (untouched, after blk2) stays focusable too.
	m.ReplaceBlock("a", &variableFocusBlock{focusable: false})

	if stop := m.focusRing.Stop(); stop != 0 {
		t.Fatalf("focusRing.Stop() = %d, want 0 (blk2 is now the first focusable entry)", stop)
	}
	if fe := m.transcript.FocusedEntry(); fe == nil || fe.Block != transcript.Block(blk2) {
		t.Fatalf("FocusedEntry() immediately after ReplaceBlock = %+v, want still focused on blk2 (not \"c\")", fe)
	}
}

func TestReplaceBlockClampsStaleOldStopAboveRange(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.transcript.Append(transcript.Entry{ID: "a", Block: &variableFocusBlock{focusable: true}}) // stop 0
	m.transcript.Append(transcript.Entry{ID: "b", Block: &fakeBlock{}})                         // stop 1
	m.transcript.Append(transcript.Entry{ID: "c", Block: &fakeBlock{}})                         // stop 2

	// Focus the LAST stop ("c") so oldStop (its StopForID) is the highest
	// index, then replace an EARLIER entry ("a") in a way that removes it
	// from the stop sequence entirely (Focusable() false) while ALSO making
	// the currently-focused "c" non-focusable via its own swap below is not
	// needed -- instead, directly force the clamp branch by replacing the
	// focused entry itself with a non-focusable block while a swap upstream
	// has already shrunk stop count: replace "a" first to shrink the stops,
	// then replace "c" (currently focused) with a non-focusable block so
	// oldStop (2) is clamped against the new, smaller stop count.
	m.ReplaceBlock("a", &variableFocusBlock{focusable: false}) // stops now: b=0, c=1
	m.focusRing.FocusStop(1)                                   // refocus "c" at its new stop index
	m.syncFocus()
	if fe := m.transcript.FocusedEntry(); fe == nil || fe.ID != "c" {
		t.Fatalf("setup: focused entry = %+v, want c", fe)
	}

	m.ReplaceBlock("c", &variableFocusBlock{focusable: false}) // stops now: b=0 only; oldStop was 1 >= n(1)

	fe := m.transcript.FocusedEntry()
	if fe == nil || fe.ID != "b" {
		t.Fatalf("FocusedEntry() = %+v, want clamped to the remaining stop (b)", fe)
	}
}

// --- Mouse support -----------------------------------------------------

func TestMouseMode_MouseTeaMode(t *testing.T) {
	if got := MouseOff.mouseTeaMode(); got != tea.MouseModeNone {
		t.Fatalf("MouseOff.mouseTeaMode() = %v, want MouseModeNone", got)
	}
	if got := MouseCellMotion.mouseTeaMode(); got != tea.MouseModeCellMotion {
		t.Fatalf("MouseCellMotion.mouseTeaMode() = %v, want MouseModeCellMotion", got)
	}
}

func TestMouse_DefaultIsOff(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	if m.MouseEnabled() {
		t.Fatal("MouseEnabled() = true, want false by default")
	}
	if view := m.View(); view.MouseMode != tea.MouseModeNone {
		t.Fatalf("View().MouseMode = %v, want MouseModeNone", view.MouseMode)
	}
}

func TestMouse_WithMouseOffStaysOff(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithMouse(MouseOff))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.MouseEnabled() {
		t.Fatal("MouseEnabled() = true, want false")
	}
	if view := m.View(); view.MouseMode != tea.MouseModeNone {
		t.Fatalf("View().MouseMode = %v, want MouseModeNone", view.MouseMode)
	}

	// m1 (r1 review): an explicit WithMouse(MouseOff) must not clobber the
	// mode a later SetMouseEnabled(true) restores -- it must still come
	// back as MouseCellMotion (New's default), never a silent no-op.
	m.SetMouseEnabled(true)
	if view := m.View(); view.MouseMode != tea.MouseModeCellMotion {
		t.Fatalf("View().MouseMode after SetMouseEnabled(true) = %v, want MouseModeCellMotion", view.MouseMode)
	}
}

func TestMouse_WithMouseCellMotionEnablesFromStart(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithMouse(MouseCellMotion))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if !m.MouseEnabled() {
		t.Fatal("MouseEnabled() = false, want true")
	}
	if view := m.View(); view.MouseMode != tea.MouseModeCellMotion {
		t.Fatalf("View().MouseMode = %v, want MouseModeCellMotion", view.MouseMode)
	}
}

func TestMouse_SetMouseEnabledToggle(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)

	// SetMouseEnabled(true) without ever calling WithMouse falls back to
	// MouseCellMotion (New's default mouseMode), not a silent no-op.
	m.SetMouseEnabled(true)
	if !m.MouseEnabled() {
		t.Fatal("MouseEnabled() = false after SetMouseEnabled(true)")
	}
	if view := m.View(); view.MouseMode != tea.MouseModeCellMotion {
		t.Fatalf("View().MouseMode = %v, want MouseModeCellMotion", view.MouseMode)
	}

	m.SetMouseEnabled(false)
	if m.MouseEnabled() {
		t.Fatal("MouseEnabled() = true after SetMouseEnabled(false)")
	}
	if view := m.View(); view.MouseMode != tea.MouseModeNone {
		t.Fatalf("View().MouseMode = %v, want MouseModeNone", view.MouseMode)
	}
}

func TestMouse_WheelScrollsTranscript(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithMouse(MouseCellMotion))
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 8})
	for i := 0; i < 100; i++ {
		m.AppendAssistant(fmt.Sprintf("line %d", i))
	}
	before := m.transcript.View()

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	afterUp := m.transcript.View()
	if afterUp == before {
		t.Fatal("wheel up did not change the transcript view (already at top with room to scroll up? check fixture)")
	}

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	afterDown := m.transcript.View()
	if afterDown != before {
		t.Fatalf("wheel down did not return to the original view:\nbefore=%q\nafterDown=%q", before, afterDown)
	}
}

func TestMouse_WheelIgnoredWhileOverlayOpen(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithMouse(MouseCellMotion))
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 8})
	for i := 0; i < 100; i++ {
		m.AppendAssistant(fmt.Sprintf("line %d", i))
	}
	before := m.transcript.View()
	m.PushOverlay(&fakeOverlay{})
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if got := m.transcript.View(); got != before {
		t.Fatal("wheel scrolled the transcript while an overlay was open")
	}
}

// M3 (r1 review): with a SidePanel installed and the pane split, a wheel
// event in the side panel's column routes to the SidePanel instead of
// scrolling the transcript, and every wheel event -- routed to the
// SidePanel or not -- is also always forwarded to an optional MsgHandler.

func TestMouse_WheelInSidePanelColumnRoutesToSidePanel(t *testing.T) {
	h := &fakeHandler{}
	panel := &fakeSidePanel{title: "workspace"}
	m := New(h, WithMouse(MouseCellMotion), WithSidePanel(panel))
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 20})
	for i := 0; i < 100; i++ {
		m.AppendAssistant(fmt.Sprintf("line %d", i))
	}
	beforeTranscript := m.transcript.View()

	sideX := m.chatWidth() + splitSeparatorWidth + 1
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: sideX})

	if panel.updates == 0 {
		t.Fatal("SidePanel.Update was not called for a wheel event in its column")
	}
	if _, ok := panel.lastMsg.(tea.MouseWheelMsg); !ok {
		t.Fatalf("SidePanel received %T, want tea.MouseWheelMsg", panel.lastMsg)
	}
	if got := m.transcript.View(); got != beforeTranscript {
		t.Fatal("transcript scrolled even though the wheel event was in the side panel's column")
	}
	if len(h.msgsSeen) == 0 {
		t.Fatal("MsgHandler.OnMsg was not called for the wheel event")
	}
}

func TestMouse_WheelInChatColumnStillScrollsWithSidePanelInstalled(t *testing.T) {
	h := &fakeHandler{}
	panel := &fakeSidePanel{title: "workspace"}
	m := New(h, WithMouse(MouseCellMotion), WithSidePanel(panel))
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 20})
	for i := 0; i < 100; i++ {
		m.AppendAssistant(fmt.Sprintf("line %d", i))
	}
	beforeTranscript := m.transcript.View()
	updatesBefore := panel.updates // WindowSizeMsg above already forwarded once

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 2})

	if got := m.transcript.View(); got == beforeTranscript {
		t.Fatal("transcript did not scroll for a wheel event in the chat column")
	}
	if panel.updates != updatesBefore {
		t.Fatal("SidePanel.Update was called for a wheel event in the chat column")
	}
	if len(h.msgsSeen) == 0 {
		t.Fatal("MsgHandler.OnMsg was not called")
	}
}

// cmdSidePanel2 is a minimal SidePanel whose Update returns a caller-supplied
// tea.Cmd, for exercising handleMouseWheel's batching of a non-nil SidePanel
// command (fakeSidePanel above always returns nil).
type cmdSidePanel2 struct {
	cmd tea.Cmd
}

func (p *cmdSidePanel2) Title() string                               { return "panel" }
func (p *cmdSidePanel2) View(width, height int, focused bool) string { return "panel" }
func (p *cmdSidePanel2) Update(msg tea.Msg) (SidePanel, tea.Cmd)     { return p, p.cmd }

func TestMouse_WheelBatchesSidePanelCmd(t *testing.T) {
	h := &fakeHandler{}
	ran := false
	panel := &cmdSidePanel2{cmd: func() tea.Msg { ran = true; return nil }}
	m := New(h, WithMouse(MouseCellMotion), WithSidePanel(panel))
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 20})

	sideX := m.chatWidth() + splitSeparatorWidth + 1
	_, cmd := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: sideX})
	if cmd == nil {
		t.Fatal("expected a non-nil batched cmd")
	}
	cmd() // drive the batch; the SidePanel's cmd must be among what runs
	if !ran {
		t.Fatal("SidePanel's returned cmd was not included in the batch")
	}
}

func TestMouse_WheelForwardedToMsgHandlerWithoutSidePanel(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithMouse(MouseCellMotion))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	for i := 0; i < 50; i++ {
		m.AppendAssistant(fmt.Sprintf("line %d", i))
	}
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if len(h.msgsSeen) != 1 {
		t.Fatalf("msgsSeen = %d, want 1", len(h.msgsSeen))
	}
	if _, ok := h.msgsSeen[0].(tea.MouseWheelMsg); !ok {
		t.Fatalf("msgsSeen[0] = %T, want tea.MouseWheelMsg", h.msgsSeen[0])
	}
}

// cmdMsgHandler is a Handler+MsgHandler whose OnMsg returns a
// caller-supplied tea.Cmd, for exercising handleMouseWheel's batching of a
// non-nil MsgHandler command (fakeHandler.OnMsg above always returns nil).
type cmdMsgHandler struct {
	cmd tea.Cmd
}

func (h *cmdMsgHandler) Submit(text string) tea.Cmd { return nil }
func (h *cmdMsgHandler) OnMsg(msg tea.Msg) tea.Cmd  { return h.cmd }

// r2 review minor: a wheel event over the BUILT-IN sidebar (no SidePanel
// installed) moves its cursor like Up/Down would, rather than scrolling the
// transcript -- the sidebar has no scroll offset of its own, it's a cursor
// list.
func TestMouse_WheelOverBuiltInSidebarMovesCursorNotTranscript(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithMouse(MouseCellMotion))
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 20})
	m.PinToSidebar(session.EntityRef{Type: "t", Keys: map[string]string{"id": "1"}})
	m.PinToSidebar(session.EntityRef{Type: "t", Keys: map[string]string{"id": "2"}})
	m.PinToSidebar(session.EntityRef{Type: "t", Keys: map[string]string{"id": "3"}})
	for i := 0; i < 100; i++ {
		m.AppendAssistant(fmt.Sprintf("line %d", i))
	}
	beforeTranscript := m.transcript.View()
	if got := m.sidebar.Cursor(); got != 0 {
		t.Fatalf("initial cursor = %d, want 0", got)
	}

	sideX := m.chatWidth() + splitSeparatorWidth + 1
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: sideX})
	if got := m.sidebar.Cursor(); got != 1 {
		t.Fatalf("cursor after wheel-down = %d, want 1", got)
	}
	if got := m.transcript.View(); got != beforeTranscript {
		t.Fatal("transcript scrolled from a wheel event over the sidebar")
	}

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: sideX})
	if got := m.sidebar.Cursor(); got != 0 {
		t.Fatalf("cursor after wheel-up = %d, want 0", got)
	}
}

// r2 review minor: a wheel event, wherever it routes, must still reach
// transcript Blocks via the normal broadcast path (restored -- it was
// dropped when the dedicated tea.MouseWheelMsg case in Update stopped
// falling through to dispatchUnhandled).
// r3 review minor 2: a wheel event in the CHAT column must NOT also
// broadcast to transcript Blocks -- only the direct viewport scroll fires
// there. Broadcasting there too would move a Block that handles the wheel
// itself (scrolling its own internal view) TWICE for one tick: once via its
// own Update from the broadcast, once via the outer viewport's
// ScrollUp/Down. See TestMouse_WheelInSideColumnReachesTranscriptBlocksViaBroadcast
// for the complementary side-column case, where broadcasting IS safe
// (nothing else moves the transcript there).
// wheelConsumerBlock is a transcript.Block that also implements
// transcript.WheelConsumer, for exercising the r4-review "focused Block
// gets first refusal" rule in the chat column.
type wheelConsumerBlock struct {
	fakeBlock
	consume bool
}

func (b *wheelConsumerBlock) ConsumesWheel(msg tea.MouseWheelMsg) bool { return b.consume }

// r4 review minor: a wheel event over the SIDE column (sidebar/SidePanel)
// must never reach the transcript at all -- not the viewport, not any
// Block -- the transcript has nothing to do with a wheel tick over the
// sidebar.
func TestMouse_WheelInSideColumnNeverReachesTranscript(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithMouse(MouseCellMotion))
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 20})
	block := &wheelConsumerBlock{consume: true}
	m.AppendBlock(block)
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift}) // focus the block
	beforeTranscript := m.transcript.View()

	sideX := m.chatWidth() + splitSeparatorWidth + 1
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: sideX})

	if block.updates != 0 {
		t.Fatalf("transcript Block received the wheel event over the side column (updates=%d), want 0", block.updates)
	}
	if got := m.transcript.View(); got != beforeTranscript {
		t.Fatal("transcript viewport moved for a wheel event over the side column")
	}
}

// r4 review minor: in the CHAT column, a focused Block implementing
// transcript.WheelConsumer that reports it consumes the event gets the
// message delivered to it INSTEAD OF chatshell scrolling the transcript
// viewport.
func TestMouse_WheelChatColumn_ConsumingFocusedBlockSkipsViewportScroll(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithMouse(MouseCellMotion))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	block := &wheelConsumerBlock{consume: true}
	m.AppendBlock(block)
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift}) // focus the block
	beforeTranscript := m.transcript.View()

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 2})

	if block.updates != 1 {
		t.Fatalf("block.updates = %d, want 1 (delivered once)", block.updates)
	}
	if _, ok := block.lastEvent.(tea.MouseWheelMsg); !ok {
		t.Fatalf("block.lastEvent = %T, want tea.MouseWheelMsg", block.lastEvent)
	}
	if got := m.transcript.View(); got != beforeTranscript {
		t.Fatal("transcript viewport also scrolled even though the focused Block consumed the wheel event")
	}
}

// r4 review minor: a focused Block that implements WheelConsumer but
// DECLINES the event (ConsumesWheel returns false) falls back to
// chatshell's own viewport scroll -- and the Block is never delivered the
// message at all (it declined, nothing to dispatch).
func TestMouse_WheelChatColumn_DecliningFocusedBlockFallsBackToViewportScroll(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithMouse(MouseCellMotion))
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 8})
	for i := 0; i < 100; i++ {
		m.AppendAssistant(fmt.Sprintf("line %d", i))
	}
	// Appended LAST (so it's near the bottom, where the viewport already
	// sits, leaving room to scroll UP into the 100 lines above it once
	// focused).
	block := &wheelConsumerBlock{consume: false}
	m.AppendBlock(block)
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift}) // focus the block (the only focusable stop)
	beforeTranscript := m.transcript.View()

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 2})

	if block.updates != 0 {
		t.Fatalf("block.updates = %d, want 0 (it declined, so it must not be dispatched to)", block.updates)
	}
	if got := m.transcript.View(); got == beforeTranscript {
		t.Fatal("transcript viewport did not scroll despite the focused Block declining the wheel event")
	}
}

// r4 review minor: a non-WheelConsumer focused Block (the common case)
// falls back to chatshell's own viewport scroll, same as no Block focused
// at all.
func TestMouse_WheelChatColumn_NonConsumerBlockFallsBackToViewportScroll(t *testing.T) {
	h := &fakeHandler{}
	m := New(h, WithMouse(MouseCellMotion))
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 8})
	for i := 0; i < 100; i++ {
		m.AppendAssistant(fmt.Sprintf("line %d", i))
	}
	block := &fakeBlock{} // appended LAST -- see the decline test above for why
	m.AppendBlock(block)
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift})
	beforeTranscript := m.transcript.View()

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 2})

	if block.updates != 0 {
		t.Fatalf("block.updates = %d, want 0 (fakeBlock does not implement WheelConsumer)", block.updates)
	}
	if got := m.transcript.View(); got == beforeTranscript {
		t.Fatal("transcript viewport did not scroll despite the focused Block not implementing WheelConsumer")
	}
}

func TestMouse_WheelBatchesMsgHandlerCmd(t *testing.T) {
	ran := false
	h := &cmdMsgHandler{cmd: func() tea.Msg { ran = true; return nil }}
	m := New(h, WithMouse(MouseCellMotion))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	_, cmd := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if cmd == nil {
		t.Fatal("expected a non-nil batched cmd")
	}
	cmd()
	if !ran {
		t.Fatal("MsgHandler's returned cmd was not included in the batch")
	}
}

// --- Zone / FocusedEntryID -------------------------------------------------

func TestZone_ReflectsFocusRing(t *testing.T) {
	h := &fakeHandler{}
	m := New(h)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	if got := m.Zone(); got != focus.ZoneInput {
		t.Fatalf("Zone() = %v, want ZoneInput", got)
	}

	m.AppendBlock(&fakeBlock{})
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift})
	if got := m.Zone(); got != focus.ZoneTranscript {
		t.Fatalf("Zone() = %v, want ZoneTranscript", got)
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift})
	if got := m.Zone(); got != focus.ZoneSidebar {
		t.Fatalf("Zone() = %v, want ZoneSidebar", got)
	}
}

func TestFocusedEntryID_ReportsFocusedTranscriptEntry(t *testing.T) {
	h := &fakeHandler{}
	m := New(h)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})

	if got := m.FocusedEntryID(); got != "" {
		t.Fatalf("FocusedEntryID() = %q, want \"\" before anything is focused", got)
	}

	if ok := m.AppendBlockWithID("blk-1", &fakeBlock{}); !ok {
		t.Fatal("AppendBlockWithID failed")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift})
	if got := m.FocusedEntryID(); got != "blk-1" {
		t.Fatalf("FocusedEntryID() = %q, want blk-1", got)
	}

	// Moving focus to the sidebar leaves the transcript unfocused again.
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift})
	if got := m.FocusedEntryID(); got != "" {
		t.Fatalf("FocusedEntryID() = %q, want \"\" once focus left the transcript", got)
	}
}

// --- PopOverlay / async-safe overlay pattern -------------------------------

func TestPopOverlay_ClosesTopOverlay(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.PushOverlay(&fakeOverlay{name: "a"})
	m.PushOverlay(&fakeOverlay{name: "b"})
	if len(m.overlays) != 2 {
		t.Fatalf("overlays = %d, want 2", len(m.overlays))
	}
	m.PopOverlay()
	if len(m.overlays) != 1 {
		t.Fatalf("overlays after PopOverlay = %d, want 1", len(m.overlays))
	}
	if name := m.overlays[0].(*fakeOverlay).name; name != "a" {
		t.Fatalf("remaining overlay = %q, want a", name)
	}
	m.PopOverlay()
	if len(m.overlays) != 0 {
		t.Fatalf("overlays after 2nd PopOverlay = %d, want 0", len(m.overlays))
	}
}

func TestPopOverlay_NoopWhenNoneOpen(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	if cmd := m.PopOverlay(); cmd != nil {
		t.Fatal("expected nil cmd popping an empty overlay stack")
	}
	if len(m.overlays) != 0 {
		t.Fatalf("overlays = %d, want 0", len(m.overlays))
	}
}

// asyncResultMsg is a stand-in for a product's own async submit-result
// message (e.g. a save-to-server response), used by
// TestAsyncOverlay_StaysOpenOnFailureClosesOnSuccess below.
type asyncResultMsg struct{ ok bool }

// asyncFakeOverlay is a minimal Overlay whose own Update never itself
// closes it (done is always false) -- exactly the async-safe pattern
// PopOverlay documents: the PRODUCT closes it later, once an async result
// message (routed through MsgHandler.OnMsg, per dispatchUnhandled -- never
// overlay input) tells it the submit succeeded.
type asyncFakeOverlay struct {
	lastErr string
}

func (o *asyncFakeOverlay) View(width, height int) string { return "overlay" }

func (o *asyncFakeOverlay) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	return o, nil, false
}

// asyncOverlayHandler is a Handler+MsgHandler that reacts to asyncResultMsg
// by either closing the overlay it holds (success) or recording an error on
// it while leaving it open (failure) -- the product-side half of the
// async-safe overlay pattern PopOverlay documents.
type asyncOverlayHandler struct {
	model   *Model
	overlay *asyncFakeOverlay
}

func (h *asyncOverlayHandler) Submit(text string) tea.Cmd { return nil }

func (h *asyncOverlayHandler) OnMsg(msg tea.Msg) tea.Cmd {
	res, ok := msg.(asyncResultMsg)
	if !ok {
		return nil
	}
	if res.ok {
		// r3 review MAJOR: CloseOverlay(h.overlay), not PopOverlay() --
		// PopOverlay closes whatever is CURRENTLY on top, which is wrong
		// once a second overlay might have been stacked on top of this one
		// before the async result arrived (see
		// TestCloseOverlay_ClosesCorrectOverlayEvenWhenAnotherIsStackedOnTop).
		h.model.CloseOverlay(h.overlay)
	} else {
		h.overlay.lastErr = "submit failed"
	}
	return nil
}

func TestAsyncOverlay_StaysOpenOnFailureClosesOnSuccess(t *testing.T) {
	overlay := &asyncFakeOverlay{}
	h := &asyncOverlayHandler{overlay: overlay}
	m := New(h)
	h.model = m
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.PushOverlay(overlay)
	if len(m.overlays) != 1 {
		t.Fatalf("overlays = %d, want 1", len(m.overlays))
	}

	// A failed async result is NOT overlay input (isOverlayInputMsg only
	// classifies key/paste/mouse messages), so it reaches OnMsg via
	// dispatchUnhandled's default routing even while the overlay is open,
	// and the product leaves the overlay open with an error recorded.
	m.Update(asyncResultMsg{ok: false})
	if len(m.overlays) != 1 {
		t.Fatal("overlay closed on a failed async submit, want it to stay open")
	}
	if overlay.lastErr != "submit failed" {
		t.Fatalf("overlay.lastErr = %q, want it updated in place", overlay.lastErr)
	}

	// A successful async result closes it via CloseOverlay.
	m.Update(asyncResultMsg{ok: true})
	if len(m.overlays) != 0 {
		t.Fatal("overlay still open after a successful async submit")
	}
}

// TestCloseOverlay_ClosesCorrectOverlayEvenWhenAnotherIsStackedOnTop is the
// r3 review MAJOR regression PopOverlay could not handle: dialog A's async
// submit is in flight, the user opens dialog B on top of it, and A's result
// lands -- CloseOverlay(A) must remove A specifically and leave B open,
// where PopOverlay() would have wrongly closed B (whatever is on top).
func TestCloseOverlay_ClosesCorrectOverlayEvenWhenAnotherIsStackedOnTop(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	a := &fakeOverlay{name: "a"}
	b := &fakeOverlay{name: "b"}
	m.PushOverlay(a)
	m.PushOverlay(b)
	if len(m.overlays) != 2 {
		t.Fatalf("overlays = %d, want 2", len(m.overlays))
	}

	if ok := m.CloseOverlay(a); !ok {
		t.Fatal("CloseOverlay(a) = false, want true")
	}
	if len(m.overlays) != 1 {
		t.Fatalf("overlays after CloseOverlay(a) = %d, want 1", len(m.overlays))
	}
	if got := m.overlays[0].(*fakeOverlay).name; got != "b" {
		t.Fatalf("remaining overlay = %q, want b (a was removed, not the top)", got)
	}
}

func TestCloseOverlay_NotFoundReturnsFalse(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.PushOverlay(&fakeOverlay{name: "a"})
	other := &fakeOverlay{name: "never pushed"}
	if ok := m.CloseOverlay(other); ok {
		t.Fatal("CloseOverlay of an overlay never pushed = true, want false")
	}
	if len(m.overlays) != 1 {
		t.Fatalf("overlays = %d, want 1 (untouched)", len(m.overlays))
	}
}

func TestCloseOverlay_EmptyStackReturnsFalse(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	if ok := m.CloseOverlay(&fakeOverlay{}); ok {
		t.Fatal("CloseOverlay on an empty stack = true, want false")
	}
}

// nonPointerOverlay is a VALUE-typed Overlay (never a *T), used to prove
// CloseOverlay reports false rather than panicking or matching by value
// equality -- see Overlay's doc: implementations MUST be pointer types.
type nonPointerOverlay struct{}

func (nonPointerOverlay) View(width, height int) string { return "" }
func (nonPointerOverlay) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	return nonPointerOverlay{}, nil, false
}

func TestCloseOverlay_NonPointerOverlayNeverMatches(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	m.PushOverlay(nonPointerOverlay{})
	if ok := m.CloseOverlay(nonPointerOverlay{}); ok {
		t.Fatal("CloseOverlay matched a non-pointer Overlay, want false")
	}
	if len(m.overlays) != 1 {
		t.Fatalf("overlays = %d, want 1 (untouched)", len(m.overlays))
	}
}

func TestCloseOverlay_NilPointerOverlayNeverMatches(t *testing.T) {
	h := &fakeHandler{}
	m := newTestShell(h)
	var nilOverlay *fakeOverlay
	if ok := m.CloseOverlay(nilOverlay); ok {
		t.Fatal("CloseOverlay matched a nil *fakeOverlay, want false")
	}
}
