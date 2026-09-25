package chatshell

import (
	"context"
	"errors"
	"image/color"
	"iter"
	"reflect"
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
	"github.com/strongo/aichat/tui/theme"
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
//
// An Overlay implementation MUST be a POINTER type (r3 review, minor 1):
// updateOverlay stores whatever value Update returns back into the stack
// (m.overlays[top] = updated), so a value-typed Overlay's mutations inside
// Update are trivially preserved that way regardless -- but CloseOverlay's
// identity match (see its doc) can only ever find a POINTER back on the
// stack, since Go's interface equality on a struct value compares fields,
// not "is this the same logical dialog", and a value Overlay a product
// still holds a copy of will never == the (possibly mutated, definitely
// re-wrapped) value Update last returned. A product using the async-safe
// overlay pattern (see PopOverlay/CloseOverlay) MUST hold and pass the same
// *T it originally gave PushOverlay.
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

// WithSidebarRenderer sets how sidebar entries render. Preserves any
// title a WithSidebarTitle call already set (in either order) by reading
// the previous m.sidebar's own Title() before replacing it wholesale.
func WithSidebarRenderer(render sidebar.Renderer) Option {
	return func(m *Model) { m.sidebar = sidebar.New(render).WithTitle(m.sidebar.Title()) }
}

// WithSidebarTitle sets the default sidebar's header text (default
// "Pinned") -- content only, e.g. a product's own name for its
// working-context list (founder, r11, sneat-cli coordinator review: "any
// product using the default sidebar" should be able to give it a proper
// header instead of the internal "Sidebar" implementation name). A no-op
// for a product using WithSidePanel instead -- a SidePanel supplies its
// own header entirely.
func WithSidebarTitle(title string) Option {
	return func(m *Model) { m.sidebar = m.sidebar.WithTitle(title) }
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

// WithTopBar sets a product-rendered top bar, replacing the default themed
// title line. Prefer WithTopBarProvider (content-only: title/context/menu
// items, rendered through the shared theme.TopBar chrome) — WithTopBar
// remains for a product that needs a top bar theme.TopBar can't express;
// it fully replaces chatshell's own styling, so nothing here applies to it.
func WithTopBar(render func(width int) string) Option {
	return func(m *Model) { m.topBarFn = render }
}

// WithStatusBar sets a product-rendered status bar, replacing the default
// SetStatus-driven status line(s). Prefer WithHintsProvider (content-only:
// a Hint list plus trailing segments, rendered through the shared
// theme.RenderHints chrome) — WithStatusBar remains for a product that
// needs a status bar theme.RenderHints can't express; it fully replaces
// chatshell's own styling, so nothing here applies to it.
func WithStatusBar(render func(width int) string) Option {
	return func(m *Model) { m.statusBarFn = render }
}

// TopBarProvider returns the top bar's CONTENT for width: a title, an
// optional context string (e.g. the current project/space), and menu items
// (with the active one marked) — chatshell renders it through the shared
// theme.TopBar chrome every render, so the product supplies content only,
// never colour. It takes priority over WithTopBar/the plain WithTitle
// default when set.
type TopBarProvider func(width int) (title, context string, items []theme.MenuItem)

// WithTopBarProvider sets a content-only top bar (see TopBarProvider).
func WithTopBarProvider(provide TopBarProvider) Option {
	return func(m *Model) { m.topBarProvider = provide }
}

// HintsProvider returns the status/hints bar's CONTENT for width: the
// current key hints (e.g. {"Enter", "send"}) plus optional trailing
// segments (e.g. a product/session summary, a hyperlink) — chatshell
// renders it through the shared theme.RenderHints chrome every render, so
// the product supplies content only, never colour. It takes priority over
// WithStatusBar/the plain SetStatus default when set.
type HintsProvider func(width int) (hints []theme.Hint, segments []string)

// WithHintsProvider sets a content-only status/hints bar (see
// HintsProvider).
func WithHintsProvider(provide HintsProvider) Option {
	return func(m *Model) { m.hintsProvider = provide }
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
	topBarProvider       TopBarProvider
	hintsProvider        HintsProvider
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

	// chips are the composer's attachment chips, rendered above the input
	// (see chip.go). chipFocus is the focused chip's index into chips, or -1
	// when none is focused (the input itself holds keyboard focus).
	//
	// composerUndo, when non-nil, is a snapshot of BOTH the composer text
	// and the chip list as they stood immediately before the FIRST change
	// (Esc's text-clear step, Esc's chip-clear step, a chip removal, or
	// ClearChips) since the last successful Shift+Esc/Ctrl+Y restore, submit,
	// or composer text edit — see snapshotComposerUndo.
	chips        []Chip
	chipFocus    int
	composerUndo *composerDraft
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
		chipFocus: -1,
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

// entryFocusable mirrors transcript.Entry's own (unexported) focusability
// rule: a Block entry defers to its Block.Focusable(), a plain-text entry is
// focusable only when it's a user message. ReplaceBlock needs this because
// it tracks the focused entry by RAW POSITION rather than by ID (see
// stopForRawIndex): transcript.StopForID/FocusedEntry alone cannot be
// trusted for that purpose across a ReplaceBlock call — see below.
func entryFocusable(e transcript.Entry) bool {
	if e.Block != nil {
		return e.Block.Focusable()
	}
	return e.Role == transcript.RoleUser
}

// stopForRawIndex returns the stop number entries[idx] occupies (a count of
// focusable entries up to and including idx), or -1 when idx is out of range
// or entries[idx] isn't itself focusable.
func stopForRawIndex(entries []transcript.Entry, idx int) int {
	if idx < 0 || idx >= len(entries) || !entryFocusable(entries[idx]) {
		return -1
	}
	stop := -1
	for i := 0; i <= idx; i++ {
		if entryFocusable(entries[i]) {
			stop++
		}
	}
	return stop
}

// ReplaceBlock refreshes/re-runs a grid (or any other transcript.Block) in
// place: the entry identified by entryID keeps its position and ID, but
// renders b from now on. If that entry currently holds transcript focus, it
// keeps it (the focus ring stop is recomputed for the entry, not left
// pointing at whatever raw index it used to occupy — necessary because
// replacing a Block can change its own Focusable() answer).
//
// The previously-focused entry is tracked by its RAW POSITION in
// m.transcript.Entries(), not by transcript.StopForID(fe.ID): that ID-keyed
// lookup always reports -1 for an entry with an empty ID (which
// AppendUser/AppendAssistant/plain AppendBlock all leave empty — only
// AppendBlockWithID sets one), and transcript.ReplaceBlock's own internal
// focus cursor likewise only re-resolves itself by ID when the focused entry
// has one. So a no-ID focused entry would otherwise silently look unfocused
// (or land on the wrong stop) the moment an EARLIER entry's swap shifts stop
// numbers — even though that entry's own Block never changed. Raw position
// is safe to track across the call because transcript.ReplaceBlock only
// mutates one entry's Block in place; it never reorders, inserts or removes
// entries.
//
// m.focusRing (chatshell's own zone/stop tracker) is resynced to whatever
// was decided too: syncFocus later reapplies focusRing.Stop() into the
// transcript on the next zone change or resize, so leaving it stale would
// silently undo the recomputed focus the next time that happens.
func (m *Model) ReplaceBlock(entryID string, b transcript.Block) {
	var hadFocus bool
	var oldStop int
	focusedRawIdx := -1
	if m.focusRing.Zone() == focus.ZoneTranscript {
		if fe := m.transcript.FocusedEntry(); fe != nil {
			hadFocus = true
			entries := m.transcript.Entries()
			for i := range entries {
				if &entries[i] == fe {
					focusedRawIdx = i
					break
				}
			}
			oldStop = stopForRawIndex(entries, focusedRawIdx)
		}
	}

	m.transcript.ReplaceBlock(entryID, b)

	if m.focusRing.Zone() != focus.ZoneTranscript {
		return
	}
	if hadFocus {
		// Re-derive the stop directly from the tracked raw position, rather
		// than trusting transcript.FocusedEntry()/StopForID after the swap
		// (see the no-ID staleness note above).
		if stop := stopForRawIndex(m.transcript.Entries(), focusedRawIdx); stop >= 0 {
			m.focusRing.FocusStop(stop)
			// transcript's own focus index only re-resolves itself by ID,
			// which is a no-op for a no-ID entry (see above): push the
			// recomputed stop into it explicitly, or the highlight stays
			// wherever transcript.ReplaceBlock left it (lost, or on the
			// wrong Block) until some unrelated zone change/resize incidentally
			// calls syncFocus next.
			m.syncFocus()
			return
		}
	} else if fe := m.transcript.FocusedEntry(); fe != nil {
		// No entry was focused before the swap, but one now resolves at
		// chatshell's unchanged focusRing.Stop() (e.g. a stop that pointed
		// past the end now lands on a newly-focusable entry): resync to it.
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
	// transcript has no focusable entry left at all. oldStop is never
	// negative here: it's stopForRawIndex of the entry FocusedEntry() just
	// reported focused, so only the upper clamp is reachable.
	if n := m.transcript.Stops(); n > 0 {
		newStop := oldStop
		if newStop >= n {
			newStop = n - 1
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
//
// ClearTranscript also drops any pending Shift+Esc/Ctrl+Y composer-draft
// snapshot and clears chip focus (M1, r1 review) -- e.g. a session switch,
// where the OLD session's "undo my last chip removal" and chip-row cursor
// position no longer mean anything against the NEW session's own chips
// (which a product typically installs right after via SetChips). It does
// NOT itself clear m.chips: which chips belong to the new session is the
// product's call, made via SetChips, not ClearTranscript's.
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
	m.composerUndo = nil
	m.chipFocus = -1
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

// PopOverlay closes the TOP overlay PROGRAMMATICALLY -- without waiting for
// its own Update to report done. It is a no-op (returns nil) when no
// overlay is open.
//
// PopOverlay is TOP-ONLY: it closes whatever happens to be on top at the
// moment it is called, regardless of which overlay a caller "meant". That
// is exactly right for the common case (at most one overlay is ever open at
// a time), but WRONG for the async-safe pattern below once a SECOND overlay
// can be stacked on top of the first before its async result arrives (r3
// review, MAJOR) -- e.g. dialog A's submit is in flight, the user opens
// dialog B on top of it, and A's result lands: PopOverlay would close B,
// not A. Use CloseOverlay(o) instead whenever more than one overlay might
// ever be on the stack at once; PopOverlay remains for the simpler
// single-overlay case (or for closing "whatever's on top" on purpose, e.g.
// an Esc-equivalent product action).
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
// From there the product calls CloseOverlay(o) with the SAME *T it passed
// to PushOverlay on success (see Overlay's doc: it MUST be a pointer type),
// or -- to show an error while keeping the user's draft -- updates the
// overlay in place (e.g. via an optional `interface{ OnResult(any) }`
// capability the product's own Overlay implements, or simply because the
// product holds that same pointer and can mutate it directly).
func (m *Model) PopOverlay() tea.Cmd {
	if len(m.overlays) == 0 {
		return nil
	}
	m.overlays = m.overlays[:len(m.overlays)-1]
	return nil
}

// CloseOverlay removes o from the overlay stack WHEREVER IT IS -- not only
// if it's on top -- matching by POINTER IDENTITY (see Overlay's doc: an
// Overlay MUST be a pointer type for this to ever find it). It reports
// whether o was found and removed; false is a no-op. This is the
// identity-safe replacement for PopOverlay in the async-safe overlay
// pattern once a second overlay might be stacked on top of the one an
// async result is meant to close (r3 review, MAJOR -- see PopOverlay's
// doc).
//
// A non-pointer Overlay (or a nil pointer) can never be matched -- o's
// pointer identity is extracted via reflection rather than Go's `==`
// specifically to avoid a runtime panic comparing two interface values
// whose dynamic type is non-comparable (e.g. one holding a slice or map
// field); CloseOverlay simply reports false for such an Overlay instead of
// crashing.
func (m *Model) CloseOverlay(o Overlay) bool {
	target, ok := overlayIdentity(o)
	if !ok {
		return false
	}
	for i, existing := range m.overlays {
		if id, ok := overlayIdentity(existing); ok && id == target {
			m.overlays = append(m.overlays[:i], m.overlays[i+1:]...)
			return true
		}
	}
	return false
}

// overlayIdentity extracts o's pointer identity for CloseOverlay's match,
// or reports ok=false when o is not a non-nil pointer (reflect.Value.
// Pointer panics on most other kinds, which this guards against).
func overlayIdentity(o Overlay) (uintptr, bool) {
	v := reflect.ValueOf(o)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return 0, false
	}
	return v.Pointer(), true
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
// default sidebar) wrapped in the shared theme.PanelFrame (founder
// 2026-09-25: "Same for ... side panel" — a full frame, focus-bordered
// when the zone has focus, matching every other bordered element): width
// is the frame's OUTER width, so the content itself (and the height a
// SidePanel is asked to lay out) is sized to PanelFrameSize's smaller
// inner box via panelInnerWidth/panelInnerHeight — never the raw column
// width chatshell allotted the whole sidebar zone.
func (m *Model) panelView(width int, focused bool) string {
	inner := m.panelInnerWidth(width)
	innerHeight := m.panelInnerHeight()
	var content string
	if m.sidePanel != nil {
		content = m.sidePanel.View(inner, innerHeight, focused)
	} else {
		content = m.sidebar.View(inner, focused)
	}
	// header is always "" here: both a SidePanel and the default sidebar
	// already render their own header as their content's own first line
	// (sidebar.Model.View's own theme.PanelHeader call) -- PanelFrame's
	// header parameter exists for a future caller that wants PanelFrame to
	// supply it instead, not used by chatshell today.
	return theme.PanelFrame(width, innerHeight, "", content, focused)
}

// panelInnerWidth returns the content width available INSIDE
// theme.PanelFrame for a panel of the given OUTER width.
func (m *Model) panelInnerWidth(width int) int {
	cols, _ := theme.PanelFrameSize()
	return max(1, width-cols)
}

// chatColumnHeight is the TOTAL row count the chat column occupies in
// View(): history (plus its trailing busy-spinner line when Busy()), the
// open slash-command menu, the pre-chips/composer margin row, the chip
// row, and the composer itself -- exactly what View() stacks into
// chatParts before lipgloss.JoinHorizontal joins it with the side panel.
// The side panel must span this SAME total (see panelInnerHeight), not
// just historyHeight() alone -- founder, r12 coordinator review,
// verbatim: "the panel spans the transcript area AND the composer rows
// (the composer sits only in the left column)" -- the earlier
// historyHeight()-only budget left the panel exactly composerHeight()+
// theme.ContentMargins(m.height) rows short of the composer's own bottom
// edge (a real regression the coordinator measured: "panel bottom edge
// on row 25 while the composer's bottom edge is on row 29").
func (m *Model) chatColumnHeight() int {
	busySpinnerLine := 0
	if m.busy {
		busySpinnerLine = 1
	}
	return m.historyHeight() + busySpinnerLine + m.menuHeight() + theme.ContentMargins(m.height) + m.chipsHeight(m.chatWidth()) + m.composerHeight()
}

// panelInnerHeight returns the content height available INSIDE
// theme.PanelFrame, given the frame's own top/bottom border rows —
// chatColumnHeight() is the OUTER row budget the chat column and the
// side panel column both match (View joins them side by side), so the
// panel's own content must be that minus PanelFrameSize's row overhead
// to keep the two columns' BOTTOM edges aligned.
func (m *Model) panelInnerHeight() int {
	_, rows := theme.PanelFrameSize()
	return max(1, m.chatColumnHeight()-rows)
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

// Init starts the composer's cursor blink and asks the terminal for its
// ACTUAL background colour (OSC 11, via bubbletea's own
// tea.RequestBackgroundColor()/tea.BackgroundColorMsg) — founder
// 2026-09-25 (r10 coordinator review, verbatim): "derive surface tints
// from the ACTUAL terminal background ... Fallback to the current
// defaults when the terminal doesn't answer. Chatshell applies the
// message; products do nothing." See Update's tea.BackgroundColorMsg case
// for where the answer, if any, gets applied (theme.SetTerminalBackground)
// — a terminal that never answers simply leaves theme's own guessed
// default in effect, exactly as before this feature existed.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, tea.RequestBackgroundColor)
}

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
	case tea.BackgroundColorMsg:
		// The terminal answered Init's tea.RequestBackgroundColor() (OSC
		// 11) — apply it so every card/composer surface tint derives from
		// the REAL background instead of theme's guessed default. See
		// Init's own doc; a product does nothing further.
		theme.SetTerminalBackground(msg.Color)
		return m, nil

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

	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)

	default:
		return m, m.dispatchUnhandled(msg)
	}
}

// mouseWheelScrollLines is how many transcript lines one wheel tick moves,
// matching a typical terminal's own default scroll step.
const mouseWheelScrollLines = 3

// splitSeparatorWidth is 0: View() draws NO gap of its own between the
// chat column and the side panel/sidebar when split (see View, chatWidth/
// sidebarWidth) -- theme.PanelFrame draws the panel's own single-column
// divider as the FIRST column of the side panel itself (founder
// 2026-09-25: "no boxes around boxes" — one divider, not a chatshell gap
// plus a PanelFrame border on top of it), so X == chatWidth() is already
// the divider/side column, not a gap before it. handleMouseWheel uses
// this to tell whether a wheel event's X falls in the chat column or the
// side column.
const splitSeparatorWidth = 0

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

	switch {
	case m.splitEnabled() && msg.X >= m.chatWidth()+splitSeparatorWidth:
		// Side column: ONLY the SidePanel/sidebar gets it -- the transcript
		// has nothing to do with a wheel event over the sidebar/SidePanel
		// column (r4 review: broadcasting to transcript Blocks here, as an
		// earlier revision did, read backwards -- there is no reason a
		// wheel tick over the SIDEBAR should reach the TRANSCRIPT).
		//
		// A product SidePanel gets the raw event (free to interpret
		// X/Y/Button itself); the BUILT-IN sidebar has no scroll offset of
		// its own -- it is a cursor list -- so a wheel tick moves its
		// cursor the same way Up/Down would (r2 review minor: "scroll the
		// sidebar" for a cursor list IS moving the cursor).
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
		// Chat column: the FOCUSED transcript Block gets first refusal (r4
		// review) via transcript.WheelConsumer -- a Block that wants to
		// scroll its own internal view (e.g. a grid's row list) for this
		// event says so, and chatshell delivers the message to it INSTEAD
		// OF scrolling the transcript viewport itself; only when no Block
		// is focused, the focused Block doesn't implement WheelConsumer, or
		// it declines this particular event does chatshell fall back to
		// scrolling the viewport directly. Exactly one of the two ever
		// happens for one wheel tick -- never both, so a Block handling the
		// wheel itself is never double-moved by chatshell's own scroll.
		if consumed, cmd := m.transcript.DeliverWheelToFocusedBlock(msg); consumed {
			cmds = append(cmds, cmd)
		} else {
			switch msg.Button {
			case tea.MouseWheelUp:
				m.transcript.ScrollUp(mouseWheelScrollLines)
			case tea.MouseWheelDown:
				m.transcript.ScrollDown(mouseWheelScrollLines)
			}
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
	composerCols, _ := theme.ComposerFrameSize()
	m.input.SetWidth(max(1, m.chatWidth()-composerCols))
	if m.sidePanel == nil && m.sidebar.Visible() {
		m.sidebar.SetWidth(m.panelInnerWidth(m.sidebarWidth()))
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
	// The trailing "-1" this used to subtract dated back to
	// theme.PanelFrame's own single-column "│" divider, reserved here on
	// TOP of chatWidth()'s own budget. PanelFrame no longer draws
	// anything of its own outside the OUTER width panelView already
	// passes it (its 2-column gap+marker come out of THAT budget, via
	// panelInnerWidth) -- so the panel's own outer width is simply
	// whatever remains after chatWidth(), with no separate frame
	// allowance here. Kept as -1 after this round-10 gap/marker redesign
	// would have shrunk the panel's usable content by one extra column
	// for no reason, tipping tabStripHeader from its full label set to
	// the short one at borderline widths -- a real r10 regression this
	// fixes.
	return max(1, m.width-2-m.chatWidth())
}

// historyHeight is the transcript viewport's fixed height: the terminal
// height minus every OTHER row View() stacks around it -- the top bar
// (topBarHeight, which a product's own WithTopBar may render as more than
// one line), the open slash-command menu (menuHeight, 0 when it isn't
// showing), the chip row(s) (chipsHeight), the composer's own content line
// plus its theme.ComposerFrame border rows (composerHeight), the status
// line(s) (statusSegmentHeight), and -- while Busy() -- the spinner's own
// trailing "\n" + text line View() appends after the transcript (r2
// review, B1: that line was previously NOT reserved, so View() rendered
// one line taller than m.height for the entire duration of a stream) -- so
// that View()'s total rendered height always equals m.height exactly (a
// pre-existing gap this REQ fixes: historyHeight previously assumed a
// constant "4" rows of chrome, silently wrong once a product's top bar
// wrapped to more than one line, the slash-command menu was open, or busy,
// either under- or over-filling the screen).
//
// historyHeight is PURE (always computed fresh from current state) -- it is
// View() re-applying it to m.transcript's actual viewport size, on EVERY
// render (not just after WindowSizeMsg/F6/a chip change), that keeps the
// transcript's rendered size from ever going stale relative to it; see
// View's own doc.
func (m *Model) historyHeight() int {
	busySpinnerLine := 0
	if m.busy {
		busySpinnerLine = 1
	}
	// marginRows accounts for all THREE blank rows View() inserts when the
	// terminal is tall enough (theme.ContentMargins): one between the top
	// bar and the content below it, one between the last transcript card
	// and the composer, and (r12) one between the composer and the
	// status bar -- but only when statusBarVisible() (there's nothing to
	// separate the composer FROM when there's no status bar at all -- see
	// View()'s own doc, and statusBarVisible's). All collapse to 0 below
	// theme.MarginCollapseRows terminal rows, same as here.
	marginCount := 2
	if m.statusBarVisible() {
		marginCount = 3
	}
	marginRows := marginCount * theme.ContentMargins(m.height)
	return max(1, m.height-m.topBarHeight()-m.menuHeight()-m.composerHeight()-m.statusSegmentHeight()-m.chipsHeight(m.chatWidth())-busySpinnerLine-marginRows)
}

// composerHeight is the composer's total rendered row count: its own
// single content line plus theme.ComposerFrame's top/bottom border rows.
func (m *Model) composerHeight() int {
	_, rows := theme.ComposerFrameSize()
	if m.composerUsesChipsAsTopEdge() {
		// The composer's own top edge row is omitted -- the chip row
		// immediately above it (already counted by chipsHeight) performs
		// that role instead (see View()'s doc and theme.
		// ComposerFrameNoTopEdge).
		rows--
	}
	return 1 + rows
}

// composerUsesChipsAsTopEdge reports whether View() will render the
// composer via theme.ComposerFrameNoTopEdge with the chip strip acting as
// its top half-block edge, instead of theme.ComposerFrame's own top edge
// -- true exactly when there's at least one chip AND half-block edges are
// actually active (theme.HalfBlockEdgesActive()); in fallback mode the
// chip row still renders as its own separate line above a full
// ComposerFrame, unchanged from before this feature existed.
func (m *Model) composerUsesChipsAsTopEdge() bool {
	return len(m.chips) > 0 && theme.HalfBlockEdgesActive()
}

// topBarHeight is the rendered top bar's line count -- 1 for the default
// bold title, or however many lines a product's own WithTopBar renders.
func (m *Model) topBarHeight() int {
	return strings.Count(m.topBarView(), "\n") + 1
}

// menuHeight is the open slash-command menu's rendered line count, or 0
// when it isn't currently showing.
func (m *Model) menuHeight() int {
	menu := m.commandMenuView()
	if menu == "" {
		return 0
	}
	return strings.Count(menu, "\n") + 1
}

// statusBarVisible reports whether View() renders ANY status/hints
// segment at all -- false only when there is truly nothing to show: no
// WithHintsProvider, no legacy WithStatusBar, and SetStatus was never
// given non-empty text. False means View() omits BOTH the pre-status
// margin row and the status segment itself, so the composer's own bottom
// edge becomes the terminal's literal last row -- founder, r12, verbatim,
// seeing the rendered result in Warp: "with no hints, the composer's
// bottom edge must be the last screen line (no trailing empty rows);
// with hints, the hints are the last line(s)."
func (m *Model) statusBarVisible() bool {
	return m.hintsProvider != nil || m.statusBarFn != nil || m.status != ""
}

// statusSegmentHeight is how many rows View()'s status segment occupies:
// 0 when statusBarVisible() is false (nothing to show at all -- see its
// own doc), the status text's own line count when SetStatus has been
// given something, or 1 for an active hints/status provider whose own
// text happens to be empty -- lipgloss.JoinVertical still renders one
// blank row for an EMPTY final segment, same as it would for a one-line
// one, so an active-but-empty status is not "0 rows of chrome" (only a
// genuinely ABSENT one is).
func (m *Model) statusSegmentHeight() int {
	if !m.statusBarVisible() {
		return 0
	}
	if m.status == "" {
		return 1
	}
	return len(m.statusLines())
}

func (m *Model) statusLines() []string {
	if m.status == "" {
		return nil
	}
	return strings.Split(m.status, "\n")
}

// statusBarView renders the status/hints bar, in priority order: a
// WithHintsProvider content provider (rendered through the shared
// theme.RenderHints chrome), else a legacy WithStatusBar full-string
// function, else the plain SetStatus text wrapped in the shared theme.Bar
// chrome -- every path, including the true default, now renders through
// tui/theme (founder 2026-09-25).
func (m *Model) statusBarView() string {
	switch {
	case m.hintsProvider != nil:
		hints, segments := m.hintsProvider(m.width)
		return theme.RenderHints(m.width, hints, segments...)
	case m.statusBarFn != nil:
		return m.statusBarFn(m.width)
	default:
		// theme.Bar truncates/pads a single line; a multi-line SetStatus
		// value (e.g. a product reporting several status rows at once) gets
		// the same chrome applied line by line, not truncated across the
		// whole block.
		lines := m.statusLines()
		if len(lines) == 0 {
			lines = []string{""}
		}
		styled := make([]string, len(lines))
		for i, line := range lines {
			styled[i] = theme.StatusLine(m.width, line)
		}
		return strings.Join(styled, "\n")
	}
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
	case "shift+esc", "ctrl+y":
		// Restores the composer draft (text + chips) as it stood before the
		// most recent run of changes (see snapshotComposerUndo). A no-op
		// (falls through to the zone dispatch below, same as any key
		// chatshell doesn't claim) when busy or when there is nothing to
		// restore.
		if !m.busy {
			if cmd, ok := m.restoreComposerDraft(); ok {
				return m, cmd
			}
		}
	case "esc":
		// Esc's priority order: close an open slash-command menu first (it
		// stays closed until the input value changes); then cancel if busy;
		// then -- in the input zone -- the composer's own two-step clear
		// (clearComposerStep: text first, then chips); then let a focused
		// Block capture it (EscCapturer); then the default focus-ring Esc.
		if m.focusRing.Zone() == focus.ZoneInput && len(m.commandMenuMatches()) > 0 {
			m.commandMenuDismissed = m.input.Value()
			m.commandMenuIndex = 0
			return m, nil
		}
		if m.busy {
			m.cancelBusy()
			return m, nil
		}
		if m.focusRing.Zone() == focus.ZoneInput {
			if cmd, cleared := m.clearComposerStep(); cleared {
				return m, cmd
			}
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
		m.chipFocus = -1
		m.input.Blur()
		m.transcript.Focus(m.focusRing.Stop())
	case focus.ZoneSidebar:
		m.chipFocus = -1
		m.input.Blur()
		m.transcript.Blur()
	default:
		// Any explicit zone-change back to the composer (Esc, Shift+Down
		// past the last transcript stop, ...) hands keyboard focus to the
		// input, so a stale chip focus (from before the zone changed away)
		// is cleared too -- cycleChipFocus is the only other place that
		// moves chip focus, and it manages input.Focus()/Blur() itself
		// without going through syncFocus.
		m.chipFocus = -1
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
	// Chip focus/removal is checked before the plain composer keys below: a
	// chip row only exists above the input when there's at least one chip,
	// and Tab/Shift+Tab must reach it before any other interpretation.
	if len(m.chips) > 0 {
		switch msg.String() {
		case "tab", "shift+tab":
			m.cycleChipFocus(msg.String() == "tab")
			return m, nil
		case "ctrl+d":
			// Built-in "remove the last chip" shortcut (DataTug's own
			// Ctrl+D), independent of chip focus. Snapshots like any other
			// removal, so Shift+Esc/Ctrl+Y can undo it.
			return m, m.removeChipAt(len(m.chips) - 1)
		}
		if m.chipFocus >= 0 {
			switch msg.String() {
			case "left":
				if m.chipFocus > 0 {
					m.chipFocus--
				}
				return m, nil
			case "right":
				if m.chipFocus < len(m.chips)-1 {
					m.chipFocus++
				}
				return m, nil
			case "backspace", "delete":
				return m, m.removeChipAt(m.chipFocus)
			}
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
		m.composerUndo = nil
		// m1: submitting with a chip focused ends chip focus and returns
		// keyboard focus to the input, same as any other way of leaving the
		// chip row.
		m.chipFocus = -1
		m.input.Focus()
		m.AppendUser(text)
		if m.handler == nil {
			return m, nil
		}
		return m, m.handler.Submit(text)
	}
	previousValue := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() != previousValue {
		// Any composer text edit drops a pending Shift+Esc/Ctrl+Y draft,
		// matching DataTug's own composerUndo reset on input change --
		// once the user has typed something new, "undo" no longer refers
		// to a coherent prior state.
		m.composerUndo = nil
	}
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

// topBarView renders the top bar, in priority order: a WithTopBarProvider
// content provider (rendered through the shared theme.TopBar chrome), else
// a legacy WithTopBar full-string function, else the default themed title
// line (theme.TopBar with just WithTitle's title, no context/items) --
// every path (including the true default, no options at all) now renders
// through tui/theme, so the polished look is the default, not something a
// product opts into (founder 2026-09-25). Factored out of View so
// chipsTopY (mouse hit-testing) can measure its actual rendered height
// instead of assuming one line -- a legacy WithTopBar function is free to
// render more than one.
func (m *Model) topBarView() string {
	switch {
	case m.topBarProvider != nil:
		title, context, items := m.topBarProvider(m.width)
		return theme.TopBar(m.width, title, context, items)
	case m.topBarFn != nil:
		return m.topBarFn(m.width)
	default:
		return theme.TopBar(m.width, m.title, "", nil)
	}
}

// View renders the chat screen. It re-applies resize() FIRST, on every
// call (r2 review, B1) -- not only in response to WindowSizeMsg/F6/Ctrl+
// Left/Right/a chip-list change, the only events that previously called
// it -- because historyHeight() (and therefore how tall the transcript
// SHOULD be) also depends on state that changes without going through any
// of those: typing "/" opens the slash-command menu, SetStatus changes the
// status segment's height, and SetBusy(true)/StartStream reserves the
// spinner line. Without this, m.transcript's ACTUAL viewport size (set via
// SetSize, and otherwise sticky) drifts from the CURRENT historyHeight()
// value between renders -- both the total rendered line count (over- or
// under-filling the screen) and chipsTopY's click math (computed fresh
// from the CURRENT historyHeight() at click time, but answering for
// whatever was ACTUALLY drawn by the last, possibly stale, render) go
// wrong. Calling resize() here is cheap (it only sets sizes) and
// idempotent, so doing it unconditionally on every render is simpler and
// more robust than hunting down every call site that can change chrome
// height.
// composerTextAreaStyles returns textarea styles that paint NO background
// of their own: every StyleState field below sets at most a foreground
// colour (never Background), so the composer's own ComposerFrame fill
// (theme.SurfaceColors unfocused, theme.ComposerFocusColors focused) shows
// through completely, composited via theme.PaintOver.
//
// This replaces bubbles' own textarea.DefaultStyles(), whose
// Focused.CursorLine sets an OPAQUE Background (white in light mode, pure
// black in dark mode) across the ENTIRE line the cursor sits on -- for a
// single-line composer, that is the whole input. theme.PaintOver only
// reasserts a fill after a bare ANSI reset; it cannot see through another
// explicit background SGR the nested content emits, so that background
// painted straight over the composer's own fill -- the "large bright
// light-blue slab with a BLACK inner input line" regression a round-9
// coordinator render caught (verbatim: typed/placeholder text was legible
// only because it happened to sit on that unintended black band, not
// because the composer's OWN fill was showing).
//
// Text/CursorLine/Placeholder foregrounds match whichever composer surface
// is currently showing (unfocused vs focused), verified against
// bodyTextMinRatio/placeholderMinRatio by theme.TestContrastMeetsWCAG's
// composer pairs -- so typed text and the placeholder stay readable in
// both focus states and both Dark variants without this package picking
// its own colour literals (theme remains the one place that decides).
func composerTextAreaStyles(focused bool) textarea.Styles {
	_, blurredFG := theme.SurfaceColors()
	_, focusedFG := theme.ComposerFocusColors()
	placeholderFG := theme.MutedColor()

	state := func(textFG color.Color) textarea.StyleState {
		return textarea.StyleState{
			Base:        lipgloss.NewStyle(),
			Text:        lipgloss.NewStyle().Foreground(textFG),
			CursorLine:  lipgloss.NewStyle().Foreground(textFG),
			Placeholder: lipgloss.NewStyle().Foreground(placeholderFG),
			Prompt:      lipgloss.NewStyle(),
			EndOfBuffer: lipgloss.NewStyle(),
			LineNumber:  lipgloss.NewStyle(),
		}
	}
	cursorFG := blurredFG
	if focused {
		cursorFG = focusedFG
	}
	return textarea.Styles{
		Blurred: state(blurredFG),
		Focused: state(focusedFG),
		Cursor:  textarea.CursorStyle{Color: cursorFG, Shape: tea.CursorBlock, Blink: true},
	}
}

// View renders the chat screen. It re-applies resize() FIRST, on every
// call (r2 review, B1) -- not only in response to WindowSizeMsg/F6/Ctrl+
// Left/Right/a chip-list change, the only events that previously called
// it -- because historyHeight() (and therefore how tall the transcript
// SHOULD be) also depends on state that changes without going through any
// of those: typing "/" opens the slash-command menu, SetStatus changes the
// status segment's height, and SetBusy(true)/StartStream reserves the
// spinner line. Without this, m.transcript's ACTUAL viewport size (set via
// SetSize, and otherwise sticky) drifts from the CURRENT historyHeight()
// value between renders -- both the total rendered line count (over- or
// under-filling the screen) and chipsTopY's click math (computed fresh
// from the CURRENT historyHeight() at click time, but answering for
// whatever was ACTUALLY drawn by the last, possibly stale, render) go
// wrong. Calling resize() here is cheap (it only sets sizes) and
// idempotent, so doing it unconditionally on every render is simpler and
// more robust than hunting down every call site that can change chrome
// height.
func (m *Model) View() tea.View {
	m.resize()
	inputFocused := m.focusRing.Zone() == focus.ZoneInput
	m.input.SetStyles(composerTextAreaStyles(inputFocused))
	top := m.topBarView()
	history := m.transcript.View()
	if m.busy {
		history += "\n" + m.spinner.View() + " thinking…"
	}
	composer := theme.ComposerFrame(m.chatWidth(), m.input.View(), inputFocused)
	if m.composerUsesChipsAsTopEdge() {
		composer = theme.ComposerFrameNoTopEdge(m.chatWidth(), m.input.View(), inputFocused)
	}
	menu := m.commandMenuView()
	chatParts := []string{history}
	if menu != "" {
		chatParts = append(chatParts, menu)
	}
	// One blank row between the last transcript card and whatever comes
	// next (founder 2026-09-25 margins ruling), collapsing under
	// theme.MarginCollapseRows terminal rows -- historyHeight() reserves
	// the matching row(s), so this never over- or under-fills the screen.
	// Placed BEFORE the chip row (not between chips and the composer): a
	// chip strip is logically part of the composer -- founder, r10,
	// verbatim, on seeing a blank row land there instead: "it should be
	// part of the composer. There is unexpected empty line between
	// border/chips line and the text line" — the composer/chips block
	// must sit flush together, whatever margin exists goes above ALL of
	// it.
	for range theme.ContentMargins(m.height) {
		chatParts = append(chatParts, "")
	}
	if chips := m.chipsView(m.chatWidth(), inputFocused); chips != "" {
		// Chips render above the input, closest to the composer -- after
		// the slash-command menu (which sits directly above the input only
		// while no chips are focused-adjacent) and before it.
		chatParts = append(chatParts, chips)
	}
	chatParts = append(chatParts, composer)
	chat := lipgloss.JoinVertical(lipgloss.Left, chatParts...)
	body := chat
	if m.splitEnabled() {
		side := m.panelView(m.sidebarWidth(), m.focusRing.Zone() == focus.ZoneSidebar)
		// No manual glue between chat and side: theme.PanelFrame already
		// draws the one divider between them (founder 2026-09-25: "no
		// boxes around boxes" — a single vertical divider, not a second
		// one from chatshell on top of it).
		body = lipgloss.JoinHorizontal(lipgloss.Top, chat, side)
	}
	// One blank row between the top bar and the content below it, for
	// BOTH columns at once -- a single full-width blank row here sits
	// above the already-joined chat+side body, so one row of chatshell's
	// own margin logic covers the "both columns" requirement without the
	// side panel needing to know about margins itself.
	topParts := []string{top}
	for range theme.ContentMargins(m.height) {
		topParts = append(topParts, "")
	}
	// A THIRD blank row, same collapse rule, now separates the composer
	// from the status bar too -- founder, r12, verbatim, seeing the
	// rendered result in Warp: "status line should have top margin ...
	// It should be last line on screen" (superseding the r9 "no blank row
	// between the composer and the hints/status bar" rule -- see theme's
	// own vertical-margins doc). The status bar itself is the LAST part
	// joined below, with nothing after it, so it always lands on the
	// terminal's own last row -- but ONLY when there is one: with nothing
	// to show at all (statusBarVisible() false), neither this margin row
	// NOR a blank status placeholder is appended, so the COMPOSER's own
	// bottom edge becomes the terminal's last row instead (founder, r12,
	// same round: "with no hints, the composer's bottom edge must be the
	// last screen line (no trailing empty rows)").
	parts := append([]string{}, topParts...)
	parts = append(parts, body)
	if m.statusBarVisible() {
		for range theme.ContentMargins(m.height) {
			parts = append(parts, "")
		}
		parts = append(parts, m.statusBarView())
	}
	content := lipgloss.JoinVertical(lipgloss.Left, parts...)
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
