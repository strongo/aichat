package mdrender

import (
	"errors"
	"strings"
	"testing"

	glamour "charm.land/glamour/v2"

	"github.com/strongo/aichat/tui/theme"
)

func TestRenderBasicMarkdown(t *testing.T) {
	out := Render("# Hello\n\nworld", 80)
	if !strings.Contains(out, "Hello") || !strings.Contains(out, "world") {
		t.Fatalf("Render missing expected content: %q", out)
	}
}

func TestRenderNarrowWidthClampsToMinWordWrap(t *testing.T) {
	// Width so small that width-wordWrapMargin would go negative/below
	// minWordWrap; Render must not panic and must still produce output.
	out := Render("some text", 1)
	if out == "" {
		t.Fatal("Render with tiny width returned empty string")
	}
}

type fakeTermRenderer struct {
	out string
	err error
}

func (f fakeTermRenderer) Render(string) (string, error) { return f.out, f.err }

func TestRenderFallsBackOnConstructorError(t *testing.T) {
	prev := NewTermRenderer
	NewTermRenderer = func(options ...glamour.TermRendererOption) (TermRenderer, error) {
		return nil, errors.New("boom")
	}
	t.Cleanup(func() { NewTermRenderer = prev })

	text := "raw fallback text"
	if got := Render(text, 80); got != text {
		t.Fatalf("Render() = %q, want raw text %q on constructor error", got, text)
	}
}

func TestRenderFallsBackOnRenderError(t *testing.T) {
	prev := NewTermRenderer
	NewTermRenderer = func(options ...glamour.TermRendererOption) (TermRenderer, error) {
		return fakeTermRenderer{err: errors.New("render boom")}, nil
	}
	t.Cleanup(func() { NewTermRenderer = prev })

	text := "raw fallback text 2"
	if got := Render(text, 80); got != text {
		t.Fatalf("Render() = %q, want raw text %q on render error", got, text)
	}
}

func TestRenderTrimsWhitespace(t *testing.T) {
	prev := NewTermRenderer
	NewTermRenderer = func(options ...glamour.TermRendererOption) (TermRenderer, error) {
		return fakeTermRenderer{out: "\n\n  padded  \n\n"}, nil
	}
	t.Cleanup(func() { NewTermRenderer = prev })

	if got := Render("x", 80); got != "padded" {
		t.Fatalf("Render() = %q, want trimmed %q", got, "padded")
	}
}

func TestStyleDefaultsToEmptyAutoTrackingTheme(t *testing.T) {
	if Style != "" {
		t.Fatalf("Style default = %q, want empty (auto, tracks theme.Dark)", Style)
	}
}

// TestResolvedStyleTracksThemeDarkWhenStyleUnset covers the bug this
// replaced: glamour defaulting to a dark style regardless of tui/theme's
// current variant, which rendered assistant markdown near-invisible on a
// light card (pale text on a light background). With Style left at its
// default (""), resolvedStyle must follow theme.Dark on every call.
func TestResolvedStyleTracksThemeDarkWhenStyleUnset(t *testing.T) {
	prevDark := theme.Dark
	t.Cleanup(func() { theme.Dark = prevDark })

	theme.Dark = true
	if got := resolvedStyle(); got != "dark" {
		t.Fatalf("resolvedStyle() with theme.Dark=true = %q, want %q", got, "dark")
	}

	theme.Dark = false
	if got := resolvedStyle(); got != "light" {
		t.Fatalf("resolvedStyle() with theme.Dark=false = %q, want %q", got, "light")
	}
}

// TestResolvedStyleHonoursExplicitOverride covers a caller pinning Style
// regardless of theme.Dark (e.g. "notty"/"ascii" for a non-colour terminal).
func TestResolvedStyleHonoursExplicitOverride(t *testing.T) {
	prevStyle, prevDark := Style, theme.Dark
	t.Cleanup(func() { Style, theme.Dark = prevStyle, prevDark })

	Style = "ascii"
	theme.Dark = false
	if got := resolvedStyle(); got != "ascii" {
		t.Fatalf("resolvedStyle() with explicit Style override = %q, want %q", got, "ascii")
	}
}
