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
func MutedColor() color.Color { return pick(lipgloss.Color("#6B6B63"), lipgloss.Color("#8A8F98")) }

// AccentColor is the shared emphasis colour for a hint's key/shortcut
// (distinct from FocusColor, which marks focus/selection specifically).
func AccentColor() color.Color { return pick(lipgloss.Color("#8A5A00"), lipgloss.Color("#E8C25A")) }

func barColors() (bg, fg color.Color) {
	return pick(lipgloss.Color("#D8DCE6"), lipgloss.Color("#2E3440")),
		pick(lipgloss.Color("#101820"), lipgloss.Color("#ECEFF4"))
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

// CardPadding is the horizontal space, in columns, kept between a card's
// border and its content on each side.
const CardPadding = 1

// borderColumns is how many columns a card's left+right border consumes.
const borderColumns = 2

// InnerWidth returns the content width available inside a card of the
// given OUTER width — what a Block should render at (via
// theme.InnerWidth(width)) so its own View(width, focused) output lines up
// exactly with the card Card(...) then wraps it in.
func InnerWidth(width int) int {
	return max(1, width-borderColumns-2*CardPadding)
}

// Card renders body (and, when non-empty, header above it in bold) inside a
// padded, coloured, rounded-border card for role, at OUTER width — the
// same visual language across every product: a distinct but harmonious
// background/border per role, and a clearly visible, thicker, accent
// border while focused (DataTug's current selection highlighting is the
// reference look).
func Card(role Role, header, body string, width int, focused bool) string {
	bg, border, fg := colorsFor(role)
	b := lipgloss.RoundedBorder()
	if focused {
		b = lipgloss.ThickBorder()
		border = FocusColor()
	}
	style := lipgloss.NewStyle().
		Background(bg).
		Foreground(fg).
		BorderStyle(b).
		BorderForeground(border).
		BorderBackground(bg).
		Padding(0, CardPadding).
		Width(InnerWidth(width))
	content := body
	if header != "" {
		content = lipgloss.NewStyle().Bold(true).Foreground(fg).Render(header) + "\n" + body
	}
	return style.Render(content)
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

// RenderHints renders the shared hints/status bar: each Hint as its key (in
// AccentColor, bold) followed by its label (in MutedColor), hints separated
// by two spaces, then any trailing right-of-hints segments (e.g. a product/
// session summary or a hyperlink) separated by " │ " — DataTug's original
// statusBar layout, now the default for every product. A product supplies
// only the hint list and segment strings; chatshell.WithHintsProvider (or
// the plain SetStatus default) wires this in automatically.
func RenderHints(width int, hints []Hint, segments ...string) string {
	keyStyle := lipgloss.NewStyle().Bold(true).Foreground(AccentColor())
	labelStyle := lipgloss.NewStyle().Foreground(MutedColor())
	parts := make([]string, 0, len(hints))
	for _, h := range hints {
		parts = append(parts, keyStyle.Render(h.Key)+" "+labelStyle.Render(h.Label))
	}
	content := strings.Join(parts, "  ")
	if len(segments) > 0 {
		joined := strings.Join(segments, " │ ")
		if content != "" {
			content = joined + "   " + content
		} else {
			content = joined
		}
	}
	return Bar(width, content)
}

// --- composer frame --------------------------------------------------

// composerBorderColumns/Rows is the frame ComposerFrame draws around a
// composer's own rendered view: one column/row of border on each side (no
// extra padding — the textarea already reserves its own).
const composerBorderColumns = 2
const composerBorderRows = 2

// ComposerFrameSize returns how many extra columns/rows ComposerFrame adds
// around its content, so a caller (chatshell's resize/historyHeight) can
// size the inner input and reserve the right amount of screen space.
func ComposerFrameSize() (cols, rows int) { return composerBorderColumns, composerBorderRows }

// ComposerFrame wraps a composer's rendered input view in the shared
// bordered box: FocusColor and a thicker border while focused, MutedColor
// and a thinner one otherwise — the same accent a focused card or a
// selected panel row uses, so "the composer has focus" reads consistently
// with every other zone.
func ComposerFrame(width int, content string, focused bool) string {
	border := MutedColor()
	b := lipgloss.RoundedBorder()
	if focused {
		border = FocusColor()
		b = lipgloss.ThickBorder()
	}
	style := lipgloss.NewStyle().BorderStyle(b).BorderForeground(border).Width(max(1, width-composerBorderColumns))
	return style.Render(content)
}

// --- panel / sidebar rows ------------------------------------------------

// SelectedRow renders one side-panel/sidebar row: "  text" unselected, or
// "› text" in FocusColor+bold when selected — the same accent a focused
// card uses, so a selected list row and a focused message read as the same
// kind of thing.
func SelectedRow(text string, selected bool) string {
	if !selected {
		return "  " + text
	}
	return lipgloss.NewStyle().Bold(true).Foreground(FocusColor()).Render("› " + text)
}

// PanelHeader renders a side-panel/sidebar title header, e.g. "Sidebar" or
// a product workspace pane's tab name.
func PanelHeader(title string) string {
	return lipgloss.NewStyle().Bold(true).Render(title)
}
