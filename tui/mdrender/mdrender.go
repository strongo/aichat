// Package mdrender is aichat's shared glamour-backed markdown renderer: the
// single implementation of transcript.MarkdownRenderer every aichat product
// uses, so a markdown-rendered assistant/HTTP-response message looks
// identical everywhere with zero glamour import of its own.
//
// It generalises DataTug chat's own renderMarkdown/newMarkdownRenderer seam
// (datatug-cli/pkg/chat/chatui.go, before this package existed) — same
// fault-injection seam, same "fall back to the raw text on any renderer
// error" behaviour, now shared.
package mdrender

import (
	"strings"

	glamour "charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"

	"github.com/strongo/aichat/tui/theme"
)

// TermRenderer is the narrow seam Render needs from *glamour.TermRenderer —
// just the one method it calls — so a test can fault-inject both the
// constructor (via NewTermRenderer) and the render call itself without a
// real glamour dependency.
type TermRenderer interface {
	Render(string) (string, error)
}

// NewTermRenderer constructs the glamour renderer Render uses. It is a var,
// not a plain function call, so a test can fault-inject either the
// constructor itself (return an error) or wrap the returned TermRenderer to
// fault-inject Render.
var NewTermRenderer = func(options ...glamour.TermRendererOption) (TermRenderer, error) {
	return glamour.NewTermRenderer(options...)
}

// Style selects glamour's built-in style by name (see
// glamour.WithStandardStyle: "dark", "light", "notty", "ascii", ...). Empty
// (the default) tracks tui/theme.Dark automatically — a card's background
// comes from theme, so rendered markdown text must too, or it goes
// near-invisible on a light card (a dark-styled renderer defaulting to pale
// text over theme's light surface, the bug this replaced). Set Style
// explicitly only to pin a specific glamour style regardless of theme.Dark
// (e.g. "notty"/"ascii" for a non-colour terminal).
var Style = ""

// resolvedStyle returns the glamour style Render actually uses: Style
// itself when the caller pinned one, otherwise "dark"/"light" from
// tui/theme.Dark, read fresh on every call so a runtime theme.SetDark
// takes effect on the very next render.
func resolvedStyle() string {
	if Style != "" {
		return Style
	}
	if theme.Dark {
		return "dark"
	}
	return "light"
}

// minWordWrap is the narrowest word-wrap width Render ever asks glamour
// for, regardless of how small width is — matching DataTug's original
// max(20, width-4).
const minWordWrap = 20

// wordWrapMargin is subtracted from width before clamping to minWordWrap,
// leaving room for glamour's own margins so rendered lines don't butt
// against a card's border.
const wordWrapMargin = 4

// codeStyleOverride returns a copy of glamour's own built-in dark/light
// StyleConfig with ONE change: inline `code` no longer carries glamour's
// own fixed colour+background (ANSI-256 color 203, a red/pink, on
// glamour's own grey box in EITHER theme — founder, r12: "verify
// glamour's inline-code colour ... meets the >= 4.5:1 rule"; it clears
// 4.5:1 against glamour's own dark code-background by only a hair, ~4.55:1,
// and fails outright (~2.28:1) against its light one). Color is now
// theme.Hex(theme.AccentColor()) — the SAME accent colour ContrastPairs
// checks against EVERY role card's own background (see ContrastPairs'
// "inline code text" entries) — and BackgroundColor is cleared entirely
// (nil): inline code renders on whatever card surface already surrounds
// it, rather than a background glamour picked with no idea which role's
// tint (or which real terminal background) it will actually sit on.
func codeStyleOverride(dark bool) ansi.StyleConfig {
	cfg := styles.LightStyleConfig
	if dark {
		cfg = styles.DarkStyleConfig
	}
	accent := theme.Hex(theme.AccentColor())
	cfg.Code.Color = &accent
	cfg.Code.BackgroundColor = nil
	return cfg
}

// Render renders text as markdown at width, falling back to the raw text,
// trimmed of surrounding whitespace, on any renderer error — markdown
// rendering is a presentation nicety, never a reason to drop a response.
// It satisfies transcript.MarkdownRenderer's signature directly (pass
// mdrender.Render to chatshell.WithMarkdownRenderer / transcript.
// WithMarkdownRenderer).
func Render(text string, width int) string {
	// codeStyleOverride only replaces glamour's OWN "dark"/"light" styles
	// (resolvedStyle()'s two possible auto-tracked values) -- a caller
	// that pinned Style to something else (glamour's "notty"/"ascii"/a
	// named theme like "dracula") gets that style verbatim via
	// WithStandardStyle, same as before this round: this package has no
	// tui/theme-driven override for those, and guessing one would risk
	// breaking a deliberate non-colour-terminal choice.
	var styleOpt glamour.TermRendererOption
	switch style := resolvedStyle(); style {
	case "dark", "light":
		styleOpt = glamour.WithStyles(codeStyleOverride(style == "dark"))
	default:
		styleOpt = glamour.WithStandardStyle(style)
	}
	renderer, err := NewTermRenderer(styleOpt, glamour.WithWordWrap(max(minWordWrap, width-wordWrapMargin)))
	if err != nil {
		return text
	}
	rendered, err := renderer.Render(text)
	if err != nil {
		return text
	}
	return strings.TrimSpace(rendered)
}
