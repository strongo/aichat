package gridblock

import (
	tea "charm.land/bubbletea/v2"

	"github.com/strongo/aichat/ai/session"
	"github.com/strongo/aichat/tui"
	"github.com/strongo/aichat/tui/transcript"
	"github.com/tuigoff/tuigoff/pkg/grid"
)

// Block adapts *grid.Model to transcript.Block, transcript.EntityBlock and
// transcript.SelfFramed. Model is embedded, so every other grid method
// (Title, CapturesEsc, SetSize, SetFocused, SelectRow, ...) is promoted
// unchanged, and View(width, focused) satisfies transcript.Block as is.
type Block struct{ *grid.Model }

var (
	_ transcript.Block       = (*Block)(nil)
	_ transcript.EntityBlock = (*Block)(nil)
	_ transcript.SelfFramed  = (*Block)(nil)
	_ transcript.Titled      = (*Block)(nil)
	_ transcript.EscCapturer = (*Block)(nil)
)

// Wrap returns m as a transcript block.
func Wrap(m *grid.Model) *Block { return &Block{Model: m} }

// Focusable reports that a grid block always takes a focus stop.
func (b *Block) Focusable() bool { return true }

// SelfFramed reports that the grid draws its own complete border, so
// transcript must not wrap it in a card fill.
func (b *Block) SelfFramed() bool { return true }

// Update forwards msg to the grid and returns the same Block (identity is
// kept). The grid's pin request is translated into tui.AddToSidebarMsg.
func (b *Block) Update(msg tea.Msg) (transcript.Block, tea.Cmd) {
	_, cmd := b.Model.Update(msg)
	return b, translate(cmd)
}

// Current returns the entity under the cursor, or nil.
func (b *Block) Current() *session.EntityRef { return EntityRef(b.Model) }

// EntityRef returns the session.EntityRef stored (as pointer or value) in the
// Ref of m's highlighted row, or nil when there is none.
func EntityRef(m *grid.Model) *session.EntityRef { return refOf(m.Current()) }

func refOf(ref any) *session.EntityRef {
	switch r := ref.(type) {
	case *session.EntityRef:
		return r
	case session.EntityRef:
		return &r
	}
	return nil
}

// translate rewrites grid.PinRowMsg produced by cmd into tui.AddToSidebarMsg,
// recursing into tea.BatchMsg.
func translate(cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		switch msg := cmd().(type) {
		case grid.PinRowMsg:
			if ref := refOf(msg.Ref); ref != nil {
				return tui.AddToSidebarMsg{Ref: *ref}
			}
			return nil
		case tea.BatchMsg:
			out := make(tea.BatchMsg, len(msg))
			for i, c := range msg {
				out[i] = translate(c)
			}
			return out
		default:
			return msg
		}
	}
}
