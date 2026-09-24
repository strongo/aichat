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

// EscCapturer is an optional Block capability: when a focused block is in an
// input-like mode of its own (e.g. a grid's "/" filter box has focus), it
// returns true so Esc reaches the block (via Update) instead of chatshell's
// global "return focus to the composer" handling.
type EscCapturer interface {
	CapturesEsc() bool
}

// Targeted is an optional message capability: when a non-key message
// implements it, Model.Update forwards the message only to the entry whose
// ID matches TargetEntryID, instead of broadcasting it to every Block.
type Targeted interface {
	TargetEntryID() string
}

// MarkdownRenderer renders markdown text to terminal-safe output at width.
// Entries with Markdown set use it when the Model was built WithMarkdownRenderer;
// otherwise Markdown is inert and the entry renders as plain text.
type MarkdownRenderer func(text string, width int) string

// Entry is one transcript item.
type Entry struct {
	// ID identifies a streaming assistant entry for AppendDelta, and is the
	// target of Targeted messages. Products that never stream and never
	// target messages by entry can leave it empty.
	ID       string
	Role     Role
	Text     string
	Markdown bool
	// Block, when set, is rendered instead of Text and receives key events
	// while focused.
	Block Block

	// render caches this entry's last-rendered view so AppendDelta on one
	// streaming entry does not re-render the whole transcript; it is
	// invalidated on content change, width change or focus change.
	renderValid   bool
	renderWidth   int
	renderFocused bool
	renderOut     string
}

func (e Entry) focusable() bool {
	if e.Block != nil {
		return e.Block.Focusable()
	}
	return e.Role == RoleUser
}

// Option configures a Model at construction time.
type Option func(*Model)

// WithMarkdownRenderer sets the renderer used for entries with Markdown set.
func WithMarkdownRenderer(r MarkdownRenderer) Option {
	return func(m *Model) { m.markdownRenderer = r }
}

// SetMarkdownRenderer sets the renderer used for entries with Markdown set,
// after construction (New's caller may not own the Model's own construction
// call, e.g. tui/chatshell, which builds a *Model itself and exposes this
// via its own WithMarkdownRenderer Option).
func (m *Model) SetMarkdownRenderer(r MarkdownRenderer) { m.markdownRenderer = r }

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
	focusIndex       int
	markdownRenderer MarkdownRenderer
}

// New returns an empty, unfocused transcript.
func New(opts ...Option) *Model {
	m := &Model{viewport: viewport.New(), focusIndex: -1}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// SetSize resizes the viewport and re-renders.
func (m *Model) SetSize(width, height int) {
	m.width, m.height = max(1, width), max(1, height)
	m.viewport.SetWidth(m.width)
	m.viewport.SetHeight(m.height)
	m.Rebuild(m.shouldAutoFollow())
}

// Entries returns the current entries (read-only use expected).
func (m *Model) Entries() []Entry { return m.entries }

// Append adds a new entry to the end of the transcript. It scrolls to the
// bottom only when the transcript is unfocused or was already at the
// bottom, so focusing an earlier stop to read it is not disturbed by new
// content arriving.
func (m *Model) Append(e Entry) {
	m.entries = append(m.entries, e)
	m.Rebuild(m.shouldAutoFollow())
}

// ReplaceBlock replaces the Block of the entry identified by id in place
// (same position, same ID), e.g. to refresh or re-run a grid without
// disturbing surrounding transcript order or focus. It is a no-op if no
// entry has that ID.
func (m *Model) ReplaceBlock(id string, b Block) {
	for i := range m.entries {
		if m.entries[i].ID != "" && m.entries[i].ID == id {
			// A Block swap can change Focusable()'s answer for this entry,
			// which shifts what every stop index AFTER it maps to (Stops()/
			// entryIndexForStop count only focusable entries) -- and that
			// shift can move the CURRENTLY focused entry's stop even when
			// it isn't the one being replaced. So: remember which entry (by
			// ID, not raw index) holds focus before the swap, and re-resolve
			// its stop afterward, whether or not it was id itself.
			var focusedID string
			if fe := m.FocusedEntry(); fe != nil {
				focusedID = fe.ID
			}
			m.entries[i].Block = b
			m.entries[i].renderValid = false
			if focusedID != "" {
				m.focusIndex = m.StopForID(focusedID)
			}
			m.Rebuild(m.shouldAutoFollow())
			return
		}
	}
}

// stopForEntryIndex is entryIndexForStop's inverse: the stop index entries
// index entryIdx occupies, or -1 if that entry isn't focusable (or the
// index is out of range).
// Clear removes every entry and clears focus.
func (m *Model) Clear() {
	m.entries = nil
	m.focusIndex = -1
	m.Rebuild(false)
}

// AppendDelta appends text to the streaming entry identified by id, creating
// it (as an assistant entry) on first use. It is the transcript half of
// tui/stream's channel re-arm pattern: each EventMsg's text delta lands here.
// Only the streaming entry's cached render is invalidated; the rest of the
// transcript is reused as-is.
func (m *Model) AppendDelta(id, text string) {
	for i := range m.entries {
		if m.entries[i].ID != "" && m.entries[i].ID == id {
			m.entries[i].Text += text
			m.entries[i].renderValid = false
			m.Rebuild(m.shouldAutoFollow())
			return
		}
	}
	m.entries = append(m.entries, Entry{ID: id, Role: RoleAssistant, Text: text})
	m.Rebuild(m.shouldAutoFollow())
}

// AppendDeltaNoRender is AppendDelta's text-only half: it appends text to
// the entry identified by id (creating it, as an assistant entry, on first
// use, same as AppendDelta) but leaves its cached render untouched and does
// NOT Rebuild the viewport. It exists for a caller (chatshell's
// StartStreamMarkdown) that wants to throttle an expensive re-render (e.g.
// re-running a markdown renderer) to less than once per delta while still
// accumulating every delta's text immediately; pair it with
// InvalidateAndRebuild once per throttle window, and always at least once
// more when the stream completes.
func (m *Model) AppendDeltaNoRender(id, text string) {
	for i := range m.entries {
		if m.entries[i].ID != "" && m.entries[i].ID == id {
			m.entries[i].Text += text
			return
		}
	}
	m.entries = append(m.entries, Entry{ID: id, Role: RoleAssistant, Text: text})
}

// InvalidateAndRebuild forces the entry identified by id to re-render on the
// next Rebuild (which this also triggers), picking up whatever text
// AppendDeltaNoRender has accumulated since the last render. A no-op if no
// entry has that id.
func (m *Model) InvalidateAndRebuild(id string) {
	for i := range m.entries {
		if m.entries[i].ID != "" && m.entries[i].ID == id {
			m.entries[i].renderValid = false
			m.Rebuild(m.shouldAutoFollow())
			return
		}
	}
}

// shouldAutoFollow reports whether new content should scroll the viewport to
// the bottom: when nothing in the transcript is focused (focus is on the
// composer or sidebar) or the viewport was already scrolled to the bottom.
func (m *Model) shouldAutoFollow() bool {
	return m.focusIndex < 0 || m.viewport.AtBottom()
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

// StopForID returns the focus-ring stop index of the focusable entry
// identified by id, or -1 if there is no such entry, or it isn't focusable
// (e.g. DataTug's Ctrl+G "jump to latest grid").
func (m *Model) StopForID(id string) int {
	if id == "" {
		return -1
	}
	stop := -1
	for _, e := range m.entries {
		if !e.focusable() {
			continue
		}
		stop++
		if e.ID == id {
			return stop
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

// FocusedBlock returns the Block under focus, or nil.
func (m *Model) FocusedBlock() Block {
	e := m.FocusedEntry()
	if e == nil {
		return nil
	}
	return e.Block
}

// CapturesEsc reports whether the focused Block wants Esc routed to it
// (via Update) instead of chatshell's global "return to composer" handling.
func (m *Model) CapturesEsc() bool {
	blk := m.FocusedBlock()
	if blk == nil {
		return false
	}
	if ec, ok := blk.(EscCapturer); ok {
		return ec.CapturesEsc()
	}
	return false
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

// Update routes msg. Key presses go only to the focused entry's Block, when
// any. Every other message (e.g. a window resize, or a product message) is
// broadcast to every Block, unless it implements Targeted, in which case it
// is forwarded only to the entry with the matching ID.
func (m *Model) Update(msg tea.Msg) tea.Cmd {
	if _, isKey := msg.(tea.KeyPressMsg); !isKey {
		return m.broadcast(msg)
	}
	idx := m.entryIndexForStop(m.focusIndex)
	if idx < 0 || m.entries[idx].Block == nil {
		return nil
	}
	blk, cmd := m.entries[idx].Block.Update(msg)
	m.entries[idx].Block = blk
	m.entries[idx].renderValid = false
	m.Rebuild(false)
	return cmd
}

func (m *Model) broadcast(msg tea.Msg) tea.Cmd {
	targetID, targeted := "", false
	if t, ok := msg.(Targeted); ok {
		targetID, targeted = t.TargetEntryID(), true
	}
	var cmds []tea.Cmd
	for i := range m.entries {
		if m.entries[i].Block == nil {
			continue
		}
		if targeted && m.entries[i].ID != targetID {
			continue
		}
		blk, cmd := m.entries[i].Block.Update(msg)
		m.entries[i].Block = blk
		m.entries[i].renderValid = false
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	m.Rebuild(m.shouldAutoFollow())
	return tea.Batch(cmds...)
}

// ScrollUp/ScrollDown pass through to the viewport for plain scrolling
// (e.g. PgUp/PgDn on an unfocused transcript).
func (m *Model) ScrollUp(lines int)   { m.viewport.SetYOffset(max(0, m.viewport.YOffset()-lines)) }
func (m *Model) ScrollDown(lines int) { m.viewport.SetYOffset(m.viewport.YOffset() + lines) }

// Rebuild re-renders every entry into the viewport's content and, when an
// entry is focused, scrolls it into view (ensureBlockVisible). It mirrors
// DataTug's rebuildHistory/ensureBlockVisible in pkg/chat/ui.go, generalised
// away from DataTug-specific entry kinds. Per-entry views are cached
// (Entry.renderOut) and only recomputed when the entry's content changed
// (renderValid cleared) or its width/focused state differs from the cache.
func (m *Model) Rebuild(scrollToBottom bool) {
	width := max(1, m.width)
	blocks := make([]string, 0, len(m.entries))
	activeBlock := -1
	stop := -1
	for i := range m.entries {
		e := &m.entries[i]
		focused := false
		if e.focusable() {
			stop++
			focused = stop == m.focusIndex
			if focused {
				activeBlock = len(blocks)
			}
		}
		if e.renderValid && e.renderWidth == width && e.renderFocused == focused {
			blocks = append(blocks, e.renderOut)
			continue
		}
		var out string
		switch {
		case e.Block != nil:
			out = e.Block.View(width, focused)
		case e.Role == RoleUser:
			out = userCardView(e.Text, width, focused)
		case e.Markdown && m.markdownRenderer != nil:
			out = m.markdownRenderer(e.Text, width)
		default:
			out = plainMessageView(e.Role, e.Text, width)
		}
		e.renderOut, e.renderWidth, e.renderFocused, e.renderValid = out, width, focused, true
		blocks = append(blocks, out)
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
