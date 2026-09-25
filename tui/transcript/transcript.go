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
	"github.com/strongo/aichat/tui/theme"
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

// Titled is an optional Block capability: when implemented, its Title() is
// shown as the header of the shared card transcript draws around the
// Block's own View output (founder 2026-09-25: "Blocks ... should sit in
// the same card frame" as a plain message), e.g. a grid's record-set name
// or an HTTP response's method+URL. A Block that doesn't implement it
// renders in an untitled card.
type Titled interface {
	Title() string
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

// WheelConsumer is an optional Block capability: a FOCUSED Block that wants
// to handle a tea.MouseWheelMsg itself (e.g. scrolling its own internal
// view, such as a grid's row list) implements it. ConsumesWheel is a pure
// query -- it reports whether the Block WOULD consume msg without any side
// effect -- so DeliverWheelToFocusedBlock can decide whether to actually
// dispatch it before doing so.
type WheelConsumer interface {
	ConsumesWheel(msg tea.MouseWheelMsg) bool
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

// SetSize resizes the viewport and re-renders. It is a NO-OP when neither
// dimension actually changed (r3 review, B1): a caller (e.g. chatshell's
// View(), which re-applies its own resize() on every render so chrome that
// changes without a WindowSizeMsg -- the slash-command menu opening by
// keystroke, SetBusy, SetStatus -- stays in sync) may well call SetSize
// with the SAME width/height on every single frame. Unconditionally
// re-Rebuilding on every such call, even with nothing to resize, had two
// user-visible side effects: it silently snapped a manually-scrolled-up
// viewport back to the bottom every frame, and it re-forced a FOCUSED
// entry back into view (ensureBlockVisible) every frame too, both on top
// of the wasted re-render cost of a no-op resize.
//
// When the size DOES change, the viewport's scroll position is preserved
// UNLESS it was already at the bottom before the resize (checked BEFORE
// applying the new dimensions, since AtBottom() itself depends on them), in
// which case it keeps following -- the same "at the bottom" rule
// shouldAutoFollow applies for Append/ReplaceBlock/AppendDelta (r4 review
// folded the two rules back into one: shouldAutoFollow no longer treats
// "unfocused" as its own reason to follow, so a resize with no NEW content
// and a stream delta that IS new content now agree on exactly when
// following is appropriate).
func (m *Model) SetSize(width, height int) {
	width, height = max(1, width), max(1, height)
	if width == m.width && height == m.height {
		return
	}
	wasAtBottom := m.viewport.AtBottom()
	m.width, m.height = width, height
	m.viewport.SetWidth(m.width)
	m.viewport.SetHeight(m.height)
	m.Rebuild(wasAtBottom)
}

// Entries returns the current entries (read-only use expected).
func (m *Model) Entries() []Entry { return m.entries }

// Append adds a new entry to the end of the transcript. It scrolls to the
// bottom only when the viewport was already at the bottom (r4 review: NOT
// merely "unfocused" -- an unfocused but manually scrolled-up viewport is
// left alone too), so focusing an earlier stop to read it, or simply
// having scrolled up to re-read something, is not disturbed by new content
// arriving.
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

// shouldAutoFollow reports whether new content (a streamed delta, a plain
// Append, ...) should scroll the viewport to the bottom: ONLY when the
// viewport was already at the bottom -- regardless of whether an earlier
// stop is focused (r4 review, folding in a pre-existing gap: focus being
// unset, i.e. focusIndex < 0, used to short-circuit this to true
// unconditionally, which meant an UNFOCUSED but manually wheel-scrolled-up
// viewport got yanked back to the bottom by every single streamed delta --
// exactly the scroll-position bug REQ: transcript-setsize-idempotent-and-
// preserves-scroll already fixed for SetSize, but this path (AppendDelta/
// Append/ReplaceBlock/InvalidateAndRebuild) was still wrong). Being at the
// bottom already implies nothing EARLIER is meaningfully in view either
// way, so this one check now covers both cases correctly: focused on an
// earlier stop and scrolled away -> false (unchanged); unfocused and
// scrolled up -> false (the fix); either state and actually at the bottom
// -> true (keeps following, unchanged).
func (m *Model) shouldAutoFollow() bool {
	return m.viewport.AtBottom()
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

// DeliverWheelToFocusedBlock dispatches msg to the FOCUSED entry's Block
// ONLY (never a broadcast to every entry, unlike Update's non-key path) --
// and ONLY when that Block implements WheelConsumer and its ConsumesWheel
// reports true for msg. It reports whether msg was consumed: false means
// either no Block is focused, the focused Block doesn't implement
// WheelConsumer, or it declined this particular event, and the CALLER
// should handle the wheel event itself instead (e.g. scroll a viewport) --
// mirroring tui/chatshell's "never both" rule for its own transcript
// viewport vs. a focused Block's own wheel handling.
func (m *Model) DeliverWheelToFocusedBlock(msg tea.MouseWheelMsg) (consumed bool, cmd tea.Cmd) {
	i := m.entryIndexForStop(m.focusIndex)
	if i < 0 || m.entries[i].Block == nil {
		return false, nil
	}
	wc, ok := m.entries[i].Block.(WheelConsumer)
	if !ok || !wc.ConsumesWheel(msg) {
		return false, nil
	}
	blk, cmd := m.entries[i].Block.Update(msg)
	m.entries[i].Block = blk
	m.entries[i].renderValid = false
	m.Rebuild(false)
	return true, cmd
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
		out := m.renderEntry(e, width, focused)
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

// renderEntry renders one transcript entry as the shared card every aichat
// product's messages, markdown responses and Blocks now render as (founder
// 2026-09-25: "Message should be like a card in chat of any app" and
// "Blocks ... should sit in the same card frame"): a coloured, bordered,
// padded box from tui/theme, distinct per role, with a bold header and a
// clearly visible focus highlight when focused — never a scattered
// lipgloss.NewStyle() literal here; every colour/border/padding decision
// lives in tui/theme.
func (m *Model) renderEntry(e *Entry, width int, focused bool) string {
	switch {
	case e.Block != nil:
		header := ""
		if t, ok := e.Block.(Titled); ok {
			header = t.Title()
		}
		body := e.Block.View(theme.InnerWidth(width), focused)
		return theme.Card(theme.RoleBlock, header, body, width, focused)
	case e.Role == RoleUser:
		return theme.Card(theme.RoleUser, theme.HeaderFor(theme.RoleUser), e.Text, width, focused)
	case e.Markdown && m.markdownRenderer != nil:
		body := m.markdownRenderer(e.Text, theme.InnerWidth(width))
		return theme.Card(theme.RoleAssistant, theme.HeaderFor(theme.RoleAssistant), body, width, focused)
	case e.Role == RoleAssistant:
		return theme.Card(theme.RoleAssistant, theme.HeaderFor(theme.RoleAssistant), e.Text, width, focused)
	default:
		// RoleSystem, including an "error: ..." message (AppendSystem is
		// how chatshell reports both a plain status note and a stream/
		// command error — see chatshell.handleStreamDone/cancelBusy) — the
		// "error:" prefix is the one signal available here to give an
		// error its own distinct, more alarming card colour instead of
		// blending into ordinary system notices.
		role := theme.RoleSystem
		if strings.HasPrefix(e.Text, "error:") {
			role = theme.RoleError
		}
		return theme.Card(role, theme.HeaderFor(role), e.Text, width, focused)
	}
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
