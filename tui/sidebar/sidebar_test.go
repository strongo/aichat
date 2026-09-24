package sidebar

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/strongo/aichat/ai/session"
)

func ref(id string) session.EntityRef {
	return session.EntityRef{Type: "happening", Keys: map[string]string{"id": id}, Title: "Item " + id}
}

func TestAddDedupesAndRemove(t *testing.T) {
	m := New(nil)
	if !m.Add(ref("1")) {
		t.Fatal("first Add should report change")
	}
	if m.Add(ref("1")) {
		t.Fatal("duplicate Add should report no change")
	}
	if len(m.Refs()) != 1 {
		t.Fatalf("refs = %v", m.Refs())
	}
	if !m.Remove(ref("1")) {
		t.Fatal("Remove should report change")
	}
	if m.Remove(ref("1")) {
		t.Fatal("Remove of absent ref should report no change")
	}
}

func TestCursorMovement(t *testing.T) {
	m := New(nil)
	m.Add(ref("1"))
	m.Add(ref("2"))
	m.Add(ref("3"))
	m.Update(tea.KeyPressMsg{Text: "j", Code: 'j'})
	if m.Cursor() != 1 {
		t.Fatalf("cursor = %d, want 1", m.Cursor())
	}
	m.Update(tea.KeyPressMsg{Text: "k", Code: 'k'})
	if m.Cursor() != 0 {
		t.Fatalf("cursor = %d, want 0", m.Cursor())
	}
	// Can't move up past 0 or down past last.
	m.Update(tea.KeyPressMsg{Text: "k", Code: 'k'})
	if m.Cursor() != 0 {
		t.Fatalf("cursor = %d, want 0", m.Cursor())
	}
}

func TestUpdateXEmitsRemoveMsg(t *testing.T) {
	m := New(nil)
	m.Add(ref("1"))
	m.Add(ref("2"))
	cmd := m.Update(tea.KeyPressMsg{Text: "x", Code: 'x'})
	if cmd == nil {
		t.Fatal("x did not return a command")
	}
	msg, ok := cmd().(RemoveMsg)
	if !ok || !msg.Ref.Same(ref("1")) {
		t.Fatalf("msg = %#v, want RemoveMsg{1}", msg)
	}
	if len(m.Refs()) != 1 {
		t.Fatalf("refs after remove = %v", m.Refs())
	}
}

func TestUpdateEnterEmitsOpenMsg(t *testing.T) {
	m := New(nil)
	m.Add(ref("1"))
	cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter did not return a command")
	}
	msg, ok := cmd().(OpenMsg)
	if !ok || !msg.Ref.Same(ref("1")) {
		t.Fatalf("msg = %#v, want OpenMsg{1}", msg)
	}
}

func TestToggleVisible(t *testing.T) {
	m := New(nil)
	if !m.Visible() {
		t.Fatal("should start visible")
	}
	m.Toggle()
	if m.Visible() {
		t.Fatal("should be hidden after Toggle")
	}
}

func TestChatPercentClamped(t *testing.T) {
	m := New(nil)
	for i := 0; i < 10; i++ {
		m.ShrinkChat(5)
	}
	if m.ChatPercent() != MinChatPercent {
		t.Fatalf("ChatPercent = %d, want %d", m.ChatPercent(), MinChatPercent)
	}
	for i := 0; i < 10; i++ {
		m.GrowChat(5)
	}
	if m.ChatPercent() != MaxChatPercent {
		t.Fatalf("ChatPercent = %d, want %d", m.ChatPercent(), MaxChatPercent)
	}
}

func TestViewRendersEntriesAndEmptyState(t *testing.T) {
	m := New(nil)
	if v := m.View(30, false); !strings.Contains(v, "(empty)") {
		t.Fatalf("empty view = %q", v)
	}
	m.Add(ref("1"))
	v := m.View(30, true)
	if !strings.Contains(v, "Item 1") {
		t.Fatalf("view missing entry: %q", v)
	}
}

func TestCursorReturnsMinusOneWhenEmpty(t *testing.T) {
	m := New(nil)
	if got := m.Cursor(); got != -1 {
		t.Fatalf("Cursor() on empty sidebar = %d, want -1", got)
	}
	m.Add(ref("1"))
	if got := m.Cursor(); got != 0 {
		t.Fatalf("Cursor() after Add = %d, want 0", got)
	}
}

func TestSetVisible(t *testing.T) {
	m := New(nil)
	if !m.Visible() {
		t.Fatal("should start visible")
	}
	m.SetVisible(false)
	if m.Visible() {
		t.Fatal("SetVisible(false) did not hide the sidebar")
	}
	m.SetVisible(true)
	if !m.Visible() {
		t.Fatal("SetVisible(true) did not show the sidebar")
	}
}

func TestUpdateIgnoresNonKeyMsg(t *testing.T) {
	m := New(nil)
	m.Add(ref("1"))
	cmd := m.Update(struct{}{})
	if cmd != nil {
		t.Fatal("Update on a non-key message should return nil")
	}
	if m.Cursor() != 0 {
		t.Fatalf("cursor moved on a non-key message: %d", m.Cursor())
	}
}

func TestCustomRenderer(t *testing.T) {
	m := New(func(r session.EntityRef, width int) string { return "custom:" + r.Title })
	m.Add(ref("1"))
	v := m.View(30, false)
	if !strings.Contains(v, "custom:Item 1") {
		t.Fatalf("view = %q", v)
	}
}
