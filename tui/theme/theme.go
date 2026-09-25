// Package theme centralises aichat's visual language — colours, card
// framing, and chrome (bars, composer frame, panel rows) — so every product
// built on tui/chatshell and tui/transcript gets an identical, polished
// look with ZERO styling code of its own.
//
// Founder ruling (2026-09-25): "UI styling should be unified across apps.
// Message should be like a card in chat of any app." and (same day,
// follow-up): "Same for composer block and hints status bar and top menu
// and side panel." DataTug chat's look (pkg/chat/chatui.go's topBar/
// statusBar/renderMarkdown and its transcript entry styling) was the
// reference; this package is where that look now lives, as the DEFAULT —
// a product supplies only CONTENT (title, hint labels, menu items, message
// text); every colour, border, and padding decision lives here, once.
//
// No other aichat package should construct a lipgloss.NewStyle() with a
// hard-coded colour literal — read a colour or a render helper from here
// instead, so changing the look means editing this one file.
package theme

import (
	"image/color"
	"math"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// Role identifies which card/accent style to use. It intentionally
// duplicates transcript.Role's string values rather than importing that
// package: theme is a leaf package every other tui/ package (including
// transcript) depends on, never the reverse.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleError     Role = "error"
	// RoleBlock is a rich transcript.Block entry (a grid, an HTTP response,
	// a join-candidate list, ...) sitting in the same card frame as a plain
	// message.
	RoleBlock Role = "block"
)

// Dark selects the theme variant: true (the default) means the terminal has
// a dark background. Every colour helper in this package reads it at call
// time (not once at init), so SetDark takes effect on the very next render
// — e.g. a product toggling a light/dark preference at runtime, or a tool
// that renders the same screen once per variant (see cmd/mdrender-snapshot-
// style tooling in downstream products).
var Dark = true

// SetDark sets the theme variant explicitly, overriding auto-detection.
func SetDark(dark bool) { Dark = dark }

// detectBackground is the seam Detect calls — lipgloss.HasDarkBackground
// against the real terminal in production, fault-injectable (including a
// panic, e.g. a term.File that errors mid-query on an exotic terminal) by a
// test.
var detectBackground = func() bool { return lipgloss.HasDarkBackground(os.Stdin, os.Stdout) }

// Detect reports whether the current terminal (os.Stdin/os.Stdout) appears
// to have a dark background, via lipgloss.HasDarkBackground. It never
// panics — a non-terminal (piped output, CI, a test) simply reports the
// package's built-in default (true) — so a product can safely call
// SetDark(Detect()) once at startup without special-casing non-TTY runs.
func Detect() (isDark bool) {
	isDark = true
	defer func() {
		if recover() != nil {
			isDark = true
		}
	}()
	return detectBackground()
}

// pick returns dark when Dark is true, light otherwise — the one place this
// package chooses between a light- and dark-background colour pair.
func pick(light, dark color.Color) color.Color {
	if Dark {
		return dark
	}
	return light
}

// --- role colours ----------------------------------------------------

func userColors() (bg, border, fg color.Color) {
	return pick(lipgloss.Color("#DCE8FA"), lipgloss.Color("#1C2B3A")),
		pick(lipgloss.Color("#5B8FC7"), lipgloss.Color("#4A7FB5")),
		pick(lipgloss.Color("#10243D"), lipgloss.Color("#E4EDFA"))
}

func assistantColors() (bg, border, fg color.Color) {
	return pick(lipgloss.Color("#E9E9E1"), lipgloss.Color("#262A33")),
		pick(lipgloss.Color("#A8A89C"), lipgloss.Color("#4B4F58")),
		pick(lipgloss.Color("#1B1B18"), lipgloss.Color("#E5E5E1"))
}

func systemColors() (bg, border, fg color.Color) {
	return pick(lipgloss.Color("#F5E8BE"), lipgloss.Color("#3A331A")),
		pick(lipgloss.Color("#C7A934"), lipgloss.Color("#9C8330")),
		pick(lipgloss.Color("#4A3B00"), lipgloss.Color("#F1DE97"))
}

func errorColors() (bg, border, fg color.Color) {
	return pick(lipgloss.Color("#FBE1DE"), lipgloss.Color("#452421")),
		pick(lipgloss.Color("#C6584A"), lipgloss.Color("#B5544A")),
		pick(lipgloss.Color("#4A0F09"), lipgloss.Color("#F5C6BF"))
}

func blockColors() (bg, border, fg color.Color) {
	return pick(lipgloss.Color("#E3E5EF"), lipgloss.Color("#262B36")),
		pick(lipgloss.Color("#9195A8"), lipgloss.Color("#454B5C")),
		pick(lipgloss.Color("#181A21"), lipgloss.Color("#E3E5EE"))
}

func colorsFor(role Role) (bg, border, fg color.Color) {
	switch role {
	case RoleUser:
		return userColors()
	case RoleSystem:
		return systemColors()
	case RoleError:
		return errorColors()
	case RoleBlock:
		return blockColors()
	default:
		return assistantColors()
	}
}

// FocusColor is the one accent colour used everywhere focus/selection is
// shown: a focused transcript card's border, the composer's border while
// it holds keyboard focus, a selected sidebar/panel row, and the active
// zone's border in general — one accent, so "what has focus" always reads
// the same way regardless of which chrome is showing it.
func FocusColor() color.Color { return pick(lipgloss.Color("#1A5FC7"), lipgloss.Color("#6FB1FF")) }

// MutedColor is the shared low-emphasis text colour (hint labels, an empty
// sidebar's placeholder, unfocused chrome borders).
func MutedColor() color.Color { return pick(lipgloss.Color("#54544C"), lipgloss.Color("#A3A8B1")) }

// AccentColor is the shared emphasis colour for a hint's key/shortcut
// (distinct from FocusColor, which marks focus/selection specifically).
func AccentColor() color.Color { return pick(lipgloss.Color("#734B00"), lipgloss.Color("#E8C25A")) }

func barColors() (bg, fg color.Color) {
	return pick(lipgloss.Color("#D8DCE6"), lipgloss.Color("#2E3440")),
		pick(lipgloss.Color("#101820"), lipgloss.Color("#ECEFF4"))
}

// TerminalBackground is this package's working assumption for "the bare
// terminal background a card/composer surface must read as distinct from"
// — a typical terminal emulator default (not the most extreme possible
// pure black/white, which would understate the real regression: founder
// 2026-09-25, "the assistant card fill is indistinguishable from the
// terminal background" in dark mode was reproducible against a common
// near-black default like most terminals ship with, not against pure
// black). Every SurfaceDeltaPairs() entry is checked against this.
func TerminalBackground() color.Color {
	return pick(lipgloss.Color("#FAFAFA"), lipgloss.Color("#1E1E1E"))
}

// --- half-block surface edges --------------------------------------------
//
// Founder idea (2026-09-25, approved for r9): half-height block glyphs
// ("▄" U+2584 LOWER HALF BLOCK, "▀" U+2580 UPPER HALF BLOCK), foreground =
// the surface colour, background = TerminalBackground(), replace a card's
// or the composer's full blank top/bottom padding row -- the glyph's own
// filled half reads as the surface easing in/out a half-line, instead of
// a hard one-row jump. Requires TrueColor (the fg/bg pair must render as
// two DISTINCT, exact colours to read as a seam, not noise); HalfBlockEdges
// = false, or a non-TrueColor colour profile, falls back to the original
// full-padding-row rendering.

// HalfBlockEdges is the product-facing on/off switch (default true) --
// SetHalfBlockEdges(false) opts a product out entirely, independent of
// colour-profile detection (e.g. a product that knows its target terminal
// renders half-blocks with a visible seam despite TrueColor support).
var HalfBlockEdges = true

// SetHalfBlockEdges sets HalfBlockEdges explicitly.
func SetHalfBlockEdges(v bool) { HalfBlockEdges = v }

// detectColorProfile is the seam colorProfileSupportsTrueColor calls --
// colorprofile.Env(os.Environ()), fault-injectable by a test the same way
// detectBackground is.
var detectColorProfile = func() colorprofile.Profile { return colorprofile.Env(os.Environ()) }

// colorProfileSupportsTrueColor reports whether the detected colour
// profile is exactly TrueColor -- half-block edges need the fg/bg pair to
// render as their EXACT configured colours (a downsampled ANSI256/ANSI
// terminal could quantise surface and terminal background to the same
// palette entry, erasing the seam entirely).
func colorProfileSupportsTrueColor() bool { return detectColorProfile() == colorprofile.TrueColor }

// HalfBlockEdgesActive reports whether Card/ComposerFrame will actually
// render half-block edges for the CURRENT call: HalfBlockEdges AND a
// TrueColor colour profile. Exported so a caller composing its own content
// around a Card/ComposerFrame (chatshell's chip strip, see chip.go) can
// match its own rendering choice to whichever mode is active.
func HalfBlockEdgesActive() bool { return HalfBlockEdges && colorProfileSupportsTrueColor() }

// HalfBlockEdge renders ONE half-block edge row, width cells wide, that
// blends surfaceBG into TerminalBackground(): "▄" (foreground=surfaceBG,
// background=TerminalBackground()) for the TOP edge — the surface's fill
// appears to start half a line in — or "▀" (same colours) for the BOTTOM
// edge. Exported so a caller building its own leading/trailing fill around
// embedded content (chatshell's chip strip) can match Card/ComposerFrame's
// own edge glyph/colour rule exactly, rather than re-deriving it.
func HalfBlockEdge(width int, surfaceBG color.Color, top bool) string {
	glyph := "▀"
	if top {
		glyph = "▄"
	}
	if width <= 0 {
		return ""
	}
	return lipgloss.NewStyle().Foreground(surfaceBG).Background(TerminalBackground()).Render(strings.Repeat(glyph, width))
}

// SurfaceColors returns the neutral panel/grid surface background+
// foreground pair — RoleBlock's own colours — for any component that needs
// a plain themed surface (blending into its enclosing card) WITHOUT
// drawing its own full Card frame, e.g. tui/grid's non-highlighted cells,
// tui/sidebar's unselected rows, or a side panel's frame background. No
// aichat component should pick its own background literal for this; read
// it from here so a grid cell, a sidebar row and a card never disagree
// about what "the surface" looks like.
func SurfaceColors() (bg, fg color.Color) {
	bg, _, fg = blockColors()
	return bg, fg
}

// FocusSurfaceColors returns the SINGLE background+foreground pair every
// aichat component uses to mark "this is focused/selected": background is
// FocusColor() itself — the SAME accent a focused Card's border and the
// composer's focused border use — paired with a foreground guaranteed to
// contrast with it in both Dark variants. A grid's highlighted row, a
// sidebar's selected row, and a join block's chosen candidate all use this
// pair, so "what is selected" reads as the one consistent accent across
// every aichat component (founder 2026-09-25: "Use the single theme focus/
// selection colour everywhere").
func FocusSurfaceColors() (bg, fg color.Color) {
	return FocusColor(), pick(lipgloss.Color("#FFFFFF"), lipgloss.Color("#0B1220"))
}

// BorderColor returns FocusColor() when focused, MutedColor() otherwise —
// the one rule every bordered aichat element (a Card, the composer frame,
// a grid's own card, a panel frame) follows for its border colour, so a
// product never has to decide this for itself.
func BorderColor(focused bool) color.Color {
	if focused {
		return FocusColor()
	}
	return MutedColor()
}

// --- header labels -----------------------------------------------------

// HeaderFor returns the default role label a card shows above its body,
// e.g. "You" for RoleUser. Pass a non-empty header to Card to override it
// (e.g. a Block's own title); "" for RoleBlock (a Block untitled by
// default — see transcript.Titled).
func HeaderFor(role Role) string {
	switch role {
	case RoleUser:
		return "You"
	case RoleAssistant:
		return "Assistant"
	case RoleSystem:
		return "System"
	case RoleError:
		return "Error"
	default:
		return ""
	}
}

// --- compositing pre-styled nested content over a themed fill ------------
//
// A component this package wraps (a card's Block body, a bubbles input's
// own View(), a side panel's own row rendering) often comes back already
// carrying its OWN ANSI styling, including its own bare SGR resets
// ("\x1b[m"). lipgloss's Style.Render wraps a line with a colour prefix
// ONCE, at the start, and a reset at the end — it does not know about, and
// so cannot survive, a reset embedded partway through nested content: from
// that reset onward the terminal falls back to its own default colours
// until the line ends, i.e. our themed background/foreground silently
// disappears for the rest of the line. This was a real regression twice
// over: the composer's filled background vanished right after the input's
// own cursor-cell reset, and a side panel row's background vanished the
// same way. fillSGR/paintOver below are the fix: reassert (bg, fg)
// immediately after every reset found in nested content, so the fill
// survives whatever the nested content does internally.

// fillSGR returns the raw SGR escape sequence for a (bg, fg) pair — the
// exact bytes paintOver reasserts after every embedded reset.
func fillSGR(bg, fg color.Color) string {
	return ansi.NewStyle().BackgroundColor(bg).ForegroundColor(fg).String()
}

// paintOver composites content — which may already carry its own nested
// ANSI styling and resets — onto a (bg, fg) fill: prefixed with the fill's
// SGR sequence, with that same sequence reasserted after every bare reset
// content contains, and a final reset at the end. Card/ComposerFrame/
// PanelFrame/Bar all run their nested content through this before handing
// it to lipgloss for width/padding, so the fill can never be lost partway
// through a line regardless of what the nested content does.
func paintOver(content string, bg, fg color.Color) string {
	prefix := fillSGR(bg, fg)
	painted := strings.ReplaceAll(content, "\x1b[0m", "\x1b[0m"+prefix)
	painted = strings.ReplaceAll(painted, ansi.ResetStyle, ansi.ResetStyle+prefix)
	return prefix + painted + ansi.ResetStyle
}

// PaintOver is paintOver, exported for a product's OWN chrome that
// composites pre-styled nested content (e.g. its own row highlighting)
// onto a themed background outside of Card/ComposerFrame/PanelFrame/Bar —
// a product should reach for this instead of re-deriving the same nested-
// reset fix locally (see the package doc above these two functions).
func PaintOver(content string, bg, fg color.Color) string { return paintOver(content, bg, fg) }

// --- cards ---------------------------------------------------------------
//
// Founder ruling (2026-09-25, REPLACING the earlier bordered-card design):
// "There is unnecessary border around message card and grid. The card
// defined not by border but by background for user message and in
// generally." A card is a FILLED BACKGROUND BLOCK, never a box-drawing
// border — a role's background tint (colorsFor) IS the card, with inner
// padding and a bold header line inside the fill. Focus/selection (no
// border to thicken) instead SHIFTS the whole fill to
// FocusSurfaceColors() and adds a 1-column left accent bar in FocusColor()
// — the bar is the non-text focus indicator (>= nonTextMinRatio against
// the surrounding background; see the contrast section below), the
// background shift is the reading cue. Grids are the one exception (see
// tui/grid) — a grid keeps its own drawn border, title/footer inline in
// the border, and a scrollbar in the right border, with NO card fill
// around it (transcript's SelfFramed capability skips the Card wrap for
// it entirely).

// CardPaddingCols/Rows is the space, in columns/rows, kept between a
// card's fill edge and its content.
const CardPaddingCols = 2
const CardPaddingRows = 1

// MaxInlineGridRows is the shared default for how many data rows a grid
// shows at once when embedded inline in a transcript (tui/grid.Model's
// DefaultMaxVisibleRows) — founder 2026-09-25: "Grids embedded in the chat
// transcript show at most 10 data rows (default; make it a theme/
// transcript constant ... overridable per product via an option, not a
// per-product style)". A product overrides it per grid via
// grid.WithMaxVisibleRows(n), never by redefining this constant.
const MaxInlineGridRows = 10

// --- vertical margins --------------------------------------------------
//
// Founder ruling (2026-09-25, r9, "Margins (approved)"): a single blank
// row between the top bar and the content below it (both the chat column
// and, when split, the side panel column — one full-width blank row
// achieves both at once, see chatshell's View()), a single blank row
// between the last transcript card and the composer, and NO blank row
// between the composer and the hints/status bar. Both blank rows collapse
// to zero below MarginCollapseRows terminal rows, so a short terminal
// never loses transcript space to decoration.

// MarginRows is the number of blank rows chatshell inserts at each of the
// two margin points above, when the terminal is tall enough (see
// ContentMargins) — a single shared constant so every product's spacing
// agrees, per this package's own no-per-product-styling rule.
const MarginRows = 1

// MarginCollapseRows is the terminal row count AT OR BELOW which
// ContentMargins returns 0 — founder: "collapse both below 24 terminal
// rows".
const MarginCollapseRows = 24

// ContentMargins returns MarginRows when the terminal is taller than
// MarginCollapseRows, 0 otherwise — the one function chatshell calls at
// both margin points (top-bar/content, and last-card/composer) so the
// collapse rule lives in exactly one place.
func ContentMargins(terminalRows int) int {
	if terminalRows < MarginCollapseRows {
		return 0
	}
	return MarginRows
}

// cardBarWidth is the 1-column left accent bar Card reserves on every
// card (rendered in FocusColor() when focused, or the card's own
// background — i.e. invisible — otherwise), so a card's OUTER width never
// changes between its focused and unfocused rendering.
const cardBarWidth = 1

// InnerWidth returns the content width available inside a card of the
// given OUTER width — what a Block should render at (via
// theme.InnerWidth(width)) so its own View(width, focused) output lines up
// exactly with the card Card(...) then wraps it in.
func InnerWidth(width int) int {
	return max(1, width-cardBarWidth-2*CardPaddingCols)
}

// surfaceFill composes ONE filled surface block — a Card or a
// ComposerFrame — from already-painted content: padded horizontally by
// paddingCols at OUTER width, with a 1-column left accent bar (barColor)
// down every CONTENT row (the bar "spans the content rows" only — an edge
// row's bar column instead gets a plain TerminalBackground() filler cell;
// the simplest of the edge treatments the founder's half-block idea left
// open, see HalfBlockEdge's package doc). When HalfBlockEdgesActive(),
// topEdge/bottomEdge each request a HalfBlockEdge row in place of a full
// blank padding row on that side; otherwise (fallback) both sides always
// get a full padding row regardless of topEdge/bottomEdge, matching this
// package's original (pre-half-block) rendering exactly.
func surfaceFill(bg, fg, barColor color.Color, barWidth, paddingCols int, content string, width int, topEdge, bottomEdge bool) string {
	half := HalfBlockEdgesActive()
	vPad := 1
	if half {
		vPad = 0
	}
	innerWidth := max(1, width-barWidth-2*paddingCols)
	rendered := lipgloss.NewStyle().
		Background(bg).
		Foreground(fg).
		Padding(vPad, paddingCols).
		Width(innerWidth).
		Render(content)
	bar := lipgloss.NewStyle().Background(barColor).Render(" ")
	lines := strings.Split(rendered, "\n")
	for i, line := range lines {
		lines[i] = bar + line
	}
	block := strings.Join(lines, "\n")
	if !half {
		return block
	}
	filler := lipgloss.NewStyle().Background(TerminalBackground()).Render(strings.Repeat(" ", barWidth))
	if topEdge {
		block = filler + HalfBlockEdge(width-barWidth, bg, true) + "\n" + block
	}
	if bottomEdge {
		block = block + "\n" + filler + HalfBlockEdge(width-barWidth, bg, false)
	}
	return block
}

// Card renders body (and, when non-empty, header above it in bold) as a
// filled, coloured background block for role, at OUTER width — see the
// package doc above for the no-border design and its focus/selection
// language. Every aichat product's message/Block cards render through
// this one function.
func Card(role Role, header, body string, width int, focused bool) string {
	bg, _, fg := colorsFor(role)
	barColor := bg // blends into the fill — invisible — when unfocused.
	if focused {
		bg, fg = FocusSurfaceColors()
		barColor = FocusColor()
	}
	content := paintOver(body, bg, fg)
	if header != "" {
		content = lipgloss.NewStyle().Bold(true).Background(bg).Foreground(fg).Render(header) + "\n" + content
	}
	return surfaceFill(bg, fg, barColor, cardBarWidth, CardPaddingCols, content, width, true, true)
}

// --- bars (top bar / hints-status bar) ------------------------------------

// Bar pads content to width and fills the remainder with the shared chrome
// background, truncating with an ellipsis if content overflows — so a bar's
// right edge always reaches the terminal edge, whatever a product supplied.
// content may already contain nested lipgloss-styled spans (e.g.
// RenderHints' per-hint colouring); Bar only sets the background/width
// frame around it, not a foreground override, so those spans' own colours
// survive.
func Bar(width int, content string) string {
	bg, fg := barColors()
	base := lipgloss.NewStyle().Bold(true).Foreground(fg)
	truncated := ansi.Truncate(content, max(1, width), "…")
	return lipgloss.NewStyle().Background(bg).Width(max(1, width)).Render(paintOver(base.Render(truncated), bg, fg))
}

// MenuItem is one top-bar menu entry/tab, e.g. DataTug's "Project: Foo
// [F3]" segment or a tab strip's tab.
type MenuItem struct {
	Label  string
	Active bool
}

// TopBar renders the shared top-bar chrome: a bold title, an optional
// context string (e.g. the current project/space), and menu items with the
// active one underlined — DataTug's original topBar look, now the default
// for every product. A product supplies only title/context/items;
// chatshell.WithTopBarProvider wires this in automatically.
func TopBar(width int, title, context string, items []MenuItem) string {
	parts := make([]string, 0, 2+len(items))
	if title != "" {
		parts = append(parts, title)
	}
	if context != "" {
		parts = append(parts, context)
	}
	for _, it := range items {
		s := lipgloss.NewStyle()
		if it.Active {
			s = s.Bold(true).Underline(true)
		}
		parts = append(parts, s.Render(it.Label))
	}
	return Bar(width, strings.Join(parts, " │ "))
}

// Hint is one key/action pair shown in the shared hints/status bar, e.g.
// {Key: "Enter", Label: "send"}.
type Hint struct {
	Key   string
	Label string
}

// RenderHints renders the shared hints/status bar: any trailing segments
// (e.g. a product/session summary or a hyperlink) first, then each Hint as
// its key (in AccentColor, bold) followed by its label (in MutedColor) —
// DataTug's original statusBar layout, now the default for every product.
// Unlike a single-line Bar, RenderHints WRAPS: a segment or hint that would
// overflow the current line starts a new line instead of being silently
// truncated (ported from DataTug's own wrapStatusSegments, now shared —
// founder 2026-09-25: "Same for ... hints status bar"), each wrapped line
// rendered through Bar so every line keeps the shared chrome. A product
// supplies only the hint list and segment strings; chatshell.
// WithHintsProvider (or the plain SetStatus default) wires this in
// automatically.
func RenderHints(width int, hints []Hint, segments ...string) string {
	keyStyle := lipgloss.NewStyle().Bold(true).Foreground(AccentColor())
	labelStyle := lipgloss.NewStyle().Foreground(MutedColor())
	tokens := make([]hintToken, 0, len(hints)+len(segments))
	// Segments are plain, unstyled product strings (e.g. "F6/Shift+→
	// workspace") — splittable at word boundaries when a whole segment
	// doesn't fit any line, rather than mid-word truncated.
	for _, seg := range segments {
		tokens = append(tokens, hintToken{text: seg, splittable: true})
	}
	// A Hint's key/label pair renders as ONE already-styled token and must
	// stay atomic: founder/coordinator (r9): "never cut a key/label pair".
	// It goes on its own line rather than being split apart, even when the
	// pair itself doesn't fit the width.
	for _, h := range hints {
		tokens = append(tokens, hintToken{text: keyStyle.Render(h.Key) + " " + labelStyle.Render(h.Label), splittable: false})
	}
	lines := wrapTokens(tokens, max(1, width))
	if len(lines) == 0 {
		return Bar(width, "")
	}
	styled := make([]string, len(lines))
	for i, line := range lines {
		styled[i] = Bar(width, line)
	}
	return strings.Join(styled, "\n")
}

// hintToken is one RenderHints token queued for wrapTokens: text plus
// whether wrapTokens may split it into its individual space-separated
// words when the whole token doesn't fit a line by itself.
type hintToken struct {
	text       string
	splittable bool
}

// wrapTokens packs tokens onto as few lines as fit within maxWidth,
// breaking to a new line only when the next token would not fit — ported
// from DataTug's wrapStatusSegments (datatug-cli/pkg/chat/chatui.go,
// before this package existed). A token wider than maxWidth on its own is
// placed on its own line; if it is ALSO splittable (a plain multi-word
// segment, never a Hint's key/label pair), it is instead broken at word
// boundaries and those words packed the same way — so a too-long segment
// wraps onto more lines instead of being cut mid-word with an ellipsis
// (founder/coordinator, r9: "F6/Shift+→ work…" truncating a segment was
// the regression this fixes), while a non-splittable token (a Hint pair)
// never loses its key or its label.
func wrapTokens(tokens []hintToken, maxWidth int) []string {
	const separator = "   "
	lines := make([]string, 0, len(tokens))
	current := ""
	flush := func() {
		if current != "" {
			lines = append(lines, current)
			current = ""
		}
	}
	place := func(word string) {
		candidate := word
		if current != "" {
			candidate = current + separator + word
		}
		if current != "" && ansi.StringWidth(candidate) > maxWidth {
			flush()
			current = word
			return
		}
		current = candidate
	}
	for _, tok := range tokens {
		if !tok.splittable || ansi.StringWidth(tok.text) <= maxWidth {
			place(tok.text)
			continue
		}
		for _, word := range strings.Fields(tok.text) {
			place(word)
		}
	}
	flush()
	return lines
}

// --- colour blending -------------------------------------------------

// blend linearly mixes two colours' sRGB channels, t in [0,1] weighting b
// (t=0 returns a, t=1 returns b) — used for a "one step off" tint that
// isn't itself one of the named role/focus colours (see
// composerFocusColors).
func blend(a, b color.Color, t float64) color.Color {
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	mix := func(x, y uint32) uint8 {
		v := float64(x)*(1-t) + float64(y)*t
		return uint8(v / 257) // 16-bit -> 8-bit
	}
	return color.RGBA{R: mix(ar, br), G: mix(ag, bg), B: mix(ab, bb), A: 0xFF}
}

// --- composer frame --------------------------------------------------
//
// Founder ruling (2026-09-25): "Composer: consistent with this language —
// prefer a filled input area (background) with a focus accent rather than
// a heavy box." Same language as Card: a filled SurfaceColors background,
// no border, shifting to FocusSurfaceColors plus a 1-column left accent
// bar while focused.

// composerBarWidth is the 1-column left accent bar ComposerFrame reserves
// (see cardBarWidth's identical reasoning: a constant outer width whether
// or not it's focused).
const composerBarWidth = 1

// composerPaddingCols/Rows is the space, in columns/rows, kept between the
// composer's fill edge and the input's own content — founder 2026-09-25:
// "1 line padding above/below the text ... 2 cols left padding" (kept
// symmetric left/right, same as Card's own padding language), so the
// composer still reads as a filled box even when the input is empty.
const composerPaddingCols = 2
const composerPaddingRows = 1

// ComposerFrameSize returns how many extra columns/rows ComposerFrame adds
// around its content, so a caller (chatshell's resize/historyHeight) can
// size the inner input and reserve the right amount of screen space.
func ComposerFrameSize() (cols, rows int) {
	return composerBarWidth + 2*composerPaddingCols, 2 * composerPaddingRows
}

// composerFocusTint is the mixing weight ComposerFocusColors blends
// FocusColor() into the composer's unfocused surface by — a "slightly
// stronger tint", not the full bright FocusColor() fill Card/
// FocusSurfaceColors uses. Founder correction, twice over: first (r9)
// "a SUBTLE filled area (a tint one step off the terminal background,
// like the cards) ... focus = the thin left accent bar in the focus
// colour + slightly stronger tint (NOT a full bright fill)"; then,
// verbatim, more precisely: "the background of composer should just
// slightly differ from overall/chat background ... Focus is shown by the
// thin left accent bar (and at most a MARGINALLY stronger tint)". 0.04
// keeps the FOCUSED fill's own delta against TerminalBackground() inside
// [minComposerSurfaceDelta, maxComposerSurfaceDelta] (~1.26:1 dark,
// ~1.28:1 light) — barely past the UNFOCUSED fill's own ~1.18/1.20:1,
// never the earlier 0.22 blend's ~1.8:1 "bright slab". A Card
// intentionally goes full FocusSurfaceColors on focus (there's no better
// cue: an entire message either has focus or doesn't), but the composer
// already reads as focused via its left accent bar and the cursor inside
// it, so its own fill only needs the smallest nudge toward the accent,
// not become it.
const composerFocusTint = 0.04

// ComposerFocusColors returns the composer's FOCUSED fill: its own
// unfocused SurfaceColors() background blended composerFocusTint of the
// way toward FocusColor() (foreground unchanged — the blend is subtle
// enough that SurfaceColors' foreground still meets bodyTextMinRatio
// against it; verified by TestContrastMeetsWCAG's composer pairs).
func ComposerFocusColors() (bg, fg color.Color) {
	surfaceBG, surfaceFG := SurfaceColors()
	return blend(surfaceBG, FocusColor(), composerFocusTint), surfaceFG
}

// ComposerFrame wraps a composer's rendered input view in the shared
// filled background: SurfaceColors unfocused, ComposerFocusColors (a
// subtle tint toward FocusColor(), not the full bright fill a Card uses)
// plus a left accent bar in FocusColor() while focused — so "the composer
// has focus" reads primarily from the accent bar, with the fill only
// nudged, never a "bright slab". content is run through paintOver first: a
// bubbles input's own View() carries its own ANSI styling (cursor cell,
// etc.) including its own resets, which would otherwise cut this fill's
// background off partway through the line (the "lost composer background"
// regression — see the paintOver doc above).
func ComposerFrame(width int, content string, focused bool) string {
	return composerFrame(width, content, focused, true)
}

// ComposerFrameNoTopEdge is ComposerFrame without its own top edge row —
// for when a caller is rendering something ELSE immediately above that
// already performs the top edge's job visually (chatshell's chip strip,
// see chip.go's chipsEdgeRow: founder, r9, "have attachment chips in the
// top line of the composer ... the chips row that currently sits inside
// the composer moves here"). In FALLBACK mode (HalfBlockEdgesActive()
// false), there is no separate "edge row" concept to omit, so this is
// identical to ComposerFrame — the caller's own chip row still renders as
// its own line above, exactly as before this feature existed.
func ComposerFrameNoTopEdge(width int, content string, focused bool) string {
	return composerFrame(width, content, focused, false)
}

func composerFrame(width int, content string, focused, topEdge bool) string {
	bg, fg := SurfaceColors()
	barColor := bg
	if focused {
		bg, fg = ComposerFocusColors()
		barColor = FocusColor()
	}
	return surfaceFill(bg, fg, barColor, composerBarWidth, composerPaddingCols, paintOver(content, bg, fg), width, topEdge, true)
}

// --- panel / sidebar rows ------------------------------------------------

// SelectedRow renders one side-panel/sidebar row: "  text" unselected, or a
// FocusSurfaceColors()-highlighted "› text" when selected — the SAME accent
// a focused card's border and a grid's highlighted row use, so a selected
// list row, a focused message, and a selected grid row all read as the
// same kind of thing (founder 2026-09-25: "Use the single theme focus/
// selection colour everywhere").
func SelectedRow(text string, selected bool) string {
	if !selected {
		return "  " + text
	}
	bg, fg := FocusSurfaceColors()
	return lipgloss.NewStyle().Bold(true).Background(bg).Foreground(fg).Render("› " + text)
}

// PanelHeader renders a side-panel/sidebar title header, e.g. "Sidebar" or
// a product workspace pane's tab name.
func PanelHeader(title string) string {
	return lipgloss.NewStyle().Bold(true).Render(title)
}

// panelDividerWidth is the single vertical divider PanelFrame draws
// between the transcript and the side panel — founder 2026-09-25: "side
// panel may keep a frame line only where it separates the panel from the
// transcript (a single vertical divider is fine) — no boxes around
// boxes." One column, no border on the other three sides, no padding (a
// panel's own content, e.g. tui/sidebar's header line, manages its own
// spacing).
const panelDividerWidth = 1

// PanelFrameSize returns how many extra columns/rows PanelFrame adds
// around its content, so a caller (chatshell's panel sizing) can size the
// panel's own content and reserve the right amount of screen space —
// mirroring ComposerFrameSize. Rows is always 0.
func PanelFrameSize() (cols, rows int) { return panelDividerWidth, 0 }

// PanelFrame draws the single divider between the transcript and a side
// panel's (the default sidebar, or a product SidePanel) own content:
// BorderColor(focused) — the same accent a focused card/composer uses —
// so the side panel reads as "this has focus" consistently with every
// other zone (founder 2026-09-25: "Same for ... side panel"). Founder
// correction (same day, after seeing it rendered): "exactly ONE separator
// column ... a single thin vertical line glyph, in the theme's muted
// border colour ... no background fill, no stripes, no double lines."
// PanelFrame draws exactly that — a foreground-only "│" glyph, one column,
// no background of its own — never addLeftBar's filled-bg accent column
// (right for a card/composer's focus accent, wrong here: a background-
// filled divider is what read as a "striped grey band" against the
// panel's own row backgrounds). The panel's own content (e.g. tui/
// sidebar's own header line, or a product SidePanel's own tab strip)
// supplies whatever header/label it wants and its OWN row backgrounds;
// PanelFrame only supplies the divider and the
// panel's background fill.
func PanelFrame(width int, content string, focused bool) string {
	divider := lipgloss.NewStyle().Foreground(BorderColor(focused)).Render("│")
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = divider + line
	}
	return strings.Join(lines, "\n")
}

// --- contrast (WCAG 2.x) --------------------------------------------------
//
// Founder ruling (2026-09-25): "Make sure we have good contrast and texts
// are readable" — made measurable, not eyeballed. Every (foreground,
// background) combination this package actually paints together is named
// below (ContrastPairs) and checked by TestContrastMeetsWCAG, for BOTH
// Dark variants, against the thresholds a colour change can never
// regress silently:
//
//   - body text (message text, grid cells, side-panel rows, hint labels)
//     on its actual background, and selected/highlighted row text on the
//     selection background: >= bodyTextMinRatio (WCAG AA normal text).
//   - a focus border/accent against its surrounding background:
//     >= nonTextMinRatio (WCAG AA non-text contrast) — a border only
//     needs to be DISTINGUISHABLE, not read as text.
//   - "muted" text (MutedColor) is read at the SAME bodyTextMinRatio as
//     any other text — lower emphasis MUST come from weight/saturation,
//     never from dropping below the text threshold. placeholderMinRatio
//     exists for the one deliberate exception a product's OWN placeholder
//     text (e.g. a composer's "Ask anything...", not itself part of this
//     package) may use instead, documented here rather than silently
//     assumed.

const (
	// bodyTextMinRatio is the WCAG AA "normal text" contrast minimum,
	// applied to every body/muted/selected/header text pair below.
	bodyTextMinRatio = 4.5
	// nonTextMinRatio is the WCAG AA "non-text contrast" minimum, applied
	// to a focus border/accent against its surrounding background.
	nonTextMinRatio = 3.0
	// placeholderMinRatio is NOT enforced by this package's own pairs —
	// documented here as the floor a product's own placeholder text (a
	// deliberately de-emphasised affordance, not a themed colour pair)
	// may use instead of bodyTextMinRatio.
	placeholderMinRatio = 3.0
)

// ContrastPair names one (foreground, background) combination this
// package actually paints together, and the minimum WCAG 2.x contrast
// ratio (see Contrast) it must meet.
type ContrastPair struct {
	Name         string
	FG, BG       color.Color
	MinimumRatio float64
}

// ContrastPairs returns every (foreground, background) combination this
// package paints together, for the CURRENT Dark variant (call SetDark
// first to get the other variant's set) — the enumerable source of truth
// TestContrastMeetsWCAG checks, so a future colour change that regresses
// readability fails a test instead of shipping.
func ContrastPairs() []ContrastPair {
	pairs := make([]ContrastPair, 0, 32)
	for _, role := range []Role{RoleUser, RoleAssistant, RoleSystem, RoleError, RoleBlock} {
		bg, _, fg := colorsFor(role)
		pairs = append(pairs,
			ContrastPair{Name: string(role) + " card body/header text", FG: fg, BG: bg, MinimumRatio: bodyTextMinRatio},
			ContrastPair{Name: string(role) + " focused card border vs card background", FG: FocusColor(), BG: bg, MinimumRatio: nonTextMinRatio},
		)
	}

	surfaceBG, surfaceFG := SurfaceColors()
	pairs = append(pairs,
		ContrastPair{Name: "surface body text (grid cell, sidebar row, panel frame)", FG: surfaceFG, BG: surfaceBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "muted text on surface (grid footer, unfocused chip)", FG: MutedColor(), BG: surfaceBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "accent text on surface (grid selected-column cell)", FG: AccentColor(), BG: surfaceBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "focus border vs surface background", FG: FocusColor(), BG: surfaceBG, MinimumRatio: nonTextMinRatio},
	)

	focusBG, focusFG := FocusSurfaceColors()
	pairs = append(pairs,
		ContrastPair{Name: "selected/highlighted row text on selection background", FG: focusFG, BG: focusBG, MinimumRatio: bodyTextMinRatio},
	)

	barBG, barFG := barColors()
	pairs = append(pairs,
		ContrastPair{Name: "top/status bar text", FG: barFG, BG: barBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "hint key (AccentColor) on bar background", FG: AccentColor(), BG: barBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "hint label (MutedColor) on bar background", FG: MutedColor(), BG: barBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "focus border vs bar background", FG: FocusColor(), BG: barBG, MinimumRatio: nonTextMinRatio},
	)

	composerBG, composerFG := SurfaceColors()
	focusComposerBG, focusComposerFG := ComposerFocusColors()
	pairs = append(pairs,
		ContrastPair{Name: "composer typed text (unfocused)", FG: composerFG, BG: composerBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "composer typed text (focused)", FG: focusComposerFG, BG: focusComposerBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "composer placeholder (unfocused)", FG: MutedColor(), BG: composerBG, MinimumRatio: placeholderMinRatio},
		ContrastPair{Name: "composer placeholder (focused)", FG: MutedColor(), BG: focusComposerBG, MinimumRatio: placeholderMinRatio},
		ContrastPair{Name: "focus bar vs composer fill (focused)", FG: FocusColor(), BG: focusComposerBG, MinimumRatio: nonTextMinRatio},
	)

	return pairs
}

// minSurfaceDelta is the minimum WCAG contrast ratio a card/composer
// surface fill must have against TerminalBackground() — founder 2026-09-25
// (r9 coordinator review): "the assistant card fill is indistinguishable
// from the terminal background" in dark mode. Unlike bodyTextMinRatio/
// nonTextMinRatio (both about reading TEXT), this is about the fill
// itself being perceivable as a card/composer AT ALL against bare
// terminal — a much lower bar (WCAG has no named threshold for this; 1.15
// is this package's own floor, picked so the r9 regression — an ~1.06
// ratio between the assistant dark fill and a common near-black terminal
// default — fails loudly instead of shipping again).
const minSurfaceDelta = 1.15

// minComposerSurfaceDelta/maxComposerSurfaceDelta bound the composer
// fill's delta against TerminalBackground() on BOTH sides — founder,
// verbatim: "the background of composer should just slightly differ from
// overall/chat background ... contrast ratio vs chat background between
// ~1.1:1 and ~1.3:1, never a bright/saturated fill". Unlike the card
// roles (minSurfaceDelta is a FLOOR only — a card is allowed to be more
// obviously tinted than a composer), the composer gets an explicit
// ceiling too, so a future focus-tint change can't quietly turn it back
// into a "bright slab" (the exact r9 regression this bounds).
const (
	minComposerSurfaceDelta = 1.1
	maxComposerSurfaceDelta = 1.3
)

// SurfaceDeltaPair names one card/composer surface fill and the
// contrast-ratio range (see Contrast) it must sit within against
// TerminalBackground(). MaximumRatio of 0 means no upper bound (every
// card role: more tint than the floor is fine, there's no "too much"
// ceiling for a card the way there is for the composer).
type SurfaceDeltaPair struct {
	Name         string
	Surface      color.Color
	MinimumRatio float64
	MaximumRatio float64
}

// SurfaceDeltaPairs returns every card-role and composer surface fill this
// package paints directly on the terminal background, for the CURRENT Dark
// variant — the source of truth TestSurfaceDistinctFromTerminalBackground
// checks, so a card/composer tint that's crept too close to
// TerminalBackground() (unreadable as "a card" at all, independent of its
// text's own contrast) -- or, for the composer, too far from it (the
// "bright slab" regression) -- fails a test instead of shipping.
func SurfaceDeltaPairs() []SurfaceDeltaPair {
	pairs := make([]SurfaceDeltaPair, 0, 8)
	for _, role := range []Role{RoleUser, RoleAssistant, RoleSystem, RoleError, RoleBlock} {
		bg, _, _ := colorsFor(role)
		pairs = append(pairs, SurfaceDeltaPair{Name: string(role) + " card fill vs terminal background", Surface: bg, MinimumRatio: minSurfaceDelta})
	}
	composerBG, _ := SurfaceColors()
	focusComposerBG, _ := ComposerFocusColors()
	pairs = append(pairs,
		SurfaceDeltaPair{Name: "composer fill (unfocused) vs terminal background", Surface: composerBG, MinimumRatio: minComposerSurfaceDelta, MaximumRatio: maxComposerSurfaceDelta},
		SurfaceDeltaPair{Name: "composer fill (focused) vs terminal background", Surface: focusComposerBG, MinimumRatio: minComposerSurfaceDelta, MaximumRatio: maxComposerSurfaceDelta},
	)
	return pairs
}

// relativeLuminance computes a colour's WCAG 2.x relative luminance from
// its sRGB channels (color.Color.RGBA(), 16-bit, alpha ignored — every
// colour this package defines is fully opaque).
func relativeLuminance(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	linear := func(channel uint32) float64 {
		v := float64(channel) / 65535
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(r) + 0.7152*linear(g) + 0.0722*linear(b)
}

// Contrast computes the WCAG 2.x contrast ratio between two colours —
// (L1+0.05)/(L2+0.05) with L1 the lighter relative luminance — the metric
// ContrastPairs' MinimumRatio thresholds are expressed in.
func Contrast(a, b color.Color) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}
