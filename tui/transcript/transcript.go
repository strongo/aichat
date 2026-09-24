// Package transcript renders the scrolling chat history shared by every
// aichat product: plain user/assistant messages, streamed assistant text and
// rich Block entries (e.g. a tui/grid result). It generalises DataTug chat's
// entries/history viewport (rebuildHistory / ensureBlockVisible in
// datatug-cli/pkg/chat/ui.go) behind a product-neutral Model.
package transcript

import (
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/strongo/aichat/ai/session"
)

// Role of a transcript entry.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// Block is a rich transcript entry that owns its own rendering and key
// handling, e.g. a tui/grid result. Update returns the (possibly new) Block
// value, matching the Bubble Tea value-model convention.
type Block interface {
	View(width int, focused bool) string
	Update(msg tea.Msg) (Block, tea.Cmd)
	Focusable() bool
}

// EntityBlock is a Block that can report the entity currently under the
// cursor, e.g. a grid's highlighted row. tui/chatshell uses it for "Add to
// sidebar" and for FocusedRef().
type EntityBlock interface {
	Block
	Current() *session.EntityRef
}

// Entry is one transcript item.
type Entry struct {
	// ID identifies a streaming assistant entry for AppendDelta. Products
	// that never stream can leave it empty.
	ID       string
	Role     Role
	Text     string
	Markdown bool
	// Block, when set, is rendered instead of Text and receives key events
	// while focused.
	Block Block
}

func (e Entry) focusable() bool {
	if e.Block != nil {
		return e.Block.Focusable()
	}
	return e.Role == RoleUser
}

// Model is the transcript viewport: an ordered list of Entry plus a
// bubbles/viewport rendering them, a focus index over focusable entries
// ("stops"), and the DataTug ensureBlockVisible scrolling behaviour.
type Model struct {
	viewport viewport.Model
	entries  []Entry
	width    int
	height   int
	// focusIndex is the focused stop (an index into the focusable subset of
	// entries, in transcript order), or -1 when nothing is focused.
	focusIndex int
}

// New returns an empty, unfocused transcript.
func New() *Model {
	return &Model{viewport: viewport.New(), focusIndex: -1}
}

// SetSize resizes the viewport and re-renders.
func (m *Model) SetSize(width, height int) {
	m.width, m.height = max(1, width), max(1, height)
	m.viewport.SetWidth(m.width)
	m.viewport.SetHeight(m.height)
	m.Rebuild(false)
}

// Entries returns the current entries (read-only use expected).
func (m *Model) Entries() []Entry { return m.entries }

// Append adds a new entry to the end of the transcript.
func (m *Model) Append(e Entry) {
	m.entries = append(m.entries, e)
	m.Rebuild(true)
}

// AppendDelta appends text to the streaming entry identified by id, creating
// it (as an assistant entry) on first use. It is the transcript half of
// tui/stream's channel re-arm pattern: each EventMsg's text delta lands here.
func (m *Model) AppendDelta(id, text string) {
	for i := range m.entries {
		if m.entries[i].ID != "" && m.entries[i].ID == id {
			m.entries[i].Text += text
			m.Rebuild(true)
			return
		}
	}
	m.entries = append(m.entries, Entry{ID: id, Role: RoleAssistant, Text: text})
	m.Rebuild(true)
}

// Stops returns the number of focusable entries.
func (m *Model) Stops() int {
	n := 0
	for _, e := range m.entries {
		if e.focusable() {
			n++
		}
	}
	return n
}

// entryIndexForStop maps a stop index to its entries index, or -1.
func (m *Model) entryIndexForStop(stop int) int {
	if stop < 0 {
		return -1
	}
	n := -1
	for i, e := range m.entries {
		if e.focusable() {
			n++
			if n == stop {
				return i
			}
		}
	}
	return -1
}

// Focus focuses transcript stop, or clears focus for a negative index.
func (m *Model) Focus(stop int) {
	m.focusIndex = stop
	m.Rebuild(false)
}

// Blur clears transcript focus.
func (m *Model) Blur() {
	m.focusIndex = -1
	m.Rebuild(false)
}

// FocusedEntry returns the currently focused entry, or nil.
func (m *Model) FocusedEntry() *Entry {
	i := m.entryIndexForStop(m.focusIndex)
	if i < 0 {
		return nil
	}
	return &m.entries[i]
}

// Current returns the entity ref under focus, when the focused entry's Block
// implements EntityBlock.
func (m *Model) Current() *session.EntityRef {
	e := m.FocusedEntry()
	if e == nil || e.Block == nil {
		return nil
	}
	if eb, ok := e.Block.(EntityBlock); ok {
		return eb.Current()
	}
	return nil
}

// Update routes msg to the focused entry's Block, when any, and re-renders.
func (m *Model) Update(msg tea.Msg) tea.Cmd {
	idx := m.entryIndexForStop(m.focusIndex)
	if idx < 0 || m.entries[idx].Block == nil {
		return nil
	}
	blk, cmd := m.entries[idx].Block.Update(msg)
	m.entries[idx].Block = blk
	m.Rebuild(false)
	return cmd
}

// ScrollUp/ScrollDown pass through to the viewport for plain scrolling
// (e.g. PgUp/PgDn on an unfocused transcript).
func (m *Model) ScrollUp(lines int)   { m.viewport.SetYOffset(max(0, m.viewport.YOffset()-lines)) }
func (m *Model) ScrollDown(lines int) { m.viewport.SetYOffset(m.viewport.YOffset() + lines) }

// Rebuild re-renders every entry into the viewport's content and, when an
// entry is focused, scrolls it into view (ensureBlockVisible). It mirrors
// DataTug's rebuildHistory/ensureBlockVisible in pkg/chat/ui.go, generalised
// away from DataTug-specific entry kinds.
func (m *Model) Rebuild(scrollToBottom bool) {
	width := max(1, m.width)
	blocks := make([]string, 0, len(m.entries))
	activeBlock := -1
	stop := -1
	for _, e := range m.entries {
		focused := false
		if e.focusable() {
			stop++
			focused = stop == m.focusIndex
			if focused {
				activeBlock = len(blocks)
			}
		}
		switch {
		case e.Block != nil:
			blocks = append(blocks, e.Block.View(width, focused))
		case e.Role == RoleUser:
			blocks = append(blocks, userCardView(e.Text, width, focused))
		default:
			blocks = append(blocks, plainMessageView(e.Role, e.Text, width))
		}
	}
	m.viewport.SetContent(strings.Join(blocks, "\n\n"))
	if scrollToBottom {
		m.viewport.GotoBottom()
	}
	if activeBlock >= 0 {
		m.ensureBlockVisible(blocks, activeBlock)
	}
}

// View renders the transcript viewport.
func (m *Model) View() string { return m.viewport.View() }

func userCardView(text string, width int, focused bool) string {
	bar := "│"
	style := lipgloss.NewStyle()
	if focused {
		style = style.Bold(true)
	}
	body := style.Width(max(1, width-2)).Render(text)
	return bar + " You: " + "\n" + body
}

func plainMessageView(role Role, text string, width int) string {
	return lipgloss.NewStyle().Width(max(1, width)).Render(string(role) + ": " + text)
}

// renderedLineCount is the number of on-screen lines block occupies once
// wrapped to width (a block may already contain hard line breaks).
func renderedLineCount(block string, width int) int {
	if block == "" {
		return 1
	}
	lines := strings.Split(block, "\n")
	total := 0
	for _, line := range lines {
		w := lipgloss.Width(line)
		if width <= 0 {
			total++
			continue
		}
		total += max(1, (w+width-1)/width)
	}
	return total
}

// ensureBlockVisible scrolls the viewport so blocks[blockIndex] (and a slice
// of the preceding block for context) is visible, without over-scrolling
// when it already fits. Ported from DataTug's ui.go ensureBlockVisible.
func (m *Model) ensureBlockVisible(blocks []string, blockIndex int) {
	width := max(1, m.viewport.Width())
	start := 0
	for i := 0; i < blockIndex; i++ {
		start += renderedLineCount(blocks[i], width) + 1
	}
	blockHeight := renderedLineCount(blocks[blockIndex], width)
	top := m.viewport.YOffset()
	height := max(1, m.viewport.Height())
	target := start
	if blockIndex > 0 {
		previousHeight := renderedLineCount(blocks[blockIndex-1], width)
		previousSpan := previousHeight + 1
		contextHeight := min(previousSpan, max(0, height-blockHeight))
		if contextHeight == 0 {
			contextHeight = min(previousSpan, max(2, height/3))
		}
		target = max(0, start-contextHeight)
	}
	if target < top || blockHeight >= height || start+blockHeight > top+height {
		m.viewport.SetYOffset(target)
	}
}
