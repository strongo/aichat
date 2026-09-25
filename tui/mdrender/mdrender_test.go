package mdrender

import (
	"errors"
	"strings"
	"testing"

	glamour "charm.land/glamour/v2"
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

func TestStyleDefaultsToDark(t *testing.T) {
	if Style != "dark" {
		t.Fatalf("Style default = %q, want %q", Style, "dark")
	}
}
