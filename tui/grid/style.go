package grid

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/strongo/aichat/tui/theme"
)

// Style is a grid's border/header color preset. It is presentation only:
// changing it never rewrites rows or columns. Ported from DataTug's
// table_style.go (tableStyle), generalised so any product can define and
// cycle through its own presets.
type Style struct {
	Name        string
	BorderColor color.Color
	HeaderStyle lipgloss.Style
}

// Built-in style presets, ported from DataTug's table_style.go. Every
// colour is resolved fresh from tui/theme on each access (via a function,
// not a package-level var baked in at import time — theme.Dark can change
// at runtime, e.g. a product calling theme.SetDark, and a memoised colour
// would silently keep rendering the OLD variant forever after — see
// styleLines/styleSoft/styleMinimal below), so a grid always renders in
// whichever Dark variant is current, light or dark, never a hard-coded
// background that only looks right in one of them.
var (
	StyleLines   = styleLines()
	StyleSoft    = styleSoft()
	StyleMinimal = styleMinimal()
)

// styleLines/styleSoft/styleMinimal build this preset's Style fresh from
// the CURRENT tui/theme colours. ParseStyle/cycling only ever compare
// Style.Name, so a Style value captured before a later theme.SetDark call
// (e.g. these three package vars above, kept for API compatibility) is
// only ever used as a NAME lookup key, never rendered directly — grid.go's
// buildTable instead re-resolves the live preset via currentStyle(m.style.
// Name) on every render, which always reflects theme's current variant.
// Lines/Soft/Minimal stay texturally distinguishable from each other (a
// different BorderColor each) while every colour still comes from
// tui/theme, never a preset-local literal: Lines uses MutedColor() (the
// same neutral border every other unfocused frame in aichat uses), Soft
// AccentColor() (a warmer, softer divider), Minimal FocusColor() (reused
// here purely for its own distinct hue, not to imply focus — Minimal's
// header carries no background fill, which is what actually reads as
// "minimal").
func styleLines() Style {
	bg, fg := theme.SurfaceColors()
	return Style{
		Name:        "Lines",
		BorderColor: theme.MutedColor(),
		HeaderStyle: lipgloss.NewStyle().Background(bg).Foreground(fg).Bold(true),
	}
}

func styleSoft() Style {
	bg, fg := theme.SurfaceColors()
	return Style{
		Name:        "Soft",
		BorderColor: theme.AccentColor(),
		HeaderStyle: lipgloss.NewStyle().Background(bg).Foreground(fg).Bold(true),
	}
}

func styleMinimal() Style {
	_, fg := theme.SurfaceColors()
	return Style{
		Name:        "Minimal",
		BorderColor: theme.FocusColor(),
		HeaderStyle: lipgloss.NewStyle().Foreground(fg).Bold(true),
	}
}

// currentStyle re-resolves a Style preset by name against theme's CURRENT
// colours — grid.go's buildTable calls this instead of using m.style
// directly, so a grid always renders in the live Dark variant regardless
// of when its Style value was captured (WithStyle/SetStyle, a saved
// session's persisted style name, ...).
func currentStyle(name string) Style {
	switch name {
	case "Soft":
		return styleSoft()
	case "Minimal":
		return styleMinimal()
	default:
		return styleLines()
	}
}

// Styles lists the built-in presets in cycling order.
var Styles = []Style{StyleLines, StyleSoft, StyleMinimal}

// ParseStyle finds a built-in preset by Name (e.g. as persisted in a saved
// session), defaulting to StyleLines for an unknown or empty name.
func ParseStyle(name string) Style {
	for _, s := range Styles {
		if s.Name == name {
			return s
		}
	}
	return StyleLines
}

func (s Style) dividerStyle() lipgloss.Style {
	return lipgloss.NewStyle().BorderForeground(s.BorderColor)
}

// activeTitleStyle/inactiveTitleStyle/activeBorderStyle/
// selectedOutlineStyle/inactiveBorderStyle/selectedCellStyle are FUNCTIONS,
// not package vars, for the same reason styleLines/styleSoft/styleMinimal
// above are: every one of them must reflect theme's CURRENT Dark variant
// on every render, not whatever it was when the package first loaded.
func activeTitleStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Foreground(theme.FocusColor())
}
func inactiveTitleStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(theme.MutedColor())
}
func activeBorderStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(theme.FocusColor())
}
func selectedOutlineStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(theme.BorderColor(true))
}
func inactiveBorderStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(theme.BorderColor(false))
}
func selectedCellStyle() lipgloss.Style {
	// Foreground-only (unlike FocusSurfaceColors' background+foreground
	// pair the ROW highlight below uses): this marks the SELECTED COLUMN
	// (h/l), a lighter-weight cue than "the current row" — every cell in
	// that column getting FocusSurfaceColors' full background would
	// visually compete with, not complement, the actual row highlight.
	return lipgloss.NewStyle().Bold(true).Foreground(theme.AccentColor())
}

// columnStyle mirrors DataTug's gridColumnStyle: numeric columns align
// right, the selected column is bold/highlighted.
func columnStyle(col Column, selected bool) lipgloss.Style {
	alignment := lipgloss.Left
	if col.Numeric {
		alignment = lipgloss.Right
	}
	if selected {
		return selectedCellStyle().Align(alignment)
	}
	return lipgloss.NewStyle().Align(alignment)
}

// padAnsiLine pads/truncates an already-styled line to an exact display
// width, ANSI-aware. Ported from DataTug's ui.go.
func padAnsiLine(line string, width int) string {
	if width <= 0 {
		return ""
	}
	line = ansi.Truncate(line, width, "…")
	return line + strings.Repeat(" ", max(0, width-lipgloss.Width(line)))
}

// ansiTruncate is borderLine's seam over ansi.Truncate. ansi.Truncate's own
// guarantee (the result never renders wider than the requested width) makes
// the "still doesn't fit" defensive branch below unreachable through any
// real text this package can construct — see TestBorderLineLabelStillTooWide,
// which swaps this var out to drive that branch directly.
var ansiTruncate = ansi.Truncate

// borderLine draws a horizontal card border with a centered label. Ported
// from DataTug's ui.go.
func borderLine(left, text, right string, width int) string {
	if width <= 0 {
		return ""
	}
	if width < 3 {
		return strings.Repeat("─", max(1, width))
	}
	available := width - 2
	if available < 3 {
		return left + strings.Repeat("─", available) + right
	}
	label := " " + ansiTruncate(text, available-2, "…") + " "
	if lipgloss.Width(label) > available {
		return left + strings.Repeat("─", available) + right
	}
	fill := max(0, available-lipgloss.Width(label))
	return left + label + strings.Repeat("─", fill) + right
}
