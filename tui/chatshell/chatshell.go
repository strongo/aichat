package chatshell

import (
	"context"
	"errors"
	"iter"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/session"
	"github.com/strongo/aichat/tui"
	"github.com/strongo/aichat/tui/focus"
	"github.com/strongo/aichat/tui/sidebar"
	"github.com/strongo/aichat/tui/stream"
	"github.com/strongo/aichat/tui/transcript"
)

// splitMinWidth is the terminal width at or above which the sidebar renders
// as a split pane (DataTug's splitEnabled: u.width >= 104).
const splitMinWidth = 104

// markdownRenderThrottle caps how often StartStreamMarkdown re-runs the
// configured MarkdownRenderer while a stream is in flight: at most once per
// window, plus once on any delta crossing a newline (a likely-stable
// rendering point, e.g. a completed list item or paragraph), plus always
// once more on completion so the final render is never stale.
const markdownRenderThrottle = 100 * time.Millisecond

// Command is one slash command the composer's menu offers.
type Command struct {
	Name string
	Help string
}

// Handler is how a product answers a submitted chat turn.
type Handler interface {
	Submit(text string) tea.Cmd
}

// SidebarObserver is an optional Handler capability: when implemented,
// chatshell calls OnSidebarChange after every sidebar mutation (pin, unpin,
// AddToSidebarMsg, sidebar removal).
type SidebarObserver interface {
	OnSidebarChange(refs []session.EntityRef)
}

// StreamObserver is an optional Handler capability: when implemented,
// chatshell calls OnStreamEvent for every event a StartStream-driven stream
// produces (Started/TextDelta/Structured/Usage/Completed/Error), in
// addition to chatshell's own built-in handling (rendering text deltas,
// appending non-fatal errors). Its returned tea.Cmd, if any, is batched
// alongside the stream's own re-arm command.
//
// OnStreamDone is called exactly once per StartStream call, whatever the
// outcome — success (err nil), a fatal error, or a user/product
// cancellation (err satisfying chatshell's isCanceled) — so a product can
// roll back speculative state or record diagnostics. It fires even for a
// stream that was superseded by a later StartStream call before finishing.
type StreamObserver interface {
	OnStreamEvent(id string, ev ai.Event) tea.Cmd
	OnStreamDone(id string, err error) tea.Cmd
}

// MsgHandler is an optional Handler capability: when implemented, chatshell
// forwards every message it does not itself recognise (e.g. a product
// message, or a Block message such as grid.RowActivatedMsg) to OnMsg, in
// addition to broadcasting it to the transcript's Blocks.
type MsgHandler interface {
	OnMsg(msg tea.Msg) tea.Cmd
}

// SidePanel replaces the default sidebar (working-context list) when set,
// e.g. a product workspace pane with its own tabs, explorer and bookmarks.
// It participates in the focus ring exactly as the built-in sidebar does
// (Shift+Right/Left, F6 toggle, Ctrl+←/→ split 40–75%).
type SidePanel interface {
	Title() string
	View(width, height int, focused bool) string
	Update(msg tea.Msg) (SidePanel, tea.Cmd)
}

// Overlay is a modal dialog: once pushed (PushOverlay) it captures every key
// event until its Update returns done, and renders centred over the screen.
type Overlay interface {
	View(width, height int) string
	Update(msg tea.Msg) (o Overlay, cmd tea.Cmd, done bool)
}

// GlobalKeysFunc is checked before chatshell's own key handling (so a
// product can claim keys like F3/F4 pickers ahead of chatshell's defaults).
// cmd is returned as-is; consumed true stops chatshell from handling the key
// at all this cycle.
type GlobalKeysFunc func(msg tea.KeyPressMsg) (cmd tea.Cmd, consumed bool)

// Option configures a Model at construction time.
type Option func(*Model)

// WithCommands sets the slash commands the composer offers.
func WithCommands(commands []Command) Option {
	return func(m *Model) { m.commands = commands }
}

// WithSidebarRenderer sets how sidebar entries render.
func WithSidebarRenderer(render sidebar.Renderer) Option {
	return func(m *Model) { m.sidebar = sidebar.New(render) }
}

// WithSidePanel installs a product SidePanel in place of the default
// sidebar. It starts visible, matching the default sidebar's own start
// state.
func WithSidePanel(p SidePanel) Option {
	return func(m *Model) {
		m.sidePanel = p
		m.sidePanelVisible = true
		m.sidePanelChatPercent = 65
	}
}

// WithGlobalKeys sets a product key hook checked before chatshell's own key
// handling (see GlobalKeysFunc).
func WithGlobalKeys(fn GlobalKeysFunc) Option {
	return func(m *Model) { m.globalKeys = fn }
}

// WithTopBar sets a product-rendered top bar, replacing the default bold
// title line.
func WithTopBar(render func(width int) string) Option {
	return func(m *Model) { m.topBarFn = render }
}

// WithStatusBar sets a product-rendered status bar, replacing the default
// SetStatus-driven status line(s).
func WithStatusBar(render func(width int) string) Option {
	return func(m *Model) { m.statusBarFn = render }
}

// WithContext sets the context streamed responses and Handler calls run
// under (defaults to context.Background()). StartStream derives a
// cancellable child of it per stream.
func WithContext(ctx context.Context) Option {
	return func(m *Model) { m.ctx = ctx }
}

// WithTitle sets the top-bar title (defaults to "aichat").
func WithTitle(title string) Option {
	return func(m *Model) { m.title = title }
}

// WithMarkdownRenderer sets the renderer used for entries appended via
// AppendAssistantMarkdown (or any transcript.Entry with Markdown set), e.g.
// a glamour-backed renderer for agent or HTTP-response markdown.
func WithMarkdownRenderer(r transcript.MarkdownRenderer) Option {
	return func(m *Model) { m.transcript.SetMarkdownRenderer(r) }
}

// MouseMode selects whether chatshell requests terminal mouse reporting and,
// if so, which tea.MouseMode it asks for.
type MouseMode int

const (
	// MouseOff requests no mouse reporting (the default: a terminal's own
	// native text selection/copy keeps working).
	MouseOff MouseMode = iota
	// MouseCellMotion requests click, release and wheel events (but not
	// plain motion/hover) -- enough to scroll the transcript with the wheel
	// without giving up terminal-native text selection on most terminals.
	MouseCellMotion
)

// mouseTeaMode maps a MouseMode to the tea.MouseMode View() sets.
func (mm MouseMode) mouseTeaMode() tea.MouseMode {
	if mm == MouseCellMotion {
		return tea.MouseModeCellMotion
	}
	return tea.MouseModeNone
}

// WithMouse sets the initial mouse mode (see MouseMode). Products that want
// a runtime toggle (e.g. DataTug's F2 capture toggle, which needs the
// terminal's native mouse selection back while capturing) call
// SetMouseEnabled after construction; WithMouse only sets the starting
// state and, for MouseCellMotion, the mode SetMouseEnabled(true) re-enables
// later. The default (no WithMouse call) is MouseOff.
func WithMouse(mode MouseMode) Option {
	return func(m *Model) {
		// m1 (r1 review): WithMouse(MouseOff) must NOT clobber mouseMode
		// down to MouseOff -- doing so would make a later
		// SetMouseEnabled(true) a silent no-op (mouseTeaMode() on
		// MouseOff is always tea.MouseModeNone). Only an actual enabling
		// mode updates mouseMode; MouseOff only clears mouseEnabled,
		// leaving New's MouseCellMotion default (or an earlier WithMouse
		// call's mode) in place for SetMouseEnabled(true) to restore.
		if mode != MouseOff {
			m.mouseMode = mode
		}
		m.mouseEnabled = mode != MouseOff
	}
}

// Model is the reusable chat screen.
type Model struct {
	ctx     context.Context
	handler Handler

	transcript *transcript.Model
	input      textarea.Model
	sidebar    *sidebar.Model
	focusRing  *focus.Ring
	spinner    spinner.Model

	// sidePanel, when set (WithSidePanel), replaces sidebar for the whole
	// sidebar zone: visibility, split percent and rendering all route
	// through it instead of the sidebar field above.
	sidePanel            SidePanel
	sidePanelVisible     bool
	sidePanelChatPercent int
	globalKeys           GlobalKeysFunc
	topBarFn             func(width int) string
	statusBarFn          func(width int) string
	overlays             []Overlay

	commands             []Command
	commandMenuIndex     int
	commandMenuDismissed string // input value the menu was last Esc-dismissed for

	title  string
	status string
	width  int
	height int
	busy   bool
	quit   bool

	// streamID/streamCancel identify and cancel the in-flight StartStream
	// call, if any. handleStreamDone still notifies StreamObserver for a
	// stale (superseded) Done, but only a Done matching streamID clears busy
	// and appends a transcript entry.
	streamID     string
	streamCancel context.CancelFunc

	// streamMarkdown and lastMarkdownRender throttle StartStreamMarkdown's
	// re-render: streamMarkdown is true while the current stream renders
	// through the MarkdownRenderer, and lastMarkdownRender is when it last
	// actually re-rendered (as opposed to merely accumulating text via
	// transcript.AppendDeltaNoRender) — see handleStreamEvent.
	streamMarkdown     bool
	lastMarkdownRender time.Time
	// markdownTickPending is true while a follow-up markdownRenderTickMsg
	// is scheduled (m1, r3 review): a throttled-out delta with no further
	// deltas arriving would otherwise leave its trailing fragment
	// unrendered indefinitely (nothing re-invalidates the entry until the
	// NEXT delta or stream completion) — this guarantees a render within
	// ~markdownRenderThrottle regardless. Guards against scheduling a pile
	// of redundant ticks while one is already in flight.
	markdownTickPending bool
	// nowFunc, when set, replaces time.Now for the markdown-render throttle
	// (tests only, via a fake clock — avoids real sleeps in a wall-clock
	// throttle test). nil means time.Now.
	nowFunc func() time.Time
	// tickFunc, when set, replaces tea.Tick for the markdown-render
	// follow-up (tests only — avoids a real ~100ms sleep per test). nil
	// means tea.Tick.
	tickFunc func(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd

	// busyCancel is the cancel func for a product's own SetBusy(true) phase
	// (e.g. a decision chain), set via SetBusyCancel. Esc/Ctrl+C while busy
	// and no stream is active calls it directly, since — unlike a stream —
	// there is no DoneMsg to asynchronously report the cancellation.
	busyCancel func()

	// ctrlCArmed is set by a first Ctrl+C while busy (which cancels); a
	// second, immediately-following Ctrl+C always quits instead of trying to
	// cancel again. Any other key clears it.
	ctrlCArmed bool

	// mouseMode is the tea.MouseMode View() requests while mouseEnabled is
	// true (see WithMouse/SetMouseEnabled); mouseEnabled false always
	// reports tea.MouseModeNone regardless of mouseMode, so a later
	// SetMouseEnabled(true) restores the configured mode rather than a
	// forgotten MouseOff.
	mouseMode    MouseMode
	mouseEnabled bool
}

// New returns a chat screen driven by handler.
func New(handler Handler, opts ...Option) *Model {
	input := textarea.New()
	input.Placeholder = "Ask anything..."
	input.Prompt = ""
	input.ShowLineNumbers = false
	input.SetHeight(1)
	input.Focus()

	m := &Model{
		ctx:        context.Background(),
		handler:    handler,
		transcript: transcript.New(),
		input:      input,
		sidebar:    sidebar.New(nil),
		focusRing:  focus.New(),
		spinner:    spinner.New(spinner.WithSpinner(spinner.Dot)),
		title:      "aichat",
		width:      80,
		height:     24,
		// mouseEnabled defaults false (MouseOff); mouseMode defaults to
		// MouseCellMotion so a product that calls SetMouseEnabled(true)
		// without ever calling WithMouse still gets a sensible mode rather
		// than a silent no-op (MouseOff's tea.MouseMode is always None).
		mouseMode: MouseCellMotion,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// --- Product-facing API -----------------------------------------------

// SetMouseEnabled toggles mouse reporting at runtime, e.g. DataTug's F2
// capture toggle (a terminal's own native text-selection/copy is unusable
// while mouse reporting is on, so a product that wants both needs a key to
// flip between them). enabled true requests the mode configured via
// WithMouse (MouseCellMotion by default if WithMouse was never called);
// enabled false requests no mouse reporting at all. The new mode takes
// effect on the next View() -- chatshell has no way to push it to the
// terminal outside the normal render cycle.
func (m *Model) SetMouseEnabled(enabled bool) { m.mouseEnabled = enabled }

// MouseEnabled reports whether mouse reporting is currently requested (see
// SetMouseEnabled).
func (m *Model) MouseEnabled() bool { return m.mouseEnabled }

// AppendUser appends a user message to the transcript.
func (m *Model) AppendUser(text string) {
	m.transcript.Append(transcript.Entry{Role: transcript.RoleUser, Text: text})
}

// AppendAssistant appends a (non-streamed) assistant message.
func (m *Model) AppendAssistant(text string) {
	m.transcript.Append(transcript.Entry{Role: transcript.RoleAssistant, Text: text})
}

// AppendAssistantMarkdown appends a non-streamed assistant message rendered
// through the configured MarkdownRenderer (WithMarkdownRenderer), e.g.
// agent or HTTP-response markdown via glamour. With no renderer configured
// it behaves like AppendAssistant (transcript.Entry.Markdown is inert then).
func (m *Model) AppendAssistantMarkdown(text string) {
	m.transcript.Append(transcript.Entry{Role: transcript.RoleAssistant, Text: text, Markdown: true})
}

// AppendSystem appends a system/status message (e.g. an error).
func (m *Model) AppendSystem(text string) {
	m.transcript.Append(transcript.Entry{Role: transcript.RoleSystem, Text: text})
}

// AppendBlock appends a rich transcript.Block (e.g. a tui/grid result).
func (m *Model) AppendBlock(block transcript.Block) {
	m.transcript.Append(transcript.Entry{Block: block})
}

// AppendBlockWithID appends a rich transcript.Block under a caller-chosen,
// stable id, so a product can later FocusEntry(id) (e.g. DataTug's Ctrl+G
// "jump to latest grid") or ReplaceBlock(id, ...) it. id must be non-empty
// and unique among live entries (and must not clash with an in-flight
// StartStream id, since both identify a transcript entry the same way);
// AppendBlockWithID reports false and does not append on any such
// collision, so a caller can generate a fresh id and retry instead of
// silently corrupting FocusEntry/ReplaceBlock addressing.
func (m *Model) AppendBlockWithID(id string, block transcript.Block) bool {
	if id == "" {
		return false
	}
	if id == m.streamID {
		return false
	}
	for _, e := range m.transcript.Entries() {
		if e.ID == id {
			return false
		}
	}
	m.transcript.Append(transcript.Entry{ID: id, Block: block})
	return true
}

// StartStream starts a streamed assistant entry. id must be unique per
// turn; deltas render progressively into the transcript, and a spinner runs
// until the first delta (or completion) arrives.
//
// The Model owns the per-stream context: it creates a cancellable child of
// its own context (WithContext) and passes it to open, which must use it to
// build the actual provider call (e.g. `return provider.Stream(ctx, req)`)
// so a cancellation reaches the live request, not just this local pump — an
// adapter observing ctx.Done() aborts its call and yields
// ai.ErrCodeCanceled. The context is cancelled automatically when a new
// StartStream call supersedes this one, or explicitly by the user (Esc or
// Ctrl+C while busy). A cancelled stream ends with a "(stopped)" transcript
// entry, not an error, and StreamObserver.OnStreamDone (if implemented)
// always fires once the stream ends, however it ended.
func (m *Model) StartStream(id string, open func(ctx context.Context) iter.Seq2[ai.Event, error]) tea.Cmd {
	return m.startStream(id, false, open)
}

// StartStreamMarkdown is StartStream, but the streaming entry is rendered
// through the configured MarkdownRenderer (WithMarkdownRenderer) as it
// accumulates, same as AppendAssistantMarkdown for a non-streamed message —
// e.g. DataTug's agent/HTTP markdown responses. With no renderer configured
// it behaves like StartStream (transcript.Entry.Markdown is inert then).
func (m *Model) StartStreamMarkdown(id string, open func(ctx context.Context) iter.Seq2[ai.Event, error]) tea.Cmd {
	return m.startStream(id, true, open)
}

func (m *Model) startStream(id string, markdown bool, open func(ctx context.Context) iter.Seq2[ai.Event, error]) tea.Cmd {
	m.cancelStream()
	ctx, cancel := context.WithCancel(m.ctx)
	m.streamID, m.streamCancel = id, cancel
	m.streamMarkdown = markdown
	m.lastMarkdownRender = time.Time{}
	m.markdownTickPending = false
	m.busy = true
	m.ctrlCArmed = false
	m.transcript.Append(transcript.Entry{ID: id, Role: transcript.RoleAssistant, Text: "", Markdown: markdown})
	seq := open(ctx)
	return tea.Batch(stream.Start(ctx, id, seq), m.spinner.Tick)
}

// cancelStream cancels the in-flight stream, if any. Its DoneMsg (Err =
// context.Canceled, or an ai.Error{Code: ai.ErrCodeCanceled} once the ai/
// provider translates the cancellation) arrives later, as usual.
func (m *Model) cancelStream() {
	if m.streamCancel != nil {
		m.streamCancel()
	}
}

// cancelBusy is Esc/Ctrl+C's "stop whatever is busy" action. A live stream
// is cancelled through the usual context path (its DoneMsg renders
// "(stopped)" asynchronously, see handleStreamDone); a bare SetBusy(true)
// phase has no such message, so cancelBusy calls its registered
// SetBusyCancel func (if any), clears busy and appends "(stopped)" itself,
// right away.
func (m *Model) cancelBusy() {
	if m.streamCancel != nil {
		m.cancelStream()
		return
	}
	if m.busyCancel != nil {
		m.busyCancel()
		m.busyCancel = nil
	}
	m.busy = false
	m.AppendSystem("(stopped)")
}

// SetBusy marks a product-driven phase that precedes (or stands in for) a
// stream — e.g. a decision chain or a deterministic query — as in flight:
// the composer stops accepting input and the spinner runs, exactly as while
// a stream is in flight. The returned tea.Cmd starts the spinner and must be
// returned from Update/a command chain when busy is true; it is nil when
// busy is false. SetBusy(false) clears any SetBusyCancel func registered for
// the phase that just ended.
func (m *Model) SetBusy(busy bool) tea.Cmd {
	m.busy = busy
	if !busy {
		m.busyCancel = nil
		return nil
	}
	m.ctrlCArmed = false
	return m.spinner.Tick
}

// SetBusyCancel registers the cancel func for the current SetBusy(true)
// phase (e.g. a context.CancelFunc for the ctx a decision chain runs
// under). Esc/Ctrl+C while busy and no stream is active calls it — see
// cancelBusy. Products that call SetBusy(true) but have nothing cancellable
// may leave this unset.
func (m *Model) SetBusyCancel(cancel func()) { m.busyCancel = cancel }

// SetStatus sets the status line text (provider/model/path/usage, etc.).
func (m *Model) SetStatus(text string) { m.status = text }

// SidePanelPinner is an optional SidePanel capability: when a product's
// SidePanel implements it, PinToSidebar/UnpinFromSidebar/SidebarRefs and
// AddToSidebarMsg route to it instead of the built-in sidebar's ref list. A
// SidePanel that does NOT implement it makes Pin/Unpin/SidebarRefs a
// documented no-op (never silently falls back to the now-hidden default
// sidebar's own state).
type SidePanelPinner interface {
	// PinRef adds ref; it reports whether the set actually changed.
	PinRef(ref session.EntityRef) bool
	// UnpinRef removes ref; it reports whether the set actually changed.
	UnpinRef(ref session.EntityRef) bool
	// Refs returns the current pinned refs.
	Refs() []session.EntityRef
}

// PinToSidebar adds ref to the sidebar (or, with a SidePanel implementing
// SidePanelPinner, to it instead) and notifies an OnSidebarChange Handler,
// if any.
func (m *Model) PinToSidebar(ref session.EntityRef) {
	if m.sidePanel != nil {
		if p, ok := m.sidePanel.(SidePanelPinner); ok && p.PinRef(ref) {
			m.notifySidebarChange()
		}
		return
	}
	if m.sidebar.Add(ref) {
		m.notifySidebarChange()
	}
}

// UnpinFromSidebar removes ref from the sidebar (or SidePanelPinner) and
// notifies an OnSidebarChange Handler, if any.
func (m *Model) UnpinFromSidebar(ref session.EntityRef) {
	if m.sidePanel != nil {
		if p, ok := m.sidePanel.(SidePanelPinner); ok && p.UnpinRef(ref) {
			m.notifySidebarChange()
		}
		return
	}
	if m.sidebar.Remove(ref) {
		m.notifySidebarChange()
	}
}

func (m *Model) notifySidebarChange() {
	if obs, ok := m.handler.(SidebarObserver); ok {
		obs.OnSidebarChange(m.SidebarRefs())
	}
}

// FocusedRef returns the entity ref under focus: the transcript's focused
// block's Current() when the transcript zone has focus, or the sidebar
// cursor's ref when the sidebar zone has focus (the default sidebar only —
// a SidePanel exposes no cursor in its pinned contract, so this is nil while
// one is active). It is nil when the composer has focus, or nothing is
// under the cursor.
func (m *Model) FocusedRef() *session.EntityRef {
	switch m.focusRing.Zone() {
	case focus.ZoneTranscript:
		return m.transcript.Current()
	case focus.ZoneSidebar:
		if m.sidePanel != nil {
			return nil
		}
		refs := m.sidebar.Refs()
		cursor := m.sidebar.Cursor()
		if cursor < 0 || cursor >= len(refs) {
			return nil
		}
		ref := refs[cursor]
		return &ref
	default:
		return nil
	}
}

// SelectionRefs returns the transcript's current selection: the focused
// block's entity, when any (a Block may later report more than one, e.g. a
// grid's multi-selected rows; today this is FocusedRef's single entity).
// It is distinct from the sidebar's pins — see SidebarRefs.
func (m *Model) SelectionRefs() []session.EntityRef {
	if ref := m.transcript.Current(); ref != nil {
		return []session.EntityRef{*ref}
	}
	return nil
}

// SidebarRefs returns the sidebar's pinned refs: the default sidebar's, or
// a SidePanelPinner SidePanel's, or nil when a SidePanel is active but
// doesn't implement SidePanelPinner (see PinToSidebar).
func (m *Model) SidebarRefs() []session.EntityRef {
	if m.sidePanel != nil {
		if p, ok := m.sidePanel.(SidePanelPinner); ok {
			return append([]session.EntityRef(nil), p.Refs()...)
		}
		return nil
	}
	return append([]session.EntityRef(nil), m.sidebar.Refs()...)
}

// Busy reports whether a stream, or a product SetBusy(true) phase, is in
// flight.
func (m *Model) Busy() bool { return m.busy }

// ReplaceBlock refreshes/re-runs a grid (or any other transcript.Block) in
// place: the entry identified by entryID keeps its position and ID, but
// renders b from now on. If that entry currently holds transcript focus, it
// keeps it (the focus ring stop is recomputed for the entry, not left
// pointing at whatever raw index it used to occupy — necessary because
// replacing a Block can change its own Focusable() answer).
//
// m.focusRing (chatshell's own zone/stop tracker) is resynced to whatever
// transcript.ReplaceBlock decided too: syncFocus later reapplies
// focusRing.Stop() into the transcript on the next zone change or resize, so
// leaving it stale would silently undo transcript's own recomputed focus the
// next time that happens.
func (m *Model) ReplaceBlock(entryID string, b transcript.Block) {
	var hadFocus bool
	var oldStop int
	if m.focusRing.Zone() == focus.ZoneTranscript {
		if fe := m.transcript.FocusedEntry(); fe != nil {
			hadFocus = true
			oldStop = m.transcript.StopForID(fe.ID)
		}
	}

	m.transcript.ReplaceBlock(entryID, b)

	if m.focusRing.Zone() != focus.ZoneTranscript {
		return
	}
	if fe := m.transcript.FocusedEntry(); fe != nil {
		// Same entry (or a different one whose stop shifted) is still
		// focusable: resync focusRing's own stop, per m5.
		if stop := m.transcript.StopForID(fe.ID); stop >= 0 {
			m.focusRing.FocusStop(stop)
		}
		return
	}
	if !hadFocus {
		return
	}
	// m2 (r3 review): the previously-focused entry is no longer focusable
	// (this swap made it so, or an earlier entry's swap shifted stops out
	// from under it). Move to the NEAREST remaining focusable stop --
	// oldStop clamped into range, since the entries around a removed stop
	// slide down to fill it -- or hand off to the composer if the
	// transcript has no focusable entry left at all.
	if n := m.transcript.Stops(); n > 0 {
		newStop := oldStop
		if newStop >= n {
			newStop = n - 1
		}
		if newStop < 0 {
			newStop = 0
		}
		m.focusRing.FocusStop(newStop)
	} else {
		m.focusRing.FocusInput()
	}
	m.syncFocus()
}

// FocusEntry moves focus to the transcript entry identified by id (e.g.
// DataTug's Ctrl+G "jump to latest grid"), scrolling it into view. It
// reports whether such a focusable entry exists; when it doesn't, focus is
// left unchanged.
func (m *Model) FocusEntry(id string) bool {
	stop := m.transcript.StopForID(id)
	if stop < 0 {
		return false
	}
	m.focusRing.FocusStop(stop)
	m.syncFocus()
	return true
}

// SetComposerText sets the composer's text and moves the cursor to the end,
// e.g. an edit-previous-message flow.
func (m *Model) SetComposerText(s string) {
	m.input.SetValue(s)
	m.input.CursorEnd()
}

// ClearTranscript cancels any in-flight stream, empties the transcript and
// returns focus to the composer, e.g. /clear or a session switch. Cancelling
// first (rather than leaving the stream running against a now-empty
// transcript) matters because handleStreamEvent only applies an EventMsg
// whose ID still matches the current stream — a stray delta for the
// cancelled stream is otherwise silently dropped instead of resurrecting a
// transcript entry the clear just removed. A product's own SetBusy(true)
// phase (no stream, e.g. a decision chain) has no DoneMsg to cancel it
// asynchronously, so ClearTranscript also invokes the registered
// SetBusyCancel callback directly, same as cancelBusy does for Esc/Ctrl+C.
func (m *Model) ClearTranscript() {
	m.cancelStream()
	m.streamID = ""
	m.streamCancel = nil
	if m.busyCancel != nil {
		m.busyCancel()
		m.busyCancel = nil
	}
	m.busy = false
	m.streamMarkdown = false
	m.markdownTickPending = false
	m.transcript.Clear()
	m.focusRing.FocusInput()
	m.syncFocus()
}

// PushOverlay pushes a modal dialog onto the overlay stack. The top overlay
// captures every key event (and every other message chatshell would
// otherwise handle itself) until its Update returns done, and is rendered
// centred over the screen.
func (m *Model) PushOverlay(o Overlay) tea.Cmd {
	m.overlays = append(m.overlays, o)
	return nil
}

// PopOverlay closes the top overlay PROGRAMMATICALLY -- without waiting for
// its own Update to report done -- e.g. after an async round trip a
// product's own Handler drove to completion. It is a no-op (returns nil)
// when no overlay is open.
//
// Async-safe overlay pattern: an Overlay may need to stay open ACROSS an
// async round trip (e.g. a form whose Enter submits to a server before it
// can close). Its own Update returns done: false plus a product tea.Cmd on
// submit -- exactly like any other command chatshell dispatches. The
// product's own result message, once it arrives, is NOT itself overlay
// input (isOverlayInputMsg only classifies key/paste/mouse messages), so it
// takes the normal Update path and reaches an optional MsgHandler.OnMsg
// (dispatchUnhandled's default routing) EVEN WHILE THE OVERLAY IS STILL
// OPEN -- an open overlay only captures key/paste/mouse input, never this.
// From there the product either calls PopOverlay on success, or -- to show
// an error while keeping the user's draft -- updates the overlay in place
// (e.g. via an optional `interface{ OnResult(any) }` capability the
// product's own Overlay implements, or simply because the product holds
// the same Overlay pointer it passed to PushOverlay and can mutate it
// directly).
func (m *Model) PopOverlay() tea.Cmd {
	if len(m.overlays) == 0 {
		return nil
	}
	m.overlays = m.overlays[:len(m.overlays)-1]
	return nil
}

// Zone reports which focus zone currently has focus: the composer
// (focus.ZoneInput), a transcript stop (focus.ZoneTranscript), or the
// sidebar/SidePanel (focus.ZoneSidebar) -- e.g. for a product's
// context-specific status hint.
func (m *Model) Zone() focus.Zone { return m.focusRing.Zone() }

// FocusedEntryID reports the transcript entry id currently under focus
// (Zone() == focus.ZoneTranscript), or "" when the transcript isn't
// focused, no entry is focused, or the focused entry was never given an id
// (AppendBlockWithID/StartStream's id; a plain AppendUser/AppendAssistant/
// AppendBlock entry has none).
func (m *Model) FocusedEntryID() string {
	e := m.transcript.FocusedEntry()
	if e == nil {
		return ""
	}
	return e.ID
}

// --- side panel / sidebar unification -------------------------------------

// panelVisible reports whether the sidebar zone (SidePanel or the default
// sidebar, whichever is active) is currently visible.
func (m *Model) panelVisible() bool {
	if m.sidePanel != nil {
		return m.sidePanelVisible
	}
	return m.sidebar.Visible()
}

// togglePanel implements F6.
func (m *Model) togglePanel() {
	if m.sidePanel != nil {
		m.sidePanelVisible = !m.sidePanelVisible
		return
	}
	m.sidebar.Toggle()
}

// panelChatPercent returns the chat pane's current width share when split.
func (m *Model) panelChatPercent() int {
	if m.sidePanel != nil {
		return m.sidePanelChatPercent
	}
	return m.sidebar.ChatPercent()
}

// growPanelChat implements Ctrl+←/→, clamped to [sidebar.MinChatPercent,
// sidebar.MaxChatPercent] for both the SidePanel and the default sidebar.
func (m *Model) growPanelChat(delta int) {
	if m.sidePanel != nil {
		m.sidePanelChatPercent = max(sidebar.MinChatPercent, min(sidebar.MaxChatPercent, m.sidePanelChatPercent+delta))
		return
	}
	m.sidebar.GrowChat(delta)
}

// panelView renders the active sidebar-zone content (SidePanel or the
// default sidebar) at width, focused as given.
func (m *Model) panelView(width int, focused bool) string {
	if m.sidePanel != nil {
		return m.sidePanel.View(width, m.historyHeight(), focused)
	}
	return m.sidebar.View(width, focused)
}

// updatePanel forwards msg to the active sidebar-zone content.
func (m *Model) updatePanel(msg tea.Msg) tea.Cmd {
	if m.sidePanel != nil {
		var cmd tea.Cmd
		m.sidePanel, cmd = m.sidePanel.Update(msg)
		return cmd
	}
	return m.sidebar.Update(msg)
}

// --- tea.Model -----------------------------------------------------------

func (m *Model) Init() tea.Cmd { return textarea.Blink }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// A WindowSizeMsg always resizes the shell, even with an overlay open
	// (the overlay renders over the freshly resized screen next), and is
	// also forwarded to an active SidePanel so it can lay itself out.
	if wsz, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = wsz.Width, wsz.Height
		m.resize()
		cmds := []tea.Cmd{m.transcript.Update(msg)}
		if m.sidePanel != nil {
			var cmd tea.Cmd
			m.sidePanel, cmd = m.sidePanel.Update(msg)
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	}

	// Per the Overlay contract, the top overlay captures only key/paste/
	// mouse input — everything else (stream events, the spinner tick,
	// sidebar/product messages, ...) takes the normal path below so a
	// stream can keep completing, busy can keep clearing, etc. while a
	// dialog is open.
	if len(m.overlays) > 0 && isOverlayInputMsg(msg) {
		return m.updateOverlay(msg)
	}

	switch msg := msg.(type) {
	case tui.AddToSidebarMsg:
		m.PinToSidebar(msg.Ref)
		return m, nil

	case sidebar.RemoveMsg:
		m.notifySidebarChange()
		return m, nil

	case sidebar.OpenMsg:
		return m, nil

	case stream.EventMsg:
		return m.handleStreamEvent(msg)

	case stream.DoneMsg:
		return m.handleStreamDone(msg)

	case markdownRenderTickMsg:
		m.handleMarkdownRenderTick(msg)
		return m, nil

	case spinner.TickMsg:
		if !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.MouseWheelMsg:
		return m.handleMouseWheel(msg)

	default:
		return m, m.dispatchUnhandled(msg)
	}
}

// mouseWheelScrollLines is how many transcript lines one wheel tick moves,
// matching a typical terminal's own default scroll step.
const mouseWheelScrollLines = 3

// splitSeparatorWidth is the width, in columns, of the " │ " divider View()
// draws between the chat column and the side panel/sidebar when split
// (see View, chatWidth/sidebarWidth) -- handleMouseWheel uses it to tell
// whether a wheel event's X falls in the chat column or past the divider.
const splitSeparatorWidth = 3

// handleMouseWheel scrolls the transcript viewport, UNLESS a product
// SidePanel is installed and the event's X falls in its column (past the
// chat column and its " │ " divider) while the pane is split -- then the
// event is forwarded to the SidePanel instead (e.g. a product's own
// scrollable list), and the transcript does not scroll. Either way, the
// event is then ALSO forwarded to an optional MsgHandler (same as every
// other message dispatchUnhandled reaches), so a product can react to
// wheel events beyond just scrolling. It is reachable only while mouse
// reporting is on (View's MouseMode gates whether the terminal ever sends
// these events at all), but does not itself re-check mouseEnabled -- a
// wheel event that already arrived is honoured regardless, same as
// chatshell honours a key press it happens to receive.
func (m *Model) handleMouseWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	// cmds may collect nil entries below -- tea.Batch (via its compactCmds
	// helper) ignores them, so every branch can append unconditionally
	// instead of each needing its own "if cmd != nil" guard, several of
	// which would otherwise be unreachable in practice (e.g. tui/sidebar's
	// Update never returns a non-nil cmd for an Up/Down key).
	var cmds []tea.Cmd

	// A wheel event must still reach transcript Blocks via the normal
	// broadcast path (e.g. a grid reacting to it), same as
	// dispatchUnhandled does for every message chatshell does not itself
	// fully own -- unconditionally, regardless of which routing branch
	// below actually applies.
	cmds = append(cmds, m.transcript.Update(msg))

	switch {
	case m.splitEnabled() && msg.X >= m.chatWidth()+splitSeparatorWidth:
		// In the panel column: a product SidePanel gets the raw event
		// (free to interpret X/Y/Button itself); the BUILT-IN sidebar has
		// no scroll offset of its own -- it is a cursor list -- so a wheel
		// tick moves its cursor the same way Up/Down would (r2 review
		// minor: "scroll the sidebar" for a cursor list IS moving the
		// cursor).
		if m.sidePanel != nil {
			var cmd tea.Cmd
			m.sidePanel, cmd = m.sidePanel.Update(msg)
			cmds = append(cmds, cmd)
		} else {
			key := tea.KeyPressMsg{Code: tea.KeyDown}
			if msg.Button == tea.MouseWheelUp {
				key = tea.KeyPressMsg{Code: tea.KeyUp}
			}
			cmds = append(cmds, m.sidebar.Update(key))
		}
	default:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.transcript.ScrollUp(mouseWheelScrollLines)
		case tea.MouseWheelDown:
			m.transcript.ScrollDown(mouseWheelScrollLines)
		}
	}

	if h, ok := m.handler.(MsgHandler); ok {
		cmds = append(cmds, h.OnMsg(msg))
	}
	return m, tea.Batch(cmds...)
}

// isOverlayInputMsg reports whether msg is user input an open Overlay
// should capture: key presses, paste, and mouse events. Everything else
// (stream pump messages, the spinner tick, product/sidebar messages, ...)
// is NOT overlay input and takes chatshell's normal path even while an
// overlay is open — see Update.
func isOverlayInputMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case tea.KeyPressMsg, tea.KeyReleaseMsg:
		return true
	case tea.PasteMsg, tea.PasteStartMsg, tea.PasteEndMsg:
		return true
	case tea.MouseClickMsg, tea.MouseReleaseMsg, tea.MouseWheelMsg, tea.MouseMotionMsg:
		return true
	default:
		return false
	}
}

// updateOverlay forwards msg exclusively to the top overlay on the stack,
// popping it once its Update reports done. Called only while len(m.overlays)
// > 0 (see Update): an active overlay captures every message chatshell would
// otherwise handle itself, per the Overlay contract.
func (m *Model) updateOverlay(msg tea.Msg) (tea.Model, tea.Cmd) {
	top := len(m.overlays) - 1
	updated, cmd, done := m.overlays[top].Update(msg)
	if done {
		m.overlays = m.overlays[:top]
	} else {
		m.overlays[top] = updated
	}
	return m, cmd
}

// dispatchUnhandled forwards a message chatshell does not itself recognise
// to the transcript (so a focused or targeted Block can react, e.g. a
// window resize) and to an optional MsgHandler (e.g. for a product message
// such as grid.RowActivatedMsg).
func (m *Model) dispatchUnhandled(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	if cmd := m.transcript.Update(msg); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.sidePanel != nil {
		var cmd tea.Cmd
		m.sidePanel, cmd = m.sidePanel.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if h, ok := m.handler.(MsgHandler); ok {
		if cmd := h.OnMsg(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

func (m *Model) handleStreamEvent(msg stream.EventMsg) (tea.Model, tea.Cmd) {
	// A stale event — from a stream ClearTranscript cancelled, or that a
	// later StartStream superseded — must never mutate the transcript: e.g.
	// AppendDelta's create-on-first-use fallback would otherwise resurrect a
	// transcript entry for an ID a ClearTranscript already removed. It still
	// re-arms the pump and still notifies StreamObserver (mirroring
	// handleStreamDone's stale-Done handling) so per-id product state can be
	// cleaned up either way.
	var tickCmd tea.Cmd
	if msg.ID == m.streamID {
		switch msg.Event.Type {
		case ai.EventTextDelta:
			if m.streamMarkdown {
				// Throttle the (potentially expensive) markdown re-render to
				// at most once per markdownRenderThrottle window, plus any
				// delta that crosses a newline — text still accumulates on
				// every delta via AppendDeltaNoRender regardless, so the
				// final Text is always complete even between re-renders.
				m.transcript.AppendDeltaNoRender(msg.ID, msg.Event.Text)
				if strings.Contains(msg.Event.Text, "\n") || m.now().Sub(m.lastMarkdownRender) >= markdownRenderThrottle {
					m.transcript.InvalidateAndRebuild(msg.ID)
					m.lastMarkdownRender = m.now()
					m.markdownTickPending = false
				} else if !m.markdownTickPending {
					// m1 (r3 review): this delta was throttled out. Without
					// a follow-up, its trailing fragment renders only when
					// the NEXT delta arrives (or the stream completes) —
					// if the stream stalls or that was the last delta
					// before a long gap, it would sit unrendered
					// indefinitely. Schedule one guaranteed re-render
					// ~markdownRenderThrottle out; markdownTickPending
					// guards against piling up a tick per throttled delta.
					m.markdownTickPending = true
					id := msg.ID
					tickCmd = m.tick(markdownRenderThrottle, func(time.Time) tea.Msg {
						return markdownRenderTickMsg{id: id}
					})
				}
			} else {
				m.transcript.AppendDelta(msg.ID, msg.Event.Text)
			}
		case ai.EventError:
			// Per the event contract, EventError with a nil Go error (this
			// path — a fatal error arrives as a DoneMsg instead, see
			// handleStreamDone) is non-fatal: report it but keep streaming.
			if msg.Event.Error != nil {
				m.AppendSystem("error: " + msg.Event.Error.Message)
			}
		}
	}
	cmds := []tea.Cmd{msg.Next}
	if tickCmd != nil {
		cmds = append(cmds, tickCmd)
	}
	if obs, ok := m.handler.(StreamObserver); ok {
		if cmd := obs.OnStreamEvent(msg.ID, msg.Event); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

// handleMarkdownRenderTick applies the m1 (r3 review) follow-up render
// scheduled by handleStreamEvent: a delta that was throttled out gets one
// guaranteed re-render ~markdownRenderThrottle later even with no further
// deltas. A tick for a superseded/cleared/no-longer-markdown stream is a
// no-op (mirrors handleStreamEvent's own staleness guard).
func (m *Model) handleMarkdownRenderTick(msg markdownRenderTickMsg) {
	m.markdownTickPending = false
	if msg.id != m.streamID || !m.streamMarkdown {
		return
	}
	m.transcript.InvalidateAndRebuild(msg.id)
	m.lastMarkdownRender = m.now()
}

func (m *Model) handleStreamDone(msg stream.DoneMsg) (tea.Model, tea.Cmd) {
	// A stale Done (superseded by a later StartStream, or already handled by
	// cancelBusy for the current one) still notifies StreamObserver, so a
	// product can clean up per-id state, but must not touch the CURRENT
	// stream's busy/transcript.
	if msg.ID == m.streamID {
		m.busy = false
		m.streamCancel = nil
		if m.streamMarkdown {
			// Guarantee the final accumulated text is rendered even if the
			// last delta(s) landed inside a throttle window and never
			// triggered their own re-render.
			m.transcript.InvalidateAndRebuild(msg.ID)
			m.streamMarkdown = false
			m.markdownTickPending = false
		}
		switch {
		case isCanceled(msg.Err):
			m.AppendSystem("(stopped)")
		case msg.Err != nil:
			m.AppendSystem("error: " + msg.Err.Error())
		}
	}
	var cmd tea.Cmd
	if obs, ok := m.handler.(StreamObserver); ok {
		cmd = obs.OnStreamDone(msg.ID, msg.Err)
	}
	return m, cmd
}

// isCanceled reports whether err represents a user-initiated cancellation:
// either tui/stream's own ctx.Done() race (context.Canceled) or, once the
// ai/ provider has translated it, ai.Error{Code: ai.ErrCodeCanceled}.
func isCanceled(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	var aerr *ai.Error
	return errors.As(err, &aerr) && aerr.Code == ai.ErrCodeCanceled
}

func (m *Model) resize() {
	m.transcript.SetSize(m.chatWidth(), m.historyHeight())
	m.input.SetWidth(max(1, m.chatWidth()-2))
	if m.sidePanel == nil && m.sidebar.Visible() {
		m.sidebar.SetWidth(m.sidebarWidth())
	}
}

func (m *Model) splitEnabled() bool { return m.width >= splitMinWidth && m.panelVisible() }

func (m *Model) chatWidth() int {
	if !m.splitEnabled() {
		return max(1, m.width-2)
	}
	inner := max(1, m.width-2)
	return max(42, min(inner-24, inner*m.panelChatPercent()/100))
}

func (m *Model) sidebarWidth() int {
	if !m.splitEnabled() {
		return 0
	}
	return max(1, m.width-2-m.chatWidth()-1)
}

func (m *Model) historyHeight() int {
	return max(1, m.height-4-len(m.statusLines()))
}

func (m *Model) statusLines() []string {
	if m.status == "" {
		return nil
	}
	return strings.Split(m.status, "\n")
}

func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.globalKeys != nil {
		if cmd, consumed := m.globalKeys(msg); consumed {
			return m, cmd
		}
	}
	if msg.String() != "ctrl+c" {
		m.ctrlCArmed = false
	}
	switch msg.String() {
	case "ctrl+c":
		if m.busy {
			if m.ctrlCArmed {
				// A second, immediately-following Ctrl+C always quits,
				// whether or not the first one's cancellation has finished.
				m.quit = true
				return m, tea.Quit
			}
			m.ctrlCArmed = true
			m.cancelBusy()
			return m, nil
		}
		m.quit = true
		return m, tea.Quit
	case "esc":
		// Esc's priority order: close an open slash-command menu first (it
		// stays closed until the input value changes); then cancel if busy;
		// then let a focused Block capture it (EscCapturer); then the
		// default focus-ring Esc (return to the composer).
		if m.focusRing.Zone() == focus.ZoneInput && len(m.commandMenuMatches()) > 0 {
			m.commandMenuDismissed = m.input.Value()
			m.commandMenuIndex = 0
			return m, nil
		}
		if m.busy {
			m.cancelBusy()
			return m, nil
		}
		if m.focusRing.Zone() == focus.ZoneTranscript && m.transcript.CapturesEsc() {
			cmd := m.transcript.Update(msg)
			return m, cmd
		}
		m.focusRing.Esc()
		m.syncFocus()
		return m, nil
	case "f6":
		m.togglePanel()
		if !m.panelVisible() && m.focusRing.Zone() == focus.ZoneSidebar {
			// Hiding the sidebar zone while it holds focus returns focus to
			// wherever it was before Shift+Right (or the input, if there is
			// nowhere to return to).
			m.focusRing.ShiftLeft(m.transcript.Stops())
			m.syncFocus()
		}
		m.resize()
		return m, nil
	case "ctrl+left", "ctrl+right":
		delta := -5
		if msg.String() == "ctrl+right" {
			delta = 5
		}
		m.growPanelChat(delta)
		m.resize()
		return m, nil
	case "shift+up":
		if m.focusRing.Zone() == focus.ZoneInput && strings.TrimSpace(m.input.Value()) != "" {
			break // let the composer handle cursor movement instead
		}
		if m.focusRing.ShiftUp(m.transcript.Stops()) {
			m.syncFocus()
			return m, nil
		}
	case "shift+down":
		if m.focusRing.ShiftDown(m.transcript.Stops()) {
			m.syncFocus()
			return m, nil
		}
	case "shift+right":
		if m.splitEnabled() && m.focusRing.ShiftRight() {
			m.syncFocus()
			return m, nil
		}
	case "shift+left":
		if m.focusRing.ShiftLeft(m.transcript.Stops()) {
			m.syncFocus()
			return m, nil
		}
		// Nothing to return to (not in the sidebar): fall through so the
		// key reaches the composer/transcript/sidebar normally instead of
		// being silently swallowed.
	}

	switch m.focusRing.Zone() {
	case focus.ZoneSidebar:
		cmd := m.updatePanel(msg)
		return m, cmd
	case focus.ZoneTranscript:
		cmd := m.transcript.Update(msg)
		return m, cmd
	default:
		return m.handleInputKey(msg)
	}
}

// now returns the current time for the markdown-render throttle, using
// nowFunc when a test has installed a fake clock.
func (m *Model) now() time.Time {
	if m.nowFunc != nil {
		return m.nowFunc()
	}
	return time.Now()
}

// tick schedules fn to fire after d, using tickFunc when a test has
// installed a fake one (m1, r3 review's markdown-render follow-up).
func (m *Model) tick(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	if m.tickFunc != nil {
		return m.tickFunc(d, fn)
	}
	return tea.Tick(d, fn)
}

// markdownRenderTickMsg is the m1 (r3 review) follow-up render: scheduled
// whenever a text delta accumulates without triggering an immediate
// markdown re-render (throttled out), it guarantees the trailing fragment
// still renders within ~markdownRenderThrottle even if no further delta
// ever arrives (e.g. the stream stalls, or that was simply the last delta
// before a long gap).
type markdownRenderTickMsg struct{ id string }

func (m *Model) syncFocus() {
	switch m.focusRing.Zone() {
	case focus.ZoneTranscript:
		m.input.Blur()
		m.transcript.Focus(m.focusRing.Stop())
	case focus.ZoneSidebar:
		m.input.Blur()
		m.transcript.Blur()
	default:
		m.transcript.Blur()
		m.input.Focus()
	}
}

func (m *Model) handleInputKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.busy {
		// The composer is disabled while busy (StartStream or a product
		// SetBusy(true) phase); Esc/Ctrl+C-to-cancel is handled earlier, in
		// handleKey, before we ever reach here.
		return m, nil
	}
	if matches := m.commandMenuMatches(); len(matches) > 0 {
		switch msg.String() {
		case "up":
			if m.commandMenuIndex > 0 {
				m.commandMenuIndex--
			}
			return m, nil
		case "down":
			if m.commandMenuIndex+1 < len(matches) {
				m.commandMenuIndex++
			}
			return m, nil
		case "enter", "tab":
			m.input.SetValue(matches[min(m.commandMenuIndex, len(matches)-1)].Name + " ")
			m.input.CursorEnd()
			m.commandMenuIndex = 0
			return m, nil
		}
	}
	switch msg.String() {
	case "shift+enter":
		m.input.InsertString("\n")
		return m, nil
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		m.input.Reset()
		m.commandMenuIndex = 0
		m.commandMenuDismissed = ""
		m.AppendUser(text)
		if m.handler == nil {
			return m, nil
		}
		return m, m.handler.Submit(text)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.commandMenuIndex = 0
	return m, cmd
}

func (m *Model) commandMenuMatches() []Command {
	value := m.input.Value()
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, " \t\n") || m.commandMenuDismissed == value {
		return nil
	}
	matches := make([]Command, 0, len(m.commands))
	for _, c := range m.commands {
		if strings.HasPrefix(c.Name, value) {
			matches = append(matches, c)
		}
	}
	return matches
}

func (m *Model) View() tea.View {
	top := lipgloss.NewStyle().Bold(true).Render(m.title)
	if m.topBarFn != nil {
		top = m.topBarFn(m.width)
	}
	history := m.transcript.View()
	if m.busy {
		history += "\n" + m.spinner.View() + " thinking…"
	}
	composer := m.input.View()
	menu := m.commandMenuView()
	chatParts := []string{history, composer}
	if menu != "" {
		chatParts = []string{history, menu, composer}
	}
	chat := lipgloss.JoinVertical(lipgloss.Left, chatParts...)
	body := chat
	if m.splitEnabled() {
		side := m.panelView(m.sidebarWidth(), m.focusRing.Zone() == focus.ZoneSidebar)
		body = lipgloss.JoinHorizontal(lipgloss.Top, chat, " │ ", side)
	}
	status := strings.Join(m.statusLines(), "\n")
	if m.statusBarFn != nil {
		status = m.statusBarFn(m.width)
	}
	content := lipgloss.JoinVertical(lipgloss.Left, top, body, status)
	if n := len(m.overlays); n > 0 {
		content = m.renderOverlay(content, m.overlays[n-1])
	}
	view := tea.NewView(content)
	view.AltScreen = true
	if m.mouseEnabled {
		view.MouseMode = m.mouseMode.mouseTeaMode()
	}
	return view
}

// renderOverlay composes the top overlay's view centred over base.
func (m *Model) renderOverlay(base string, o Overlay) string {
	ow, oh := max(1, m.width*2/3), max(1, m.height*2/3)
	overlayView := o.View(ow, oh)
	// Clamp regardless of what the overlay actually drew: a misbehaving or
	// content-driven Overlay.View must never blow out the layout past the
	// box it was asked to render into.
	clamped := lipgloss.NewStyle().MaxWidth(ow).MaxHeight(oh).Render(overlayView)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, clamped, lipgloss.WithWhitespaceChars(" "))
}

func (m *Model) commandMenuView() string {
	matches := m.commandMenuMatches()
	if len(matches) == 0 {
		return ""
	}
	lines := make([]string, 0, len(matches)+1)
	lines = append(lines, "Commands · ↑↓ choose · Enter insert · Esc close")
	for i, c := range matches {
		prefix := "  "
		if i == m.commandMenuIndex {
			prefix = "› "
		}
		lines = append(lines, prefix+c.Name+"  "+c.Help)
	}
	return strings.Join(lines, "\n")
}
