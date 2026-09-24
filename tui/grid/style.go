package grid

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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

// Built-in style presets, ported from DataTug's table_style.go.
var (
	StyleLines = Style{
		Name:        "Lines",
		BorderColor: lipgloss.Color("241"),
		HeaderStyle: lipgloss.NewStyle().Background(lipgloss.Color("237")).Foreground(lipgloss.Color("255")).Bold(true),
	}
	StyleSoft = Style{
		Name:        "Soft",
		BorderColor: lipgloss.Color("235"),
		HeaderStyle: lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("250")).Bold(true),
	}
	StyleMinimal = Style{
		Name:        "Minimal",
		BorderColor: lipgloss.Color("232"),
		HeaderStyle: lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Bold(true),
	}
)

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

var (
	activeTitleStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("51"))
	inactiveTitleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	activeBorderStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("51"))
	selectedOutlineStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	inactiveBorderStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	selectedCellStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
)

// columnStyle mirrors DataTug's gridColumnStyle: numeric columns align
// right, the selected column is bold/highlighted.
func columnStyle(col Column, selected bool) lipgloss.Style {
	alignment := lipgloss.Left
	if col.Numeric {
		alignment = lipgloss.Right
	}
	if selected {
		return selectedCellStyle.Align(alignment)
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
	label := " " + ansi.Truncate(text, available-2, "…") + " "
	if lipgloss.Width(label) > available {
		return left + strings.Repeat("─", available) + right
	}
	fill := max(0, available-lipgloss.Width(label))
	return left + label + strings.Repeat("─", fill) + right
}
