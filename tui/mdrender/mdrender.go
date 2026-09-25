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
// glamour.WithStandardStyle: "dark", "light", "notty", "ascii", ...).
// Defaults to "dark", matching DataTug's original renderMarkdown. A product
// that wants to follow theme.Dark can set this from theme.Dark itself (they
// are deliberately not wired together here — glamour's "light"/"dark"
// style names and this package's own terminal-background choice are
// independent axes a product may want to control separately).
var Style = "dark"

// minWordWrap is the narrowest word-wrap width Render ever asks glamour
// for, regardless of how small width is — matching DataTug's original
// max(20, width-4).
const minWordWrap = 20

// wordWrapMargin is subtracted from width before clamping to minWordWrap,
// leaving room for glamour's own margins so rendered lines don't butt
// against a card's border.
const wordWrapMargin = 4

// Render renders text as markdown at width, falling back to the raw text,
// trimmed of surrounding whitespace, on any renderer error — markdown
// rendering is a presentation nicety, never a reason to drop a response.
// It satisfies transcript.MarkdownRenderer's signature directly (pass
// mdrender.Render to chatshell.WithMarkdownRenderer / transcript.
// WithMarkdownRenderer).
func Render(text string, width int) string {
	renderer, err := NewTermRenderer(glamour.WithStandardStyle(Style), glamour.WithWordWrap(max(minWordWrap, width-wordWrapMargin)))
	if err != nil {
		return text
	}
	rendered, err := renderer.Render(text)
	if err != nil {
		return text
	}
	return strings.TrimSpace(rendered)
}
