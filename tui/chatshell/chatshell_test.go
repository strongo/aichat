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
)

type fakeHandler struct {
	submitted    []string
	sidebarSeen  [][]session.EntityRef
	submitResult tea.Cmd
}

func (h *fakeHandler) Submit(text string) tea.Cmd {
	h.submitted = append(h.submitted, text)
	return h.submitResult
}

func (h *fakeHandler) OnSidebarChange(refs []session.EntityRef) {
	h.sidebarSeen = append(h.sidebarSeen, append([]session.EntityRef(nil), refs...))
}

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
	cmd := m.StartStream("turn-1", seqOf(events...))
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
	cmd := m.StartStream("turn-1", seqOf(events...))
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
	if len(m.SelectionRefs()) != 1 {
		t.Fatalf("sidebar refs = %v", m.SelectionRefs())
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
	if len(m.SelectionRefs()) != 0 {
		t.Fatalf("refs = %v", m.SelectionRefs())
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
