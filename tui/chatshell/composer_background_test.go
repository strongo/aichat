package chatshell

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/strongo/aichat/tui/theme"
)

// backgroundSGRPattern matches every background-setting SGR component this
// package or its dependencies could plausibly emit: 24-bit "48;2;r;g;b",
// 256-colour "48;5;n", and the eight standard/bright background codes
// "40"-"47"/"100"-"107" (the exact shape of the r9 "black inner input
// line" regression -- bubbles' default CursorLine style painted a plain
// "40"/"47" standard-colour background, not a themed 48;2;... one).
var backgroundSGRPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// composerBackgroundSGRs returns every DISTINCT background colour actually
// painted somewhere within rendered (a composer's full ComposerFrame
// output), as the raw SGR component strings ("48;2;r;g;b", "40", ...).
// Foreground-only and reset codes are ignored.
func composerBackgroundSGRs(rendered string) map[string]bool {
	found := map[string]bool{}
	for _, seq := range backgroundSGRPattern.FindAllString(rendered, -1) {
		body := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b["), "m")
		if body == "" {
			continue
		}
		parts := strings.Split(body, ";")
		for i := 0; i < len(parts); i++ {
			switch parts[i] {
			case "48":
				// "48;2;r;g;b" (24-bit) or "48;5;n" (256-colour) -- consume
				// the whole component so its own r/g/b/n fields aren't
				// misread as separate top-level codes on the next loop.
				if i+1 < len(parts) && parts[i+1] == "2" && i+4 < len(parts) {
					found[strings.Join(parts[i:i+5], ";")] = true
					i += 4
				} else if i+1 < len(parts) && parts[i+1] == "5" && i+2 < len(parts) {
					found[strings.Join(parts[i:i+3], ";")] = true
					i += 2
				}
			case "40", "41", "42", "43", "44", "45", "46", "47",
				"100", "101", "102", "103", "104", "105", "106", "107":
				found[parts[i]] = true
			}
		}
	}
	return found
}

// fillSGRComponent returns the "48;2;r;g;b" component theme.PaintOver/
// lipgloss would emit for bg, in 8-bit-per-channel decimal -- the ONE
// background composerBackgroundSGRs is allowed to find for a given
// composer render.
func fillSGRComponent(bg interface{ RGBA() (r, g, b, a uint32) }) string {
	r, g, b, _ := bg.RGBA()
	return fmt.Sprintf("48;2;%d;%d;%d", r/257, g/257, b/257)
}

// TestComposerPaintsExactlyOneBackground covers the founder's verbatim r9
// correction: "Text background in composer should match background of the
// composer... no second background (no black/darker input line, no
// highlight band behind the text)". Every background colour ANYWHERE in a
// rendered composer -- padding, the prompt column, typed text, the
// placeholder, the cursor line -- must be the SAME one composer fill
// colour (SurfaceColors unfocused, theme.ComposerFocusColors focused), in
// both Dark variants, both with and without typed text. This is the
// regression test for bubbles' own default textarea.Styles, whose
// Focused.CursorLine painted a distinct opaque background ("40"/"47")
// straight across the whole input line -- composerTextAreaStyles must
// never regress back to setting ANY background of its own.
func TestComposerPaintsExactlyOneBackground(t *testing.T) {
	prevDark := theme.Dark
	t.Cleanup(func() { theme.SetDark(prevDark) })
	for _, dark := range []bool{true, false} {
		theme.SetDark(dark)
		for _, focused := range []bool{true, false} {
			for _, typed := range []bool{true, false} {
				h := &fakeHandler{}
				m := newTestShell(h)
				if typed {
					m.input.SetValue("Show me all invoices from Acme")
				}
				var wantBG interface{ RGBA() (r, g, b, a uint32) }
				if focused {
					bg, _ := theme.ComposerFocusColors()
					wantBG = bg
				} else {
					bg, _ := theme.ComposerColors()
					wantBG = bg
				}
				m.input.SetStyles(composerTextAreaStyles(focused))
				rendered := theme.ComposerFrame(m.chatWidth(), m.input.View(), focused)
				got := composerBackgroundSGRs(rendered)
				want := fillSGRComponent(wantBG)
				delete(got, want)
				// The left accent bar paints FocusColor() when focused --
				// the ONE other background this package's own design
				// intends inside ComposerFrame's rendered block (it is the
				// accent bar, never a text/cursor-line background). Any
				// OTHER surviving entry is the regression this test
				// exists to catch.
				if focused {
					accent := fillSGRComponent(theme.FocusColor())
					delete(got, accent)
				}
				if len(got) != 0 {
					t.Fatalf("dark=%v focused=%v typed=%v: found background(s) other than the composer fill (%s) and, if focused, the accent bar: %v\nrendered=%q",
						dark, focused, typed, want, got, rendered)
				}
			}
		}
	}
}
