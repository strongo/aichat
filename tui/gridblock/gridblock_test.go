package gridblock

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/strongo/aichat/ai/session"
	"github.com/strongo/aichat/tui"
	"github.com/strongo/aichat/tui/transcript"
	"github.com/strongo/strongo-tui/pkg/grid"
)

func newBlock(ref any) *Block {
	cols := []grid.Column{{Name: "name"}}
	rows := []grid.Row{{Values: []any{"alpha"}, Ref: ref}}
	return Wrap(grid.New(cols, rows, grid.WithTitle("results")))
}

func key(s string) tea.KeyPressMsg {
	r := []rune(s)
	return tea.KeyPressMsg{Code: r[0], Text: s}
}

func TestBlockContract(t *testing.T) {
	b := newBlock(nil)
	if !b.Focusable() || !b.SelfFramed() {
		t.Fatal("Focusable and SelfFramed must both be true")
	}
	if b.Title() != "results" {
		t.Errorf("Title = %q", b.Title())
	}
	if !strings.Contains(b.View(40, true), "alpha") {
		t.Error("View lacks row content")
	}
	got, _ := b.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if got != transcript.Block(b) {
		t.Error("Update must return the same Block")
	}
}

func TestCurrentPointerValueAndNone(t *testing.T) {
	ref := session.EntityRef{Type: "k", Title: "one"}
	if got := newBlock(&ref).Current(); got == nil || !got.Same(ref) || got.Title != ref.Title {
		t.Errorf("pointer ref: got %v", got)
	}
	if got := newBlock(ref).Current(); got == nil || !got.Same(ref) || got.Title != ref.Title {
		t.Errorf("value ref: got %v", got)
	}
	if got := newBlock(nil).Current(); got != nil {
		t.Errorf("no ref: got %v", got)
	}
	if got := newBlock("other").Current(); got != nil {
		t.Errorf("foreign ref type: got %v", got)
	}
	var nilPtr *session.EntityRef
	if got := EntityRef(newBlock(nilPtr).Model); got != nil {
		t.Errorf("typed nil: got %v", got)
	}
}

func TestPinTranslatedToAddToSidebar(t *testing.T) {
	ref := session.EntityRef{Type: "k", Title: "one"}
	_, cmd := newBlock(&ref).Update(key("+"))
	if cmd == nil {
		t.Fatal("expected a command")
	}
	msg := cmd()
	if got, ok := msg.(tui.AddToSidebarMsg); !ok || !got.Ref.Same(ref) {
		t.Fatalf("msg = %#v, want AddToSidebarMsg{%v}", msg, ref)
	}
}

func TestTranslate(t *testing.T) {
	if translate(nil) != nil {
		t.Error("nil cmd must stay nil")
	}
	other := translate(func() tea.Msg { return "x" })()
	if other != "x" {
		t.Errorf("passthrough = %v", other)
	}
	if got := translate(func() tea.Msg { return grid.PinRowMsg{Ref: 5} })(); got != nil {
		t.Errorf("unusable ref must drop the message, got %v", got)
	}
	ref := session.EntityRef{Type: "k", Title: "two"}
	batch := translate(func() tea.Msg {
		return tea.BatchMsg{
			nil,
			func() tea.Msg { return grid.PinRowMsg{Ref: ref} },
			func() tea.Msg { return "y" },
		}
	})()
	bm, ok := batch.(tea.BatchMsg)
	if !ok || len(bm) != 3 {
		t.Fatalf("batch = %#v", batch)
	}
	if bm[0] != nil {
		t.Error("nil entry must stay nil")
	}
	if got, ok := bm[1]().(tui.AddToSidebarMsg); !ok || !got.Ref.Same(ref) {
		t.Errorf("batch pin = %v", got)
	}
	if bm[2]() != "y" {
		t.Error("batch passthrough")
	}
}
