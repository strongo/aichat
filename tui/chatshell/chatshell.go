package chatshell

import (
	"context"
	"errors"
	"iter"
	"strings"

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

// Model is the reusable chat screen.
type Model struct {
	ctx     context.Context
	handler Handler

	transcript *transcript.Model
	input      textarea.Model
	sidebar    *sidebar.Model
	focusRing  *focus.Ring
	spinner    spinner.Model

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

	// busyCancel is the cancel func for a product's own SetBusy(true) phase
	// (e.g. a decision chain), set via SetBusyCancel. Esc/Ctrl+C while busy
	// and no stream is active calls it directly, since — unlike a stream —
	// there is no DoneMsg to asynchronously report the cancellation.
	busyCancel func()

	// ctrlCArmed is set by a first Ctrl+C while busy (which cancels); a
	// second, immediately-following Ctrl+C always quits instead of trying to
	// cancel again. Any other key clears it.
	ctrlCArmed bool
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
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// --- Product-facing API -----------------------------------------------

// AppendUser appends a user message to the transcript.
func (m *Model) AppendUser(text string) {
	m.transcript.Append(transcript.Entry{Role: transcript.RoleUser, Text: text})
}

// AppendAssistant appends a (non-streamed) assistant message.
func (m *Model) AppendAssistant(text string) {
	m.transcript.Append(transcript.Entry{Role: transcript.RoleAssistant, Text: text})
}

// AppendSystem appends a system/status message (e.g. an error).
func (m *Model) AppendSystem(text string) {
	m.transcript.Append(transcript.Entry{Role: transcript.RoleSystem, Text: text})
}

// AppendBlock appends a rich transcript.Block (e.g. a tui/grid result).
func (m *Model) AppendBlock(block transcript.Block) {
	m.transcript.Append(transcript.Entry{Block: block})
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
	m.cancelStream()
	ctx, cancel := context.WithCancel(m.ctx)
	m.streamID, m.streamCancel = id, cancel
	m.busy = true
	m.ctrlCArmed = false
	m.transcript.Append(transcript.Entry{ID: id, Role: transcript.RoleAssistant, Text: ""})
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

// PinToSidebar adds ref to the sidebar and notifies an OnSidebarChange
// Handler, if any.
func (m *Model) PinToSidebar(ref session.EntityRef) {
	if m.sidebar.Add(ref) {
		m.notifySidebarChange()
	}
}

// UnpinFromSidebar removes ref from the sidebar and notifies an
// OnSidebarChange Handler, if any.
func (m *Model) UnpinFromSidebar(ref session.EntityRef) {
	if m.sidebar.Remove(ref) {
		m.notifySidebarChange()
	}
}

func (m *Model) notifySidebarChange() {
	if obs, ok := m.handler.(SidebarObserver); ok {
		obs.OnSidebarChange(append([]session.EntityRef(nil), m.sidebar.Refs()...))
	}
}

// FocusedRef returns the entity ref under focus: the transcript's focused
// block's Current() when the transcript zone has focus, or the sidebar
// cursor's ref when the sidebar zone has focus. It is nil when the composer
// has focus, or nothing is under the cursor.
func (m *Model) FocusedRef() *session.EntityRef {
	switch m.focusRing.Zone() {
	case focus.ZoneTranscript:
		return m.transcript.Current()
	case focus.ZoneSidebar:
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

// SidebarRefs returns the sidebar's pinned refs, the product's saved
// working context (distinct from SelectionRefs, the transcript selection).
func (m *Model) SidebarRefs() []session.EntityRef {
	return append([]session.EntityRef(nil), m.sidebar.Refs()...)
}

// Busy reports whether a stream, or a product SetBusy(true) phase, is in
// flight.
func (m *Model) Busy() bool { return m.busy }

// --- tea.Model -----------------------------------------------------------

func (m *Model) Init() tea.Cmd { return textarea.Blink }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		cmd := m.transcript.Update(msg)
		return m, cmd

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

	case spinner.TickMsg:
		if !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	default:
		return m, m.dispatchUnhandled(msg)
	}
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
	switch msg.Event.Type {
	case ai.EventTextDelta:
		m.transcript.AppendDelta(msg.ID, msg.Event.Text)
	case ai.EventError:
		// Per the event contract, EventError with a nil Go error (this path
		// — a fatal error arrives as a DoneMsg instead, see
		// handleStreamDone) is non-fatal: report it but keep streaming.
		if msg.Event.Error != nil {
			m.AppendSystem("error: " + msg.Event.Error.Message)
		}
	}
	cmds := []tea.Cmd{msg.Next}
	if obs, ok := m.handler.(StreamObserver); ok {
		if cmd := obs.OnStreamEvent(msg.ID, msg.Event); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

func (m *Model) handleStreamDone(msg stream.DoneMsg) (tea.Model, tea.Cmd) {
	// A stale Done (superseded by a later StartStream, or already handled by
	// cancelBusy for the current one) still notifies StreamObserver, so a
	// product can clean up per-id state, but must not touch the CURRENT
	// stream's busy/transcript.
	if msg.ID == m.streamID {
		m.busy = false
		m.streamCancel = nil
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
	if m.sidebar.Visible() {
		m.sidebar.SetWidth(m.sidebarWidth())
	}
}

func (m *Model) splitEnabled() bool { return m.width >= splitMinWidth && m.sidebar.Visible() }

func (m *Model) chatWidth() int {
	if !m.splitEnabled() {
		return max(1, m.width-2)
	}
	inner := max(1, m.width-2)
	return max(42, min(inner-24, inner*m.sidebar.ChatPercent()/100))
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
		m.sidebar.Toggle()
		if !m.sidebar.Visible() && m.focusRing.Zone() == focus.ZoneSidebar {
			// Hiding the sidebar while it holds focus returns focus to
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
		m.sidebar.GrowChat(delta)
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
		cmd := m.sidebar.Update(msg)
		return m, cmd
	case focus.ZoneTranscript:
		cmd := m.transcript.Update(msg)
		return m, cmd
	default:
		return m.handleInputKey(msg)
	}
}

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
		side := m.sidebar.View(m.sidebarWidth(), m.focusRing.Zone() == focus.ZoneSidebar)
		body = lipgloss.JoinHorizontal(lipgloss.Top, chat, " │ ", side)
	}
	status := strings.Join(m.statusLines(), "\n")
	content := lipgloss.JoinVertical(lipgloss.Left, top, body, status)
	view := tea.NewView(content)
	view.AltScreen = true
	return view
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
