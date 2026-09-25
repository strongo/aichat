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
	"fmt"
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
//
// Founder 2026-09-25 (r10 coordinator review): "Derive surface tints from
// the ACTUAL terminal background ... compute card/composer/bar surfaces
// as small luminance shifts of the real background". A role's card
// surface is no longer a fixed hex pair picked by hand for the dark/light
// case — it's TerminalBackground() (the REAL reported background when
// known, see SetTerminalBackground; the guessed default otherwise)
// blended cardTintAmount of the way toward that role's fixed hue anchor,
// so a card reads as "that role, tinted onto THIS terminal" whatever the
// terminal's actual background turns out to be (a plain dark grey, a
// bright white, or something hued like Solarized dark #002b36). Text
// colour is picked for contrast against the COMPUTED bg (surfaceText),
// not a fixed hex either, for the same reason.

// roleHue is the fixed tint each role blends toward TerminalBackground()
// to produce its card surface — independent of light/dark; the blend
// amount (cardTintAmount) plus surfaceText's contrast-safe foreground
// picking handle both variants and any real background's own hue.
func roleHue(role Role) color.Color {
	switch role {
	case RoleUser:
		return lipgloss.Color("#4A7FB5")
	case RoleSystem:
		return lipgloss.Color("#C7A934")
	case RoleError:
		return lipgloss.Color("#B5544A")
	case RoleBlock:
		return lipgloss.Color("#8B93A8")
	default: // RoleAssistant (and any unrecognised role)
		return lipgloss.Color("#9A9488")
	}
}

// cardTintAmount is how far a card surface blends from TerminalBackground()
// toward its roleHue — tuned so the resulting delta against
// TerminalBackground() clears minSurfaceDelta for every role against a
// representative dark, light, AND hued (e.g. Solarized dark, #002b36)
// terminal background; see TestSurfaceDistinctFromTerminalBackground,
// which checks exactly this for several simulated backgrounds.
const cardTintAmount = 0.20

// surfaceText returns whichever of two safe, near-neutral candidates (a
// light near-white, a dark near-black) has the HIGHER contrast against
// bg — guarantees readable text on a surface tint derived from an
// arbitrary real terminal background, instead of a fixed hex chosen for
// only the two built-in dark/light guesses.
func surfaceText(bg color.Color) color.Color {
	light := lipgloss.Color("#F3F3EE")
	dark := lipgloss.Color("#12120F")
	if Contrast(light, bg) >= Contrast(dark, bg) {
		return light
	}
	return dark
}

// ContrastText is surfaceText, exported for a caller that pairs text with
// a background OTHER than one of this package's own derived surfaces —
// e.g. tui/grid's "highlighted but unfocused" row, which used to pair a
// role surface's own foreground with MutedColor() as the background: a
// combination that's only safe in ONE Dark variant (MutedColor() itself
// flips light/dark, but a role surface's foreground didn't flip to
// match), which is what produced the r10 regression — a dark grid row
// text on a dark MutedColor() background in light mode, unreadable. Any
// caller pairing text with an arbitrary/foreign background should read
// its colour from here instead of guessing.
func ContrastText(bg color.Color) color.Color { return surfaceText(bg) }

// colorsFor returns a role's card surface (bg), its hue anchor (border —
// kept for a caller that still wants the role's identifying colour, e.g.
// a future bordered variant; Card itself does not draw one), and a
// contrast-safe foreground for that surface.
func colorsFor(role Role) (bg, border, fg color.Color) {
	hue := roleHue(role)
	bg = blend(TerminalBackground(), hue, cardTintAmount)
	return bg, hue, surfaceText(bg)
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

// realTerminalBackground is set by SetTerminalBackground once the terminal
// has actually answered an OSC 11 query (chatshell.go, via bubbletea's
// tea.RequestBackgroundColor()/BackgroundColorMsg) — nil until then, and
// again nil for any terminal that never answers (most don't hang; a few
// stay silent), in which case TerminalBackground() falls back to this
// package's own guessed default below.
var realTerminalBackground color.Color

// SetTerminalBackground records the terminal's ACTUAL reported background
// colour and derives Dark from ITS luminance — founder 2026-09-25 (r10
// coordinator review, verbatim): "derive surface tints from the ACTUAL
// terminal background ... Fallback to the current defaults when the
// terminal doesn't answer. Chatshell applies the message; products do
// nothing." A product never calls this itself: chatshell wires
// tea.RequestBackgroundColor()/BackgroundColorMsg handling in
// automatically (see chatshell.go's Init/Update). Passing nil clears it,
// reverting TerminalBackground() to the guessed default and leaving Dark
// as whatever it was already set to.
func SetTerminalBackground(c color.Color) {
	realTerminalBackground = c
	if c != nil {
		Dark = relativeLuminance(c) < 0.5
	}
}

// TerminalBackground is "the bare terminal background a card/composer
// surface reads its tint from, and must stay distinguishable against":
// the terminal's own ACTUAL reported background (SetTerminalBackground)
// when known, otherwise this package's guessed default for the current
// Dark variant — a typical terminal emulator default (not the most
// extreme possible pure black/white, which would understate the r9
// regression this guess exists to catch: founder 2026-09-25, "the
// assistant card fill is indistinguishable from the terminal background"
// in dark mode was reproducible against a common near-black default like
// most terminals ship with, not against pure black). Every
// SurfaceDeltaPairs() entry is checked against this, and colorsFor blends
// every card surface FROM it.
func TerminalBackground() color.Color {
	if realTerminalBackground != nil {
		return realTerminalBackground
	}
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

// HalfBlockEdge renders ONE half-block edge row, width cells wide: "▄"
// (foreground=surfaceBG) for the TOP edge — the surface's fill appears to
// start half a line in — or "▀" (same foreground) for the BOTTOM edge.
// Deliberately sets NO background at all (SGR stays at the terminal's own
// default, "49") — founder 2026-09-25 (r10 coordinator review, verbatim):
// "Edge rows and any 'terminal background' cells must NOT set a
// background at all (default bg, SGR 49) — only the fg = surface colour
// on the ▄/▀ glyphs." Painting an explicit TerminalBackground() guess here
// was the r10 regression: on a real terminal whose background differs
// from that guess (a Warp theme, Solarized, ...), it showed as a visibly
// wrong-coloured band above/below every card and the composer. Leaving
// the background cell UNSET lets the real terminal's own background show
// through exactly, always correct by construction — no guess needed for
// the glyph's own "off" half at all. Exported so a caller building its
// own leading/trailing fill around embedded content (chatshell's chip
// strip) can match Card/ComposerFrame's own edge glyph/colour rule
// exactly, rather than re-deriving it.
func HalfBlockEdge(width int, surfaceBG color.Color, top bool) string {
	glyph := "▀"
	if top {
		glyph = "▄"
	}
	if width <= 0 {
		return ""
	}
	return lipgloss.NewStyle().Foreground(surfaceBG).Render(strings.Repeat(glyph, width))
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
	bg, _, fg = colorsFor(RoleBlock)
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
// achieves both at once, see chatshell's View()), and a single blank row
// between the last transcript card and the composer. SUPERSEDED, r12
// (founder, verbatim, seeing the rendered result in Warp): "status line
// should have top margin ... It should be last line on screen" — a THIRD
// blank row now separates the composer from the hints/status bar too (the
// r9 "NO blank row" rule is dropped), and the status bar itself is pinned
// to the terminal's own last row, nothing rendered below it. All three
// blank rows collapse to zero below MarginCollapseRows terminal rows, so a
// short terminal never loses transcript space to decoration.

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

// MarkerColumnWidth is the 1-column focus-marker gutter every Card/
// ComposerFrame reserves to the left of its own surface (see
// surfaceFill's cardBarWidth/composerBarWidth) — blank when unfocused, so
// a card/composer's SURFACE (its own drawn left edge, "▄"/"▀"/fill) always
// starts in the SAME column, terminal column MarkerColumnWidth, whether
// focused or not. A SelfFramed transcript Block (tui/grid — the one
// exception to the Card wrap, see transcript's own SelfFramed doc) draws
// its OWN border directly rather than going through Card, so it must
// reserve this same gutter itself to keep its border lined up with every
// other surface's left edge — see ReserveMarkerColumn.
const MarkerColumnWidth = cardBarWidth

// ReserveMarkerColumn prefixes every line of content — already rendered
// at width-MarkerColumnWidth columns — with a blank MarkerColumnWidth-
// column gutter, producing a block exactly width columns wide whose own
// first drawn column (content's own column 0) lands in terminal column
// MarkerColumnWidth, the same column a Card/ComposerFrame's own surface
// starts in. It never draws a marker glyph itself — a SelfFramed block's
// own focus signalling (e.g. tui/grid's border colour) is unaffected;
// this only shifts position (founder correction, 2026-09-25: "if a
// focused grid currently signals focus only by border colour, keep that;
// just align it").
func ReserveMarkerColumn(content string) string {
	gutter := strings.Repeat(" ", MarkerColumnWidth)
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = gutter + line
	}
	return strings.Join(lines, "\n")
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
// markerCell renders one focus-marker cell (or blank when unfocused/
// unmarked): glyph in FocusColor(), NO background set at all — the
// terminal's own default background shows through, since the marker sits
// OUTSIDE the surface (see surfaceFill's own doc for why).
func markerCell(glyph string, width int, marked bool) string {
	if !marked {
		return strings.Repeat(" ", width)
	}
	return lipgloss.NewStyle().Foreground(FocusColor()).Render(strings.Repeat(glyph, width))
}

// surfaceFill composes ONE filled surface block — a Card or a
// ComposerFrame — from already-painted content: padded horizontally by
// paddingCols at OUTER width, with a barWidth-column focus MARKER
// (markerCell) reserved immediately to the LEFT of the surface — always
// reserved (blank when unfocused, so focusing never shifts layout), never
// part of the surface's own fill. Founder 2026-09-25 (r10 coordinator
// review, verbatim, superseding an earlier same-day "in-surface accent
// bar" instruction): "The focus accent is a narrow marker OUTSIDE the
// surface, in the column immediately left of it, drawn on the terminal
// background (no bg SGR): content rows '▌' fg = focus colour; top edge
// row '▖' ...; bottom edge row '▘' ... The surface itself starts one
// column to the right of the marker column; reserve that marker column
// for every surface (blank when unfocused) so focusing doesn't shift
// layout." One idiom for Card, ComposerFrame(NoTopEdge), and (chatshell's
// own) the side panel.
//
// When HalfBlockEdgesActive(), topEdge/bottomEdge each request a
// HalfBlockEdge row (with its OWN marker cell, '▖'/'▘') in place of a full
// blank padding row on that side; otherwise (fallback) both sides always
// get a full padding row regardless of topEdge/bottomEdge, each with the
// SAME '▌' content-row marker (founder: "Fallback HalfBlockEdges=false:
// marker '▌' on all rows of the surface including the full padding
// rows"), matching this package's original (pre-half-block) rendering
// otherwise unchanged.
func surfaceFill(bg, fg color.Color, focused bool, barWidth, paddingCols int, content string, width int, topEdge, bottomEdge bool) string {
	half := HalfBlockEdgesActive()
	vPad := 1
	if half {
		vPad = 0
	}
	// boxWidth is the surface's OWN width, excluding only the outer
	// marker column -- lipgloss's Style.Width already counts Padding
	// INSIDE the width it's given (confirmed directly: Padding(0,2).
	// Width(20).Render("hi") renders exactly 20 columns wide, padding
	// included, not 20 columns of TEXT plus 4 more of padding). The
	// previous width-barWidth-2*paddingCols here double-subtracted
	// paddingCols -- once here, then AGAIN when Padding itself ate into
	// that already-narrowed box -- shrinking every content row (and the
	// composer's own text line) by 2*paddingCols columns short of the
	// ▄/▀ edge rows, which already correctly spanned width-barWidth.
	// Founder, r13, verbatim: "ask anything line has narrower background
	// then top and bottom lines" (confirmed on both the composer and
	// message cards, edges ~4 columns wider than content -- exactly
	// 2*CardPaddingCols/2*composerPaddingCols).
	boxWidth := max(1, width-barWidth)
	rendered := lipgloss.NewStyle().
		Background(bg).
		Foreground(fg).
		Padding(vPad, paddingCols).
		Width(boxWidth).
		Render(content)
	lines := strings.Split(rendered, "\n")
	marker := markerCell("▌", barWidth, focused)
	for i, line := range lines {
		lines[i] = marker + line
	}
	block := strings.Join(lines, "\n")
	if !half {
		return block
	}
	if topEdge {
		block = markerCell("▖", barWidth, focused) + HalfBlockEdge(width-barWidth, bg, true) + "\n" + block
	}
	if bottomEdge {
		block = block + "\n" + markerCell("▘", barWidth, focused) + HalfBlockEdge(width-barWidth, bg, false)
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
	if focused {
		bg, fg = FocusSurfaceColors()
	}
	content := paintOver(body, bg, fg)
	if header != "" {
		content = lipgloss.NewStyle().Bold(true).Background(bg).Foreground(fg).Render(header) + "\n" + content
	}
	return surfaceFill(bg, fg, focused, cardBarWidth, CardPaddingCols, content, width, true, true)
}

// --- bars (top bar / hints-status bar) ------------------------------------

// HintsInset returns the LEFT/RIGHT column padding TopBar keeps from the
// terminal edge — the SAME columns a Card's own text keeps
// (cardBarWidth+CardPaddingCols on the left — where a card's own marker
// column would sit, plus its padding; CardPaddingCols on the right) — so
// the top bar's title starts in the same column a card's text does
// (founder, r12, verbatim: "[status line] Should have horizontal
// padding", coordinator's own restatement: "align its first key with the
// card text column, same right inset"; item 3, same round: give the top
// bar the same padding for the same alignment). SUPERSEDED for the
// hints/status bar itself, r14 (founder, verbatim: "Status panel should
// be aligned with composer border, not composer text") — see
// StatusInset, which StatusLine now uses instead.
func HintsInset() (left, right int) { return cardBarWidth + CardPaddingCols, CardPaddingCols }

// StatusInset returns the LEFT/RIGHT column padding StatusLine (the
// hints/status bar) keeps — aligned to the composer's own SURFACE edge
// (the column immediately after its marker column, where its own ▄/▀
// edge itself begins and ends), NOT the card/composer TEXT column
// HintsInset aligns the top bar to — founder, r14, verbatim: "Status
// panel should be aligned with composer border, not composer text."
// Right is 0: the status line's own right end reaches the composer
// surface's right edge exactly, the same as the ▄/▀ edge (which spans
// the full outer width minus only the marker column).
func StatusInset() (left, right int) { return composerBarWidth, 0 }

// insetLine pads content to width - left - right (truncating with an
// ellipsis if it overflows) and surrounds it with left/right blank
// columns — the shared horizontal-inset shape Bar and StatusLine both
// build on, so a hint/title's first character always lands on the same
// column regardless of which of the two chromes is rendering it.
func insetLine(width int, content string, left, right int) (padded string, inner int) {
	inner = max(1, width-left-right)
	truncated := ansi.Truncate(content, inner, "…")
	return strings.Repeat(" ", left) + lipgloss.NewStyle().Width(inner).Render(truncated) + strings.Repeat(" ", right), inner
}

// Bar pads content to width and fills the remainder with the shared chrome
// background, truncating with an ellipsis if content overflows — so a bar's
// right edge always reaches the terminal edge, whatever a product supplied.
// content may already contain nested lipgloss-styled spans (e.g. TopBar's
// per-item styling); Bar only sets the background/width frame and the
// HintsInset() horizontal padding around it, not a foreground override, so
// those spans' own colours survive. Used by TopBar only — see StatusLine
// for the hints/status bar's own (backgroundless) chrome.
func Bar(width int, content string) string {
	bg, fg := barColors()
	base := lipgloss.NewStyle().Bold(true).Foreground(fg)
	left, right := HintsInset()
	padded, _ := insetLine(width, base.Render(content), left, right)
	return lipgloss.NewStyle().Background(bg).Width(max(1, width)).Render(paintOver(padded, bg, fg))
}

// StatusLine renders ONE hints/status-bar row with NO background at all
// (the terminal's own default background shows through, SGR 49) and
// StatusInset()'s horizontal padding — founder, r12, verbatim, seeing the
// rendered result in Warp: "status line should have top margin and have
// no background ... Should have horizontal padding"; r14, verbatim,
// superseding the alignment specifically: "Status panel should be
// aligned with composer border, not composer text." content may already
// carry its own nested styling (RenderHints' per-hint key/label
// colouring) — StatusLine adds none of its own beyond the padding, so
// those spans' own colours are the only styling on the line; unlike Bar,
// there is no fill to reassert after a nested reset (paintOver exists to
// protect a BACKGROUND from an embedded reset — with none set here, there
// is nothing for a reset to cut off).
func StatusLine(width int, content string) string {
	left, right := StatusInset()
	padded, _ := insetLine(width, content, left, right)
	return padded
}

// padRight pads content with plain trailing spaces up to width, measured
// by ansi.StringWidth (ANSI-aware, so embedded colour codes are never
// counted as visible columns) — a no-op when content is already width or
// wider. Deliberately NOT lipgloss's own Style.Width(): that wraps
// content onto MULTIPLE lines once it exceeds the given width (it is a
// wrapping primitive, not a non-destructive pad) — wrong here, and the
// bug hintsWrappedLine below hit before switching to this: an atomic
// over-wide hint token got split mid-word across two physical lines
// instead of staying on its own single (over-wide) line.
func padRight(content string, width int) string {
	if w := ansi.StringWidth(content); w < width {
		return content + strings.Repeat(" ", width-w)
	}
	return content
}

// hintsWrappedLine pads ONE already-wrapped RenderHints line to width with
// StatusInset()'s left/right blank columns, WITHOUT truncating it —
// unlike StatusLine, wrapTokens already guarantees this specific line
// fits its own packing budget (width - StatusInset() columns), so
// truncating it again here would be redundant at best; at worst, for the
// ONE deliberate exception — a single hint pair or word wider than the
// whole line, which wrapTokens still gives its own line rather than
// cutting (see wrapTokens' own doc: "never cut a key/label pair" /
// "never truncated mid-word with an ellipsis") — re-truncating here
// would silently defeat that guarantee, and lipgloss's own Width() WRAPS
// rather than pads (see padRight's own doc), which would defeat it
// differently. That one line is simply allowed to render WIDER than
// width instead; a lost hint or a mid-word cut is worse either way.
func hintsWrappedLine(width int, content string) string {
	left, right := StatusInset()
	inner := max(1, width-left-right)
	return strings.Repeat(" ", left) + padRight(content, inner) + strings.Repeat(" ", right)
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
	left, right := StatusInset()
	lines := wrapTokens(tokens, max(1, width-left-right))
	if len(lines) == 0 {
		return hintsWrappedLine(width, "")
	}
	styled := make([]string, len(lines))
	for i, line := range lines {
		styled[i] = hintsWrappedLine(width, line)
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

// ComposerTextColumn returns the absolute column (0-based, from the
// composer's own left edge — the marker column) where the composer's
// typed text / placeholder starts: the marker column plus the left
// padding. A caller drawing content ABOVE the input on the composer's own
// surface (chatshell's chip strip) uses this to line its own content up
// with that same column — founder, r11 (Warp feedback, verbatim): "I
// think first chip text should be aligned with text of the message."
func ComposerTextColumn() int { return composerBarWidth + composerPaddingCols }

// ComposerChipLeadingFill returns how many "▄" edge-filler columns belong
// between the composer's marker column and the first attachment chip's
// own cell, so that chip's LABEL — which starts 1 column into the cell,
// after the cell's own leading inner-padding space — lands exactly on
// ComposerTextColumn(). It also guarantees the composer's own top-left
// corner (marker + at least one edge-filler column) is always rendered
// before any chip, never overdrawn by one — founder, r11: "Attachment
// chips should have margin on left so left top corner is always
// rendered."
func ComposerChipLeadingFill() int { return composerPaddingCols - 1 }

// ComposerChipMarker renders the same focus-marker cell (see markerCell)
// the composer's own edges show, for a caller (chatshell's chip strip)
// that draws the composer's top edge itself when chips are present.
func ComposerChipMarker(focused bool) string {
	return markerCell("▖", composerBarWidth, focused)
}

// composerTintAmount is how far the composer's OWN unfocused fill blends
// from TerminalBackground() toward the block hue — DELIBERATELY weaker
// than cardTintAmount and computed separately (not SurfaceColors(), which
// a grid cell/sidebar row/panel frame also read and needs the stronger
// card-level tint to be visible as "a surface" in its own right): the
// composer must stay within [minComposerSurfaceDelta,
// maxComposerSurfaceDelta] even BEFORE the focus nudge below, while a
// card's tint only has a floor (SurfaceDeltaPairs), never a ceiling.
const composerTintAmount = 0.11

// ComposerColors returns the composer's UNFOCUSED fill: TerminalBackground()
// blended composerTintAmount toward the block hue, with a contrast-safe
// foreground (surfaceText) — see the package doc on colorsFor for why
// this derives from the REAL/guessed terminal background rather than a
// fixed hex pair.
func ComposerColors() (bg, fg color.Color) {
	bg = blend(TerminalBackground(), roleHue(RoleBlock), composerTintAmount)
	return bg, surfaceText(bg)
}

// chipTintAmount is how far an attachment chip's fill blends from
// ComposerColors()' own background toward the block hue — deliberately
// STRONGER than composerTintAmount so a chip clears minChipSurfaceDelta
// against the composer surface it sits on (not just against
// TerminalBackground(), which ComposerColors already separately clears):
// founder 2026-09-25 (r10 coordinator review, verbatim): "Chips are there
// ... but too subtle to notice (low contrast vs the composer edge/tint).
// Make them clearly visible: chip surface a distinct step from the
// composer surface".
const chipTintAmount = 0.25

// minChipSurfaceDelta is the minimum contrast ratio a chip's fill must
// have against ComposerColors()' background — the composer-specific
// analogue of minSurfaceDelta (a card's floor against
// TerminalBackground()), checked by ChipDeltaPair/TestChipDistinctFromComposer.
const minChipSurfaceDelta = 1.15

// ChipColors returns an attachment chip's UNFOCUSED fill: ComposerColors'
// own background blended chipTintAmount further toward the block hue,
// with a contrast-safe foreground (surfaceText) — label text and the "×"
// close glyph both read this pair, so both clear bodyTextMinRatio
// (verified by ContrastPairs' chip entries).
func ChipColors() (bg, fg color.Color) {
	composerBG, _ := ComposerColors()
	bg = blend(composerBG, roleHue(RoleBlock), chipTintAmount)
	return bg, surfaceText(bg)
}

// ChipCloseColor returns the colour a chip's "×" close glyph reads in —
// "muted, but still >= 4.5:1" (founder 2026-09-25, r10 coordinator
// review): MutedColor() itself whenever it clears bodyTextMinRatio
// against bg, otherwise the SAME full-contrast colour the chip's own
// label text uses (surfaceText(bg)) — MutedColor() is a fixed pair picked
// for the ORIGINAL (weaker) surface tints; a stronger chip fill can push
// it under 4.5:1 for a given real terminal background, and this
// guarantees compliance either way rather than assuming it always holds.
func ChipCloseColor(bg color.Color) color.Color {
	muted := MutedColor()
	if Contrast(muted, bg) >= bodyTextMinRatio {
		return muted
	}
	return surfaceText(bg)
}

// ChipDeltaPair names the attachment-chip fill and the minimum contrast
// ratio it must have against ComposerColors()' background — for the
// CURRENT Dark variant — the source of truth
// TestChipDistinctFromComposer checks, mirroring SurfaceDeltaPairs' own
// card-vs-terminal floor but for chip-vs-composer.
type ChipDeltaPair struct {
	Name         string
	ComposerBG   color.Color
	Surface      color.Color
	MinimumRatio float64
}

// ChipDeltaPairs returns the one chip-vs-composer delta pair (unfocused
// chip fill vs the unfocused composer background it sits on — a focused
// chip uses FocusSurfaceColors(), already the maximum-contrast accent
// pair and not a "blend toward invisible" risk the way the unfocused chip
// is).
func ChipDeltaPairs() []ChipDeltaPair {
	composerBG, _ := ComposerColors()
	chipBG, _ := ChipColors()
	return []ChipDeltaPair{
		{Name: "chip fill vs composer background", ComposerBG: composerBG, Surface: chipBG, MinimumRatio: minChipSurfaceDelta},
	}
}

// composerFocusTint is the mixing weight ComposerFocusColors blends
// FocusColor() into the composer's unfocused fill by — a "slightly
// stronger tint", not the full bright FocusColor() fill Card/
// FocusSurfaceColors uses. Founder correction, twice over: first (r9)
// "a SUBTLE filled area (a tint one step off the terminal background,
// like the cards) ... focus = the thin left accent bar in the focus
// colour + slightly stronger tint (NOT a full bright fill)"; then,
// verbatim, more precisely: "the background of composer should just
// slightly differ from overall/chat background ... Focus is shown by the
// thin left accent bar (and at most a MARGINALLY stronger tint)". 0.03
// keeps the FOCUSED fill's own delta against TerminalBackground() inside
// [minComposerSurfaceDelta, maxComposerSurfaceDelta] — barely past the
// UNFOCUSED fill's own delta, never a "bright slab". A Card intentionally
// goes full FocusSurfaceColors on focus (there's no better cue: an entire
// message either has focus or doesn't), but the composer already reads as
// focused via its left accent bar and the cursor inside it, so its own
// fill only needs the smallest nudge toward the accent, not become it.
const composerFocusTint = 0.03

// ComposerFocusColors returns the composer's FOCUSED fill: ComposerColors'
// own unfocused background blended composerFocusTint of the way toward
// FocusColor() (foreground unchanged — the blend is subtle enough that
// ComposerColors' foreground still meets bodyTextMinRatio against it;
// verified by TestContrastMeetsWCAG's composer pairs).
func ComposerFocusColors() (bg, fg color.Color) {
	surfaceBG, surfaceFG := ComposerColors()
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
	bg, fg := ComposerColors()
	if focused {
		bg, fg = ComposerFocusColors()
	}
	return surfaceFill(bg, fg, focused, composerBarWidth, composerPaddingCols, paintOver(content, bg, fg), width, topEdge, true)
}

// --- panel / sidebar rows ------------------------------------------------

// SelectedRow renders one side-panel/sidebar row: "  text" unselected, or a
// FocusSurfaceColors()-highlighted "› text" when selected — the SAME accent
// a focused card's border and a grid's highlighted row use, so a selected
// list row, a focused message, and a selected grid row all read as the
// same kind of thing (founder 2026-09-25: "Use the single theme focus/
// selection colour everywhere").
// width is the row's own full inner width (the same width its text was
// laid out at) -- selected passes it to Width() so the highlight fill
// spans every column of the row, not just as many as "› "+text happens to
// need (2026-09-25 fix: a short row previously left its own highlight
// looking like a narrow chip instead of a full-width selected bar, the
// same "surface spans identical columns on every row" rule Card/
// PanelFrame already follow).
func SelectedRow(text string, selected bool, width int) string {
	if !selected {
		return "  " + text
	}
	bg, fg := FocusSurfaceColors()
	return lipgloss.NewStyle().Bold(true).Background(bg).Foreground(fg).Width(max(1, width)).Render("› " + text)
}

// PanelHeader renders a side-panel/sidebar title header, e.g. "Sidebar" or
// a product workspace pane's tab name.
func PanelHeader(title string) string {
	return lipgloss.NewStyle().Bold(true).Render(title)
}

// panelGapWidth is the plain gap PanelFrame leaves between the transcript
// and the side panel — founder 2026-09-25 (superseding the earlier "│"
// divider): "Let's remove border between chat and side panels - margin is
// enough. Replace the '│' divider with a gap: 2 columns of plain terminal
// background (no bg SGR, no glyph) between the transcript column and the
// side panel; the side panel is its own surface... so the gap reads as
// the separation." SUPERSEDED IN PART, r13 (founder, verbatim: "Side
// panel should NOT have left accent border treatment. Can we just
// slightly change background?"): the panel no longer reserves a column as
// a focus MARKER — panel focus is shown by PanelFocusColors() instead
// (see its own doc). Only ONE blank column survives, as the divider
// between the chat column and the panel's own surface (2026-09-25 margin
// fix: a stale second reserved column here — PanelFrameSize() claiming 2
// when PanelFrame itself only ever drew 1 — silently widened chatshell's
// own outer-margin budget by an extra column, the root cause of the side
// panel ending 2 columns short of the terminal's right edge instead of 1
// (matching the chat column's own single-column left margin); see
// chatshell's chatWidth/sidebarWidth doc).
const panelGapWidth = 1

// panelPaddingCols is the panel content's own left/right inset from its
// surface edge — the SAME CardPaddingCols a card's text keeps from ITS
// surface edge (2026-09-25 coordinator finding: panel header/rows started
// flush against the panel surface's first column ("Beacon", "●
// Project" touching the edge) while every card's text sits
// CardPaddingCols in; PanelFrame now reserves the same inset, via
// surfaceFill's own paddingCols parameter, so panel content lines up with
// card text).
const panelPaddingCols = CardPaddingCols

// PanelFrameSize returns how many extra columns/rows PanelFrame adds
// around its content, so a caller (chatshell's panel sizing) can size the
// panel's own content and reserve the right amount of screen space —
// mirroring ComposerFrameSize. cols is the one gap column (panelGapWidth)
// plus panelPaddingCols on BOTH sides of the content. rows is a CONSTANT 2
// regardless of HalfBlockEdgesActive() — the SAME vPad-vs-edges
// equalisation ComposerFrameSize relies on (see surfaceFill's own doc): 1
// padding row top+bottom in fallback mode, or the top/bottom half-block
// edge rows in half-block mode — either way exactly 2 extra rows, so a
// caller's row
// budget never has to branch on which mode is active.
func PanelFrameSize() (cols, rows int) { return panelGapWidth + 2*panelPaddingCols, 2 }

// panelTintAmount is how far the panel's OWN unfocused fill blends from
// TerminalBackground() toward the block hue — the SAME weight
// SurfaceColors() itself uses (a panel reads as "the same kind of plain
// neutral surface" a grid cell/sidebar row already is), kept as its own
// named constant here (rather than calling SurfaceColors() directly) so
// PanelColors/PanelFocusColors read as a single deliberate pair, not an
// incidental reuse.
const panelTintAmount = cardTintAmount

// panelFocusTint is how far the panel's FOCUSED fill blends, from its own
// UNFOCUSED fill (not from TerminalBackground() directly), toward the
// block hue — founder, r13, verbatim: "Side panel should NOT have left
// accent border treatment. Can we just slightly change background?"
// Panel focus is now a subtle surface-tint shift instead of a marker
// column: picked so the two panel surfaces' own contrast ratio against
// EACH OTHER (not against terminal background) stays perceptible but
// small — TestPanelFocusSurfaceDeltaIsSubtle checks it lands in
// [minPanelFocusDelta, maxPanelFocusDelta] in both theme.Dark variants
// and against every simulatedTerminalBackgrounds entry.
const panelFocusTint = 0.12

// minPanelFocusDelta/maxPanelFocusDelta bound PanelFocusColors()' own
// background against PanelColors()' (see panelFocusTint's doc) — founder,
// r13: "delta small but perceptible."
const (
	minPanelFocusDelta = 1.05
	maxPanelFocusDelta = 1.2
)

// PanelColors returns the side panel's own UNFOCUSED surface background+
// foreground pair — SurfaceColors() in every way but its own named
// constant (panelTintAmount), so a change to one never silently retunes
// the other even though they start out equal.
func PanelColors() (bg, fg color.Color) {
	bg = blend(TerminalBackground(), roleHue(RoleBlock), panelTintAmount)
	return bg, surfaceText(bg)
}

// PanelFocusColors returns the side panel's FOCUSED surface pair: its own
// unfocused bg (PanelColors()) blended a further panelFocusTint toward the
// block hue — see panelFocusTint's own doc. Foreground is re-derived
// (surfaceText) against the NEW bg, so it always clears bodyTextMinRatio
// regardless of the tint shift.
func PanelFocusColors() (bg, fg color.Color) {
	unfocusedBG, _ := PanelColors()
	bg = blend(unfocusedBG, roleHue(RoleBlock), panelFocusTint)
	return bg, surfaceText(bg)
}

// PanelFrame renders side-panel content (SidePanel or the default
// sidebar) as a single FULL-HEIGHT card surface, spanning EXACTLY height
// rows of content (PanelFrameSize's own row overhead is added on top of
// that, same as Card/ComposerFrame) — founder, r12 (Warp feedback,
// verbatim): "side panel should be full height card." It reuses the
// surfaceFill machinery Card/ComposerFrame do for the fill itself and the
// half-block top/bottom edges when HalfBlockEdgesActive() — but, unlike
// Card/ComposerFrame, reserves NO focus-marker column at all (barWidth 0):
// founder, r13, verbatim: "Side panel should NOT have left accent border
// treatment... just slightly change background" — focused vs unfocused is
// PanelFocusColors() vs PanelColors() instead (see their own docs). header
// renders bold as the surface's own first content row (when non-empty);
// any row of the surface past what header+content actually fill is left
// blank SURFACE, never terminal background — founder, r12: "the empty
// space below its content filled with the panel surface". content itself
// supplies only its OWN per-row styling (e.g. tui/sidebar's SelectedRow
// highlight, which still uses the single shared FocusSurfaceColors()
// selection accent) — the surrounding fill is entirely PanelFrame's job,
// including panelPaddingCols' own left/right text inset (surfaceFill's
// paddingCols, same as Card's — 2026-09-25 fix: content used to be laid
// out flush against the surface's own first/last column). A single blank
// column of plain terminal background sits before the surface
// (panelGapWidth) — the divider between the chat column and the panel.
func PanelFrame(width, height int, header, content string, focused bool) string {
	bg, fg := PanelColors()
	if focused {
		bg, fg = PanelFocusColors()
	}
	width, height = max(1, width), max(1, height)
	lines := make([]string, height)
	start := 0
	if header != "" {
		lines[0] = lipgloss.NewStyle().Bold(true).Render(header)
		start = 1
	}
	for i, line := range strings.Split(content, "\n") {
		row := start + i
		if row >= height {
			break
		}
		lines[row] = line
	}
	body := paintOver(strings.Join(lines, "\n"), bg, fg)
	card := surfaceFill(bg, fg, false, 0, panelPaddingCols, body, max(1, width-panelGapWidth), true, true)
	cardLines := strings.Split(card, "\n")
	for i, line := range cardLines {
		cardLines[i] = " " + line
	}
	return strings.Join(cardLines, "\n")
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
			// mdrender's own inline-code style reads Hex(AccentColor()) as
			// its Code.Color, with NO background of its own (transparent —
			// inherits whichever card it's rendered inside) — so the pair
			// that must clear WCAG is AccentColor() against EVERY role's
			// own card background, not a single fixed background. Founder,
			// r12: "verify glamour's inline-code colour ... meets the
			// >= 4.5:1 rule in both themes" (a real "10:00–10:15" code span
			// rendering illegibly red was the report this replaces —
			// glamour's own built-in "dark"/"light" styles pick a fixed
			// ANSI-256 red/pink, color 203, that clears 4.5:1 against
			// glamour's OWN dark code-background by only a hair and fails
			// outright against its light one).
			ContrastPair{Name: "inline code text (AccentColor) on " + string(role) + " card background", FG: AccentColor(), BG: bg, MinimumRatio: bodyTextMinRatio},
		)
	}

	surfaceBG, surfaceFG := SurfaceColors()
	pairs = append(pairs,
		ContrastPair{Name: "surface body text (grid cell, sidebar row, panel frame)", FG: surfaceFG, BG: surfaceBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "muted text on surface (grid footer, unfocused chip)", FG: MutedColor(), BG: surfaceBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "accent text on surface (grid selected-column cell)", FG: AccentColor(), BG: surfaceBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "focus border vs surface background", FG: FocusColor(), BG: surfaceBG, MinimumRatio: nonTextMinRatio},
	)

	panelBG, panelFG := PanelColors()
	focusedPanelBG, focusedPanelFG := PanelFocusColors()
	pairs = append(pairs,
		ContrastPair{Name: "panel body text (unfocused)", FG: panelFG, BG: panelBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "panel body text (focused)", FG: focusedPanelFG, BG: focusedPanelBG, MinimumRatio: bodyTextMinRatio},
	)

	focusBG, focusFG := FocusSurfaceColors()
	pairs = append(pairs,
		ContrastPair{Name: "selected/highlighted row text on selection background", FG: focusFG, BG: focusBG, MinimumRatio: bodyTextMinRatio},
	)

	barBG, barFG := barColors()
	pairs = append(pairs,
		ContrastPair{Name: "top bar text", FG: barFG, BG: barBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "focus border vs bar background", FG: FocusColor(), BG: barBG, MinimumRatio: nonTextMinRatio},
	)

	// The hints/status bar itself paints NO background any more (theme.
	// StatusLine — founder, r12, verbatim: "status line ... have no
	// background") — its key/label text sits directly on whatever the
	// terminal's own background is, so THIS is the pair that must clear
	// WCAG now, not barColors()'s bg (which the status bar no longer
	// uses at all).
	termBG := TerminalBackground()
	pairs = append(pairs,
		ContrastPair{Name: "hint key (AccentColor) on terminal background", FG: AccentColor(), BG: termBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "hint label (MutedColor) on terminal background", FG: MutedColor(), BG: termBG, MinimumRatio: bodyTextMinRatio},
	)

	composerBG, composerFG := ComposerColors()
	focusComposerBG, focusComposerFG := ComposerFocusColors()
	pairs = append(pairs,
		ContrastPair{Name: "composer typed text (unfocused)", FG: composerFG, BG: composerBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "composer typed text (focused)", FG: focusComposerFG, BG: focusComposerBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "composer placeholder (unfocused)", FG: MutedColor(), BG: composerBG, MinimumRatio: placeholderMinRatio},
		ContrastPair{Name: "composer placeholder (focused)", FG: MutedColor(), BG: focusComposerBG, MinimumRatio: placeholderMinRatio},
		ContrastPair{Name: "focus bar vs composer fill (focused)", FG: FocusColor(), BG: focusComposerBG, MinimumRatio: nonTextMinRatio},
	)

	chipBG, chipFG := ChipColors()
	pairs = append(pairs,
		ContrastPair{Name: "chip label text", FG: chipFG, BG: chipBG, MinimumRatio: bodyTextMinRatio},
		ContrastPair{Name: "chip close glyph (muted) on chip fill", FG: ChipCloseColor(chipBG), BG: chipBG, MinimumRatio: bodyTextMinRatio},
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
	composerBG, _ := ComposerColors()
	focusComposerBG, _ := ComposerFocusColors()
	pairs = append(pairs,
		SurfaceDeltaPair{Name: "composer fill (unfocused) vs terminal background", Surface: composerBG, MinimumRatio: minComposerSurfaceDelta, MaximumRatio: maxComposerSurfaceDelta},
		SurfaceDeltaPair{Name: "composer fill (focused) vs terminal background", Surface: focusComposerBG, MinimumRatio: minComposerSurfaceDelta, MaximumRatio: maxComposerSurfaceDelta},
	)
	return pairs
}

// Hex renders c as a "#rrggbb" string — for a caller that needs a plain
// hex literal rather than a color.Color (e.g. mdrender's own glamour
// ansi.StyleConfig, whose StylePrimitive.Color is a *string), so it can
// still read every colour from this package rather than hard-coding one
// of its own (this package's own no-hard-coded-literal rule, applied to
// an external library's string-typed config).
func Hex(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
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
