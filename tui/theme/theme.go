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
	return pick(lipgloss.Color("#F1F1EC"), lipgloss.Color("#20232A")),
		pick(lipgloss.Color("#A8A89C"), lipgloss.Color("#4B4F58")),
		pick(lipgloss.Color("#1B1B18"), lipgloss.Color("#E5E5E1"))
}

func systemColors() (bg, border, fg color.Color) {
	return pick(lipgloss.Color("#FBF3D8"), lipgloss.Color("#3A331A")),
		pick(lipgloss.Color("#C7A934"), lipgloss.Color("#9C8330")),
		pick(lipgloss.Color("#4A3B00"), lipgloss.Color("#F1DE97"))
}

func errorColors() (bg, border, fg color.Color) {
	return pick(lipgloss.Color("#FBE1DE"), lipgloss.Color("#3A1D1A")),
		pick(lipgloss.Color("#C6584A"), lipgloss.Color("#B5544A")),
		pick(lipgloss.Color("#4A0F09"), lipgloss.Color("#F5C6BF"))
}

func blockColors() (bg, border, fg color.Color) {
	return pick(lipgloss.Color("#EDEEF4"), lipgloss.Color("#1E222B")),
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
	content := body
	if header != "" {
		content = lipgloss.NewStyle().Bold(true).Background(bg).Foreground(fg).Render(header) + "\n" + body
	}
	fillStyle := lipgloss.NewStyle().
		Background(bg).
		Foreground(fg).
		Padding(CardPaddingRows, CardPaddingCols).
		Width(InnerWidth(width))
	return addLeftBar(fillStyle.Render(content), barColor)
}

// addLeftBar prepends a 1-column bar (bg-filled with barColor) to every
// line of block — Card's focus indicator, and ComposerFrame's/PanelFrame's
// analogous accent column.
func addLeftBar(block string, barColor color.Color) string {
	bar := lipgloss.NewStyle().Background(barColor).Render(" ")
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		lines[i] = bar + line
	}
	return strings.Join(lines, "\n")
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
	return lipgloss.NewStyle().Background(bg).Width(max(1, width)).Render(base.Render(truncated))
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
	tokens := make([]string, 0, len(hints)+len(segments))
	tokens = append(tokens, segments...)
	for _, h := range hints {
		tokens = append(tokens, keyStyle.Render(h.Key)+" "+labelStyle.Render(h.Label))
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

// wrapTokens packs tokens onto as few lines as fit within maxWidth,
// breaking to a new line only when the next token would not fit — ported
// from DataTug's wrapStatusSegments (datatug-cli/pkg/chat/chatui.go,
// before this package existed).
func wrapTokens(tokens []string, maxWidth int) []string {
	const separator = "   "
	lines := make([]string, 0, len(tokens))
	current := ""
	for _, token := range tokens {
		token = ansi.Truncate(token, maxWidth, "…")
		candidate := token
		if current != "" {
			candidate = current + separator + token
		}
		if current != "" && ansi.StringWidth(candidate) > maxWidth {
			lines = append(lines, current)
			current = token
			continue
		}
		current = candidate
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
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

// ComposerFrameSize returns how many extra columns/rows ComposerFrame adds
// around its content, so a caller (chatshell's resize/historyHeight) can
// size the inner input and reserve the right amount of screen space. Rows
// is always 0 — a filled background needs no extra rows, unlike the
// earlier bordered design's top/bottom border lines.
func ComposerFrameSize() (cols, rows int) { return composerBarWidth, 0 }

// ComposerFrame wraps a composer's rendered input view in the shared
// filled background: SurfaceColors unfocused, FocusSurfaceColors plus a
// left accent bar while focused — the same accent a focused card or a
// selected panel row uses, so "the composer has focus" reads consistently
// with every other zone.
func ComposerFrame(width int, content string, focused bool) string {
	bg, fg := SurfaceColors()
	barColor := bg
	if focused {
		bg, fg = FocusSurfaceColors()
		barColor = FocusColor()
	}
	style := lipgloss.NewStyle().Background(bg).Foreground(fg).Width(max(1, width-composerBarWidth))
	return addLeftBar(style.Render(content), barColor)
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
// other zone (founder 2026-09-25: "Same for ... side panel"), without a
// full box around it. The panel's own content (e.g. tui/sidebar's own
// header line, or a product SidePanel's own tab strip) supplies whatever
// header/label it wants; PanelFrame only supplies the divider and the
// panel's background fill.
func PanelFrame(width int, content string, focused bool) string {
	bg, fg := SurfaceColors()
	style := lipgloss.NewStyle().Background(bg).Foreground(fg).Width(max(1, width-panelDividerWidth))
	return addLeftBar(style.Render(content), BorderColor(focused))
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
